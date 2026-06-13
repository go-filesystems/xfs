package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestMakeDirValidation(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)

	if err := makeDir(rw, 0, sb, "/", 0o755); err == nil {
		t.Fatal("expected makeDir to reject the root path")
	}

	t.Run("parent lookup error", func(t *testing.T) {
		oldLookup := makeDirPathLookup
		makeDirPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return nil, errBoom
		}
		t.Cleanup(func() { makeDirPathLookup = oldLookup })

		if err := makeDir(rw, 0, sb, "/dir", 0o755); !errors.Is(err, errBoom) {
			t.Fatalf("expected parent lookup error %v, got %v", errBoom, err)
		}
	})

	t.Run("parent is not directory", func(t *testing.T) {
		oldLookup := makeDirPathLookup
		makeDirPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(1, 0x8000, inodeFmtLocal, 0), nil
		}
		t.Cleanup(func() { makeDirPathLookup = oldLookup })

		if err := makeDir(rw, 0, sb, "/dir", 0o755); err == nil {
			t.Fatal("expected makeDir to reject non-directory parents")
		}
	})

	t.Run("already exists", func(t *testing.T) {
		oldLookup := makeDirPathLookup
		oldDir := makeDirLookupInDir
		makeDirPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(2, 0x4000, inodeFmtLocal, 0), nil
		}
		makeDirLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 9, nil }
		t.Cleanup(func() {
			makeDirPathLookup = oldLookup
			makeDirLookupInDir = oldDir
		})

		if err := makeDir(rw, 0, sb, "/dir", 0o755); err == nil {
			t.Fatal("expected makeDir to reject existing entries")
		}
	})

	t.Run("lookupInDir error", func(t *testing.T) {
		oldLookup := makeDirPathLookup
		oldDir := makeDirLookupInDir
		makeDirPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(3, 0x4000, inodeFmtLocal, 0), nil
		}
		makeDirLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 0, errBoom }
		t.Cleanup(func() {
			makeDirPathLookup = oldLookup
			makeDirLookupInDir = oldDir
		})

		if err := makeDir(rw, 0, sb, "/dir", 0o755); !errors.Is(err, errBoom) {
			t.Fatalf("expected lookupInDir error %v, got %v", errBoom, err)
		}
	})
}

