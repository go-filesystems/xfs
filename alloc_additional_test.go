package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

type allocRec struct {
	start uint32
	count uint32
}

type inobtRec struct {
	start     uint32
	freeCount uint32
	irFree    uint64
}

func allocTestSB() *superblock {
	return &superblock{
		blockSize: 512,
		agBlocks:  32,
		agCount:   2,
		inodeSize: 64,
		inopBlock: 8,
		inopBLog:  3,
		agBlkLog:  5,
		dirBlkLog: 0,
		hasCRC:    true,
		hasFType:  true,
	}
}

func newAllocRW(sb *superblock) *memRW {
	return newMemRW(int(sb.agByteOffset(sb.agCount) + 4*int64(sb.blockSize)))
}

func putBlock(rw *memRW, offset int64, blk []byte) {
	copy(rw.data[offset:], blk)
}

func putAGF(rw *memRW, partOff int64, sb *superblock, ag uint32, buf []byte) {
	putBlock(rw, sb.agFByteOffset(partOff, ag), buf)
}

func putAGI(rw *memRW, partOff int64, sb *superblock, ag uint32, buf []byte) {
	putBlock(rw, sb.agIByteOffset(partOff, ag), buf)
}

func putFSBlock(rw *memRW, partOff int64, sb *superblock, ag, agRel uint32, blk []byte) {
	putBlock(rw, partOff+int64(sb.agAbsBlock(ag, agRel))*int64(sb.blockSize), blk)
}

func makeAGFBuffer(sb *superblock, ag, bnoRoot, cntRoot, bnoLevel, cntLevel, freeBlks, longest uint32) []byte {
	buf := make([]byte, sb.blockSize)
	be := binary.BigEndian
	be.PutUint32(buf[agfOffMagic:], magicAGF)
	be.PutUint32(buf[agfOffSeqNo:], ag)
	be.PutUint32(buf[agfOffLength:], sb.agBlocks)
	be.PutUint32(buf[agfOffBnoRoot:], bnoRoot)
	be.PutUint32(buf[agfOffCntRoot:], cntRoot)
	be.PutUint32(buf[agfOffBnoLevel:], bnoLevel)
	be.PutUint32(buf[agfOffCntLevel:], cntLevel)
	be.PutUint32(buf[agfOffFreeBlks:], freeBlks)
	be.PutUint32(buf[agfOffLongest:], longest)
	return buf
}

func makeAGIBuffer(sb *superblock, ag, root, level, freeCount, newIno uint32) []byte {
	buf := make([]byte, sb.blockSize)
	be := binary.BigEndian
	be.PutUint32(buf[agiOffMagic:], magicAGI)
	be.PutUint32(buf[agiOffSeqNo:], ag)
	be.PutUint32(buf[agiOffLength:], sb.agBlocks)
	be.PutUint32(buf[agiOffRoot:], root)
	be.PutUint32(buf[agiOffLevel:], level)
	be.PutUint32(buf[agiOffFreeCount:], freeCount)
	be.PutUint32(buf[agiOffNewIno:], newIno)
	return buf
}

func makeAllocLeaf(sb *superblock, rsib uint32, recs ...allocRec) []byte {
	blk := make([]byte, sb.blockSize)
	be := binary.BigEndian
	be.PutUint16(blk[4:], 0)
	be.PutUint16(blk[6:], uint16(len(recs)))
	be.PutUint32(blk[12:], rsib)
	hdr := sb.agBTreeHdrSize()
	for i, rec := range recs {
		off := hdr + i*allocRecSize
		be.PutUint32(blk[off:], rec.start)
		be.PutUint32(blk[off+4:], rec.count)
	}
	return blk
}

