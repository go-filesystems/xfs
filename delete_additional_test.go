package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestDeleteFileAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)
	parent := newTestInode(30, 0x4000, inodeFmtLocal, 0)

	t.Run("missing parent is idempotent", func(t *testing.T) {
		oldLookup := deletePathLookup
		deletePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return nil, ErrNotFound }
		t.Cleanup(func() { deletePathLookup = oldLookup })

		if err := deleteFile(rw, 0, sb, "/file"); err != nil {
			t.Fatalf("deleteFile missing parent: %v", err)
		}
	})

	t.Run("parent lookup error", func(t *testing.T) {
		oldLookup := deletePathLookup
		deletePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return nil, errBoom }
		t.Cleanup(func() { deletePathLookup = oldLookup })

		if err := deleteFile(rw, 0, sb, "/file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected pathLookup error %v, got %v", errBoom, err)
		}
	})

	t.Run("missing child is idempotent", func(t *testing.T) {
		oldLookup := deletePathLookup
		oldDir := deleteLookupInDir
		deletePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return parent, nil }
		deleteLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 0, ErrNotFound }
		t.Cleanup(func() {
			deletePathLookup = oldLookup
			deleteLookupInDir = oldDir
		})

		if err := deleteFile(rw, 0, sb, "/file"); err != nil {
			t.Fatalf("deleteFile missing child: %v", err)
		}
	})

	t.Run("read inode and type errors", func(t *testing.T) {
		oldLookup := deletePathLookup
		oldDir := deleteLookupInDir
		oldRead := deleteReadInode
		deletePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return parent, nil }
		deleteLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 99, nil }
		deleteReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) { return nil, errBoom }
		t.Cleanup(func() {
			deletePathLookup = oldLookup
			deleteLookupInDir = oldDir
			deleteReadInode = oldRead
		})

		if err := deleteFile(rw, 0, sb, "/file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected readInode error %v, got %v", errBoom, err)
		}
		deleteReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			return newTestInode(99, 0x4000, inodeFmtLocal, 0), nil
		}
		if err := deleteFile(rw, 0, sb, "/file"); err == nil {
			t.Fatal("expected deleteFile to reject directories")
		}
	})

	t.Run("remove entry and extent errors", func(t *testing.T) {
		oldLookup := deletePathLookup
		oldDir := deleteLookupInDir
		oldRead := deleteReadInode
		oldRemove := deleteRemoveDirEntry
		oldInline := deleteInlineExtents
		oldFree := deleteFreeBlocks
		deletePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return parent, nil }
		deleteLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 100, nil }
		deleteReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			in := newTestInode(100, 0x8000, inodeFmtExtents, 0)
			return in, nil
		}
		deleteRemoveDirEntry = func(readerWriterAt, int64, *superblock, *inode, string) error { return errBoom }
		deleteInlineExtents = func(*inode) ([]extent, error) { return nil, errBoom }
		deleteFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return errBoom }
		t.Cleanup(func() {
			deletePathLookup = oldLookup
			deleteLookupInDir = oldDir
			deleteReadInode = oldRead
			deleteRemoveDirEntry = oldRemove
			deleteInlineExtents = oldInline
			deleteFreeBlocks = oldFree
		})

		if err := deleteFile(rw, 0, sb, "/file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected removeDirEntry error %v, got %v", errBoom, err)
		}
		deleteRemoveDirEntry = func(readerWriterAt, int64, *superblock, *inode, string) error { return nil }
		if err := deleteFile(rw, 0, sb, "/file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected inlineExtents error %v, got %v", errBoom, err)
		}
		deleteInlineExtents = func(*inode) ([]extent, error) { return []extent{{startBlock: 1, count: 1}}, nil }
		if err := deleteFile(rw, 0, sb, "/file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeBlocks error %v, got %v", errBoom, err)
		}
	})

	t.Run("write and free inode errors and success", func(t *testing.T) {
		oldLookup := deletePathLookup
		oldDir := deleteLookupInDir
		oldRead := deleteReadInode
		oldRemove := deleteRemoveDirEntry
		oldWrite := deleteWriteInode
		oldFree := deleteFreeInode
		deletePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return parent, nil }
		deleteLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 101, nil }
		deleteReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			return newTestInode(101, 0x8000, inodeFmtLocal, 0), nil
		}
		deleteRemoveDirEntry = func(readerWriterAt, int64, *superblock, *inode, string) error { return nil }
		deleteWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return errBoom }
		deleteFreeInode = func(readerWriterAt, int64, *superblock, uint64) error { return errBoom }
		t.Cleanup(func() {
			deletePathLookup = oldLookup
			deleteLookupInDir = oldDir
			deleteReadInode = oldRead
			deleteRemoveDirEntry = oldRemove
			deleteWriteInode = oldWrite
			deleteFreeInode = oldFree
		})

		if err := deleteFile(rw, 0, sb, "/file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeInode error %v, got %v", errBoom, err)
		}
		deleteWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		if err := deleteFile(rw, 0, sb, "/file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeInode error %v, got %v", errBoom, err)
		}
		deleteFreeInode = func(readerWriterAt, int64, *superblock, uint64) error { return nil }
		if err := deleteFile(rw, 0, sb, "/file"); err != nil {
			t.Fatalf("deleteFile success: %v", err)
		}
	})
}

func TestRemoveDirEntryAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)

	t.Run("unsupported format", func(t *testing.T) {
		dirIn := newTestInode(31, 0x4000, 99, 0)
		if err := removeDirEntry(rw, 0, sb, dirIn, "x"); err == nil {
			t.Fatal("expected removeDirEntry to reject unsupported formats")
		}
	})

	t.Run("short form success", func(t *testing.T) {
		dirIn := newTestInode(32, 0x4000, inodeFmtLocal, 0)
		fork := buildSFDir(1, []struct {
			name string
			ino  uint32
		}{{"file", 7}}, sb.hasFType)
		copy(dirIn.dataFork(), fork)
		setInodeSize(dirIn, uint64(len(fork)))
		deleteWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		if err := removeDirEntry(rw, 0, sb, dirIn, "file"); err != nil {
			t.Fatalf("removeDirEntry short form: %v", err)
		}
	})
}

func TestRemoveBlockDirEntryAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(int(sb.blockSize))
	blk := buildDirBlock([]struct {
		ino  uint64
		name string
		ft   uint8
	}{{ino: 7, name: "file", ft: 1}}, sb.hasFType)
	copy(rw.data, blk)
	dirIn := newTestInode(33, 0x4000, inodeFmtExtents, 0)
	setSingleExtent(dirIn, 0, 1)

	if err := removeBlockDirEntry(rw, 0, sb, dirIn, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := removeBlockDirEntry(rw, 0, sb, dirIn, "file"); err != nil {
		t.Fatalf("removeBlockDirEntry: %v", err)
	}
	if off, _ := findEntryInBlock(rw.data, "file", sb.hasFType, sb.hasCRC); off >= 0 {
		t.Fatal("removeBlockDirEntry did not clear the matching entry")
	}
	bad := newMemRW(0)
	bad.readHook = func(int64, []byte) error { return errBoom }
	if err := removeBlockDirEntry(bad, 0, sb, dirIn, "file"); !errors.Is(err, errBoom) {
		t.Fatalf("expected readRawBlock error %v, got %v", errBoom, err)
	}
}

func TestDeleteDirAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)
	parent := newTestInode(40, 0x4000, inodeFmtLocal, 0)
	target := newTestInode(41, 0x4000, inodeFmtLocal, 0)
	calls := 0
	oldLookup := deletePathLookup
	oldDir := deleteLookupInDir
	oldRead := deleteReadInode
	oldReadDir := deleteReadDir
	oldDeleteDir := deleteDirDeleteDir
	oldDeleteFile := deleteDirDeleteFile
	oldRemove := deleteRemoveDirEntry
	oldWrite := deleteWriteInode
	oldFree := deleteFreeInode
	deletePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
		calls++
		return parent, nil
	}
	deleteLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 41, nil }
	deleteReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) { return target, nil }
	deleteReadDir = func(io.ReaderAt, int64, *superblock, *inode) ([]DirEntry, error) {
		return []DirEntry{{Inode: 50, Name: "sub", FileType: 2}, {Inode: 51, Name: "file", FileType: 1}}, nil
	}
	deleteDirDeleteDir = func(readerWriterAt, int64, *superblock, string) error { return nil }
	deleteDirDeleteFile = func(readerWriterAt, int64, *superblock, string) error { return nil }
	deleteRemoveDirEntry = func(readerWriterAt, int64, *superblock, *inode, string) error { return nil }
	deleteWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
	deleteFreeInode = func(readerWriterAt, int64, *superblock, uint64) error { return nil }
	t.Cleanup(func() {
		deletePathLookup = oldLookup
		deleteLookupInDir = oldDir
		deleteReadInode = oldRead
		deleteReadDir = oldReadDir
		deleteDirDeleteDir = oldDeleteDir
		deleteDirDeleteFile = oldDeleteFile
		deleteRemoveDirEntry = oldRemove
		deleteWriteInode = oldWrite
		deleteFreeInode = oldFree
	})

	if err := deleteDir(rw, 0, sb, "/dir"); err != nil {
		t.Fatalf("deleteDir: %v", err)
	}
	if calls < 2 {
		t.Fatal("deleteDir did not re-read the parent after removing children")
	}
}

