package filesystem_xfs

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestXfsLink_BumpsNlinkAndSharesInode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.WriteFile("/a", []byte("payload"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	before, _ := fs.ExtendedStat("/a")
	if before.NLink != 1 {
		t.Fatalf("initial NLink = %d, want 1", before.NLink)
	}
	if err := fs.Link("/a", "/b"); err != nil {
		t.Fatalf("Link: %v", err)
	}

	sa, _ := fs.ExtendedStat("/a")
	sb, _ := fs.ExtendedStat("/b")
	if sa.Inode != sb.Inode {
		t.Errorf("inode mismatch: /a=%d /b=%d", sa.Inode, sb.Inode)
	}
	if sa.NLink != 2 {
		t.Errorf("/a NLink after link = %d, want 2", sa.NLink)
	}
	if sb.NLink != 2 {
		t.Errorf("/b NLink after link = %d, want 2", sb.NLink)
	}
	if !sa.CTime.After(before.CTime) {
		t.Errorf("ctime not bumped on link: %v -> %v", before.CTime, sa.CTime)
	}
}

func TestXfsLink_BothPathsReadSameContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	data := []byte("shared body")
	if err := fs.WriteFile("/orig", data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Link("/orig", "/dup"); err != nil {
		t.Fatalf("Link: %v", err)
	}

	for _, name := range []string{"/orig", "/dup"} {
		got, err := fs.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("ReadFile %s = %q, want %q", name, got, data)
		}
	}
}

func TestXfsLink_RejectsDirectory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.MkDir("/d", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.Link("/d", "/d2"); err == nil {
		t.Error("Link on directory unexpectedly succeeded (POSIX forbids)")
	}
}

func TestXfsLink_RejectsExistingDestination(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.WriteFile("/x", []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.WriteFile("/y", []byte("y"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Link("/x", "/y"); err == nil {
		t.Error("Link onto existing path unexpectedly succeeded")
	}
}

func TestXfsLink_MissingSource(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.Link("/nope", "/b"); err == nil {
		t.Error("Link with missing source unexpectedly succeeded")
	}
}

func TestXfsLink_MissingDestParent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.WriteFile("/x", []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Link("/x", "/no/such/dir/b"); err == nil {
		t.Error("Link with missing dst parent unexpectedly succeeded")
	}
}

func TestXfsLink_PersistsAcrossReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := fs.WriteFile("/a", []byte("persist"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Link("/a", "/b"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	fs.Close()

	fs2, err := Open(p, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs2.Close()
	got, err := fs2.ReadFile("/b")
	if err != nil {
		t.Fatalf("ReadFile after reopen: %v", err)
	}
	if !bytes.Equal(got, []byte("persist")) {
		t.Errorf("after reopen /b = %q, want %q", got, "persist")
	}
	st, _ := fs2.ExtendedStat("/b")
	if st.NLink != 2 {
		t.Errorf("after reopen /b NLink = %d, want 2", st.NLink)
	}
}
