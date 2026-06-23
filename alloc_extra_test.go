package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestAllocBlocksAdditional(t *testing.T) {
	sb := allocTestSB()
	be := binary.BigEndian

	t.Run("top-level errors and exact fit", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return nil, errBoom }
		if _, err := allocBlocks(newMemRW(0), 0, sb, 0, 5); !errors.Is(err, errBoom) {
			t.Fatalf("expected allocBlocks AGF error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agf := makeAGFBuffer(sb, 0, 7, 5, 0, 0, 100, 50)
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agf, nil }
		allocCntFindBlock = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 0, nil, 0, errBoom
		}
		if _, err := allocBlocks(newMemRW(0), 0, sb, 0, 5); !errors.Is(err, errBoom) {
			t.Fatalf("expected allocBlocks cntFindBlock error %v, got %v", errBoom, err)
		}

		leaf := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 4})
		allocCntFindBlock = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		if _, err := allocBlocks(newMemRW(0), 0, sb, 0, 5); err == nil {
			t.Fatal("expected allocBlocks to reject too-small extents")
		}

		// The whole-extent (remaining==0) delete path now descends the real cnt/bno
		// trees (allocDeleteRecordCollect), so these cases install REAL single-leaf
		// roots in the memRW rather than mocking the delete hooks. cnt root = block 5
		// (one record {10,5}), bno root = block 7 (same extent).
		setupExactFit := func() *memRW {
			// Reset the find/read/AGF hooks to the real implementations (prior cases in
			// this subtest left synthetic mocks installed; restoreAllocHooks captures
			// the current values rather than resetting them).
			allocReadAGBlock = readAGBlock
			allocWriteAGBTree = writeAGBTree
			allocCntFindBlock = cntFindBlock
			allocAGFBlock = agfBlock
			allocWriteAGF = writeAGF
			allocBnoFindRecord = bnoFindRecord
			rw := newAllocRW(sb)
			putAGF(rw, 0, sb, 0, makeAGFBuffer(sb, 0, 7, 5, 1, 1, 100, 50))
			putFSBlock(rw, 0, sb, 0, 5, makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 5}))
			putFSBlock(rw, 0, sb, 0, 7, makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 5}))
			return rw
		}

		// cnt delete write error: the cnt delete removes the record then writes the
		// leaf back; fail that write so the whole-extent delete path errors.
		restoreAllocHooks(t)
		rwCnt := setupExactFit()
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return errBoom }
		if _, err := allocBlocks(rwCnt, 0, sb, 0, 5); !errors.Is(err, errBoom) {
			t.Fatalf("expected cnt delete error %v, got %v", errBoom, err)
		}

		// AGF write error during the delete's AGF update (no collapse here, but the
		// final free-count/longest write fails).
		restoreAllocHooks(t)
		rwW := setupExactFit()
		allocWriteAGF = func(io.WriterAt, int64, *superblock, uint32, []byte) error { return errBoom }
		if _, err := allocBlocks(rwW, 0, sb, 0, 5); !errors.Is(err, errBoom) {
			t.Fatalf("expected allocBlocks writeAGF error %v, got %v", errBoom, err)
		}

		// Exact-fit success: the extent is fully consumed, deleted from both trees,
		// the AGF free count drops by 5 and longest recomputes (mocked to 0).
		restoreAllocHooks(t)
		rwOK := setupExactFit()
		var wroteFree bool
		allocWriteAGF = func(rw io.WriterAt, p int64, s *superblock, ag uint32, buf []byte) error {
			if be.Uint32(buf[agfOffFreeBlks:]) == 95 {
				wroteFree = true
			}
			return writeAGF(rw, p, s, ag, buf)
		}
		abs, err := allocBlocks(rwOK, 0, sb, 0, 5)
		if err != nil || abs != sb.agAbsBlock(0, 10) {
			t.Fatalf("allocBlocks exact fit = (%d, %v), want (%d, nil)", abs, err, sb.agAbsBlock(0, 10))
		}
		if !wroteFree {
			t.Fatal("expected allocBlocks to write free count 95 after exact-fit consume")
		}
	})

	t.Run("partial allocation", func(t *testing.T) {
		restoreAllocHooks(t)
		agf := makeAGFBuffer(sb, 0, 7, 5, 0, 0, 100, 50)
		leaf := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 9})
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agf, nil }
		allocCntFindBlock = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return errBoom }
		if _, err := allocBlocks(newMemRW(0), 0, sb, 0, 5); !errors.Is(err, errBoom) {
			t.Fatalf("expected partial alloc writeAGBTree error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agf = makeAGFBuffer(sb, 0, 7, 5, 0, 0, 100, 50)
		leaf = makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 9})
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agf, nil }
		allocCntFindBlock = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		allocBnoUpdateRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32, uint32, uint32) error {
			return errBoom
		}
		if _, err := allocBlocks(newMemRW(0), 0, sb, 0, 5); !errors.Is(err, errBoom) {
			t.Fatalf("expected partial alloc bnoUpdate error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agf = makeAGFBuffer(sb, 0, 7, 5, 0, 0, 100, 50)
		leaf = makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 9})
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agf, nil }
		allocCntFindBlock = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		allocBnoUpdateRecord = func(_ readerWriterAt, _ int64, _ *superblock, _ uint32, _ uint32, _ int, oldStart, newStart, newCount uint32) error {
			if oldStart != 10 || newStart != 15 || newCount != 4 {
				t.Fatalf("unexpected bnoUpdateRecord args %d %d %d", oldStart, newStart, newCount)
			}
			return nil
		}
		allocWriteAGF = func(io.WriterAt, int64, *superblock, uint32, []byte) error { return errBoom }
		if _, err := allocBlocks(newMemRW(0), 0, sb, 0, 5); !errors.Is(err, errBoom) {
			t.Fatalf("expected partial alloc writeAGF error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agf = makeAGFBuffer(sb, 0, 7, 5, 0, 0, 100, 50)
		leaf = makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 9})
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agf, nil }
		allocCntFindBlock = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		allocWriteAGBTree = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, _ uint32, buf []byte) error {
			if start := be.Uint32(buf[sb.agBTreeHdrSize():]); start != 15 {
				t.Fatalf("unexpected updated cnt start %d", start)
			}
			if count := be.Uint32(buf[sb.agBTreeHdrSize()+4:]); count != 4 {
				t.Fatalf("unexpected updated cnt count %d", count)
			}
			return nil
		}
		allocBnoUpdateRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32, uint32, uint32) error {
			return nil
		}
		// allocBlocks now recomputes agf_longest from the cnt B-tree; stub the
		// recompute to a known value and assert it lands in the AGF.
		allocRecomputeLongest = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, error) {
			return 4, nil
		}
		allocWriteAGF = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, buf []byte) error {
			if got := be.Uint32(buf[agfOffFreeBlks:]); got != 95 {
				t.Fatalf("unexpected free block count %d", got)
			}
			if got := be.Uint32(buf[agfOffLongest:]); got != 4 {
				t.Fatalf("unexpected longest run %d", got)
			}
			return nil
		}
		abs, err := allocBlocks(newMemRW(0), 0, sb, 0, 5)
		if err != nil || abs != 10 {
			t.Fatalf("allocBlocks partial = (%d, %v), want (10, nil)", abs, err)
		}

		// A recompute error must propagate (not be silently swallowed).
		restoreAllocHooks(t)
		agf = makeAGFBuffer(sb, 0, 7, 5, 0, 0, 100, 50)
		leaf = makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 9})
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agf, nil }
		allocCntFindBlock = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		allocBnoUpdateRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32, uint32, uint32) error {
			return nil
		}
		allocRecomputeLongest = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, error) {
			return 0, errBoom
		}
		if _, err := allocBlocks(newMemRW(0), 0, sb, 0, 5); !errors.Is(err, errBoom) {
			t.Fatalf("expected allocBlocks recompute error %v, got %v", errBoom, err)
		}
	})
}

