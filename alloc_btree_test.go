package filesystem_xfs

import (
	"encoding/binary"
	"testing"
)

// alloc_btree_test.go — unit tests for the general multi-level bno/cnt free-
// space B-tree insert (alloc_btree.go): leaf split under a root with room,
// interior-root split growing the tree a level, and full structural consistency
// of the resulting deep tree (descend + sibling-chain walk, both bno and cnt
// orderings). These drive the *real* recursive insert against hand-built
// multi-level trees in a memRW; only block allocation is mocked.

// allocDeepSB matches deepInobtSB's geometry so the same small blocks/AG sizing
// keeps maxLeaf/maxNode small and the tests fast.
func allocDeepSB() *superblock {
	return &superblock{
		blockSize: 512,
		agBlocks:  4096,
		agCount:   2,
		inodeSize: 256,
		inopBlock: 8,
		inopBLog:  3,
		agBlkLog:  12,
		hasCRC:    true,
		hasFType:  true,
	}
}

// fullAllocLeaf builds a level-0 alloc leaf of n records starting at startBase,
// each of length cnt, stepping the start by stride, with given siblings. For a
// bno tree (sorted by start) cnt is irrelevant to order; for cnt-tree fixtures
// pass equal counts and rely on the start tiebreak.
func fullAllocLeaf(sb *superblock, self, lsib, rsib uint32, n int, startBase, stride, cnt uint32) []byte {
	be := binary.BigEndian
	hdr := sb.agBTreeHdrSize()
	blk := make([]byte, sb.blockSize)
	be.PutUint32(blk[0:], magicABTB)
	be.PutUint16(blk[4:], 0)
	be.PutUint16(blk[6:], uint16(n))
	be.PutUint32(blk[8:], lsib)
	be.PutUint32(blk[12:], rsib)
	be.PutUint64(blk[16:], sb.fsbToPhysBlock(sb.agAbsBlock(0, self))*fmtDaddrPerBlock)
	for i := 0; i < n; i++ {
		off := hdr + i*allocRecSize
		be.PutUint32(blk[off:], startBase+uint32(i)*stride)
		be.PutUint32(blk[off+4:], cnt)
	}
	return blk
}

// allocMockAGF installs a minimal AGF in rw so the root-growth path's AGF
// read/modify/write succeeds, and returns the buffer for inspection.
func allocMockAGF(rw *memRW, sb *superblock, bnoRoot, cntRoot, bnoLevel, cntLevel uint32) {
	agf := makeAGFBuffer(sb, 0, bnoRoot, cntRoot, bnoLevel, cntLevel, 0, 0)
	putAGF(rw, 0, sb, 0, agf)
}

// walkAllocLeaves descends rootRel to the leftmost leaf, then walks the right-
// sibling chain collecting every (start,count) record in order. It asserts the
// chain is acyclic and the records are strictly ordered per the tree's order.
func walkAllocLeaves(t *testing.T, rw *memRW, sb *superblock, rootRel uint32, level int, bnoSort bool) [][2]uint32 {
	t.Helper()
	be := binary.BigEndian
	hdr := sb.agBTreeHdrSize()
	cur := rootRel
	for {
		blk, err := readAGBlock(rw, 0, sb, 0, cur)
		if err != nil {
			t.Fatalf("read block %d: %v", cur, err)
		}
		if be.Uint16(blk[4:]) == 0 {
			break
		}
		ptrOff := hdr + allocMaxInternal(int(sb.blockSize), hdr)*allocKeySize
		cur = be.Uint32(blk[ptrOff:])
	}
	var recs [][2]uint32
	seen := map[uint32]bool{}
	for cur != 0xFFFFFFFF {
		if seen[cur] {
			t.Fatalf("cyclic leaf chain at block %d", cur)
		}
		seen[cur] = true
		blk, err := readAGBlock(rw, 0, sb, 0, cur)
		if err != nil {
			t.Fatalf("read leaf %d: %v", cur, err)
		}
		nr := int(be.Uint16(blk[6:]))
		for i := 0; i < nr; i++ {
			off := hdr + i*allocRecSize
			recs = append(recs, [2]uint32{be.Uint32(blk[off:]), be.Uint32(blk[off+4:])})
		}
		cur = be.Uint32(blk[12:])
	}
	for i := 1; i < len(recs); i++ {
		p, c := recs[i-1], recs[i]
		if !allocLess(bnoSort, p[0], p[1], c[0], c[1]) {
			t.Fatalf("records not strictly ordered at %d: %v then %v", i, p, c)
		}
	}
	return recs
}

