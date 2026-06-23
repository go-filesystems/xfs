package filesystem_xfs

import (
	"errors"
	"io"
	"testing"
)

func setSingleExtent(in *inode, startBlock uint64, count uint32) {
	in.format = inodeFmtExtents
	in.nExts = 1
	rec := encodeExtent(extent{startOff: 0, startBlock: startBlock, count: count})
	copy(in.dataFork(), rec[:])
}

func TestWriteFileAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)
	parent := newTestInode(10, 0x4000, inodeFmtLocal, 0)

	if err := writeFile(rw, 0, sb, "/", []byte("x"), 0o644); err == nil {
		t.Fatal("expected writeFile to reject the root path")
	}

	t.Run("parent lookup error", func(t *testing.T) {
		oldLookup := writePathLookup
		writePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return nil, errBoom }
		t.Cleanup(func() { writePathLookup = oldLookup })

		if err := writeFile(rw, 0, sb, "/file", []byte("x"), 0o644); !errors.Is(err, errBoom) {
			t.Fatalf("expected parent lookup error %v, got %v", errBoom, err)
		}
	})

	t.Run("parent is not directory", func(t *testing.T) {
		oldLookup := writePathLookup
		writePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(11, 0x8000, inodeFmtLocal, 0), nil
		}
		t.Cleanup(func() { writePathLookup = oldLookup })

		if err := writeFile(rw, 0, sb, "/file", []byte("x"), 0o644); err == nil {
			t.Fatal("expected writeFile to reject non-directory parents")
		}
	})

	t.Run("lookupInDir error", func(t *testing.T) {
		oldLookup := writePathLookup
		oldDir := writeLookupInDir
		writePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return parent, nil }
		writeLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 0, errBoom }
		t.Cleanup(func() {
			writePathLookup = oldLookup
			writeLookupInDir = oldDir
		})

		if err := writeFile(rw, 0, sb, "/file", []byte("x"), 0o644); !errors.Is(err, errBoom) {
			t.Fatalf("expected lookupInDir error %v, got %v", errBoom, err)
		}
	})

	t.Run("existing file uses overwrite path", func(t *testing.T) {
		oldLookup := writePathLookup
		oldDir := writeLookupInDir
		oldRead := writeReadInode
		oldWrite := writeWriteInode
		writePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return parent, nil }
		writeLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 99, nil }
		writeReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			in := newTestInode(99, 0x8000, inodeFmtLocal, 0)
			copy(in.dataFork(), []byte("old"))
			return in, nil
		}
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		t.Cleanup(func() {
			writePathLookup = oldLookup
			writeLookupInDir = oldDir
			writeReadInode = oldRead
			writeWriteInode = oldWrite
		})

		if err := writeFile(rw, 0, sb, "/file", []byte("new"), 0o644); err != nil {
			t.Fatalf("writeFile overwrite: %v", err)
		}
	})

	t.Run("missing file uses create path", func(t *testing.T) {
		oldLookup := writePathLookup
		oldDir := writeLookupInDir
		oldAllocInode := writeAllocInode
		oldAllocBlocks := writeAllocBlocks
		oldWriteInode := writeWriteInode
		oldBlocks := writeWriteBlocksData
		oldAdd := writeAddDirEntry
		writePathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return parent, nil }
		writeLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 0, ErrNotFound }
		writeAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) { return 123, nil }
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 5, nil }
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error { return nil }
		writeAddDirEntry = func(readerWriterAt, int64, *superblock, *inode, uint64, string, uint8) error { return nil }
		t.Cleanup(func() {
			writePathLookup = oldLookup
			writeLookupInDir = oldDir
			writeAllocInode = oldAllocInode
			writeAllocBlocks = oldAllocBlocks
			writeWriteInode = oldWriteInode
			writeWriteBlocksData = oldBlocks
			writeAddDirEntry = oldAdd
		})

		if err := writeFile(rw, 0, sb, "/file", []byte("data"), 0o644); err != nil {
			t.Fatalf("writeFile create: %v", err)
		}
	})
}

func TestOverwriteFileAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)

	t.Run("readInode error", func(t *testing.T) {
		oldRead := writeReadInode
		writeReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) { return nil, errBoom }
		t.Cleanup(func() { writeReadInode = oldRead })

		if err := overwriteFile(rw, 0, sb, 1, []byte("x")); !errors.Is(err, errBoom) {
			t.Fatalf("expected readInode error %v, got %v", errBoom, err)
		}
	})

	t.Run("non regular inode", func(t *testing.T) {
		oldRead := writeReadInode
		writeReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			return newTestInode(1, 0x4000, inodeFmtLocal, 0), nil
		}
		t.Cleanup(func() { writeReadInode = oldRead })

		if err := overwriteFile(rw, 0, sb, 1, []byte("x")); err == nil {
			t.Fatal("expected overwriteFile to reject non-regular inodes")
		}
	})

	t.Run("local inline success", func(t *testing.T) {
		oldRead := writeReadInode
		oldWrite := writeWriteInode
		var wrote *inode
		writeReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			return newTestInode(2, 0x8000, inodeFmtLocal, 0), nil
		}
		writeWriteInode = func(_ io.WriterAt, _ int64, _ *superblock, in *inode) error { wrote = in; return nil }
		t.Cleanup(func() {
			writeReadInode = oldRead
			writeWriteInode = oldWrite
		})

		if err := overwriteFile(rw, 0, sb, 2, []byte("abc")); err != nil {
			t.Fatalf("overwriteFile local: %v", err)
		}
		if wrote == nil || wrote.size != 3 || string(wrote.dataFork()[:3]) != "abc" {
			t.Fatal("overwriteFile did not rewrite inline file contents")
		}
	})

	t.Run("local promote error", func(t *testing.T) {
		oldRead := writeReadInode
		oldAlloc := writeAllocBlocks
		writeReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			return newTestInode(3, 0x8000, inodeFmtLocal, 0), nil
		}
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 0, errBoom
		}
		t.Cleanup(func() {
			writeReadInode = oldRead
			writeAllocBlocks = oldAlloc
		})

		big := make([]byte, sb.inodeSize)
		if err := overwriteFile(rw, 0, sb, 3, big); !errors.Is(err, errBoom) {
			t.Fatalf("expected promote error %v, got %v", errBoom, err)
		}
	})

	t.Run("extent parse error", func(t *testing.T) {
		oldRead := writeReadInode
		writeReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			in := newTestInode(4, 0x8000, inodeFmtExtents, 1)
			in.nExts = 64
			return in, nil
		}
		t.Cleanup(func() { writeReadInode = oldRead })

		if err := overwriteFile(rw, 0, sb, 4, []byte("x")); err == nil {
			t.Fatal("expected overwriteFile to propagate inline extent parsing errors")
		}
	})

	t.Run("unsupported format", func(t *testing.T) {
		oldRead := writeReadInode
		writeReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			return newTestInode(5, 0x8000, 99, 0), nil
		}
		t.Cleanup(func() { writeReadInode = oldRead })

		if err := overwriteFile(rw, 0, sb, 5, []byte("x")); err == nil {
			t.Fatal("expected overwriteFile to reject unsupported inode formats")
		}
	})
}

func TestWriteExtentHelpersAdditional(t *testing.T) {
	sb := defaultSB()

	t.Run("writeExtentsInPlace success", func(t *testing.T) {
		oldWrite := writeWriteInode
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		t.Cleanup(func() { writeWriteInode = oldWrite })
		rw := newMemRW(int(sb.blockSize * 2))
		in := newTestInode(6, 0x8000, inodeFmtExtents, 0)
		exts := []extent{{startOff: 0, startBlock: 0, count: 2}}

		if err := writeExtentsInPlace(rw, 0, sb, in, exts, []byte("abc")); err != nil {
			t.Fatalf("writeExtentsInPlace: %v", err)
		}
		if string(rw.data[:3]) != "abc" || rw.data[sb.blockSize] != 0 {
			t.Fatal("writeExtentsInPlace did not write data and zero-fill the remaining space")
		}
	})

	t.Run("writeExtentsInPlace block error", func(t *testing.T) {
		oldWrite := writeWriteInode
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		t.Cleanup(func() { writeWriteInode = oldWrite })
		rw := newMemRW(int(sb.blockSize))
		rw.writeHook = func(int64, []byte) error { return errBoom }
		in := newTestInode(7, 0x8000, inodeFmtExtents, 0)
		exts := []extent{{startOff: 0, startBlock: 0, count: 1}}

		if err := writeExtentsInPlace(rw, 0, sb, in, exts, []byte("abc")); !errors.Is(err, errBoom) {
			t.Fatalf("expected write block error %v, got %v", errBoom, err)
		}
	})

	t.Run("writeExtentList", func(t *testing.T) {
		in := newTestInode(8, 0x8000, inodeFmtExtents, 0)
		setInodeNExtents(in, 1)
		exts := []extent{{startOff: 0, startBlock: 7, count: 1}}
		if err := writeExtentList(in, exts); err != nil {
			t.Fatalf("writeExtentList: %v", err)
		}
		if got, err := inlineExtents(in); err != nil || len(got) != 1 || got[0].startBlock != 7 {
			t.Fatalf("unexpected extents after writeExtentList: %+v, %v", got, err)
		}
		tooMany := make([]extent, len(in.dataFork())/16+1)
		if err := writeExtentList(in, tooMany); err == nil {
			t.Fatal("expected writeExtentList to reject oversized extent lists")
		}
	})

	t.Run("writeBlocksData", func(t *testing.T) {
		rw := newMemRW(int(sb.blockSize * 2))
		if err := writeBlocksData(rw, 0, sb, 0, 2, []byte("abc")); err != nil {
			t.Fatalf("writeBlocksData: %v", err)
		}
		if string(rw.data[:3]) != "abc" || rw.data[sb.blockSize] != 0 {
			t.Fatal("writeBlocksData did not write the payload and zero-pad the rest")
		}
		rw.writeHook = func(int64, []byte) error { return errBoom }
		if err := writeBlocksData(rw, 0, sb, 0, 1, []byte("x")); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeBlocksData error %v, got %v", errBoom, err)
		}
	})
}