func restoreDeleteHooks(t *testing.T) {
	oldPathLookup := deletePathLookup
	oldLookupInDir := deleteLookupInDir
	oldReadInode := deleteReadInode
	oldRemoveDirEntry := deleteRemoveDirEntry
	oldInlineExtents := deleteInlineExtents
	oldBtreeExtents := deleteBtreeExtents
	oldFreeBlocks := deleteFreeBlocks
	oldWriteInode := deleteWriteInode
	oldFreeInode := deleteFreeInode
	oldReadDir := deleteReadDir
	oldDeleteDir := deleteDirDeleteDir
	oldDeleteFile := deleteDirDeleteFile
	t.Cleanup(func() {
		deletePathLookup = oldPathLookup
		deleteLookupInDir = oldLookupInDir
		deleteReadInode = oldReadInode
		deleteRemoveDirEntry = oldRemoveDirEntry
		deleteInlineExtents = oldInlineExtents
		deleteBtreeExtents = oldBtreeExtents
		deleteFreeBlocks = oldFreeBlocks
		deleteWriteInode = oldWriteInode
		deleteFreeInode = oldFreeInode
		deleteReadDir = oldReadDir
		deleteDirDeleteDir = oldDeleteDir
		deleteDirDeleteFile = oldDeleteFile
	})
}

func TestDeleteFileRemainingBranches(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)
	parent := newTestInode(59, 0x4000, inodeFmtLocal, 0)

	t.Run("child lookup error", func(t *testing.T) {
		restoreDeleteHooks(t)
		deletePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, _ string) (*inode, error) { return parent, nil }
		deleteLookupInDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, _ string) (uint64, error) {
			return 0, errBoom
		}
		if err := deleteFile(rw, 0, sb, "/file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected child lookup error %v, got %v", errBoom, err)
		}
	})

	t.Run("btree extent paths", func(t *testing.T) {
		restoreDeleteHooks(t)
		deletePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, _ string) (*inode, error) { return parent, nil }
		deleteLookupInDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, _ string) (uint64, error) { return 60, nil }
		deleteReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint64) (*inode, error) {
			return newTestInode(60, 0x8000, inodeFmtBtree, 0), nil
		}
		deleteRemoveDirEntry = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ string) error { return nil }
		deleteBtreeExtents = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]extent, error) {
			return nil, errBoom
		}
		if err := deleteFile(rw, 0, sb, "/file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected btree extent error %v, got %v", errBoom, err)
		}
		deleteBtreeExtents = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]extent, error) {
			return []extent{{startBlock: 1, count: 1}}, nil
		}
		deleteFreeBlocks = func(_ readerWriterAt, _ int64, _ *superblock, _ uint64, _ uint32) error { return nil }
		deleteWriteInode = func(_ io.WriterAt, _ int64, _ *superblock, _ *inode) error { return nil }
		deleteFreeInode = func(_ readerWriterAt, _ int64, _ *superblock, _ uint64) error { return nil }
		if err := deleteFile(rw, 0, sb, "/file"); err != nil {
			t.Fatalf("deleteFile btree success: %v", err)
		}
	})
}

