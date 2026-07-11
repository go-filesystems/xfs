package filesystem_xfs

import (
	"errors"
	"io"
	"testing"
)

var errInjected = errors.New("injected failure")

func TestReflinkEmptyAndLocalPaths(t *testing.T) {
	fs := formatReflink(t, 2)
	defer fs.Close()

	// An empty file is stored in local (inline) format; reflinking it is a
	// plain copy through reflinkCopyLocal.
	if err := fs.WriteFile("/empty", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.Reflink("/empty", "/empty-clone"); err != nil {
		t.Fatalf("Reflink(empty): %v", err)
	}
	if got, err := fs.ReadFile("/empty-clone"); err != nil || len(got) != 0 {
		t.Fatalf("empty clone = %q %v", got, err)
	}

	// Overwriting a reflinked file with an empty payload exercises the empty
	// branch of the COW break.
	big := make([]byte, 9000)
	if err := fs.WriteFile("/f", big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.Reflink("/f", "/f2"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/f2", nil, 0o644); err != nil {
		t.Fatalf("empty COW break: %v", err)
	}
	if got, _ := fs.ReadFile("/f2"); len(got) != 0 {
		t.Fatalf("f2 not empty after overwrite: %d bytes", len(got))
	}
	// The original is intact.
	if got, _ := fs.ReadFile("/f"); len(got) != 9000 {
		t.Fatalf("original f truncated: %d", len(got))
	}
}

func TestReflinkBtreeSourceUnsupported(t *testing.T) {
	fs := formatReflink(t, 2)
	defer fs.Close()
	if err := fs.WriteFile("/f", []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Force the source to look like a btree-fork inode.
	orig := reflinkPathLookup
	defer func() { reflinkPathLookup = orig }()
	reflinkPathLookup = func(rw io.ReaderAt, off int64, sb *superblock, p string) (*inode, error) {
		in, err := orig(rw, off, sb, p)
		if err != nil {
			return nil, err
		}
		if p == "/f" {
			in.format = inodeFmtBtree
		}
		return in, nil
	}
	if err := fs.Reflink("/f", "/clone"); err == nil {
		t.Fatal("btree-fork reflink source: want unsupported error")
	}
}

func TestReflinkAllocInodeFailure(t *testing.T) {
	fs := formatReflink(t, 2)
	defer fs.Close()
	if err := fs.WriteFile("/f", make([]byte, 9000), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := reflinkAllocInode
	defer func() { reflinkAllocInode = orig }()
	reflinkAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) {
		return 0, errInjected
	}
	if err := fs.Reflink("/f", "/clone"); err == nil {
		t.Fatal("alloc inode failure: want error")
	}
}

func TestReflinkWriteInodeFailure(t *testing.T) {
	fs := formatReflink(t, 2)
	defer fs.Close()
	if err := fs.WriteFile("/f", make([]byte, 9000), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := reflinkWriteInode
	defer func() { reflinkWriteInode = orig }()
	reflinkWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return errInjected }
	if err := fs.Reflink("/f", "/clone"); err == nil {
		t.Fatal("write inode failure: want error")
	}
}

func TestReflinkAddDirEntryFailure(t *testing.T) {
	fs := formatReflink(t, 2)
	defer fs.Close()
	if err := fs.WriteFile("/f", make([]byte, 9000), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := reflinkAddDirEntry
	defer func() { reflinkAddDirEntry = orig }()
	reflinkAddDirEntry = func(readerWriterAt, int64, *superblock, *inode, uint64, string, uint8) error {
		return errInjected
	}
	if err := fs.Reflink("/f", "/clone"); err == nil {
		t.Fatal("add dir entry failure: want error")
	}
}

func TestRefcountReadErrorPaths(t *testing.T) {
	fs := formatReflink(t, 1)
	defer fs.Close()
	// AGF read failure propagates.
	orig := reflinkAGFBlock
	defer func() { reflinkAGFBlock = orig }()
	reflinkAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
		return nil, errInjected
	}
	if _, err := refcountReadRecs(fs.f, fs.partOffset, fs.sb, 0); err == nil {
		t.Fatal("refcountReadRecs AGF error: want error")
	}
}

func TestRefcountMultiLevelUnsupported(t *testing.T) {
	fs := formatReflink(t, 1)
	defer fs.Close()
	// Fake an AGF that reports a 2-level refcountbt.
	orig := reflinkAGFBlock
	defer func() { reflinkAGFBlock = orig }()
	reflinkAGFBlock = func(rw io.ReaderAt, off int64, sb *superblock, ag uint32) ([]byte, error) {
		agf, err := orig(rw, off, sb, ag)
		if err != nil {
			return nil, err
		}
		// Bump refcount level to 2 (multi-level unsupported).
		agf[agfOffRefcntLevel+3] = 2
		return agf, nil
	}
	if _, err := refcountReadRecs(fs.f, fs.partOffset, fs.sb, 0); !errors.Is(err, errRefcountFull) {
		t.Fatalf("multi-level refcountbt = %v, want errRefcountFull", err)
	}
}

func TestMinMax32(t *testing.T) {
	if max32(3, 7) != 7 || max32(9, 2) != 9 {
		t.Fatal("max32 wrong")
	}
	if min32(3, 7) != 3 || min32(9, 2) != 2 {
		t.Fatal("min32 wrong")
	}
}

func TestRefcClipInsideOutside(t *testing.T) {
	// A record wholly outside [s,e) is preserved untouched, and one straddling
	// the range is split into inside + outside parts.
	recs := []refcRec{{0, 5, 2}, {100, 20, 3}}
	out := refcIncrRange(recs, 110, 5) // touches only the second record
	// Expect: {0,5,2} kept, {100,10,3}, {110,5,4}, {115,5,3}
	want := []refcRec{{0, 5, 2}, {100, 10, 3}, {110, 5, 4}, {115, 5, 3}}
	if !equalRecs(out, want) {
		t.Fatalf("clip outside/straddle: got %+v want %+v", out, want)
	}
}
