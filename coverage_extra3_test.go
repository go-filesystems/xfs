package filesystem_xfs

import (
	"io"
	"testing"
)

// TestReflinkAllocInodeFallback forces the destination inode allocation to fail
// in the parent's AG so reflinkFile falls back to another AG.
func TestReflinkAllocInodeFallback(t *testing.T) {
	fs := formatReflink(t, 3)
	defer fs.Close()
	if err := fs.WriteFile("/f", make([]byte, 9000), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := reflinkAllocInode
	defer func() { reflinkAllocInode = orig }()
	reflinkAllocInode = func(rw readerWriterAt, off int64, sb *superblock, ag uint32) (uint64, error) {
		if ag == 0 {
			return 0, errInjected // parent AG fails
		}
		return orig(rw, off, sb, ag)
	}
	if err := fs.Reflink("/f", "/clone"); err != nil {
		t.Fatalf("Reflink with AG fallback: %v", err)
	}
	if _, err := fs.ReadFile("/clone"); err != nil {
		t.Fatalf("clone unreadable: %v", err)
	}
}

// TestRefcountFreeWriteRecsFailure fails the refcountbt write during a COW free.
func TestRefcountFreeWriteRecsFailure(t *testing.T) {
	fs := formatReflink(t, 1)
	defer fs.Close()
	if err := refcountWriteRecs(fs.f, fs.partOffset, fs.sb, 0, []refcRec{{100, 4, 2}}); err != nil {
		t.Fatal(err)
	}
	orig := reflinkWriteAGBTree
	defer func() { reflinkWriteAGBTree = orig }()
	reflinkWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error {
		return errInjected
	}
	blk := fs.sb.agAbsBlock(0, 100)
	if err := refcountFreeExtent(fs.f, fs.partOffset, fs.sb, blk, 4); err == nil {
		t.Fatal("free write-recs failure: want error")
	}
}

// TestReflinkShareExtentWriteFailureViaClone drives the refcountShareExtent
// error path through a full Reflink call.
func TestReflinkShareExtentFailureViaClone(t *testing.T) {
	fs := formatReflink(t, 2)
	defer fs.Close()
	if err := fs.WriteFile("/f", make([]byte, 9000), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := reflinkWriteAGBTree
	defer func() { reflinkWriteAGBTree = orig }()
	reflinkWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error {
		return errInjected
	}
	if err := fs.Reflink("/f", "/clone"); err == nil {
		t.Fatal("Reflink with refcount share failure: want error")
	}
}