func TestReallocAndWriteAdditional(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(9, 0x8000, inodeFmtExtents, 0)
	oldExts := []extent{{startOff: 0, startBlock: 1, count: 1}}

	t.Run("freeBlocks error", func(t *testing.T) {
		oldFree := writeFreeBlocks
		writeFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return errBoom }
		t.Cleanup(func() { writeFreeBlocks = oldFree })

		if err := reallocAndWrite(newMemRW(0), 0, sb, in, oldExts, []byte("abc")); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeBlocks error %v, got %v", errBoom, err)
		}
	})

	t.Run("no space", func(t *testing.T) {
		oldFree := writeFreeBlocks
		oldAlloc := writeAllocBlocks
		writeFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 0, errBoom }
		t.Cleanup(func() {
			writeFreeBlocks = oldFree
			writeAllocBlocks = oldAlloc
		})

		if err := reallocAndWrite(newMemRW(0), 0, sb, in, oldExts, make([]byte, sb.blockSize)); err == nil {
			t.Fatal("expected reallocAndWrite to fail when all AG allocations fail")
		}
	})

	t.Run("write extent list error", func(t *testing.T) {
		oldFree := writeFreeBlocks
		oldAlloc := writeAllocBlocks
		oldList := writeWriteExtentList
		writeFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 8, nil }
		writeWriteExtentList = func(*inode, []extent) error { return errBoom }
		t.Cleanup(func() {
			writeFreeBlocks = oldFree
			writeAllocBlocks = oldAlloc
			writeWriteExtentList = oldList
		})

		if err := reallocAndWrite(newMemRW(0), 0, sb, in, oldExts, []byte("abc")); !errors.Is(err, errBoom) {
			t.Fatalf("expected extent list error %v, got %v", errBoom, err)
		}
	})

	t.Run("write inode error", func(t *testing.T) {
		oldFree := writeFreeBlocks
		oldAlloc := writeAllocBlocks
		oldList := writeWriteExtentList
		oldWrite := writeWriteInode
		writeFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 8, nil }
		writeWriteExtentList = func(*inode, []extent) error { return nil }
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return errBoom }
		t.Cleanup(func() {
			writeFreeBlocks = oldFree
			writeAllocBlocks = oldAlloc
			writeWriteExtentList = oldList
			writeWriteInode = oldWrite
		})

		if err := reallocAndWrite(newMemRW(0), 0, sb, in, oldExts, []byte("abc")); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeInode error %v, got %v", errBoom, err)
		}
	})

	t.Run("write data error", func(t *testing.T) {
		oldFree := writeFreeBlocks
		oldAlloc := writeAllocBlocks
		oldList := writeWriteExtentList
		oldWrite := writeWriteInode
		oldData := writeWriteBlocksData
		writeFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 8, nil }
		writeWriteExtentList = func(*inode, []extent) error { return nil }
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error { return errBoom }
		t.Cleanup(func() {
			writeFreeBlocks = oldFree
			writeAllocBlocks = oldAlloc
			writeWriteExtentList = oldList
			writeWriteInode = oldWrite
			writeWriteBlocksData = oldData
		})

		if err := reallocAndWrite(newMemRW(0), 0, sb, in, oldExts, []byte("abc")); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeBlocksData error %v, got %v", errBoom, err)
		}
	})
}

func TestPromoteAndWriteAdditional(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(10, 0x8000, inodeFmtLocal, 0)
	data := make([]byte, sb.blockSize)

	t.Run("alloc error", func(t *testing.T) {
		oldAlloc := writeAllocBlocks
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 0, errBoom }
		t.Cleanup(func() { writeAllocBlocks = oldAlloc })

		if err := promoteAndWrite(newMemRW(0), 0, sb, in, data); !errors.Is(err, errBoom) {
			t.Fatalf("expected alloc error %v, got %v", errBoom, err)
		}
	})

	t.Run("downstream errors", func(t *testing.T) {
		oldAlloc := writeAllocBlocks
		oldList := writeWriteExtentList
		oldWrite := writeWriteInode
		oldData := writeWriteBlocksData
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 12, nil }
		writeWriteExtentList = func(*inode, []extent) error { return errBoom }
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error { return nil }
		t.Cleanup(func() {
			writeAllocBlocks = oldAlloc
			writeWriteExtentList = oldList
			writeWriteInode = oldWrite
			writeWriteBlocksData = oldData
		})

		if err := promoteAndWrite(newMemRW(0), 0, sb, in, data); !errors.Is(err, errBoom) {
			t.Fatalf("expected extent-list error %v, got %v", errBoom, err)
		}
		writeWriteExtentList = func(*inode, []extent) error { return nil }
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return errBoom }
		if err := promoteAndWrite(newMemRW(0), 0, sb, in, data); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeInode error %v, got %v", errBoom, err)
		}
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error { return errBoom }
		if err := promoteAndWrite(newMemRW(0), 0, sb, in, data); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeBlocksData error %v, got %v", errBoom, err)
		}
	})
}

