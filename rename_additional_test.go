package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func restoreRenameHooks(t *testing.T) {
	oldPathLookup := renamePathLookup
	oldLookupInDir := renameLookupInDir
	oldReadInode := renameReadInode
	oldDeleteDir := renameDeleteDir
	oldDeleteFile := renameDeleteFile
	oldAddDirEntry := renameAddDirEntry
	oldRemoveDirEntry := renameRemoveDirEntry
	oldUpdateDotDot := renameUpdateDotDot
	oldInlineExtents := renameInlineExtents
	oldBtreeExtents := renameBtreeExtents
	oldReadRawBlock := renameReadRawBlock
	oldWriteRawBlock := renameWriteRawBlock
	oldWriteInode := renameWriteInode
	t.Cleanup(func() {
		renamePathLookup = oldPathLookup
		renameLookupInDir = oldLookupInDir
		renameReadInode = oldReadInode
		renameDeleteDir = oldDeleteDir
		renameDeleteFile = oldDeleteFile
		renameAddDirEntry = oldAddDirEntry
		renameRemoveDirEntry = oldRemoveDirEntry
		renameUpdateDotDot = oldUpdateDotDot
		renameInlineExtents = oldInlineExtents
		renameBtreeExtents = oldBtreeExtents
		renameReadRawBlock = oldReadRawBlock
		renameWriteRawBlock = oldWriteRawBlock
		renameWriteInode = oldWriteInode
	})
}

func TestRenameEntryAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)

	t.Run("parent lookup errors", func(t *testing.T) {
		restoreRenameHooks(t)
		renamePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, p string) (*inode, error) {
			return nil, errBoom
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected source parent error %v, got %v", errBoom, err)
		}

		oldParent := newTestInode(1, 0x4000, inodeFmtLocal, 0)
		renamePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, p string) (*inode, error) {
			if p == "/from" {
				return oldParent, nil
			}
			return nil, errBoom
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected destination parent error %v, got %v", errBoom, err)
		}
	})

	t.Run("source lookup and read errors", func(t *testing.T) {
		restoreRenameHooks(t)
		oldParent := newTestInode(1, 0x4000, inodeFmtLocal, 0)
		newParent := newTestInode(2, 0x4000, inodeFmtLocal, 0)
		renamePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, p string) (*inode, error) {
			if p == "/from" {
				return oldParent, nil
			}
			return newParent, nil
		}
		renameLookupInDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, name string) (uint64, error) {
			return 0, errBoom
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected source lookup error %v, got %v", errBoom, err)
		}

		renameLookupInDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, name string) (uint64, error) {
			return 7, nil
		}
		renameReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			return nil, errBoom
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected source read error %v, got %v", errBoom, err)
		}
	})

	t.Run("noop and destination lookup error", func(t *testing.T) {
		restoreRenameHooks(t)
		parent := newTestInode(3, 0x4000, inodeFmtLocal, 0)
		renamePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, p string) (*inode, error) { return parent, nil }
		renameLookupInDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, name string) (uint64, error) {
			if name == "same" {
				return 11, nil
			}
			return 0, errBoom
		}
		renameReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			return newTestInode(11, 0x8000, inodeFmtLocal, 0), nil
		}
		renameAddDirEntry = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ uint64, name string, ftype uint8) error {
			t.Fatal("renameAddDirEntry should not run on no-op")
			return nil
		}
		renameRemoveDirEntry = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ string) error {
			t.Fatal("renameRemoveDirEntry should not run on no-op")
			return nil
		}
		if err := renameEntry(rw, 0, sb, "/same", "/same"); err != nil {
			t.Fatalf("rename no-op: %v", err)
		}

		renameLookupInDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, name string) (uint64, error) {
			if name == "old" {
				return 11, nil
			}
			return 0, errBoom
		}
		if err := renameEntry(rw, 0, sb, "/old", "/new"); !errors.Is(err, errBoom) {
			t.Fatalf("expected destination lookup error %v, got %v", errBoom, err)
		}
	})

	t.Run("existing destination handling", func(t *testing.T) {
		restoreRenameHooks(t)
		oldParent := newTestInode(4, 0x4000, inodeFmtLocal, 0)
		newParent := newTestInode(5, 0x4000, inodeFmtLocal, 0)
		renamePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, p string) (*inode, error) {
			if p == "/from" {
				return oldParent, nil
			}
			return newParent, nil
		}
		renameLookupInDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, name string) (uint64, error) {
			if name == "src" {
				return 20, nil
			}
			return 21, nil
		}
		renameReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			switch ino {
			case 20:
				return newTestInode(20, 0x4000, inodeFmtLocal, 0), nil
			case 21:
				return nil, errBoom
			default:
				return nil, ErrNotFound
			}
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected destination read error %v, got %v", errBoom, err)
		}

		renameReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			if ino == 20 {
				return newTestInode(20, 0x4000, inodeFmtLocal, 0), nil
			}
			return newTestInode(21, 0x8000, inodeFmtLocal, 0), nil
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); err == nil {
			t.Fatal("expected directory over file mismatch error")
		}

		renameReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			if ino == 20 {
				return newTestInode(20, 0x8000, inodeFmtLocal, 0), nil
			}
			return newTestInode(21, 0x4000, inodeFmtLocal, 0), nil
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); err == nil {
			t.Fatal("expected file over directory mismatch error")
		}

		renameReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			if ino == 20 {
				return newTestInode(20, 0x4000, inodeFmtLocal, 0), nil
			}
			return newTestInode(21, 0x4000, inodeFmtLocal, 0), nil
		}
		renameDeleteDir = func(_ readerWriterAt, _ int64, _ *superblock, _ string) error { return errBoom }
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected deleteDir error %v, got %v", errBoom, err)
		}

		renameReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			if ino == 20 {
				return newTestInode(20, 0x8000, inodeFmtLocal, 0), nil
			}
			return newTestInode(21, 0x8000, inodeFmtLocal, 0), nil
		}
		renameDeleteFile = func(_ readerWriterAt, _ int64, _ *superblock, _ string) error { return errBoom }
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected deleteFile error %v, got %v", errBoom, err)
		}
	})

	t.Run("re-read, add, remove and dotdot paths", func(t *testing.T) {
		restoreRenameHooks(t)
		oldParent := newTestInode(6, 0x4000, inodeFmtLocal, 0)
		newParent := newTestInode(7, 0x4000, inodeFmtLocal, 0)
		pathCalls := 0
		renamePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, p string) (*inode, error) {
			pathCalls++
			switch pathCalls {
			case 1, 3, 5:
				return oldParent, nil
			case 2, 4:
				return newParent, nil
			default:
				return oldParent, nil
			}
		}
		renameLookupInDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, name string) (uint64, error) {
			if name == "src" {
				return 30, nil
			}
			return 0, ErrNotFound
		}
		renameReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			return newTestInode(30, 0x4000, inodeFmtLocal, 0), nil
		}
		renameAddDirEntry = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ uint64, name string, ftype uint8) error {
			return errBoom
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected addDirEntry error %v, got %v", errBoom, err)
		}

		pathCalls = 0
		renameAddDirEntry = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ uint64, name string, ftype uint8) error {
			return nil
		}
		renamePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, p string) (*inode, error) {
			pathCalls++
			if pathCalls == 5 {
				return nil, errBoom
			}
			if p == "/from" {
				return oldParent, nil
			}
			return newParent, nil
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected post-add parent reread error %v, got %v", errBoom, err)
		}

		pathCalls = 0
		renamePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, p string) (*inode, error) {
			pathCalls++
			if pathCalls == 3 {
				return nil, errBoom
			}
			if p == "/from" {
				return oldParent, nil
			}
			return newParent, nil
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected old parent reread error %v, got %v", errBoom, err)
		}

		pathCalls = 0
		renamePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, p string) (*inode, error) {
			pathCalls++
			if pathCalls == 4 {
				return nil, errBoom
			}
			if p == "/from" {
				return oldParent, nil
			}
			return newParent, nil
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected new parent reread error %v, got %v", errBoom, err)
		}

		pathCalls = 0
		renamePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, p string) (*inode, error) {
			pathCalls++
			if p == "/from" {
				return oldParent, nil
			}
			return newParent, nil
		}
		renameRemoveDirEntry = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ string) error { return errBoom }
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected removeDirEntry error %v, got %v", errBoom, err)
		}

		renameRemoveDirEntry = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ string) error { return nil }
		readCalls := 0
		renameReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			readCalls++
			if readCalls == 2 {
				return nil, errBoom
			}
			return newTestInode(30, 0x4000, inodeFmtLocal, 0), nil
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected source reread error %v, got %v", errBoom, err)
		}

		renameReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			return newTestInode(30, 0x4000, inodeFmtLocal, 0), nil
		}
		renameUpdateDotDot = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ uint64) error { return errBoom }
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); !errors.Is(err, errBoom) {
			t.Fatalf("expected updateDotDot error %v, got %v", errBoom, err)
		}

		var gotName string
		var gotType uint8
		var gotParent uint64
		renameUpdateDotDot = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ uint64) error {
			gotParent = newParent.num
			return nil
		}
		renameAddDirEntry = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ uint64, name string, ftype uint8) error {
			gotName = name
			gotType = ftype
			return nil
		}
		if err := renameEntry(rw, 0, sb, "/from/src", "/to/dst"); err != nil {
			t.Fatalf("renameEntry success: %v", err)
		}
		if gotName != "dst" || gotType != 2 || gotParent != newParent.num {
			t.Fatalf("unexpected rename bookkeeping: name=%q type=%d parent=%d", gotName, gotType, gotParent)
		}
	})

	t.Run("symlink destination file type", func(t *testing.T) {
		restoreRenameHooks(t)
		parent := newTestInode(8, 0x4000, inodeFmtLocal, 0)
		renamePathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, p string) (*inode, error) { return parent, nil }
		renameLookupInDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, name string) (uint64, error) {
			if name == "src" {
				return 40, nil
			}
			return 0, ErrNotFound
		}
		renameReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			return newTestInode(40, 0xa000, inodeFmtLocal, 0), nil
		}
		var gotType uint8
		renameAddDirEntry = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ uint64, name string, ftype uint8) error {
			gotType = ftype
			return nil
		}
		renameRemoveDirEntry = func(_ readerWriterAt, _ int64, _ *superblock, _ *inode, _ string) error { return nil }
		if err := renameEntry(rw, 0, sb, "/src", "/dst"); err != nil {
			t.Fatalf("rename symlink: %v", err)
		}
		if gotType != 7 {
			t.Fatalf("expected symlink dirent type 7, got %d", gotType)
		}
	})
}

func TestXFSUpdateDotDotAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)

	t.Run("local format validation and writes", func(t *testing.T) {
		restoreRenameHooks(t)
		short := newTestInode(50, 0x4000, inodeFmtLocal, 0)
		short.raw = short.raw[:inodeCoreSize+5]
		if err := xfsUpdateDotDot(rw, 0, sb, short, 99); err == nil {
			t.Fatal("expected short local fork error")
		}

		i8 := newTestInode(51, 0x4000, inodeFmtLocal, 0)
		i8.raw = i8.raw[:inodeCoreSize+9]
		fork := i8.dataFork()
		fork[0] = 0
		fork[1] = 1
		if err := xfsUpdateDotDot(rw, 0, sb, i8, 99); err == nil {
			t.Fatal("expected 8-byte parent short fork error")
		}

		local := newTestInode(52, 0x4000, inodeFmtLocal, 0)
		copy(local.dataFork(), buildSFDir(1, nil, sb.hasFType))
		setInodeSize(local, 6)
		renameWriteInode = func(_ io.WriterAt, _ int64, _ *superblock, _ *inode) error { return errBoom }
		if err := xfsUpdateDotDot(rw, 0, sb, local, 123); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeInode error %v, got %v", errBoom, err)
		}
		renameWriteInode = func(_ io.WriterAt, _ int64, _ *superblock, _ *inode) error { return nil }
		if err := xfsUpdateDotDot(rw, 0, sb, local, 123); err != nil {
			t.Fatalf("xfsUpdateDotDot local: %v", err)
		}
		if got := binary.BigEndian.Uint32(local.dataFork()[2:6]); got != 123 {
			t.Fatalf("local parent ino=%d, want 123", got)
		}
	})

	t.Run("extent and btree errors", func(t *testing.T) {
		restoreRenameHooks(t)
		extentDir := newTestInode(53, 0x4000, inodeFmtExtents, 0)
		renameInlineExtents = func(*inode) ([]extent, error) { return nil, errBoom }
		if err := xfsUpdateDotDot(rw, 0, sb, extentDir, 77); !errors.Is(err, errBoom) {
			t.Fatalf("expected inlineExtents error %v, got %v", errBoom, err)
		}

		btreeDir := newTestInode(54, 0x4000, inodeFmtBtree, 0)
		renameBtreeExtents = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]extent, error) { return nil, errBoom }
		if err := xfsUpdateDotDot(rw, 0, sb, btreeDir, 77); !errors.Is(err, errBoom) {
			t.Fatalf("expected btreeExtents error %v, got %v", errBoom, err)
		}
	})

	t.Run("block form read, write and miss cases", func(t *testing.T) {
		restoreRenameHooks(t)
		dirIn := newTestInode(55, 0x4000, inodeFmtExtents, 0)
		renameInlineExtents = func(*inode) ([]extent, error) {
			return []extent{{startOff: 0, startBlock: 3, count: 1}}, nil
		}
		renameReadRawBlock = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) ([]byte, error) {
			return nil, errBoom
		}
		if err := xfsUpdateDotDot(rw, 0, sb, dirIn, 88); !errors.Is(err, errBoom) {
			t.Fatalf("expected readRawBlock error %v, got %v", errBoom, err)
		}

		blk := buildDirBlock([]struct {
			ino  uint64
			name string
			ft   uint8
		}{{ino: 1, name: "..", ft: 2}}, sb.hasFType)
		var written []byte
		renameReadRawBlock = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) ([]byte, error) {
			dup := append([]byte(nil), blk...)
			return dup, nil
		}
		renameWriteRawBlock = func(_ io.WriterAt, _ int64, _ *superblock, _ uint64, buf []byte) error {
			written = append([]byte(nil), buf...)
			return nil
		}
		if err := xfsUpdateDotDot(rw, 0, sb, dirIn, 88); err != nil {
			t.Fatalf("xfsUpdateDotDot block form: %v", err)
		}
		if got := binary.BigEndian.Uint64(written[dir3DataHdrSize:]); got != 88 {
			t.Fatalf("updated .. inode=%d, want 88", got)
		}

		written = nil
		blk = buildDirBlock([]struct {
			ino  uint64
			name string
			ft   uint8
		}{{ino: 1, name: "child", ft: 2}}, sb.hasFType)
		renameReadRawBlock = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) ([]byte, error) {
			return append([]byte(nil), blk...), nil
		}
		if err := xfsUpdateDotDot(rw, 0, sb, dirIn, 88); err != nil {
			t.Fatalf("xfsUpdateDotDot no-dotdot: %v", err)
		}
		if written != nil {
			t.Fatal("xfsUpdateDotDot should not write when .. is absent")
		}
	})

	t.Run("unsupported format", func(t *testing.T) {
		restoreRenameHooks(t)
		bad := newTestInode(56, 0x4000, 99, 0)
		if err := xfsUpdateDotDot(rw, 0, sb, bad, 77); err == nil {
			t.Fatal("expected unsupported format error")
		}
	})
}

