package filesystem_xfs

import (
	"io"
	"testing"
)

// TestReflinkDeleteAllFreesBlocks deletes every reference so the final delete
// drives refcountFreeExtent down to actually freeing the shared blocks.
func TestReflinkDeleteAllFreesBlocks(t *testing.T) {
	fs := formatReflink(t, 2)
	defer fs.Close()
	data := make([]byte, 12000)
	if err := fs.WriteFile("/a", data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.Reflink("/a", "/b"); err != nil {
		t.Fatal(err)
	}
	// Delete /a: refcount 2 -> 1, records removed, blocks NOT freed (b owns them).
	if err := fs.DeleteFile("/a"); err != nil {
		t.Fatal(err)
	}
	// Delete /b: blocks are now singly owned (not in the refcountbt) -> freed.
	if err := fs.DeleteFile("/b"); err != nil {
		t.Fatal(err)
	}
	// The refcountbt is empty again.
	recs, err := refcountReadRecs(fs.f, fs.partOffset, fs.sb, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("refcountbt not empty after delete-all: %+v", recs)
	}
}

func TestRefcountShareWriteFailure(t *testing.T) {
	fs := formatReflink(t, 1)
	defer fs.Close()
	orig := reflinkWriteAGBTree
	defer func() { reflinkWriteAGBTree = orig }()
	reflinkWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error {
		return errInjected
	}
	blk := fs.sb.agAbsBlock(0, 100)
	if err := refcountShareExtent(fs.f, fs.partOffset, fs.sb, blk, 4); err == nil {
		t.Fatal("share write failure: want error")
	}
}

func TestRefcountFreeReadFailure(t *testing.T) {
	fs := formatReflink(t, 1)
	defer fs.Close()
	orig := reflinkAGFBlock
	defer func() { reflinkAGFBlock = orig }()
	reflinkAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
		return nil, errInjected
	}
	blk := fs.sb.agAbsBlock(0, 100)
	if err := refcountFreeExtent(fs.f, fs.partOffset, fs.sb, blk, 4); err == nil {
		t.Fatal("free read failure: want error")
	}
}

func TestRefcountFreeBlocksFailure(t *testing.T) {
	fs := formatReflink(t, 1)
	defer fs.Close()
	// Seed a shared record then free a wider range so the unshared tail must be
	// released, failing on the free-blocks call.
	if err := refcountWriteRecs(fs.f, fs.partOffset, fs.sb, 0, []refcRec{{100, 4, 2}}); err != nil {
		t.Fatal(err)
	}
	orig := reflinkFreeBlocks
	defer func() { reflinkFreeBlocks = orig }()
	reflinkFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error {
		return errInjected
	}
	blk := fs.sb.agAbsBlock(0, 100)
	if err := refcountFreeExtent(fs.f, fs.partOffset, fs.sb, blk, 8); err == nil {
		t.Fatal("free blocks failure: want error")
	}
}

func TestRefcountReadBadMagic(t *testing.T) {
	fs := formatReflink(t, 1)
	defer fs.Close()
	orig := reflinkReadAGBlock
	defer func() { reflinkReadAGBlock = orig }()
	reflinkReadAGBlock = func(r io.ReaderAt, off int64, sb *superblock, ag, rel uint32) ([]byte, error) {
		blk, err := orig(r, off, sb, ag, rel)
		if err != nil {
			return nil, err
		}
		blk[0] = 0 // corrupt the magic
		return blk, nil
	}
	if _, err := refcountReadRecs(fs.f, fs.partOffset, fs.sb, 0); err == nil {
		t.Fatal("bad refcountbt magic: want error")
	}
}

func TestRefcountWriteReadFailure(t *testing.T) {
	fs := formatReflink(t, 1)
	defer fs.Close()
	orig := reflinkReadAGBlock
	defer func() { reflinkReadAGBlock = orig }()
	reflinkReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
		return nil, errInjected
	}
	if err := refcountWriteRecs(fs.f, fs.partOffset, fs.sb, 0, []refcRec{{1, 1, 2}}); err == nil {
		t.Fatal("write-recs read failure: want error")
	}
}

func TestReflinkInlineExtsFailure(t *testing.T) {
	fs := formatReflink(t, 2)
	defer fs.Close()
	if err := fs.WriteFile("/f", make([]byte, 9000), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := reflinkInlineExts
	defer func() { reflinkInlineExts = orig }()
	reflinkInlineExts = func(*inode) ([]extent, error) { return nil, errInjected }
	if err := fs.Reflink("/f", "/clone"); err == nil {
		t.Fatal("inlineExts failure: want error")
	}
}

func TestReflinkWriteExtListFailure(t *testing.T) {
	fs := formatReflink(t, 2)
	defer fs.Close()
	if err := fs.WriteFile("/f", make([]byte, 9000), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := reflinkWriteExtList
	defer func() { reflinkWriteExtList = orig }()
	reflinkWriteExtList = func(*inode, []extent) error { return errInjected }
	if err := fs.Reflink("/f", "/clone"); err == nil {
		t.Fatal("writeExtList failure: want error")
	}
}