func TestMakeDirAllocationAndPersistence(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)
	parent := newTestInode(4, 0x4000, inodeFmtLocal, 0)

	t.Run("allocInode fallback", func(t *testing.T) {
		oldLookup := makeDirPathLookup
		oldDir := makeDirLookupInDir
		oldAlloc := makeDirAllocInode
		oldWrite := makeDirWriteInode
		oldAdd := makeDirAddDirEntry
		calls := 0
		makeDirPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return parent, nil }
		makeDirLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 0, ErrNotFound }
		makeDirAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) {
			calls++
			if calls == 1 {
				return 0, errBoom
			}
			return 77, nil
		}
		makeDirWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		makeDirAddDirEntry = func(readerWriterAt, int64, *superblock, *inode, uint64, string, uint8) error { return nil }
		t.Cleanup(func() {
			makeDirPathLookup = oldLookup
			makeDirLookupInDir = oldDir
			makeDirAllocInode = oldAlloc
			makeDirWriteInode = oldWrite
			makeDirAddDirEntry = oldAdd
		})

		if err := makeDir(rw, 0, sb, "/dir", 0o755); err != nil {
			t.Fatalf("makeDir fallback: %v", err)
		}
		if calls != 2 {
			t.Fatalf("expected makeDir to retry allocInode on another AG, got %d calls", calls)
		}
	})

	t.Run("no free inode", func(t *testing.T) {
		oldLookup := makeDirPathLookup
		oldDir := makeDirLookupInDir
		oldAlloc := makeDirAllocInode
		makeDirPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return parent, nil }
		makeDirLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 0, ErrNotFound }
		makeDirAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) { return 0, errBoom }
		t.Cleanup(func() {
			makeDirPathLookup = oldLookup
			makeDirLookupInDir = oldDir
			makeDirAllocInode = oldAlloc
		})

		if err := makeDir(rw, 0, sb, "/dir", 0o755); !errors.Is(err, errBoom) {
			t.Fatalf("expected allocInode failure %v, got %v", errBoom, err)
		}
	})

	t.Run("write inode error", func(t *testing.T) {
		oldLookup := makeDirPathLookup
		oldDir := makeDirLookupInDir
		oldAlloc := makeDirAllocInode
		oldWrite := makeDirWriteInode
		makeDirPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return parent, nil }
		makeDirLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 0, ErrNotFound }
		makeDirAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) { return 88, nil }
		makeDirWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return errBoom }
		t.Cleanup(func() {
			makeDirPathLookup = oldLookup
			makeDirLookupInDir = oldDir
			makeDirAllocInode = oldAlloc
			makeDirWriteInode = oldWrite
		})

		if err := makeDir(rw, 0, sb, "/dir", 0o755); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeInode failure %v, got %v", errBoom, err)
		}
	})

	t.Run("add entry error", func(t *testing.T) {
		oldLookup := makeDirPathLookup
		oldDir := makeDirLookupInDir
		oldAlloc := makeDirAllocInode
		oldWrite := makeDirWriteInode
		oldAdd := makeDirAddDirEntry
		makeDirPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return parent, nil }
		makeDirLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 0, ErrNotFound }
		makeDirAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) { return 99, nil }
		makeDirWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		makeDirAddDirEntry = func(readerWriterAt, int64, *superblock, *inode, uint64, string, uint8) error { return errBoom }
		t.Cleanup(func() {
			makeDirPathLookup = oldLookup
			makeDirLookupInDir = oldDir
			makeDirAllocInode = oldAlloc
			makeDirWriteInode = oldWrite
			makeDirAddDirEntry = oldAdd
		})

		if err := makeDir(rw, 0, sb, "/dir", 0o755); !errors.Is(err, errBoom) {
			t.Fatalf("expected addDirEntry failure %v, got %v", errBoom, err)
		}
	})
}

func TestMakeDirBuildsLocalDirectoryInode(t *testing.T) {
	rw := newMemRW(0)
	sb := defaultSB()
	parent := newTestInode(0x1_0000_0000, 0x4000, inodeFmtLocal, 0)
	oldLookup := makeDirPathLookup
	oldDir := makeDirLookupInDir
	oldAlloc := makeDirAllocInode
	oldWrite := makeDirWriteInode
	oldAdd := makeDirAddDirEntry
	var wrote *inode
	makeDirPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return parent, nil }
	makeDirLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 0, ErrNotFound }
	makeDirAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) { return 123, nil }
	// makeDir writes both the new directory inode and (to bump its nlink) the
	// parent; capture the new directory inode (num 123) specifically.
	makeDirWriteInode = func(_ io.WriterAt, _ int64, _ *superblock, in *inode) error {
		if in.num == 123 {
			wrote = in
		}
		return nil
	}
	makeDirAddDirEntry = func(readerWriterAt, int64, *superblock, *inode, uint64, string, uint8) error { return nil }
	t.Cleanup(func() {
		makeDirPathLookup = oldLookup
		makeDirLookupInDir = oldDir
		makeDirAllocInode = oldAlloc
		makeDirWriteInode = oldWrite
		makeDirAddDirEntry = oldAdd
	})

	if err := makeDir(rw, 0, sb, "/dir", 0o755); err != nil {
		t.Fatalf("makeDir: %v", err)
	}
	if wrote == nil || wrote.format != inodeFmtLocal || wrote.size != 10 {
		t.Fatal("makeDir did not build an inline directory inode with an 8-byte parent")
	}
	if binary.BigEndian.Uint64(wrote.dataFork()[2:]) != parent.num {
		t.Fatal("makeDir did not encode the parent inode into the short-form header")
	}
}