func TestFreeBlocksAdditional(t *testing.T) {
	sb := allocTestSB()
	be := binary.BigEndian
	absStart := uint64(sb.agBlocks) + 3

	t.Run("errors and success", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return nil, errBoom }
		if err := freeBlocks(newMemRW(0), 0, sb, absStart, 7); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeBlocks AGF error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agf := makeAGFBuffer(sb, 1, 7, 5, 0, 0, 10, 5)
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agf, nil }
		allocBnoInsertRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32, uint32) error { return errBoom }
		if err := freeBlocks(newMemRW(0), 0, sb, absStart, 7); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeBlocks bno insert error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agf = makeAGFBuffer(sb, 1, 7, 5, 0, 0, 10, 5)
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agf, nil }
		allocBnoInsertRecord = func(_ readerWriterAt, _ int64, _ *superblock, _ uint32, _ uint32, _ int, start uint32, count uint32) error {
			if start != 3 || count != 7 {
				t.Fatalf("unexpected bno insert args %d %d", start, count)
			}
			return nil
		}
		allocCntInsertRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32, uint32) error { return errBoom }
		if err := freeBlocks(newMemRW(0), 0, sb, absStart, 7); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeBlocks cnt insert error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agf = makeAGFBuffer(sb, 1, 7, 5, 0, 0, 10, 5)
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agf, nil }
		allocBnoInsertRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32, uint32) error { return nil }
		allocCntInsertRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32, uint32) error { return nil }
		allocWriteAGF = func(io.WriterAt, int64, *superblock, uint32, []byte) error { return errBoom }
		if err := freeBlocks(newMemRW(0), 0, sb, absStart, 7); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeBlocks writeAGF error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agf = makeAGFBuffer(sb, 1, 7, 5, 0, 0, 10, 5)
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agf, nil }
		allocBnoInsertRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32, uint32) error { return nil }
		allocCntInsertRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32, uint32) error { return nil }
		// freeBlocks recomputes agf_longest from the cnt B-tree (a freed run can
		// coalesce into something larger); stub it and assert it reaches the AGF.
		allocRecomputeLongest = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, error) {
			return 7, nil
		}
		allocWriteAGF = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, buf []byte) error {
			if got := be.Uint32(buf[agfOffFreeBlks:]); got != 17 {
				t.Fatalf("unexpected free block count %d", got)
			}
			if got := be.Uint32(buf[agfOffLongest:]); got != 7 {
				t.Fatalf("unexpected longest run %d", got)
			}
			return nil
		}
		if err := freeBlocks(newMemRW(0), 0, sb, absStart, 7); err != nil {
			t.Fatalf("freeBlocks success: %v", err)
		}

		// A recompute error during free must propagate.
		restoreAllocHooks(t)
		agf = makeAGFBuffer(sb, 1, 7, 5, 0, 0, 10, 5)
		allocAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agf, nil }
		allocBnoInsertRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32, uint32) error { return nil }
		allocCntInsertRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32, uint32) error { return nil }
		allocRecomputeLongest = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, error) {
			return 0, errBoom
		}
		if err := freeBlocks(newMemRW(0), 0, sb, absStart, 7); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeBlocks recompute error %v, got %v", errBoom, err)
		}
	})
}

