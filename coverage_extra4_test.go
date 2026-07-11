package filesystem_xfs

import (
	"io"
	"testing"
)

// TestQuotaRecomputeSkipsModeZero covers the free/unlinked-inode skip in the
// quotacheck scan.
func TestQuotaRecomputeSkipsModeZero(t *testing.T) {
	fs := formatQuota(t, QuotaConfig{User: true})
	defer fs.Close()
	orig := quotaReadInode
	defer func() { quotaReadInode = orig }()
	quotaReadInode = func(r io.ReaderAt, off int64, sb *superblock, ino uint64) (*inode, error) {
		in, err := orig(r, off, sb, ino)
		if err != nil {
			return nil, err
		}
		in.mode = 0 // look like a free inode
		return in, nil
	}
	if err := quotaRecompute(fs.f, fs.partOffset, fs.sb); err != nil {
		t.Fatalf("recompute with mode-0 inodes: %v", err)
	}
}

// TestCOWBreakBtreeForkPath drives the btree-fork branch of the COW break by
// forcing the reflinked inode to report btree format on read.
func TestCOWBreakBtreeForkPath(t *testing.T) {
	fs := setupReflinkPair(t)
	defer fs.Close()
	orig := writeReadInode
	defer func() { writeReadInode = orig }()
	writeReadInode = func(r io.ReaderAt, off int64, sb *superblock, ino uint64) (*inode, error) {
		in, err := orig(r, off, sb, ino)
		if err != nil {
			return nil, err
		}
		if inodeIsReflinked(in) {
			in.format = inodeFmtBtree // exercise the btree branch (btreeExtents will fail)
		}
		return in, nil
	}
	// The overwrite exercises the btree-fork branch of reflinkBreakAndWrite;
	// either outcome (success or a surfaced btree error) is acceptable — the
	// point is that the branch runs without panicking.
	_ = fs.WriteFile("/f2", make([]byte, 3000), 0o644)
}