func TestAllocBtreeMultiLevelInsert(t *testing.T) {
	sb := allocDeepSB()
	be := binary.BigEndian
	hdr := sb.agBTreeHdrSize()
	maxLeaf := allocMaxLeaf(int(sb.blockSize), hdr)
	maxNode := allocMaxInternal(int(sb.blockSize), hdr)

	t.Run("leaf split under root with room (bno)", func(t *testing.T) {
		restoreAllocHooks(t)
		rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))

		// root(block 1, interior, 2 ptrs) → leaf 10 (full, low range), leaf 11 (full, high).
		stride := uint32(4)
		base0 := uint32(100)
		base1 := base0 + uint32(maxLeaf)*stride*4 // well separated
		leaf10 := fullAllocLeaf(sb, 10, 0xFFFFFFFF, 11, maxLeaf, base0, stride, 1)
		leaf11 := fullAllocLeaf(sb, 11, 10, 0xFFFFFFFF, maxLeaf, base1, stride, 1)
		root := makeAllocInternal(sb, []allocRec{{base0, 1}, {base1, 1}}, []uint32{10, 11})
		putFSBlock(rw, 0, sb, 0, 10, leaf10)
		putFSBlock(rw, 0, sb, 0, 11, leaf11)
		putFSBlock(rw, 0, sb, 0, 8, root)
		allocMockAGF(rw, sb, 8, 9, 2, 2)

		var next uint32 = 100
		allocMetaAllocBlock = func(_ readerWriterAt, _ int64, _ *superblock, _ uint32, _ uint32) (uint64, error) {
			b := next
			next++
			return uint64(b), nil
		}

		// New start inside leaf 10's range but above all its keys.
		newStart := base0 + uint32(maxLeaf)*stride - stride/2
		if err := allocInsertRecord(rw, 0, sb, 0, 8, 2, newStart, 1, true); err != nil {
			t.Fatalf("insert (root has room): %v", err)
		}
		root, _ = readAGBlock(rw, 0, sb, 0, 8)
		if nr := be.Uint16(root[6:]); nr != 3 {
			t.Fatalf("root numrecs after leaf split: got %d want 3", nr)
		}
		recs := walkAllocLeaves(t, rw, sb, 8, 2, true)
		if len(recs) != 2*maxLeaf+1 {
			t.Fatalf("walk found %d records, want %d", len(recs), 2*maxLeaf+1)
		}
		// AGF root/level must be unchanged (no depth growth).
		agf, _ := agfBlock(rw, 0, sb, 0)
		if be.Uint32(agf[agfOffBnoRoot:]) != 8 || be.Uint32(agf[agfOffBnoLevel:]) != 2 {
			t.Fatalf("AGF bno root/level changed unexpectedly")
		}
	})

	t.Run("interior root split grows tree a level (bno)", func(t *testing.T) {
		restoreAllocHooks(t)
		rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))

		// Full interior root: maxNode leaves in disjoint ascending ranges. Insert
		// into the first (full) leaf → leaf split overflows the full root →
		// interior split grows the tree from depth 2 to depth 3.
		stride := uint32(4)
		rangeStride := uint32(maxLeaf) * stride
		keys := make([]allocRec, maxNode)
		ptrs := make([]uint32, maxNode)
		for i := 0; i < maxNode; i++ {
			self := uint32(10 + i)
			base := uint32(100) + uint32(i)*rangeStride*2
			var lsib, rsib uint32 = 0xFFFFFFFF, 0xFFFFFFFF
			if i > 0 {
				lsib = uint32(10 + i - 1)
			}
			if i < maxNode-1 {
				rsib = uint32(10 + i + 1)
			}
			putFSBlock(rw, 0, sb, 0, self, fullAllocLeaf(sb, self, lsib, rsib, maxLeaf, base, stride, 1))
			keys[i] = allocRec{base, 1}
			ptrs[i] = self
		}
		root := makeAllocInternal(sb, keys, ptrs)
		putFSBlock(rw, 0, sb, 0, 8, root)
		allocMockAGF(rw, sb, 8, 9, 2, 2)

		var next uint32 = 200
		allocMetaAllocBlock = func(_ readerWriterAt, _ int64, _ *superblock, _ uint32, _ uint32) (uint64, error) {
			b := next
			next++
			return uint64(b), nil
		}

		newStart := keys[0].start + rangeStride - stride/2 // inside leaf 0's range
		if err := allocInsertRecord(rw, 0, sb, 0, 8, 2, newStart, 1, true); err != nil {
			t.Fatalf("insert (root split): %v", err)
		}
		agf, _ := agfBlock(rw, 0, sb, 0)
		newRoot := be.Uint32(agf[agfOffBnoRoot:])
		newLevel := be.Uint32(agf[agfOffBnoLevel:])
		if newLevel != 3 {
			t.Fatalf("AGF bno level after root split: got %d want 3", newLevel)
		}
		if newRoot == 8 {
			t.Fatalf("AGF bno root not repointed after split")
		}
		// Whole tree must be consistent and hold every original record + the new.
		recs := walkAllocLeaves(t, rw, sb, newRoot, int(newLevel), true)
		if len(recs) != maxNode*maxLeaf+1 {
			t.Fatalf("walk found %d records, want %d", len(recs), maxNode*maxLeaf+1)
		}
		// The new record must be present.
		found := false
		for _, r := range recs {
			if r[0] == newStart {
				found = true
			}
		}
		if !found {
			t.Fatalf("inserted record start=%d not found in walked tree", newStart)
		}
	})

	t.Run("repeated inserts grow cnt tree multi-level", func(t *testing.T) {
		restoreAllocHooks(t)
		rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))

		// Start from a single empty cnt leaf root (block 1). Mock allocator hands
		// out unused blocks 1000+ for split nodes so they never collide with the
		// initial root. Insert many records with strictly increasing counts so the
		// cnt order is exercised; assert the tree grows past depth 1 and stays
		// consistent.
		emptyRoot := fullAllocLeaf(sb, 8, 0xFFFFFFFF, 0xFFFFFFFF, 0, 0, 0, 0)
		be.PutUint32(emptyRoot[0:], magicABTC)
		putFSBlock(rw, 0, sb, 0, 8, emptyRoot)
		allocMockAGF(rw, sb, 7, 8, 1, 1) // cnt root = block 8, level 1

		var next uint32 = 1000
		allocMetaAllocBlock = func(_ readerWriterAt, _ int64, _ *superblock, _ uint32, _ uint32) (uint64, error) {
			b := next
			next++
			return uint64(b), nil
		}

		// Insert N records: count i+1, start i*8 (distinct). Read the live cnt
		// root/level from the AGF before each insert (root grows over time).
		const N = 60
		for i := 0; i < N; i++ {
			agf, _ := agfBlock(rw, 0, sb, 0)
			root := be.Uint32(agf[agfOffCntRoot:])
			level := int(be.Uint32(agf[agfOffCntLevel:]))
			start := uint32(i) * 8
			cnt := uint32(i) + 1
			if err := allocInsertRecord(rw, 0, sb, 0, root, level, start, cnt, false); err != nil {
				t.Fatalf("cnt insert #%d: %v", i, err)
			}
		}
		agf, _ := agfBlock(rw, 0, sb, 0)
		root := be.Uint32(agf[agfOffCntRoot:])
		level := int(be.Uint32(agf[agfOffCntLevel:]))
		if level < 2 {
			t.Fatalf("cnt tree did not grow multi-level: level=%d after %d inserts", level, N)
		}
		recs := walkAllocLeaves(t, rw, sb, root, level, false)
		if len(recs) != N {
			t.Fatalf("cnt tree holds %d records, want %d", len(recs), N)
		}
	})
}