func TestCreateFileAdditional(t *testing.T) {
	sb := defaultSB()
	parent := newTestInode(11, 0x4000, inodeFmtLocal, 0)

	t.Run("no free inode", func(t *testing.T) {
		oldAlloc := writeAllocInode
		writeAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) { return 0, errBoom }
		t.Cleanup(func() { writeAllocInode = oldAlloc })

		if err := createFile(newMemRW(0), 0, sb, parent, "file", []byte("x"), 0o644); !errors.Is(err, errBoom) {
			t.Fatalf("expected allocInode error %v, got %v", errBoom, err)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		oldAlloc := writeAllocInode
		oldWrite := writeWriteInode
		oldAdd := writeAddDirEntry
		var wrote *inode
		writeAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) { return 77, nil }
		writeWriteInode = func(_ io.WriterAt, _ int64, _ *superblock, in *inode) error { wrote = in; return nil }
		writeAddDirEntry = func(readerWriterAt, int64, *superblock, *inode, uint64, string, uint8) error { return nil }
		t.Cleanup(func() {
			writeAllocInode = oldAlloc
			writeWriteInode = oldWrite
			writeAddDirEntry = oldAdd
		})

		if err := createFile(newMemRW(0), 0, sb, parent, "empty", nil, 0o644); err != nil {
			t.Fatalf("createFile empty: %v", err)
		}
		if wrote == nil || wrote.format != inodeFmtLocal || wrote.size != 0 {
			t.Fatal("createFile did not keep empty files inline")
		}
	})

	t.Run("non-empty downstream errors", func(t *testing.T) {
		oldAllocInode := writeAllocInode
		oldAllocBlocks := writeAllocBlocks
		oldList := writeWriteExtentList
		oldData := writeWriteBlocksData
		oldWrite := writeWriteInode
		oldAdd := writeAddDirEntry
		writeAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) { return 88, nil }
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 6, nil }
		writeWriteExtentList = func(*inode, []extent) error { return errBoom }
		writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error { return nil }
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		writeAddDirEntry = func(readerWriterAt, int64, *superblock, *inode, uint64, string, uint8) error { return nil }
		t.Cleanup(func() {
			writeAllocInode = oldAllocInode
			writeAllocBlocks = oldAllocBlocks
			writeWriteExtentList = oldList
			writeWriteBlocksData = oldData
			writeWriteInode = oldWrite
			writeAddDirEntry = oldAdd
		})

		if err := createFile(newMemRW(0), 0, sb, parent, "file", []byte("abc"), 0o644); !errors.Is(err, errBoom) {
			t.Fatalf("expected extent-list error %v, got %v", errBoom, err)
		}
		writeWriteExtentList = func(*inode, []extent) error { return nil }
		writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error { return errBoom }
		if err := createFile(newMemRW(0), 0, sb, parent, "file", []byte("abc"), 0o644); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeBlocksData error %v, got %v", errBoom, err)
		}
		writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error { return nil }
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return errBoom }
		if err := createFile(newMemRW(0), 0, sb, parent, "file", []byte("abc"), 0o644); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeInode error %v, got %v", errBoom, err)
		}
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		writeAddDirEntry = func(readerWriterAt, int64, *superblock, *inode, uint64, string, uint8) error { return errBoom }
		if err := createFile(newMemRW(0), 0, sb, parent, "file", []byte("abc"), 0o644); !errors.Is(err, errBoom) {
			t.Fatalf("expected addDirEntry error %v, got %v", errBoom, err)
		}
	})
}

