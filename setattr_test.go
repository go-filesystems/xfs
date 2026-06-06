package filesystem_xfs

import (
	"path/filepath"
	"testing"
	"time"
)

// formatXfsAndWrite creates a fresh XFS image at TempDir + "disk.img", writes
// a regular file `/target`, closes the fs and returns the path. Used by the
// setattr / ExtendedStat tests below.
func formatXfsAndWrite(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := fs.WriteFile("/target", []byte("hi"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fs.Close()
	return p
}

func TestXfsExtendedStat_FreshFile(t *testing.T) {
	// initInodeV3 stamps nlink + four timestamps at creation, so we can
	// now assert the full inode metadata shape of a just-written file.
	before := time.Now().UTC().Add(-2 * time.Second)
	img := formatXfsAndWrite(t)
	after := time.Now().UTC().Add(2 * time.Second)

	fs, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	st, err := fs.ExtendedStat("/target")
	if err != nil {
		t.Fatalf("ExtendedStat: %v", err)
	}
	if !st.IsRegular() {
		t.Errorf("IsRegular() false; mode = 0x%04x", st.Mode)
	}
	if st.Size != 2 {
		t.Errorf("Size = %d, want 2", st.Size)
	}
	if st.Mode&0o777 != 0o600 {
		t.Errorf("perm bits = 0o%o, want 0o600", st.Mode&0o777)
	}
	if st.NLink != 1 {
		t.Errorf("NLink = %d, want 1 for a freshly-written regular file", st.NLink)
	}
	for _, ts := range []struct {
		name string
		t    time.Time
	}{{"atime", st.ATime}, {"mtime", st.MTime}, {"ctime", st.CTime}, {"crtime", st.CRTime}} {
		if ts.t.Before(before) || ts.t.After(after) {
			t.Errorf("%s = %v, not within [%v, %v]", ts.name, ts.t, before, after)
		}
	}
}

func TestXfsExtendedStat_FreshDirHasNlinkTwo(t *testing.T) {
	// A fresh directory should report nlink=2 (parent entry + "." self).
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.MkDir("/sub", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	st, err := fs.ExtendedStat("/sub")
	if err != nil {
		t.Fatalf("ExtendedStat: %v", err)
	}
	if !st.IsDir() {
		t.Errorf("IsDir() false; mode = 0x%04x", st.Mode)
	}
	if st.NLink != 2 {
		t.Errorf("fresh empty dir NLink = %d, want 2", st.NLink)
	}
}

func TestXfsChmod_UpdatesPermBitsKeepsType(t *testing.T) {
	img := formatXfsAndWrite(t)
	fs, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.Chmod("/target", 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	st, _ := fs.ExtendedStat("/target")
	if st.Mode&0o7777 != 0o755 {
		t.Errorf("perm bits after Chmod = 0o%o, want 0o755", st.Mode&0o7777)
	}
	if !st.IsRegular() {
		t.Errorf("Chmod clobbered file-type bit: mode = 0x%04x", st.Mode)
	}
}

func TestXfsChown_UpdatesUIDGID(t *testing.T) {
	img := formatXfsAndWrite(t)
	fs, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	const wantUID, wantGID = 1234, 5678
	if err := fs.Chown("/target", wantUID, wantGID); err != nil {
		t.Fatalf("Chown: %v", err)
	}
	st, _ := fs.ExtendedStat("/target")
	if st.UID != wantUID || st.GID != wantGID {
		t.Errorf("after Chown uid=%d gid=%d, want %d/%d", st.UID, st.GID, wantUID, wantGID)
	}
}

func TestXfsChtimes_UpdatesATimeMTime_BumpsCTime(t *testing.T) {
	img := formatXfsAndWrite(t)
	fs, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	before, _ := fs.ExtendedStat("/target")
	time.Sleep(2 * time.Millisecond)

	want := time.Date(2010, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := fs.Chtimes("/target", want, want); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	after, _ := fs.ExtendedStat("/target")
	if !after.ATime.Equal(want) {
		t.Errorf("atime = %v, want %v", after.ATime, want)
	}
	if !after.MTime.Equal(want) {
		t.Errorf("mtime = %v, want %v", after.MTime, want)
	}
	if !after.CTime.After(before.CTime) {
		t.Errorf("ctime didn't advance: %v → %v", before.CTime, after.CTime)
	}
}

func TestXfsSetAttr_MissingPath(t *testing.T) {
	img := formatXfsAndWrite(t)
	fs, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	if err := fs.Chmod("/no/such/file", 0o644); err == nil {
		t.Error("Chmod on missing path unexpectedly succeeded")
	}
	if err := fs.Chown("/no/such/file", 1, 1); err == nil {
		t.Error("Chown on missing path unexpectedly succeeded")
	}
	if err := fs.Chtimes("/no/such/file", time.Now(), time.Now()); err == nil {
		t.Error("Chtimes on missing path unexpectedly succeeded")
	}
	if _, err := fs.ExtendedStat("/no/such/file"); err == nil {
		t.Error("ExtendedStat on missing path unexpectedly succeeded")
	}
}