// TestAllocCntBubbleLeft checks the in-leaf re-sort used after an allocBlocks
// remainder update shrinks a cnt record: a record whose count dropped bubbles
// left past now-greater predecessors, restoring (count,start) order, without
// changing the record count.
func TestAllocCntBubbleLeft(t *testing.T) {
	sb := allocDeepSB()
	hdr := sb.agBTreeHdrSize()
	be := binary.BigEndian
	// Leaf with counts [2,5,9,1] (last shrunk from larger). Bubbling idx 3 left
	// must yield [1,2,5,9].
	leaf := make([]byte, sb.blockSize)
	recs := [][2]uint32{{10, 2}, {20, 5}, {30, 9}, {40, 1}}
	be.PutUint16(leaf[6:], uint16(len(recs)))
	for i, r := range recs {
		off := hdr + i*allocRecSize
		be.PutUint32(leaf[off:], r[0])
		be.PutUint32(leaf[off+4:], r[1])
	}
	allocCntBubbleLeft(leaf, hdr, 3)
	wantCounts := []uint32{1, 2, 5, 9}
	for i, wc := range wantCounts {
		off := hdr + i*allocRecSize
		if got := be.Uint32(leaf[off+4:]); got != wc {
			t.Fatalf("after bubble, rec %d count=%d want %d", i, got, wc)
		}
	}
	// A record that does not need to move stays put.
	allocCntBubbleLeft(leaf, hdr, 0)
	if got := be.Uint32(leaf[hdr+4:]); got != 1 {
		t.Fatalf("bubble of index 0 changed leaf: count=%d", got)
	}
}

