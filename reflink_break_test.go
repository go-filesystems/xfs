package filesystem_xfs

import (
	"io"
	"testing"
)

// setupReflinkPair formats a reflink fs and returns it with /f reflinked to /f2.
func setupReflinkPair(t *testing.T) *xfsFS {
	t.Helper()
	fs := formatReflink(t, 2)
	if err := fs.WriteFile("/f", make([]byte, 9000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.Reflink("/f", "/f2"); err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestCOWBreakRefcountFreeFailure(t *testing.T) {
	fs := setupReflinkPair(t)
	defer fs.Close()
	orig := reflinkAGFBlock
	defer func() { reflinkAGFBlock = orig }()
	reflinkAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
		return nil, errInjected
	}
	if err := fs.WriteFile("/f2", make([]byte, 5000), 0o644); err == nil {
		t.Fatal("COW break with refcount free failure: want error")
	}
}

func TestCOWBreakWriteExtListFailure(t *testing.T) {
	fs := setupReflinkPair(t)
	defer fs.Close()
	orig := reflinkWriteExtList
	defer func() { reflinkWriteExtList = orig }()
	reflinkWriteExtList = func(*inode, []extent) error { return errInjected }
	if err := fs.WriteFile("/f2", make([]byte, 5000), 0o644); err == nil {
		t.Fatal("COW break with extent-list write failure: want error")
	}
}

func TestCOWBreakWriteInodeFailure(t *testing.T) {
	fs := setupReflinkPair(t)
	defer fs.Close()
	orig := reflinkWriteInode
	defer func() { reflinkWriteInode = orig }()
	reflinkWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return errInjected }
	if err := fs.WriteFile("/f2", make([]byte, 5000), 0o644); err == nil {
		t.Fatal("COW break with inode write failure: want error")
	}
}

func TestCOWBreakEmptyInodeFailure(t *testing.T) {
	fs := setupReflinkPair(t)
	defer fs.Close()
	orig := reflinkWriteInode
	defer func() { reflinkWriteInode = orig }()
	reflinkWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return errInjected }
	// Empty overwrite takes the empty branch of the COW break.
	if err := fs.WriteFile("/f2", nil, 0o644); err == nil {
		t.Fatal("empty COW break with inode write failure: want error")
	}
}

func TestQuotaWriteDquotsNoExtents(t *testing.T) {
	fs := formatQuota(t, QuotaConfig{User: true})
	defer fs.Close()
	// Make the quota inode read back with an empty data fork.
	orig := quotaReadInode
	defer func() { quotaReadInode = orig }()
	quotaReadInode = func(r io.ReaderAt, off int64, sb *superblock, ino uint64) (*inode, error) {
		in, err := orig(r, off, sb, ino)
		if err != nil {
			return nil, err
		}
		in.nExts = 0 // pretend it has no extents
		return in, nil
	}
	err := quotaWriteDquots(fs.f, fs.partOffset, fs.sb, fs.sb.uQuotino, dqTypeUser, map[uint32]*quotaAcct{})
	if err == nil {
		t.Fatal("quotaWriteDquots with no extents: want error")
	}
}