func TestDeleteDirRemainingBranches(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)
	parent := newTestInode(70, 0x4000, inodeFmtLocal, 0)
	targetDir := newTestInode(71, 0x4000, inodeFmtLocal, 0)

	setBaseHooks := func(t *testing.T, secondRead *inode) {
		t.Helper()
		restoreDeleteHooks(t)
		deletePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, _ string) (*inode, error) { return parent, nil }
		deleteLookupInDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, _ string) (uint64, error) { return 71, nil }
		readCalls := 0
		deleteReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint64) (*inode, error) {
			readCalls++
			if readCalls == 1 || secondRead == nil {
				return targetDir, nil
			}
			return secondRead, nil
		}
		deleteReadDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]DirEntry, error) { return nil, nil }
		deleteDirDeleteDir = func(_ readerWriterAt, _ int64, _ *superblock, _ string) error { return nil }
		deleteDirDeleteFile = func(_ readerWriterAt, _ int64, _ *superblock, _ string) error { return nil }
		deleteRemoveDirEntry = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ string) error { return nil }
		deleteInlineExtents = func(*inode) ([]extent, error) { return []extent{{startBlock: 1, count: 1}}, nil }
		deleteBtreeExtents = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]extent, error) {
			return []extent{{startBlock: 2, count: 1}}, nil
		}
		deleteFreeBlocks = func(_ readerWriterAt, _ int64, _ *superblock, _ uint64, _ uint32) error { return nil }
		deleteWriteInode = func(_ io.WriterAt, _ int64, _ *superblock, _ *inode) error { return nil }
		deleteFreeInode = func(_ readerWriterAt, _ int64, _ *superblock, _ uint64) error { return nil }
	}

	t.Run("idempotent and lookup errors", func(t *testing.T) {
		restoreDeleteHooks(t)
		deletePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, _ string) (*inode, error) { return nil, ErrNotFound }
		if err := deleteDir(rw, 0, sb, "/dir"); err != nil {
			t.Fatalf("deleteDir missing parent: %v", err)
		}
		deletePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, _ string) (*inode, error) { return nil, errBoom }
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected parent lookup error %v, got %v", errBoom, err)
		}

		restoreDeleteHooks(t)
		deletePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, _ string) (*inode, error) { return parent, nil }
		deleteLookupInDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, _ string) (uint64, error) {
			return 0, ErrNotFound
		}
		if err := deleteDir(rw, 0, sb, "/dir"); err != nil {
			t.Fatalf("deleteDir missing child: %v", err)
		}
		deleteLookupInDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, _ string) (uint64, error) {
			return 0, errBoom
		}
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected child lookup error %v, got %v", errBoom, err)
		}
	})

	t.Run("target read and type errors", func(t *testing.T) {
		setBaseHooks(t, nil)
		deleteReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint64) (*inode, error) { return nil, errBoom }
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected readInode error %v, got %v", errBoom, err)
		}
		deleteReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint64) (*inode, error) {
			return newTestInode(71, 0x8000, inodeFmtLocal, 0), nil
		}
		if err := deleteDir(rw, 0, sb, "/dir"); err == nil {
			t.Fatal("expected deleteDir to reject non-directories")
		}
	})

	t.Run("readDir and child delete errors", func(t *testing.T) {
		setBaseHooks(t, nil)
		deleteReadDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]DirEntry, error) { return nil, errBoom }
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected readDir error %v, got %v", errBoom, err)
		}

		setBaseHooks(t, nil)
		deleteReadDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]DirEntry, error) {
			return []DirEntry{{Inode: 72, Name: "sub", FileType: 2}}, nil
		}
		deleteDirDeleteDir = func(_ readerWriterAt, _ int64, _ *superblock, _ string) error { return errBoom }
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected child dir delete error %v, got %v", errBoom, err)
		}

		setBaseHooks(t, nil)
		deleteReadDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]DirEntry, error) {
			return []DirEntry{{Inode: 73, Name: "file", FileType: 1}}, nil
		}
		deleteDirDeleteFile = func(_ readerWriterAt, _ int64, _ *superblock, _ string) error { return errBoom }
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected child file delete error %v, got %v", errBoom, err)
		}
	})

	t.Run("reread and cleanup errors", func(t *testing.T) {
		setBaseHooks(t, nil)
		pathCalls := 0
		deletePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, _ string) (*inode, error) {
			pathCalls++
			if pathCalls == 2 {
				return nil, errBoom
			}
			return parent, nil
		}
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected parent reread error %v, got %v", errBoom, err)
		}

		setBaseHooks(t, nil)
		deleteRemoveDirEntry = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ string) error { return errBoom }
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected removeDirEntry error %v, got %v", errBoom, err)
		}

		setBaseHooks(t, nil)
		readCalls := 0
		deleteReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint64) (*inode, error) {
			readCalls++
			if readCalls == 2 {
				return nil, errBoom
			}
			return targetDir, nil
		}
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected target reread error %v, got %v", errBoom, err)
		}
	})

	t.Run("extent and inode finalization errors", func(t *testing.T) {
		setBaseHooks(t, newTestInode(71, 0x4000, inodeFmtExtents, 0))
		deleteInlineExtents = func(*inode) ([]extent, error) { return nil, errBoom }
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected inlineExtents error %v, got %v", errBoom, err)
		}

		setBaseHooks(t, newTestInode(71, 0x4000, inodeFmtBtree, 0))
		deleteBtreeExtents = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]extent, error) { return nil, errBoom }
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected btreeExtents error %v, got %v", errBoom, err)
		}

		setBaseHooks(t, newTestInode(71, 0x4000, inodeFmtExtents, 0))
		deleteFreeBlocks = func(_ readerWriterAt, _ int64, _ *superblock, _ uint64, _ uint32) error { return errBoom }
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeBlocks error %v, got %v", errBoom, err)
		}

		setBaseHooks(t, nil)
		deleteWriteInode = func(_ io.WriterAt, _ int64, _ *superblock, _ *inode) error { return errBoom }
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeInode error %v, got %v", errBoom, err)
		}

		setBaseHooks(t, nil)
		deleteFreeInode = func(_ readerWriterAt, _ int64, _ *superblock, _ uint64) error { return errBoom }
		if err := deleteDir(rw, 0, sb, "/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeInode error %v, got %v", errBoom, err)
		}
	})
}