func TestDirectoryWriteHelpersAdditional(t *testing.T) {
	sb := defaultSB()

	t.Run("addDirEntry local and convert", func(t *testing.T) {
		oldWrite := writeWriteInode
		oldConvert := writeConvertSFToBlock
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		writeConvertSFToBlock = func(readerWriterAt, int64, *superblock, *inode, uint64, string, uint8) error { return errBoom }
		t.Cleanup(func() {
			writeWriteInode = oldWrite
			writeConvertSFToBlock = oldConvert
		})
		dirIn := newTestInode(12, 0x4000, inodeFmtLocal, 6)
		dirIn.dataFork()[0] = 0
		dirIn.dataFork()[1] = 0
		if err := addDirEntry(newMemRW(0), 0, sb, dirIn, 44, "file", 1); err != nil {
			t.Fatalf("addDirEntry local: %v", err)
		}
		if dirIn.dataFork()[0] != 1 {
			t.Fatal("addDirEntry did not append the short-form entry")
		}
		if err := addDirEntry(newMemRW(0), 0, sb, dirIn, 0x1_0000_0000, "big", 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected convertSFToBlock error %v, got %v", errBoom, err)
		}
	})

	t.Run("addDirEntry unsupported", func(t *testing.T) {
		dirIn := newTestInode(13, 0x4000, 99, 0)
		if err := addDirEntry(newMemRW(0), 0, sb, dirIn, 1, "file", 1); err == nil {
			t.Fatal("expected addDirEntry to reject unsupported directory formats")
		}
	})

	t.Run("convertSFToBlock downstream errors", func(t *testing.T) {
		// convertSFToBlock now gathers the entries and delegates the layout to
		// the single canonical writer writeWholeDir (via writeWriteWholeDir).
		// Its error surface is therefore (a) propagating a writeWholeDir
		// failure and (b) passing the right entry set + parent through.
		oldWhole := writeWriteWholeDir
		t.Cleanup(func() { writeWriteWholeDir = oldWhole })

		dirIn := newTestInode(14, 0x4000, inodeFmtLocal, 6)
		dirIn.dataFork()[0] = 0
		dirIn.dataFork()[1] = 0

		// writeWholeDir error propagates verbatim.
		writeWriteWholeDir = func(readerWriterAt, int64, *superblock, *inode, uint64, []dirEnt) error {
			return errBoom
		}
		if err := convertSFToBlock(newMemRW(0), 0, sb, dirIn, 55, "file", 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeWholeDir error %v, got %v", errBoom, err)
		}

		// The new entry is appended and "."/".." are dropped before delegating.
		var gotEnts []dirEnt
		var gotParent uint64
		writeWriteWholeDir = func(_ readerWriterAt, _ int64, _ *superblock, in *inode, parent uint64, ents []dirEnt) error {
			gotParent = parent
			gotEnts = ents
			if in != dirIn {
				t.Fatalf("convertSFToBlock passed a different inode")
			}
			return nil
		}
		if err := convertSFToBlock(newMemRW(0), 0, sb, dirIn, 55, "file", 1); err != nil {
			t.Fatalf("convertSFToBlock: %v", err)
		}
		if len(gotEnts) != 1 || gotEnts[0].name != "file" || gotEnts[0].ino != 55 {
			t.Fatalf("expected single new entry {file,55}, got %+v", gotEnts)
		}
		if gotParent != 0 {
			t.Fatalf("expected parent inode 0, got %d", gotParent)
		}
	})

	t.Run("addEntryToBlockDir paths", func(t *testing.T) {
		oldExts := writeDirExtents
		oldRead := writeReadRawBlock
		oldReadDir := writeReadDir
		oldWhole := writeWriteWholeDir
		t.Cleanup(func() {
			writeDirExtents = oldExts
			writeReadRawBlock = oldRead
			writeReadDir = oldReadDir
			writeWriteWholeDir = oldWhole
		})
		dirIn := newTestInode(15, 0x4000, inodeFmtExtents, 0)

		// dirParentIno propagates a dirExtents error.
		writeDirExtents = func(io.ReaderAt, int64, *superblock, *inode) ([]extent, error) { return nil, errBoom }
		if err := addEntryToBlockDir(newMemRW(0), 0, sb, dirIn, 1, "file", 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected dirExtents error %v, got %v", errBoom, err)
		}

		// dirParentIno propagates a raw-block read error.
		writeDirExtents = func(io.ReaderAt, int64, *superblock, *inode) ([]extent, error) {
			return []extent{{startOff: 0, startBlock: 0, count: 1}}, nil
		}
		writeReadRawBlock = func(io.ReaderAt, int64, *superblock, uint64) ([]byte, error) { return nil, errBoom }
		if err := addEntryToBlockDir(newMemRW(0), 0, sb, dirIn, 1, "file", 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected readRawBlock error %v, got %v", errBoom, err)
		}

		// Parent reads back fine; readDir error propagates.
		blk := make([]byte, sb.blockSize)
		writeReadRawBlock = func(io.ReaderAt, int64, *superblock, uint64) ([]byte, error) { return blk, nil }
		writeReadDir = func(io.ReaderAt, int64, *superblock, *inode) ([]DirEntry, error) { return nil, errBoom }
		if err := addEntryToBlockDir(newMemRW(0), 0, sb, dirIn, 1, "file", 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected readDir error %v, got %v", errBoom, err)
		}

		// Existing entry with the same name is rejected before any layout work.
		writeReadDir = func(io.ReaderAt, int64, *superblock, *inode) ([]DirEntry, error) {
			return []DirEntry{{Inode: 9, Name: "file", FileType: 1}}, nil
		}
		if err := addEntryToBlockDir(newMemRW(0), 0, sb, dirIn, 1, "file", 1); err == nil {
			t.Fatal("expected duplicate-name error")
		}

		// writeWholeDir error propagates.
		writeReadDir = func(io.ReaderAt, int64, *superblock, *inode) ([]DirEntry, error) { return nil, nil }
		writeWriteWholeDir = func(readerWriterAt, int64, *superblock, *inode, uint64, []dirEnt) error { return errBoom }
		if err := addEntryToBlockDir(newMemRW(0), 0, sb, dirIn, 1, "file", 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeWholeDir error %v, got %v", errBoom, err)
		}

		// Happy path: writeWholeDir succeeds, new entry passed through.
		var gotEntries []dirEnt
		writeWriteWholeDir = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ uint64, ents []dirEnt) error {
			gotEntries = ents
			return nil
		}
		if err := addEntryToBlockDir(newMemRW(0), 0, sb, dirIn, 1, "file", 1); err != nil {
			t.Fatalf("addEntryToBlockDir success: %v", err)
		}
		if len(gotEntries) != 1 || gotEntries[0].name != "file" {
			t.Fatalf("expected new entry passed to writeWholeDir, got %+v", gotEntries)
		}
	})
}

