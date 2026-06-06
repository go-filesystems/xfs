package filesystem_xfs

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestXfsSymlink_RoundtripShortTarget(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	const target = "/etc/passwd"
	if err := fs.Symlink(target, "/lnk"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	got, err := fs.ReadLink("/lnk")
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if got != target {
		t.Errorf("ReadLink = %q, want %q", got, target)
	}

	st, err := fs.ExtendedStat("/lnk")
	if err != nil {
		t.Fatalf("ExtendedStat: %v", err)
	}
	if !st.IsSymlink() {
		t.Errorf("IsSymlink false; mode = 0x%04x", st.Mode)
	}
	if st.Size != uint64(len(target)) {
		t.Errorf("Size = %d, want %d", st.Size, len(target))
	}
	if st.Mode&0o777 != 0o777 {
		t.Errorf("perm bits = 0o%o, want 0o777 (POSIX symlinks)", st.Mode&0o777)
	}
	if st.NLink != 1 {
		t.Errorf("NLink = %d, want 1", st.NLink)
	}
}

func TestXfsSymlink_RoundtripLongTarget(t *testing.T) {
	// Long enough to spill out of any plausible inode literal area
	// and force the extent path.
	target := "/" + strings.Repeat("very-long-segment/", 200) + "leaf"
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.Symlink(target, "/big"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	got, err := fs.ReadLink("/big")
	if err != nil {
		t.Fatalf("ReadLink: %v", err)
	}
	if got != target {
		t.Errorf("ReadLink mismatch: got %d bytes, want %d", len(got), len(target))
	}
}

func TestXfsSymlink_AlreadyExists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.WriteFile("/x", []byte("hi"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Symlink("/anywhere", "/x"); err == nil {
		t.Error("Symlink onto existing path unexpectedly succeeded")
	}
}

func TestXfsSymlink_MissingParent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.Symlink("/anywhere", "/no/such/dir/link"); err == nil {
		t.Error("Symlink with missing parent unexpectedly succeeded")
	}
}

func TestXfsSymlink_PersistsAcrossReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	const target = "/some/where"
	if err := fs.Symlink(target, "/persist"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	fs.Close()

	fs2, err := Open(p, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs2.Close()
	got, err := fs2.ReadLink("/persist")
	if err != nil {
		t.Fatalf("ReadLink after reopen: %v", err)
	}
	if got != target {
		t.Errorf("after reopen ReadLink = %q, want %q", got, target)
	}
}