func makeAllocInternal(sb *superblock, keys []allocRec, ptrs []uint32) []byte {
	blk := make([]byte, sb.blockSize)
	be := binary.BigEndian
	be.PutUint16(blk[4:], 1)
	be.PutUint16(blk[6:], uint16(len(keys)))
	hdr := sb.agBTreeHdrSize()
	for i, key := range keys {
		off := hdr + i*allocKeySize
		be.PutUint32(blk[off:], key.start)
		be.PutUint32(blk[off+4:], key.count)
	}
	ptrOff := hdr + len(keys)*allocKeySize
	for i, ptr := range ptrs {
		be.PutUint32(blk[ptrOff+i*allocPtrSize:], ptr)
	}
	return blk
}

func makeInobtLeaf(sb *superblock, rsib uint32, recs ...inobtRec) []byte {
	blk := make([]byte, sb.blockSize)
	be := binary.BigEndian
	be.PutUint16(blk[4:], 0)
	be.PutUint16(blk[6:], uint16(len(recs)))
	be.PutUint32(blk[12:], rsib)
	hdr := sb.agBTreeHdrSize()
	for i, rec := range recs {
		off := hdr + i*inobtRecSize
		be.PutUint32(blk[off:], rec.start)
		be.PutUint32(blk[off+4:], rec.freeCount)
		be.PutUint64(blk[off+8:], rec.irFree)
	}
	return blk
}

func makeInobtInternal(sb *superblock, keys []uint32, ptrs []uint32) []byte {
	blk := make([]byte, sb.blockSize)
	be := binary.BigEndian
	be.PutUint16(blk[4:], 1)
	be.PutUint16(blk[6:], uint16(len(keys)))
	hdr := sb.agBTreeHdrSize()
	for i, key := range keys {
		be.PutUint32(blk[hdr+i*inobtKeySize:], key)
	}
	ptrOff := hdr + len(keys)*inobtKeySize
	for i, ptr := range ptrs {
		be.PutUint32(blk[ptrOff+i*inobtPtrSize:], ptr)
	}
	return blk
}

func restoreAllocHooks(t *testing.T) {
	oldAGFBlock := allocAGFBlock
	oldWriteAGF := allocWriteAGF
	oldAGIBlock := allocAGIBlock
	oldWriteAGI := allocWriteAGI
	oldReadAGBlock := allocReadAGBlock
	oldWriteAGBTree := allocWriteAGBTree
	oldCntFindBlock := allocCntFindBlock
	oldBtreeDeleteRecord := allocBtreeDeleteRecord
	oldBnoDeleteRecord := allocBnoDeleteRecord
	oldBnoUpdateRecord := allocBnoUpdateRecord
	oldBnoFindRecord := allocBnoFindRecord
	oldBnoInsertRecord := allocBnoInsertRecord
	oldCntInsertRecord := allocCntInsertRecord
	oldInobtFindFree := allocInobtFindFree
	oldInobtFindRecord := allocInobtFindRecord
	t.Cleanup(func() {
		allocAGFBlock = oldAGFBlock
		allocWriteAGF = oldWriteAGF
		allocAGIBlock = oldAGIBlock
		allocWriteAGI = oldWriteAGI
		allocReadAGBlock = oldReadAGBlock
		allocWriteAGBTree = oldWriteAGBTree
		allocCntFindBlock = oldCntFindBlock
		allocBtreeDeleteRecord = oldBtreeDeleteRecord
		allocBnoDeleteRecord = oldBnoDeleteRecord
		allocBnoUpdateRecord = oldBnoUpdateRecord
		allocBnoFindRecord = oldBnoFindRecord
		allocBnoInsertRecord = oldBnoInsertRecord
		allocCntInsertRecord = oldCntInsertRecord
		allocInobtFindFree = oldInobtFindFree
		allocInobtFindRecord = oldInobtFindRecord
	})
}