func TestOverwriteFileRemainingBranches(t *testing.T) {
	sb := defaultSB()
	data := make([]byte, sb.blockSize*2)

	t.Run("local forkOff capacity", func(t *testing.T) {
		oldRead := writeReadInode
		oldWrite := writeWriteInode
		var wrote *inode
		writeReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			in := newTestInode(20, 0x8000, inodeFmtLocal, 0)
			in.forkOff = 2
			return in, nil
		}
		writeWriteInode = func(_ io.WriterAt, _ int64, _ *superblock, in *inode) error { wrote = in; return nil }
		t.Cleanup(func() {
			writeReadInode = oldRead
			writeWriteInode = oldWrite
		})

		if err := overwriteFile(newMemRW(0), 0, sb, 20, []byte("1234567890123456")); err != nil {
			t.Fatalf("overwriteFile forkOff: %v", err)
		}
		if wrote == nil || wrote.size != 16 {
			t.Fatal("overwriteFile did not honor forkOff-limited inline capacity")
		}
	})

	t.Run("extents in place and realloc", func(t *testing.T) {
		oldRead := writeReadInode
		oldInline := writeInlineExtents
		oldWrite := writeWriteInode
		oldFree := writeFreeBlocks
		oldAlloc := writeAllocBlocks
		oldList := writeWriteExtentList
		oldData := writeWriteBlocksData
		writeReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			in := newTestInode(21, 0x8000, inodeFmtExtents, 0)
			return in, nil
		}
		writeInlineExtents = func(*inode) ([]extent, error) { return []extent{{startOff: 0, startBlock: 1, count: 1}}, nil }
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		writeFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }
		allocCalls := 0
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			allocCalls++
			if allocCalls == 1 {
				return 0, errBoom
			}
			return 9, nil
		}
		writeWriteExtentList = func(*inode, []extent) error { return nil }
		writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error { return nil }
		t.Cleanup(func() {
			writeReadInode = oldRead
			writeInlineExtents = oldInline
			writeWriteInode = oldWrite
			writeFreeBlocks = oldFree
			writeAllocBlocks = oldAlloc
			writeWriteExtentList = oldList
			writeWriteBlocksData = oldData
		})

		if err := overwriteFile(newMemRW(int(sb.blockSize)), 0, sb, 21, []byte("abc")); err != nil {
			t.Fatalf("overwriteFile extents in place: %v", err)
		}
		if err := overwriteFile(newMemRW(0), 0, sb, 21, data); err != nil {
			t.Fatalf("overwriteFile extents realloc: %v", err)
		}
		if allocCalls < 2 {
			t.Fatal("overwriteFile did not retry extent allocation on another AG")
		}
	})

	t.Run("btree in place and realloc", func(t *testing.T) {
		oldRead := writeReadInode
		oldBtree := writeBtreeExtents
		oldWrite := writeWriteInode
		oldFree := writeFreeBlocks
		oldAlloc := writeAllocBlocks
		oldList := writeWriteExtentList
		oldData := writeWriteBlocksData
		writeReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			return newTestInode(22, 0x8000, inodeFmtBtree, 0), nil
		}
		writeBtreeExtents = func(io.ReaderAt, int64, *superblock, *inode) ([]extent, error) {
			return []extent{{startOff: 0, startBlock: 1, count: 1}}, nil
		}
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		writeFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 12, nil }
		writeWriteExtentList = func(*inode, []extent) error { return nil }
		writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error { return nil }
		t.Cleanup(func() {
			writeReadInode = oldRead
			writeBtreeExtents = oldBtree
			writeWriteInode = oldWrite
			writeFreeBlocks = oldFree
			writeAllocBlocks = oldAlloc
			writeWriteExtentList = oldList
			writeWriteBlocksData = oldData
		})

		if err := overwriteFile(newMemRW(int(sb.blockSize)), 0, sb, 22, []byte("abc")); err != nil {
			t.Fatalf("overwriteFile btree in place: %v", err)
		}
		if err := overwriteFile(newMemRW(0), 0, sb, 22, data); err != nil {
			t.Fatalf("overwriteFile btree realloc: %v", err)
		}
	})
}

