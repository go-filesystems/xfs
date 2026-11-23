package filesystem_xfs

import (
	"errors"
	"io"
	"path/filepath"
	"testing"
)

// checkTreeFS builds /d holding a subdirectory /d/s and a symlink /d/f (a
// non-directory entry), and returns the writer.
func checkTreeFS(t *testing.T) *xfsFS {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.img")
	fsi, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	t.Cleanup(func() { fsi.Close() })
	for _, d := range []string{"/d", "/d/s"} {
		if err := fsi.MkDir(d, 0o755); err != nil {
			t.Fatalf("MkDir(%s): %v", d, err)
		}
	}
	if err := fsi.Symlink("x", "/d/f"); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	return fsi.(*xfsFS)
}

// A missing parent or entry is the idempotent case: checkDirTree answers nil
// and goes no further (deleteDir then returns nil for it).
func TestCheckDirTreeStopsOnMissingParent(t *testing.T) {
	w := checkTreeFS(t)
	oldP, oldL := deletePathLookup, deleteLookupInDir
	t.Cleanup(func() { deletePathLookup, deleteLookupInDir = oldP, oldL })
	deletePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
		return stubInode(), errInjected
	}
	calls := 0
	deleteLookupInDir = func(r io.ReaderAt, off int64, sb *superblock, in *inode, name string) (uint64, error) {
		calls++
		return oldL(r, off, sb, in, name)
	}
	if err := checkDirTree(w.f, w.partOffset, w.sb, "/d"); err != nil {
		t.Fatalf("checkDirTree = %v, want nil", err)
	}
	if calls != 0 {
		t.Fatalf("entry looked up %d time(s) after the parent lookup failed", calls)
	}
}

func TestCheckDirTreeStopsOnMissingEntry(t *testing.T) {
	w := checkTreeFS(t)
	oldL, oldI := deleteLookupInDir, deleteReadInode
	t.Cleanup(func() { deleteLookupInDir, deleteReadInode = oldL, oldI })
	deleteLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) {
		return 0, errInjected
	}
	calls := 0
	deleteReadInode = func(r io.ReaderAt, off int64, sb *superblock, ino uint64) (*inode, error) {
		calls++
		return oldI(r, off, sb, ino)
	}
	if err := checkDirTree(w.f, w.partOffset, w.sb, "/d"); err != nil {
		t.Fatalf("checkDirTree = %v, want nil", err)
	}
	if calls != 0 {
		t.Fatalf("inode read %d time(s) after the entry lookup failed", calls)
	}
}

func TestCheckDirTreeReportsReadErrors(t *testing.T) {
	w := checkTreeFS(t)
	oldI, oldD := deleteReadInode, deleteReadDir
	t.Cleanup(func() { deleteReadInode, deleteReadDir = oldI, oldD })

	deleteReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
		return stubInode(), errInjected
	}
	if err := checkDirTree(w.f, w.partOffset, w.sb, "/d"); !errors.Is(err, errInjected) {
		t.Fatalf("inode read error: checkDirTree = %v, want the injected error", err)
	}
	deleteReadInode = oldI

	deleteReadDir = func(io.ReaderAt, int64, *superblock, *inode) ([]DirEntry, error) {
		return nil, errInjected
	}
	if err := checkDirTree(w.f, w.partOffset, w.sb, "/d"); !errors.Is(err, errInjected) {
		t.Fatalf("dir read error: checkDirTree = %v, want the injected error", err)
	}
}

// Only directories are listed: an entry LABELLED a directory (ftype 2) whose
// inode is the symlink /d/f must not be read as one.
func TestCheckDirTreeListsOnlyDirectories(t *testing.T) {
	w := checkTreeFS(t)
	dIn, err := pathLookup(w.f, w.partOffset, w.sb, "/d")
	if err != nil {
		t.Fatal(err)
	}
	fIno, err := lookupInDir(w.f, w.partOffset, w.sb, dIn, "f")
	if err != nil {
		t.Fatal(err)
	}
	if err := addDirEntry(w.f, w.partOffset, w.sb, dIn, fIno, "bad", 2); err != nil {
		t.Fatal(err)
	}
	oldD := deleteReadDir
	t.Cleanup(func() { deleteReadDir = oldD })
	calls := 0
	deleteReadDir = func(r io.ReaderAt, off int64, sb *superblock, in *inode) ([]DirEntry, error) {
		calls++
		return oldD(r, off, sb, in)
	}
	if err := checkDirTree(w.f, w.partOffset, w.sb, "/d"); err != nil {
		t.Fatalf("checkDirTree = %v", err)
	}
	if calls != 2 {
		t.Fatalf("directories listed = %d, want 2 (/d and /d/s)", calls)
	}
}