func TestXFSUpdateDotDotRemainingBranches(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)

	t.Run("local 8-byte parent success", func(t *testing.T) {
		restoreRenameHooks(t)
		in := newTestInode(57, 0x4000, inodeFmtLocal, 0)
		fork := in.dataFork()
		fork[0] = 0
		fork[1] = 1
		renameWriteInode = func(_ io.WriterAt, _ int64, _ *superblock, _ *inode) error { return nil }
		if err := xfsUpdateDotDot(rw, 0, sb, in, 0x1122334455667788); err != nil {
			t.Fatalf("xfsUpdateDotDot 8-byte parent: %v", err)
		}
		if got := binary.BigEndian.Uint64(fork[2:10]); got != 0x1122334455667788 {
			t.Fatalf("8-byte parent ino=%x, want %x", got, uint64(0x1122334455667788))
		}
	})

	t.Run("block edge cases do not write", func(t *testing.T) {
		restoreRenameHooks(t)
		dirIn := newTestInode(58, 0x4000, inodeFmtExtents, 0)
		leafLogBlock := dirLeafByteOffset / uint64(sb.blockSize)
		readCalled := false
		renameInlineExtents = func(*inode) ([]extent, error) {
			return []extent{{startOff: leafLogBlock, startBlock: 1, count: 1}}, nil
		}
		renameReadRawBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint64) ([]byte, error) {
			readCalled = true
			return nil, nil
		}
		if err := xfsUpdateDotDot(rw, 0, sb, dirIn, 77); err != nil {
			t.Fatalf("xfsUpdateDotDot leaf-skip: %v", err)
		}
		if readCalled {
			t.Fatal("xfsUpdateDotDot should skip non-data extents")
		}

		runNoWriteCase := func(t *testing.T, blk []byte) {
			t.Helper()
			restoreRenameHooks(t)
			wrote := false
			renameInlineExtents = func(*inode) ([]extent, error) {
				return []extent{{startOff: 0, startBlock: 2, count: 1}}, nil
			}
			renameReadRawBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint64) ([]byte, error) {
				return append([]byte(nil), blk...), nil
			}
			renameWriteRawBlock = func(_ io.WriterAt, _ int64, _ *superblock, _ uint64, _ []byte) error {
				wrote = true
				return nil
			}
			if err := xfsUpdateDotDot(rw, 0, sb, dirIn, 77); err != nil {
				t.Fatalf("xfsUpdateDotDot edge case: %v", err)
			}
			if wrote {
				t.Fatal("xfsUpdateDotDot should not write for non-matching malformed blocks")
			}
		}

		hdr := dirDataHdrSize(sb.hasCRC)
		blk := make([]byte, sb.blockSize)
		binary.BigEndian.PutUint16(blk[hdr:], dirFreeTag)
		binary.BigEndian.PutUint16(blk[hdr+2:], 4)
		runNoWriteCase(t, blk)

		blk = make([]byte, sb.blockSize)
		runNoWriteCase(t, blk)

		blk = make([]byte, sb.blockSize)
		binary.BigEndian.PutUint64(blk[hdr:], 1)
		blk[hdr+8] = 0
		runNoWriteCase(t, blk)

		blk = make([]byte, hdr+10)
		binary.BigEndian.PutUint64(blk[hdr:], 1)
		blk[hdr+8] = 5
		runNoWriteCase(t, blk)
	})
}