func TestCreateFileRemainingBranches(t *testing.T) {
	sb := defaultSB()
	parent := newTestInode(23, 0x4000, inodeFmtLocal, 0)

	t.Run("alloc inode fallback", func(t *testing.T) {
		oldAllocInode := writeAllocInode
		oldWrite := writeWriteInode
		oldAdd := writeAddDirEntry
		calls := 0
		writeAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) {
			calls++
			if calls == 1 {
				return 0, errBoom
			}
			return 91, nil
		}
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		writeAddDirEntry = func(readerWriterAt, int64, *superblock, *inode, uint64, string, uint8) error { return nil }
		t.Cleanup(func() {
			writeAllocInode = oldAllocInode
			writeWriteInode = oldWrite
			writeAddDirEntry = oldAdd
		})

		if err := createFile(newMemRW(0), 0, sb, parent, "empty", nil, 0o644); err != nil {
			t.Fatalf("createFile inode fallback: %v", err)
		}
		if calls != 2 {
			t.Fatal("createFile did not retry inode allocation on another AG")
		}
	})

	t.Run("alloc blocks fallback and no space", func(t *testing.T) {
		oldAllocInode := writeAllocInode
		oldAllocBlocks := writeAllocBlocks
		oldList := writeWriteExtentList
		oldData := writeWriteBlocksData
		oldWrite := writeWriteInode
		oldAdd := writeAddDirEntry
		writeAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) { return 92, nil }
		blockCalls := 0
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			blockCalls++
			if blockCalls == 1 {
				return 0, errBoom
			}
			return 13, nil
		}
		writeWriteExtentList = func(*inode, []extent) error { return nil }
		writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error { return nil }
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		writeAddDirEntry = func(readerWriterAt, int64, *superblock, *inode, uint64, string, uint8) error { return nil }
		t.Cleanup(func() {
			writeAllocInode = oldAllocInode
			writeAllocBlocks = oldAllocBlocks
			writeWriteExtentList = oldList
			writeWriteBlocksData = oldData
			writeWriteInode = oldWrite
			writeAddDirEntry = oldAdd
		})

		if err := createFile(newMemRW(0), 0, sb, parent, "file", []byte("abc"), 0o644); err != nil {
			t.Fatalf("createFile block fallback: %v", err)
		}
		if blockCalls < 2 {
			t.Fatal("createFile did not retry block allocation on another AG")
		}
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 0, errBoom }
		if err := createFile(newMemRW(0), 0, sb, parent, "file", []byte("abc"), 0o644); err == nil {
			t.Fatal("expected createFile to fail when no allocation group has space")
		}
	})
}