func TestAGHelpersAdditional(t *testing.T) {
	sb := allocTestSB()
	partOff := int64(0)

	t.Run("agf and agi reads", func(t *testing.T) {
		rw := newAllocRW(sb)
		putAGF(rw, partOff, sb, 0, makeAGFBuffer(sb, 0, 2, 1, 0, 0, 100, 50))
		if _, err := agfBlock(rw, partOff, sb, 0); err != nil {
			t.Fatalf("agfBlock success: %v", err)
		}
		bad := makeAGFBuffer(sb, 1, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint32(bad[agfOffMagic:], 0xDEADBEEF)
		putAGF(rw, partOff, sb, 1, bad)
		if _, err := agfBlock(rw, partOff, sb, 1); err == nil {
			t.Fatal("expected agfBlock to reject bad magic")
		}
		badRW := newAllocRW(sb)
		badRW.readHook = func(off int64, _ []byte) error {
			if off == sb.agFByteOffset(partOff, 0) {
				return errBoom
			}
			return nil
		}
		if _, err := agfBlock(badRW, partOff, sb, 0); !errors.Is(err, errBoom) {
			t.Fatalf("expected agfBlock read error %v, got %v", errBoom, err)
		}

		putAGI(rw, partOff, sb, 0, makeAGIBuffer(sb, 0, 3, 0, 10, 0))
		if _, err := agiBlock(rw, partOff, sb, 0); err != nil {
			t.Fatalf("agiBlock success: %v", err)
		}
		badAGI := makeAGIBuffer(sb, 1, 0, 0, 0, 0)
		binary.BigEndian.PutUint32(badAGI[agiOffMagic:], 0xDEADBEEF)
		putAGI(rw, partOff, sb, 1, badAGI)
		if _, err := agiBlock(rw, partOff, sb, 1); err == nil {
			t.Fatal("expected agiBlock to reject bad magic")
		}
		badRW.readHook = func(off int64, _ []byte) error {
			if off == sb.agIByteOffset(partOff, 0) {
				return errBoom
			}
			return nil
		}
		if _, err := agiBlock(badRW, partOff, sb, 0); !errors.Is(err, errBoom) {
			t.Fatalf("expected agiBlock read error %v, got %v", errBoom, err)
		}
	})

	t.Run("agf agi and btree writes", func(t *testing.T) {
		rw := newAllocRW(sb)
		agf := makeAGFBuffer(sb, 0, 0, 0, 0, 0, 7, 3)
		if err := writeAGF(rw, partOff, sb, 0, agf); err != nil {
			t.Fatalf("writeAGF: %v", err)
		}
		if got := binary.LittleEndian.Uint32(rw.data[sb.agFByteOffset(partOff, 0)+agfOffCRC:]); got == 0 {
			t.Fatal("expected writeAGF to populate the v5 CRC field")
		}

		sbNoCRC := *sb
		sbNoCRC.hasCRC = false
		agi := makeAGIBuffer(&sbNoCRC, 0, 0, 0, 7, 0)
		binary.LittleEndian.PutUint32(agi[agiOffCRC:], 0x11223344)
		if err := writeAGI(rw, partOff, &sbNoCRC, 0, agi); err != nil {
			t.Fatalf("writeAGI v4: %v", err)
		}
		if got := binary.LittleEndian.Uint32(rw.data[sbNoCRC.agIByteOffset(partOff, 0)+agiOffCRC:]); got != 0x11223344 {
			t.Fatalf("writeAGI v4 changed the CRC field unexpectedly: 0x%x", got)
		}

		btree := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 1, count: 1})
		if err := writeAGBTree(rw, partOff, sb, 0, 4, btree); err != nil {
			t.Fatalf("writeAGBTree: %v", err)
		}
		if got := binary.LittleEndian.Uint32(rw.data[int64(sb.blockSize*4)+btreeCRCOff:]); got == 0 {
			t.Fatal("expected writeAGBTree to populate the v5 CRC field")
		}

		badRW := newAllocRW(sb)
		badRW.writeHook = func(int64, []byte) error { return errBoom }
		if err := writeAGF(badRW, partOff, sb, 0, agf); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeAGF error %v, got %v", errBoom, err)
		}
		if err := writeAGI(badRW, partOff, sb, 0, makeAGIBuffer(sb, 0, 0, 0, 0, 0)); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeAGI error %v, got %v", errBoom, err)
		}
		if err := writeAGBTree(badRW, partOff, sb, 0, 1, makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 1, count: 1})); !errors.Is(err, errBoom) {
			t.Fatalf("expected writeAGBTree error %v, got %v", errBoom, err)
		}
	})

	t.Run("ag helpers and bits", func(t *testing.T) {
		rw := newAllocRW(sb)
		blk := make([]byte, sb.blockSize)
		copy(blk, []byte("data"))
		putFSBlock(rw, partOff, sb, 1, 3, blk)
		got, err := readAGBlock(rw, partOff, sb, 1, 3)
		if err != nil || string(got[:4]) != "data" {
			t.Fatalf("readAGBlock = %q, %v; want data, nil", got[:4], err)
		}
		rw.readHook = func(int64, []byte) error { return errBoom }
		if _, err := readAGBlock(rw, partOff, sb, 0, 0); !errors.Is(err, errBoom) {
			t.Fatalf("expected readAGBlock error %v, got %v", errBoom, err)
		}
		if got := sb.agAbsBlock(1, 10); got != 42 {
			t.Fatalf("agAbsBlock = %d, want 42", got)
		}
		if got := sb.agBTreeHdrSize(); got != btreeHdrSizeV5 {
			t.Fatalf("agBTreeHdrSize = %d, want %d", got, btreeHdrSizeV5)
		}
		sbNoCRC := *sb
		sbNoCRC.hasCRC = false
		if got := sbNoCRC.agBTreeHdrSize(); got != btreeHdrSizeV4 {
			t.Fatalf("agBTreeHdrSize v4 = %d, want %d", got, btreeHdrSizeV4)
		}
		if bits64(0) != 0 || bits64(0xFFFFFFFFFFFFFFFF) != 64 || bits64(0xAAAAAAAAAAAAAAAA) != 32 {
			t.Fatal("bits64 returned unexpected results")
		}
	})
}