func TestAllocInodeAdditional(t *testing.T) {
	sb := allocTestSB()
	be := binary.BigEndian

	t.Run("errors and success", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return nil, errBoom }
		if _, err := allocInode(newMemRW(0), 0, sb, 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected allocInode AGI error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agi := makeAGIBuffer(sb, 1, 4, 0, 10, 0)
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agi, nil }
		allocInobtFindFree = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, []byte, int, error) {
			return 0, nil, 0, errBoom
		}
		if _, err := allocInode(newMemRW(0), 0, sb, 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected allocInode inobt error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agi = makeAGIBuffer(sb, 1, 4, 0, 10, 0)
		leaf := makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 64, freeCount: 1, irFree: 0})
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agi, nil }
		allocInobtFindFree = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		if _, err := allocInode(newMemRW(0), 0, sb, 1); err == nil {
			t.Fatal("expected allocInode to reject records with an empty ir_free bitmap")
		}

		restoreAllocHooks(t)
		agi = makeAGIBuffer(sb, 1, 4, 0, 10, 0)
		leaf = makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 64, freeCount: 1, irFree: 0x2})
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agi, nil }
		allocInobtFindFree = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return errBoom }
		if _, err := allocInode(newMemRW(0), 0, sb, 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected allocInode writeAGBTree error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agi = makeAGIBuffer(sb, 1, 4, 0, 10, 0)
		leaf = makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 64, freeCount: 1, irFree: 0x2})
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agi, nil }
		allocInobtFindFree = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		allocWriteAGI = func(io.WriterAt, int64, *superblock, uint32, []byte) error { return errBoom }
		if _, err := allocInode(newMemRW(0), 0, sb, 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected allocInode writeAGI error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agi = makeAGIBuffer(sb, 1, 4, 0, 10, 0)
		leaf = makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 64, freeCount: 1, irFree: 0x2})
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agi, nil }
		allocInobtFindFree = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		allocWriteAGI = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, buf []byte) error {
			if got := be.Uint32(buf[agiOffFreeCount:]); got != 9 {
				t.Fatalf("unexpected AGI free count %d", got)
			}
			if got := be.Uint32(buf[agiOffNewIno:]); got != 65 {
				t.Fatalf("unexpected AGI new inode %d", got)
			}
			return nil
		}
		ino, err := allocInode(newMemRW(0), 0, sb, 1)
		if err != nil || ino != inoFromAGRel(sb, 1, 65) {
			t.Fatalf("allocInode = (%d, %v), want (%d, nil)", ino, err, inoFromAGRel(sb, 1, 65))
		}
		if got := be.Uint32(leaf[sb.agBTreeHdrSize()+4:]); got != 0 {
			t.Fatalf("unexpected updated leaf free count %d", got)
		}
		if got := be.Uint64(leaf[sb.agBTreeHdrSize()+8:]); got != 0 {
			t.Fatalf("unexpected updated ir_free bitmap %x", got)
		}
	})
}