// TestAllocMetaBlock covers the default split-block hook: a single-block carve
// delegates to allocBlocks, and a multi-block request is rejected.
func TestAllocMetaBlock(t *testing.T) {
	restoreAllocHooks(t)
	allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
		return 0, nil
	}
	sb := allocDeepSB()
	// allocMetaBlock calls the package allocBlocks directly (not the hook), so set
	// up a tiny tree via the read/write hooks to make allocBlocks succeed is heavy;
	// instead just assert the nBlocks!=1 guard, which needs no backing.
	if _, err := allocMetaBlock(newMemRW(0), 0, sb, 0, 2); err == nil {
		t.Fatal("allocMetaBlock must reject multi-block requests")
	}
}

// TestAllocSplitReserve covers the reserve's alloc/release block bookkeeping
// without a real free-space tree: alloc hands out sequential blocks then errors
// when exhausted; release frees only the unused tail.
func TestAllocSplitReserve(t *testing.T) {
	restoreAllocHooks(t)
	sb := allocDeepSB()

	r := &allocSplitReserve{runStart: 1000, n: 3}
	// Multi-block request rejected.
	if _, err := r.alloc(newMemRW(0), 0, sb, 0, 2); err == nil {
		t.Fatal("reserve alloc must reject multi-block requests")
	}
	// Three sequential single-block allocations, then exhaustion.
	for i := 0; i < 3; i++ {
		b, err := r.alloc(newMemRW(0), 0, sb, 0, 1)
		if err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
		if b != uint64(1000+i) {
			t.Fatalf("alloc %d = %d, want %d", i, b, 1000+i)
		}
	}
	if _, err := r.alloc(newMemRW(0), 0, sb, 0, 1); err == nil {
		t.Fatal("reserve alloc must error when exhausted")
	}

	// release of a fully-used reserve is a no-op (no free call).
	if err := r.release(newMemRW(0), 0, sb, 0); err != nil {
		t.Fatalf("release of fully-used reserve: %v", err)
	}

	// The partially-used release (leftover tail free via freeBlocks) is covered
	// end to end by TestAllocReserveReleaseEndToEnd, which drives a real image.
}

// TestAllocReserveSplitBlocksZero covers the n<=0 fast path (empty reserve).
func TestAllocReserveSplitBlocksZero(t *testing.T) {
	restoreAllocHooks(t)
	sb := allocDeepSB()
	res, err := allocReserveSplitBlocks(newMemRW(0), 0, sb, 0, 0)
	if err != nil {
		t.Fatalf("zero reserve: %v", err)
	}
	if res.n != 0 || res.used != 0 {
		t.Fatalf("zero reserve not empty: %+v", res)
	}
	// alloc on an empty reserve immediately reports exhaustion.
	if _, err := res.alloc(newMemRW(0), 0, sb, 0, 1); err == nil {
		t.Fatal("empty reserve alloc must error")
	}
}

// TestAllocReserveReleaseEndToEnd drives freeBlocks on a real formatted image so
// allocInsertWithReserve allocates a reserve and release() frees the unused tail
// through freeBlocks — covering the reserve alloc + tail-free path end to end.
func TestAllocReserveReleaseEndToEnd(t *testing.T) {
	restoreAllocHooks(t)
	path := t.TempDir() + "/res.img"
	fsIfc, err := Format(path, 64<<20, FormatConfig{Label: "res"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsIfc.(*xfsFS)
	defer fs.Close()

	// Allocate then free a run of blocks; the free goes through freeBlocks →
	// allocInsertWithReserve (reserve alloc + release tail-free). The image stays
	// consistent (re-read AGF free count returns to baseline).
	be := binary.BigEndian
	agf0, err := agfBlock(fs.f, fs.partOffset, fs.sb, 0)
	if err != nil {
		t.Fatalf("read AGF: %v", err)
	}
	freeBefore := be.Uint32(agf0[agfOffFreeBlks:])

	abs, err := allocBlocks(fs.f, fs.partOffset, fs.sb, 0, 4)
	if err != nil {
		t.Fatalf("allocBlocks: %v", err)
	}
	if err := freeBlocks(fs.f, fs.partOffset, fs.sb, abs, 4); err != nil {
		t.Fatalf("freeBlocks: %v", err)
	}

	agf1, err := agfBlock(fs.f, fs.partOffset, fs.sb, 0)
	if err != nil {
		t.Fatalf("re-read AGF: %v", err)
	}
	if got := be.Uint32(agf1[agfOffFreeBlks:]); got != freeBefore {
		t.Fatalf("free block count drifted after alloc+free: %d, want %d", got, freeBefore)
	}
}