func TestCntFindBlockAdditional(t *testing.T) {
	sb := allocTestSB()
	rw := newMemRW(0)

	t.Run("leaf match and sibling traversal", func(t *testing.T) {
		restoreAllocHooks(t)
		leaf1 := makeAllocLeaf(sb, 3, allocRec{start: 10, count: 2})
		leaf2 := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 20, count: 6})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 2 {
				return leaf1, nil
			}
			return leaf2, nil
		}
		leafRel, _, idx, err := cntFindBlock(rw, 0, sb, 0, 2, 0, 5)
		if err != nil || leafRel != 3 || idx != 0 {
			t.Fatalf("cntFindBlock sibling = (%d, %d, %v), want (3, 0, nil)", leafRel, idx, err)
		}

		restoreAllocHooks(t)
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, _ uint32) ([]byte, error) {
			return makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 2}), nil
		}
		if _, _, _, err := cntFindBlock(rw, 0, sb, 0, 2, 0, 5); err == nil {
			t.Fatal("expected cntFindBlock to fail when no sibling can satisfy the request")
		}
	})

	t.Run("internal traversal and errors", func(t *testing.T) {
		restoreAllocHooks(t)
		root := makeAllocInternal(sb, []allocRec{{start: 0, count: 1}}, []uint32{7})
		leaf := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 30, count: 8})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 2 {
				return root, nil
			}
			return leaf, nil
		}
		leafRel, _, idx, err := cntFindBlock(rw, 0, sb, 0, 2, 1, 5)
		if err != nil || leafRel != 7 || idx != 0 {
			t.Fatalf("cntFindBlock internal = (%d, %d, %v), want (7, 0, nil)", leafRel, idx, err)
		}

		restoreAllocHooks(t)
		tooSmall := make([]byte, sb.blockSize)
		binary.BigEndian.PutUint16(tooSmall[4:], 1)
		binary.BigEndian.PutUint16(tooSmall[6:], 100)
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, _ uint32) ([]byte, error) { return tooSmall, nil }
		if _, _, _, err := cntFindBlock(rw, 0, sb, 0, 2, 1, 5); err == nil {
			t.Fatal("expected cntFindBlock to reject undersized internal blocks")
		}

		restoreAllocHooks(t)
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, _ uint32) ([]byte, error) { return nil, errBoom }
		if _, _, _, err := cntFindBlock(rw, 0, sb, 0, 2, 0, 5); !errors.Is(err, errBoom) {
			t.Fatalf("expected cntFindBlock read error %v, got %v", errBoom, err)
		}
	})
}

