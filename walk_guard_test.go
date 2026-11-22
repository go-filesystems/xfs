package filesystem_xfs

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-volumes/safeio"
)

// Regression for the 2026-11-22 audit: pathLookupResolve bounded symlink DEPTH
// but not the TOTAL links followed. A chain lNN -> "/lNN+1/lNN+1", the last one
// -> "/", doubles the lookups at every level: 39 links cost 2^38 directory
// lookups for one ListDir("/l00"). The shared safeio.SymlinkBudget refuses it.
func TestSymlinkFanoutIsRefused(t *testing.T) {
	const depth = 39
	p := filepath.Join(t.TempDir(), "disk.img")
	w, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	for k := 0; k < depth; k++ {
		target := "/"
		if k < depth-1 {
			target = fmt.Sprintf("/l%02d/l%02d", k+1, k+1)
		}
		if err := w.Symlink(target, fmt.Sprintf("/l%02d", k)); err != nil {
			t.Fatalf("Symlink %d: %v", k, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fs, err := Open(p, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	// The tail of the chain still resolves.
	if _, err := fs.ListDir(fmt.Sprintf("/l%02d", depth-1)); err != nil {
		t.Fatalf("tail link: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := fs.ListDir("/l00")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrSymlinkLoop) || !errors.Is(err, safeio.ErrSymlinkLoop) {
			t.Fatalf("ListDir(/l00) = %v, want ErrSymlinkLoop", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("ListDir(/l00) still running after 20s (2^%d lookups)", depth-1)
	}
}

// Regression for the 2026-11-22 audit: a directory entry of type "directory"
// naming an ANCESTOR inode (a directory hard link, which a hostile image simply
// contains) made DeleteDir walk /a/b/up/b/up/... for ever holding the fs lock.
// It must now return safeio.ErrCycle and leave the tree untouched.
func TestDeleteDirCycleIsRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fsi, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	for _, d := range []string{"/a", "/a/b"} {
		if err := fsi.MkDir(d, 0o755); err != nil {
			t.Fatalf("MkDir(%s): %v", d, err)
		}
	}
	w := fsi.(*xfsFS)
	aIn, err := pathLookup(w.f, w.partOffset, w.sb, "/a")
	if err != nil {
		t.Fatalf("pathLookup(/a): %v", err)
	}
	bIn, err := pathLookup(w.f, w.partOffset, w.sb, "/a/b")
	if err != nil {
		t.Fatalf("pathLookup(/a/b): %v", err)
	}
	// Hostile entry: /a/b/up is a directory entry (ftype 2) for inode(/a).
	if err := addDirEntry(w.f, w.partOffset, w.sb, bIn, aIn.num, "up", 2); err != nil {
		t.Fatalf("addDirEntry: %v", err)
	}
	if err := fsi.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs, err := Open(p, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	done := make(chan error, 1)
	go func() { done <- fs.DeleteDir("/a") }()
	select {
	case err := <-done:
		if !errors.Is(err, safeio.ErrCycle) {
			t.Fatalf("DeleteDir(/a) = %v, want safeio.ErrCycle", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("DeleteDir(/a) still running after 20s")
	}
	if _, err := fs.ListDir("/a/b"); err != nil {
		t.Fatalf("tree was modified by the refused delete: %v", err)
	}
	// A healthy tree still deletes.
	if err := fs.MkDir("/ok", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fs.DeleteDir("/ok"); err != nil {
		t.Fatalf("DeleteDir(/ok): %v", err)
	}
}