func TestFreeInodeAdditional(t *testing.T) {
	sb := allocTestSB()
	be := binary.BigEndian
	ino := inoFromAGRel(sb, 1, 66)

	t.Run("errors and success", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return nil, errBoom }
		if err := freeInode(newMemRW(0), 0, sb, ino); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeInode AGI error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agi := makeAGIBuffer(sb, 1, 4, 0, 2, 0)
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agi, nil }
		allocInobtFindRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 0, nil, 0, errBoom
		}
		if err := freeInode(newMemRW(0), 0, sb, ino); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeInode inobt error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agi = makeAGIBuffer(sb, 1, 4, 0, 2, 0)
		leaf := makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 64, freeCount: 0, irFree: 0})
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agi, nil }
		allocInobtFindRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return errBoom }
		if err := freeInode(newMemRW(0), 0, sb, ino); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeInode writeAGBTree error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agi = makeAGIBuffer(sb, 1, 4, 0, 2, 0)
		leaf = makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 64, freeCount: 0, irFree: 0})
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agi, nil }
		allocInobtFindRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		allocWriteAGI = func(io.WriterAt, int64, *superblock, uint32, []byte) error { return errBoom }
		if err := freeInode(newMemRW(0), 0, sb, ino); !errors.Is(err, errBoom) {
			t.Fatalf("expected freeInode writeAGI error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		agi = makeAGIBuffer(sb, 1, 4, 0, 2, 0)
		leaf = makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 64, freeCount: 0, irFree: 0})
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) { return agi, nil }
		allocInobtFindRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		allocWriteAGI = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, buf []byte) error {
			if got := be.Uint32(buf[agiOffFreeCount:]); got != 3 {
				t.Fatalf("unexpected AGI free count %d", got)
			}
			return nil
		}
		if err := freeInode(newMemRW(0), 0, sb, ino); err != nil {
			t.Fatalf("freeInode success: %v", err)
		}
		if got := be.Uint32(leaf[sb.agBTreeHdrSize()+4:]); got != 1 {
			t.Fatalf("unexpected leaf free count %d", got)
		}
		if got := be.Uint64(leaf[sb.agBTreeHdrSize()+8:]); got != 0x4 {
			t.Fatalf("unexpected leaf ir_free bitmap %x", got)
		}
	})
}

func TestAllocInternalBranchBreaks(t *testing.T) {
	sb := allocTestSB()

	t.Run("bno and inobt child selection break", func(t *testing.T) {
		restoreAllocHooks(t)
		bnoRoot := makeAllocInternal(sb, []allocRec{{start: 0, count: 1}, {start: 100, count: 1}}, []uint32{5, 6})
		bnoLeaf := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 50, count: 1})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 2 {
				return bnoRoot, nil
			}
			return bnoLeaf, nil
		}
		if _, _, _, err := bnoFindRecord(newMemRW(0), 0, sb, 0, 2, 1, 50); err != nil {
			t.Fatalf("bnoFindRecord break path: %v", err)
		}

		restoreAllocHooks(t)
		inobtRoot := makeInobtInternal(sb, []uint32{0, 128}, []uint32{5, 6})
		inobtLeaf := makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 64, freeCount: 1, irFree: 1})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 2 {
				return inobtRoot, nil
			}
			return inobtLeaf, nil
		}
		if _, _, _, err := inobtFindRecord(newMemRW(0), 0, sb, 0, 2, 1, 65); err != nil {
			t.Fatalf("inobtFindRecord break path: %v", err)
		}
	})

	t.Run("allocBTreeInsert cnt internal break", func(t *testing.T) {
		restoreAllocHooks(t)
		root := makeAllocInternal(sb, []allocRec{{start: 0, count: 1}, {start: 100, count: 10}}, []uint32{5, 6})
		leaf := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 30, count: 8})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 2 {
				return root, nil
			}
			return leaf, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		if err := allocBTreeInsert(newMemRW(0), 0, sb, 0, 2, 1, 40, 5, false); err != nil {
			t.Fatalf("allocBTreeInsert cnt break path: %v", err)
		}
	})
}