func TestBtreeAndBnoHelpersAdditional(t *testing.T) {
	sb := allocTestSB()

	t.Run("btreeDeleteRecord", func(t *testing.T) {
		restoreAllocHooks(t)
		leaf := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 5}, allocRec{start: 20, count: 8})
		if err := btreeDeleteRecord(newMemRW(0), 0, sb, 0, 0, 0, -1, 5, leaf, allocRecSize, false); err == nil {
			t.Fatal("expected btreeDeleteRecord to reject invalid indexes")
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return errBoom }
		if err := btreeDeleteRecord(newMemRW(0), 0, sb, 0, 0, 0, 0, 5, leaf, allocRecSize, false); !errors.Is(err, errBoom) {
			t.Fatalf("expected btreeDeleteRecord write error %v, got %v", errBoom, err)
		}
		leaf = makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 5}, allocRec{start: 20, count: 8})
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		if err := btreeDeleteRecord(newMemRW(0), 0, sb, 0, 0, 0, 0, 5, leaf, allocRecSize, false); err != nil {
			t.Fatalf("btreeDeleteRecord: %v", err)
		}
		hdr := sb.agBTreeHdrSize()
		if got := binary.BigEndian.Uint16(leaf[6:]); got != 1 {
			t.Fatalf("expected numrecs 1, got %d", got)
		}
		if got := binary.BigEndian.Uint32(leaf[hdr:]); got != 20 {
			t.Fatalf("expected shifted record start 20, got %d", got)
		}
	})

	t.Run("bnoFindRecord and wrappers", func(t *testing.T) {
		restoreAllocHooks(t)
		leaf := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 1}, allocRec{start: 20, count: 2})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, _ uint32) ([]byte, error) { return leaf, nil }
		leafRel, _, idx, err := bnoFindRecord(newMemRW(0), 0, sb, 0, 2, 0, 20)
		if err != nil || leafRel != 2 || idx != 1 {
			t.Fatalf("bnoFindRecord leaf = (%d, %d, %v), want (2, 1, nil)", leafRel, idx, err)
		}
		if _, _, _, err := bnoFindRecord(newMemRW(0), 0, sb, 0, 2, 0, 30); err == nil {
			t.Fatal("expected bnoFindRecord to fail when the startblock is absent")
		}

		restoreAllocHooks(t)
		root := makeAllocInternal(sb, []allocRec{{start: 0, count: 1}, {start: 100, count: 1}}, []uint32{5, 6})
		leaf = makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 120, count: 4})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 2 {
				return root, nil
			}
			return leaf, nil
		}
		if _, _, idx, err := bnoFindRecord(newMemRW(0), 0, sb, 0, 2, 1, 120); err != nil || idx != 0 {
			t.Fatalf("bnoFindRecord internal: idx=%d err=%v", idx, err)
		}

		restoreAllocHooks(t)
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, _ uint32) ([]byte, error) { return nil, errBoom }
		if _, _, _, err := bnoFindRecord(newMemRW(0), 0, sb, 0, 2, 0, 10); !errors.Is(err, errBoom) {
			t.Fatalf("expected bnoFindRecord read error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		allocBnoFindRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 0, nil, 0, errBoom
		}
		if err := bnoDeleteRecord(newMemRW(0), 0, sb, 0, 2, 0, 10); !errors.Is(err, errBoom) {
			t.Fatalf("expected bnoDeleteRecord error %v, got %v", errBoom, err)
		}
		calledDelete := false
		allocBnoFindRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 5, makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 1}), 0, nil
		}
		allocBtreeDeleteRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, int, uint32, []byte, int, bool) error {
			calledDelete = true
			return nil
		}
		if err := bnoDeleteRecord(newMemRW(0), 0, sb, 0, 2, 0, 10); err != nil {
			t.Fatalf("bnoDeleteRecord: %v", err)
		}
		if !calledDelete {
			t.Fatal("expected bnoDeleteRecord to delegate to btreeDeleteRecord")
		}
	})

	t.Run("bnoUpdateRecord", func(t *testing.T) {
		restoreAllocHooks(t)
		allocBnoFindRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 0, nil, 0, errBoom
		}
		if err := bnoUpdateRecord(newMemRW(0), 0, sb, 0, 2, 0, 10, 11, 1); !errors.Is(err, errBoom) {
			t.Fatalf("expected bnoUpdateRecord lookup error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		leaf := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 1}, allocRec{start: 20, count: 1}, allocRec{start: 30, count: 1})
		allocBnoFindRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 5, leaf, 1, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return errBoom }
		if err := bnoUpdateRecord(newMemRW(0), 0, sb, 0, 2, 0, 20, 21, 2); !errors.Is(err, errBoom) {
			t.Fatalf("expected bnoUpdateRecord write error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		leaf = makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 10, count: 1}, allocRec{start: 20, count: 1}, allocRec{start: 30, count: 1})
		allocBnoFindRecord = func(readerWriterAt, int64, *superblock, uint32, uint32, int, uint32) (uint32, []byte, int, error) {
			return 5, leaf, 0, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		if err := bnoUpdateRecord(newMemRW(0), 0, sb, 0, 2, 0, 10, 25, 2); err != nil {
			t.Fatalf("bnoUpdateRecord swap: %v", err)
		}
		hdr := sb.agBTreeHdrSize()
		if got0, got1 := binary.BigEndian.Uint32(leaf[hdr:]), binary.BigEndian.Uint32(leaf[hdr+8:]); got0 != 20 || got1 != 25 {
			t.Fatalf("unexpected record order after swap: %d, %d", got0, got1)
		}
	})
}

func TestAllocBTreeInsertAdditional(t *testing.T) {
	sb := allocTestSB()

	t.Run("direct helper errors and internal descent", func(t *testing.T) {
		restoreAllocHooks(t)
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) { return nil, errBoom }
		if err := allocBTreeInsert(newMemRW(0), 0, sb, 0, 2, 0, 10, 1, true); !errors.Is(err, errBoom) {
			t.Fatalf("expected allocBTreeInsert read error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		full := make([]byte, sb.blockSize)
		binary.BigEndian.PutUint16(full[4:], 0)
		binary.BigEndian.PutUint16(full[6:], uint16((len(full)-sb.agBTreeHdrSize())/allocRecSize))
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) { return full, nil }
		if err := allocBTreeInsert(newMemRW(0), 0, sb, 0, 2, 0, 10, 1, true); err == nil {
			t.Fatal("expected allocBTreeInsert to reject full leaves")
		}

		restoreAllocHooks(t)
		root := makeAllocInternal(sb, []allocRec{{start: 0, count: 1}, {start: 100, count: 2}}, []uint32{5, 6})
		leaf := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 30, count: 1})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 2 {
				return root, nil
			}
			return leaf, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		if err := allocBTreeInsert(newMemRW(0), 0, sb, 0, 2, 1, 120, 3, true); err != nil {
			t.Fatalf("allocBTreeInsert internal: %v", err)
		}
	})

	t.Run("bno and cnt wrapper inserts", func(t *testing.T) {
		restoreAllocHooks(t)
		leaf := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 30, count: 1})
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) { return leaf, nil }
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		if err := bnoInsertRecord(newMemRW(0), 0, sb, 0, 2, 0, 20, 5); err != nil {
			t.Fatalf("bnoInsertRecord: %v", err)
		}
		hdr := sb.agBTreeHdrSize()
		if got0, got1 := binary.BigEndian.Uint32(leaf[hdr:]), binary.BigEndian.Uint32(leaf[hdr+8:]); got0 != 20 || got1 != 30 {
			t.Fatalf("unexpected bno insert order: %d, %d", got0, got1)
		}

		restoreAllocHooks(t)
		leaf = makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 30, count: 10})
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) { return leaf, nil }
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		if err := cntInsertRecord(newMemRW(0), 0, sb, 0, 2, 0, 20, 5); err != nil {
			t.Fatalf("cntInsertRecord: %v", err)
		}
		if got0, got1 := binary.BigEndian.Uint32(leaf[hdr+4:]), binary.BigEndian.Uint32(leaf[hdr+12:]); got0 != 5 || got1 != 10 {
			t.Fatalf("unexpected cnt insert order: %d, %d", got0, got1)
		}
	})
}