func TestDeleteDirectoryHelpersRemainingBranches(t *testing.T) {
	sb := defaultSB()

	t.Run("removeSFEntry malformed and 8-byte paths", func(t *testing.T) {
		restoreDeleteHooks(t)
		short := newTestInode(80, 0x4000, inodeFmtLocal, 0)
		short.raw = short.raw[:inodeCoreSize+1]
		if err := removeSFEntry(newMemRW(0), 0, sb, short, "x"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound for short fork, got %v", err)
		}

		trunc := newTestInode(81, 0x4000, inodeFmtLocal, 0)
		trunc.raw = trunc.raw[:inodeCoreSize+6]
		trunc.dataFork()[0] = 1
		if err := removeSFEntry(newMemRW(0), 0, sb, trunc, "x"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound for truncated entry, got %v", err)
		}

		badName := newTestInode(82, 0x4000, inodeFmtLocal, 0)
		badName.raw = badName.raw[:inodeCoreSize+9]
		badName.dataFork()[0] = 1
		badName.dataFork()[6] = 5
		if err := removeSFEntry(newMemRW(0), 0, sb, badName, "x"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound for truncated name, got %v", err)
		}

		makeI8Dir := func() *inode {
			i8 := newTestInode(83, 0x4000, inodeFmtLocal, 0)
			fork := i8.dataFork()
			fork[0] = 1
			fork[1] = 1
			binary.BigEndian.PutUint64(fork[2:], 1)
			fork[10] = 3
			copy(fork[13:], []byte("big"))
			fork[16] = 1
			binary.BigEndian.PutUint64(fork[17:], 9)
			return i8
		}
		deleteWriteInode = func(_ io.WriterAt, _ int64, _ *superblock, _ *inode) error { return errBoom }
		if err := removeSFEntry(newMemRW(0), 0, sb, makeI8Dir(), "big"); !errors.Is(err, errBoom) {
			t.Fatalf("expected removeSFEntry write error %v, got %v", errBoom, err)
		}
		deleteWriteInode = func(_ io.WriterAt, _ int64, _ *superblock, _ *inode) error { return nil }
		if err := removeSFEntry(newMemRW(0), 0, sb, makeI8Dir(), "big"); err != nil {
			t.Fatalf("removeSFEntry i8 success: %v", err)
		}
	})

	t.Run("removeDirEntry and block helpers", func(t *testing.T) {
		blk := buildDirBlock([]struct {
			ino  uint64
			name string
			ft   uint8
		}{{ino: 7, name: "file", ft: 1}}, sb.hasFType)
		rw := newMemRW(int(sb.blockSize))
		copy(rw.data, blk)
		dirIn := newTestInode(84, 0x4000, inodeFmtExtents, 0)
		setSingleExtent(dirIn, 0, 1)
		if err := removeDirEntry(rw, 0, sb, dirIn, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected extent dispatch ErrNotFound, got %v", err)
		}

		bad := newTestInode(85, 0x4000, 99, 0)
		if err := removeBlockDirEntry(newMemRW(0), 0, sb, bad, "x"); err == nil {
			t.Fatal("expected removeBlockDirEntry dirExtents error")
		}

		leafOnly := newTestInode(86, 0x4000, inodeFmtExtents, 0)
		leafOnly.nExts = 1
		rec := encodeExtent(extent{startOff: dirLeafByteOffset / uint64(sb.blockSize), startBlock: 1, count: 1})
		copy(leafOnly.dataFork(), rec[:])
		if err := removeBlockDirEntry(newMemRW(0), 0, sb, leafOnly, "file"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound for leaf-only extents, got %v", err)
		}

		copy(rw.data, blk)
		off, sz := findEntryInBlock(rw.data, "file", sb.hasFType, sb.hasCRC)
		if err := removeBlockDirEntry(rw, 0, sb, dirIn, "file"); err != nil {
			t.Fatalf("removeBlockDirEntry merge: %v", err)
		}
		if got := int(binary.BigEndian.Uint16(rw.data[off+2:])); got <= sz {
			t.Fatalf("expected merged free slot length > %d, got %d", sz, got)
		}

		badWrite := newMemRW(int(sb.blockSize))
		copy(badWrite.data, blk)
		badWrite.writeHook = func(int64, []byte) error { return errBoom }
		if err := removeBlockDirEntry(badWrite, 0, sb, dirIn, "file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeRawBlock error %v, got %v", errBoom, err)
		}
	})

	t.Run("findEntryInBlock edge cases", func(t *testing.T) {
		hdr := dirDataHdrSize(sb.hasCRC)
		blk := make([]byte, hdr+10)
		binary.BigEndian.PutUint16(blk[hdr:], dirFreeTag)
		binary.BigEndian.PutUint16(blk[hdr+2:], 4)
		if off, sz := findEntryInBlock(blk, "x", sb.hasFType, sb.hasCRC); off != -1 || sz != 0 {
			t.Fatalf("expected no entry for short free slot, got (%d, %d)", off, sz)
		}

		blk = make([]byte, hdr+24)
		if off, sz := findEntryInBlock(blk, "x", sb.hasFType, sb.hasCRC); off != -1 || sz != 0 {
			t.Fatalf("expected no entry for sentinel block, got (%d, %d)", off, sz)
		}

		blk = make([]byte, hdr+16)
		binary.BigEndian.PutUint64(blk[hdr:], 1)
		blk[hdr+8] = 0
		if off, sz := findEntryInBlock(blk, "x", sb.hasFType, sb.hasCRC); off != -1 || sz != 0 {
			t.Fatalf("expected no entry for zero namelen, got (%d, %d)", off, sz)
		}

		blk = make([]byte, hdr+16)
		binary.BigEndian.PutUint64(blk[hdr:], 1)
		blk[hdr+8] = 8
		if off, sz := findEntryInBlock(blk, "x", sb.hasFType, sb.hasCRC); off != -1 || sz != 0 {
			t.Fatalf("expected no entry for truncated name, got (%d, %d)", off, sz)
		}
	})
}

func TestDeleteHelperRemainingSpecifics(t *testing.T) {
	sb := defaultSB()

	t.Run("removeSFEntry truncated name", func(t *testing.T) {
		in := newTestInode(97, 0x4000, inodeFmtLocal, 0)
		in.raw = in.raw[:inodeCoreSize+12]
		in.dataFork()[0] = 1
		in.dataFork()[6] = 5
		if err := removeSFEntry(newMemRW(0), 0, sb, in, "x"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound for truncated short-form name, got %v", err)
		}
	})

	t.Run("removeSFEntry not found", func(t *testing.T) {
		in := newTestInode(98, 0x4000, inodeFmtLocal, 0)
		fork := buildSFDir(1, []struct {
			name string
			ino  uint32
		}{{"other", 7}}, sb.hasFType)
		copy(in.dataFork(), fork)
		if err := removeSFEntry(newMemRW(0), 0, sb, in, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound for missing short-form entry, got %v", err)
		}
	})
}