func TestDirectoryWriteHelpersRemaining(t *testing.T) {
	sb := defaultSB()

	t.Run("addDirEntry extent path", func(t *testing.T) {
		oldExts := writeDirExtents
		oldRead := writeReadRawBlock
		oldWrite := writeWriteRawBlock
		writeDirExtents = func(io.ReaderAt, int64, *superblock, *inode) ([]extent, error) {
			return []extent{{startOff: 0, startBlock: 0, count: 1}}, nil
		}
		blk := make([]byte, sb.blockSize)
		markSlotFree(blk, dirDataHdrSize(sb.hasCRC), len(blk)-dirDataHdrSize(sb.hasCRC))
		writeReadRawBlock = func(io.ReaderAt, int64, *superblock, uint64) ([]byte, error) { return blk, nil }
		writeWriteRawBlock = func(io.WriterAt, int64, *superblock, uint64, []byte) error { return nil }
		t.Cleanup(func() {
			writeDirExtents = oldExts
			writeReadRawBlock = oldRead
			writeWriteRawBlock = oldWrite
		})
		dirIn := newTestInode(24, 0x4000, inodeFmtExtents, 0)
		if err := addDirEntry(newMemRW(0), 0, sb, dirIn, 7, "file", 1); err != nil {
			t.Fatalf("addDirEntry extents: %v", err)
		}
	})

	t.Run("convertSFToBlock alloc and v4 success", func(t *testing.T) {
		oldAlloc := writeAllocBlocks
		oldData := writeWriteBlocksData
		oldList := writeWriteExtentList
		oldWrite := writeWriteInode
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 0, errBoom }
		t.Cleanup(func() {
			writeAllocBlocks = oldAlloc
			writeWriteBlocksData = oldData
			writeWriteExtentList = oldList
			writeWriteInode = oldWrite
		})
		dirIn := newTestInode(25, 0x4000, inodeFmtLocal, 0)
		fork := buildSFDir(1, []struct {
			name string
			ino  uint32
		}{
			{".", 25},
			{"old", 26},
		}, false)
		copy(dirIn.dataFork(), fork)
		setInodeSize(dirIn, uint64(len(fork)))
		if err := convertSFToBlock(newMemRW(0), 0, sb, dirIn, 55, "new", 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected convertSFToBlock alloc error %v, got %v", errBoom, err)
		}
		sbNoCRC := *sb
		sbNoCRC.hasCRC = false
		writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 4, nil }
		writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error { return nil }
		writeWriteExtentList = func(*inode, []extent) error { return nil }
		writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
		if err := convertSFToBlock(newMemRW(0), 0, &sbNoCRC, dirIn, 55, "new", 1); err != nil {
			t.Fatalf("convertSFToBlock v4: %v", err)
		}
	})
}

func TestWriteAdditionalFinalBranches(t *testing.T) {
	sb := defaultSB()

	t.Run("overwriteFile btree extent error", func(t *testing.T) {
		oldRead := writeReadInode
		oldBtree := writeBtreeExtents
		writeReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			return newTestInode(120, 0x8000, inodeFmtBtree, 0), nil
		}
		writeBtreeExtents = func(io.ReaderAt, int64, *superblock, *inode) ([]extent, error) { return nil, errBoom }
		t.Cleanup(func() {
			writeReadInode = oldRead
			writeBtreeExtents = oldBtree
		})
		if err := overwriteFile(newMemRW(0), 0, sb, 120, []byte("x")); !errors.Is(err, errBoom) {
			t.Fatalf("expected btree overwrite error %v, got %v", errBoom, err)
		}
	})

	t.Run("convertSFToBlock sfReadDir error", func(t *testing.T) {
		oldSFReadDir := writeSFReadDir
		writeSFReadDir = func([]byte, bool) ([]DirEntry, error) { return nil, errBoom }
		t.Cleanup(func() { writeSFReadDir = oldSFReadDir })
		dirIn := newTestInode(121, 0x4000, inodeFmtLocal, 0)
		if err := convertSFToBlock(newMemRW(0), 0, sb, dirIn, 7, "new", 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected convertSFToBlock sfReadDir error %v, got %v", errBoom, err)
		}
	})

	t.Run("addEntryToBlockDir skips leaf extents to find the parent data block", func(t *testing.T) {
		oldDirExtents := writeDirExtents
		oldReadRaw := writeReadRawBlock
		oldReadDir := writeReadDir
		oldWhole := writeWriteWholeDir
		leafLogBlock := dirLeafByteOffset / uint64(sb.blockSize)
		// First extent is a leaf block (must be skipped); the logical-0 data
		// block holding ".." is block 2.
		writeDirExtents = func(io.ReaderAt, int64, *superblock, *inode) ([]extent, error) {
			return []extent{{startOff: leafLogBlock, startBlock: 1, count: 1}, {startOff: 0, startBlock: 2, count: 1}}, nil
		}
		var readBlock uint64
		writeReadRawBlock = func(_ io.ReaderAt, _ int64, _ *superblock, block uint64) ([]byte, error) {
			readBlock = block
			if block != 2 {
				t.Fatalf("read block %d; leaf extent should have been skipped", block)
			}
			return make([]byte, sb.blockSize), nil
		}
		writeReadDir = func(io.ReaderAt, int64, *superblock, *inode) ([]DirEntry, error) { return nil, nil }
		wrote := false
		writeWriteWholeDir = func(readerWriterAt, int64, *superblock, *inode, uint64, []dirEnt) error {
			wrote = true
			return nil
		}
		t.Cleanup(func() {
			writeDirExtents = oldDirExtents
			writeReadRawBlock = oldReadRaw
			writeReadDir = oldReadDir
			writeWriteWholeDir = oldWhole
		})
		if err := addEntryToBlockDir(newMemRW(0), 0, sb, newTestInode(122, 0x4000, inodeFmtExtents, 0), 7, "file", 1); err != nil {
			t.Fatalf("addEntryToBlockDir: %v", err)
		}
		if readBlock != 2 || !wrote {
			t.Fatalf("expected parent read from data block 2 then writeWholeDir, readBlock=%d wrote=%v", readBlock, wrote)
		}
	})
}