func TestInobtHelpersAdditional(t *testing.T) {
	sb := allocTestSB()

	t.Run("inobtFindFree", func(t *testing.T) {
		restoreAllocHooks(t)
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) { return nil, errBoom }
		if _, _, _, err := inobtFindFree(newMemRW(0), 0, sb, 0, 2, 0); !errors.Is(err, errBoom) {
			t.Fatalf("expected inobtFindFree read error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		leaf1 := makeInobtLeaf(sb, 3, inobtRec{start: 0, freeCount: 0, irFree: 0})
		leaf2 := makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 64, freeCount: 2, irFree: 0x3})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 2 {
				return leaf1, nil
			}
			return leaf2, nil
		}
		leafRel, _, idx, err := inobtFindFree(newMemRW(0), 0, sb, 0, 2, 0)
		if err != nil || leafRel != 3 || idx != 0 {
			t.Fatalf("inobtFindFree sibling = (%d, %d, %v), want (3, 0, nil)", leafRel, idx, err)
		}

		restoreAllocHooks(t)
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 0, freeCount: 0, irFree: 0}), nil
		}
		if _, _, _, err := inobtFindFree(newMemRW(0), 0, sb, 0, 2, 0); err == nil {
			t.Fatal("expected inobtFindFree to fail when no leaf has free inodes")
		}

		restoreAllocHooks(t)
		root := makeInobtInternal(sb, []uint32{0}, []uint32{5})
		leaf := makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 0, freeCount: 1, irFree: 1})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 2 {
				return root, nil
			}
			return leaf, nil
		}
		if _, _, _, err := inobtFindFree(newMemRW(0), 0, sb, 0, 2, 1); err != nil {
			t.Fatalf("inobtFindFree internal: %v", err)
		}
	})

	t.Run("inobtFindRecord", func(t *testing.T) {
		restoreAllocHooks(t)
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) { return nil, errBoom }
		if _, _, _, err := inobtFindRecord(newMemRW(0), 0, sb, 0, 2, 0, 0); !errors.Is(err, errBoom) {
			t.Fatalf("expected inobtFindRecord read error %v, got %v", errBoom, err)
		}

		restoreAllocHooks(t)
		leaf := makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 64, freeCount: 1, irFree: 1})
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) { return leaf, nil }
		leafRel, _, idx, err := inobtFindRecord(newMemRW(0), 0, sb, 0, 2, 0, 65)
		if err != nil || leafRel != 2 || idx != 0 {
			t.Fatalf("inobtFindRecord leaf = (%d, %d, %v), want (2, 0, nil)", leafRel, idx, err)
		}
		if _, _, _, err := inobtFindRecord(newMemRW(0), 0, sb, 0, 2, 0, 200); err == nil {
			t.Fatal("expected inobtFindRecord to fail when the inode chunk is absent")
		}

		restoreAllocHooks(t)
		root := makeInobtInternal(sb, []uint32{0, 128}, []uint32{5, 6})
		leaf = makeInobtLeaf(sb, 0xFFFFFFFF, inobtRec{start: 128, freeCount: 1, irFree: 1})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 2 {
				return root, nil
			}
			return leaf, nil
		}
		if _, _, _, err := inobtFindRecord(newMemRW(0), 0, sb, 0, 2, 1, 130); err != nil {
			t.Fatalf("inobtFindRecord internal: %v", err)
		}
	})
}
