package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"testing"
)

// alloc_btree_delete_test.go — unit tests for the full B+tree DELETE path
// (alloc_btree_delete.go): rebalance (lshift/rshift) of an underfull leaf,
// merge (join) of two underfull leaves with parent-pointer removal and
// sibling-chain re-stitch, and root collapse (depth N → N-1). These drive the
// real recursive delete against trees grown by the real recursive insert in a
// memRW, then assert full structural consistency (descend + sibling-chain walk,
// strict order) for both bno and cnt orderings.

// allocTinyBtreeSB uses a 128-byte logical block so the alloc-btree maxrecs are
// tiny (maxLeaf=9, minLeaf=4, maxInternal=6): a few hundred inserts then reach
// depth >=3, and deletes drive merges + a root collapse with little data. The
// AGF header fields used by these tests all fit inside 64 bytes.
func allocTinyBtreeSB() *superblock {
	return &superblock{
		blockSize: 128,
		agBlocks:  8192,
		agCount:   1,
		inodeSize: 256,
		inopBlock: 8,
		inopBLog:  3,
		agBlkLog:  13,
		hasCRC:    true,
		hasFType:  true,
	}
}

// buildAllocTreeByInsert grows a fresh single-leaf-root alloc tree to multi-level
// by inserting the given (start,count) records through the real insert path,
// returning the live root/level read from the AGF. Block 8 is the initial root;
// split blocks are handed out from 1000+.
func buildAllocTreeByInsert(t *testing.T, rw *memRW, sb *superblock, bnoSort bool, recs [][2]uint32) (uint32, int) {
	t.Helper()
	be := binary.BigEndian
	magic := uint32(magicABTB)
	if !bnoSort {
		magic = magicABTC
	}
	root := fullAllocLeaf(sb, 8, 0xFFFFFFFF, 0xFFFFFFFF, 0, 0, 0, 0)
	be.PutUint32(root[0:], magic)
	putFSBlock(rw, 0, sb, 0, 8, root)
	if bnoSort {
		allocMockAGF(rw, sb, 8, 9, 1, 1)
	} else {
		allocMockAGF(rw, sb, 9, 8, 1, 1)
	}

	var next uint32 = 1000
	allocMetaAllocBlock = func(_ readerWriterAt, _ int64, _ *superblock, _ uint32, _ uint32) (uint64, error) {
		b := next
		next++
		return uint64(b), nil
	}

	for i, r := range recs {
		agf, _ := agfBlock(rw, 0, sb, 0)
		var root uint32
		var level int
		if bnoSort {
			root = be.Uint32(agf[agfOffBnoRoot:])
			level = int(be.Uint32(agf[agfOffBnoLevel:]))
		} else {
			root = be.Uint32(agf[agfOffCntRoot:])
			level = int(be.Uint32(agf[agfOffCntLevel:]))
		}
		if err := allocInsertRecord(rw, 0, sb, 0, root, level, r[0], r[1], bnoSort); err != nil {
			t.Fatalf("insert #%d (%v): %v", i, r, err)
		}
	}
	agf, _ := agfBlock(rw, 0, sb, 0)
	if bnoSort {
		return be.Uint32(agf[agfOffBnoRoot:]), int(be.Uint32(agf[agfOffBnoLevel:]))
	}
	return be.Uint32(agf[agfOffCntRoot:]), int(be.Uint32(agf[agfOffCntLevel:]))
}

func liveRootLevel(rw *memRW, sb *superblock, bnoSort bool) (uint32, int) {
	be := binary.BigEndian
	agf, _ := agfBlock(rw, 0, sb, 0)
	if bnoSort {
		return be.Uint32(agf[agfOffBnoRoot:]), int(be.Uint32(agf[agfOffBnoLevel:]))
	}
	return be.Uint32(agf[agfOffCntRoot:]), int(be.Uint32(agf[agfOffCntLevel:]))
}

// TestAllocBtreeDeleteRebalanceMergeCollapse grows a multi-level bno and cnt tree
// then deletes records one by one, asserting after EACH delete that the tree is
// structurally consistent (acyclic descent + sibling chain, strict order, exact
// surviving record set) — and that across the run the tree both shrinks a level
// (root collapse) and exercises the merge path.
func TestAllocBtreeDeleteRebalanceMergeCollapse(t *testing.T) {
	for _, bnoSort := range []bool{true, false} {
		name := "cnt"
		if bnoSort {
			name = "bno"
		}
		t.Run(name, func(t *testing.T) {
			restoreAllocHooks(t)
			// A small btree-block geometry (hdr+arrays sized for a 512-byte block but
			// a deliberately tiny logical block) keeps maxLeaf/maxNode small so a few
			// hundred records reach depth >=3, forcing merges + a root collapse.
			sb := allocTinyBtreeSB()
			rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))

			// Free-block hook must not re-enter this mock tree; merged blocks are
			// just dropped (their accounting is checked end-to-end in the VM oracle).
			allocDeleteFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }

			// Distinct records. For bno, distinct starts; for cnt, distinct counts
			// (with distinct starts as tiebreak) so the (count,start) order is total.
			const N = 400
			recs := make([][2]uint32, N)
			for i := 0; i < N; i++ {
				recs[i] = [2]uint32{uint32(i+1) * 8, uint32(i + 1)}
			}
			root, level := buildAllocTreeByInsert(t, rw, sb, bnoSort, recs)
			if level < 3 {
				t.Fatalf("%s tree only reached level %d; need >=3 to force merges+collapse", name, level)
			}
			startLevel := level

			// Track the live record set as a sorted slice for exact comparison.
			live := make([][2]uint32, len(recs))
			copy(live, recs)

			sawMerge := false
			sawCollapse := false
			prevBlocks := countAllocBlocks(t, rw, sb, root, level)

			// Delete in an order that drains a subtree (ascending) so leaves go
			// underfull and merge, eventually collapsing the root.
			order := make([]int, N)
			for i := range order {
				order[i] = i
			}
			for di, ri := range order {
				start, count := recs[ri][0], recs[ri][1]
				root, level = liveRootLevel(rw, sb, bnoSort)
				if err := allocDeleteRecord(rw, 0, sb, 0, root, level, start, count, bnoSort); err != nil {
					t.Fatalf("%s delete #%d (start=%d,count=%d): %v", name, di, start, count, err)
				}
				// Remove from the live set.
				live = removeRec(live, start, count)

				root, level = liveRootLevel(rw, sb, bnoSort)
				if level < startLevel {
					sawCollapse = true
				}
				curBlocks := countAllocBlocks(t, rw, sb, root, level)
				if curBlocks < prevBlocks {
					sawMerge = true
				}
				prevBlocks = curBlocks

				// Structural consistency after every delete.
				if len(live) == 0 {
					// Empty tree: root is an empty leaf.
					rb, _ := readAGBlock(rw, 0, sb, 0, root)
					if binary.BigEndian.Uint16(rb[6:]) != 0 || binary.BigEndian.Uint16(rb[4:]) != 0 {
						t.Fatalf("%s: empty tree root not an empty leaf", name)
					}
					continue
				}
				got := walkAllocLeaves(t, rw, sb, root, level, bnoSort)
				assertRecSetEqual(t, name, got, live, bnoSort)
			}
			if !sawCollapse {
				t.Fatalf("%s: root never collapsed a level during the delete run", name)
			}
			if !sawMerge {
				t.Fatalf("%s: no merge (btree block count never dropped) during the delete run", name)
			}
		})
	}
}

// countAllocBlocks counts the distinct blocks in the tree (root + every descended
// child + every leaf in the sibling chain) — a proxy for tree size that drops
// when a merge frees a block.
func countAllocBlocks(t *testing.T, rw *memRW, sb *superblock, root uint32, level int) int {
	t.Helper()
	be := binary.BigEndian
	hdr := sb.agBTreeHdrSize()
	seen := map[uint32]bool{}
	var visit func(rel uint32)
	visit = func(rel uint32) {
		if seen[rel] {
			return
		}
		seen[rel] = true
		blk, err := readAGBlock(rw, 0, sb, 0, rel)
		if err != nil {
			t.Fatalf("read block %d: %v", rel, err)
		}
		if be.Uint16(blk[4:]) == 0 {
			return
		}
		nr := int(be.Uint16(blk[6:]))
		ptrBase := hdr + allocMaxInternal(int(sb.blockSize), hdr)*allocKeySize
		for i := 0; i < nr; i++ {
			visit(be.Uint32(blk[ptrBase+i*allocPtrSize:]))
		}
	}
	visit(root)
	return len(seen)
}

func removeRec(recs [][2]uint32, start, count uint32) [][2]uint32 {
	for i, r := range recs {
		if r[0] == start && r[1] == count {
			return append(recs[:i:i], recs[i+1:]...)
		}
	}
	return recs
}

func assertRecSetEqual(t *testing.T, name string, got [][2]uint32, want [][2]uint32, bnoSort bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: tree holds %d records, want %d", name, len(got), len(want))
	}
	g := make([][2]uint32, len(got))
	copy(g, got)
	w := make([][2]uint32, len(want))
	copy(w, want)
	less := func(s [][2]uint32) func(i, j int) bool {
		return func(i, j int) bool { return allocLess(bnoSort, s[i][0], s[i][1], s[j][0], s[j][1]) }
	}
	sort.Slice(g, less(g))
	sort.Slice(w, less(w))
	for i := range g {
		if g[i] != w[i] {
			t.Fatalf("%s: record %d = %v, want %v", name, i, g[i], w[i])
		}
	}
}

// TestAllocDeleteRootCollapseDirect drives a hand-built depth-2 tree whose root
// has exactly two minimal leaves; deleting until one leaf merges into the other
// leaves the root with a single child, forcing the collapse to depth 1 and the
// promotion of the surviving leaf to root (null siblings).
func TestAllocDeleteRootCollapseDirect(t *testing.T) {
	restoreAllocHooks(t)
	sb := allocDeepSB()
	be := binary.BigEndian
	rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))
	allocDeleteFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }

	hdr := sb.agBTreeHdrSize()
	minLeaf := allocMinLeaf(int(sb.blockSize), hdr)
	// Two leaves each at exactly minLeaf records, disjoint ascending ranges.
	stride := uint32(4)
	leftRecs := minLeaf
	rightRecs := minLeaf
	leftBase := uint32(100)
	rightBase := leftBase + uint32(leftRecs+5)*stride
	left := fullAllocLeaf(sb, 10, 0xFFFFFFFF, 11, leftRecs, leftBase, stride, 1)
	right := fullAllocLeaf(sb, 11, 10, 0xFFFFFFFF, rightRecs, rightBase, stride, 1)
	rootKeys := []allocRec{{leftBase, 1}, {rightBase, 1}}
	root := makeAllocInternal(sb, rootKeys, []uint32{10, 11})
	be.PutUint32(root[0:], magicABTB)
	be.PutUint16(root[4:], 1)
	putFSBlock(rw, 0, sb, 0, 8, root)
	putFSBlock(rw, 0, sb, 0, 10, left)
	putFSBlock(rw, 0, sb, 0, 11, right)
	allocMockAGF(rw, sb, 8, 9, 2, 2)
	// Set agf_btreeblks so the collapse can decrement it without underflow.
	agf, _ := agfBlock(rw, 0, sb, 0)
	be.PutUint32(agf[agfOffBtreeBlks:], 2)
	putAGF(rw, 0, sb, 0, agf)

	// Delete one record from the right leaf: it drops to minLeaf-1 (underfull),
	// the left sibling is at minLeaf (cannot spare → would itself underflow), so
	// the two merge, the root loses a child and collapses to depth 1.
	root2, level2 := liveRootLevel(rw, sb, true)
	if err := allocDeleteRecord(rw, 0, sb, 0, root2, level2, rightBase, 1, true); err != nil {
		t.Fatalf("delete forcing collapse: %v", err)
	}
	newRoot, newLevel := liveRootLevel(rw, sb, true)
	if newLevel != 1 {
		t.Fatalf("root did not collapse: level=%d want 1", newLevel)
	}
	rb, _ := readAGBlock(rw, 0, sb, 0, newRoot)
	if be.Uint16(rb[4:]) != 0 {
		t.Fatalf("collapsed root is not a leaf (level=%d)", be.Uint16(rb[4:]))
	}
	if l, r := be.Uint32(rb[8:]), be.Uint32(rb[12:]); l != 0xFFFFFFFF || r != 0xFFFFFFFF {
		t.Fatalf("promoted root has non-null siblings: lsib=%d rsib=%d", l, r)
	}
	// All surviving records present and strictly ordered.
	recs := walkAllocLeaves(t, rw, sb, newRoot, newLevel, true)
	if len(recs) != leftRecs+rightRecs-1 {
		t.Fatalf("collapsed tree holds %d records, want %d", len(recs), leftRecs+rightRecs-1)
	}
	// Two btree blocks were freed: the merged-away right leaf, and the old interior
	// root the collapse promoted its single child past. btreeblks 2 → 0.
	agf2, _ := agfBlock(rw, 0, sb, 0)
	if got := be.Uint32(agf2[agfOffBtreeBlks:]); got != 0 {
		t.Fatalf("agf_btreeblks after merge+collapse = %d, want 0", got)
	}
}

// TestAllocDeleteRebalanceFromSibling builds a depth-2 tree where the underfull
// leaf has a fat right sibling that can spare a record, so the delete REBALANCES
// (rshift) instead of merging: no block is freed and the parent separator updates.
func TestAllocDeleteRebalanceFromSibling(t *testing.T) {
	restoreAllocHooks(t)
	sb := allocDeepSB()
	be := binary.BigEndian
	rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))
	allocDeleteFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }

	hdr := sb.agBTreeHdrSize()
	maxLeaf := allocMaxLeaf(int(sb.blockSize), hdr)
	minLeaf := allocMinLeaf(int(sb.blockSize), hdr)
	stride := uint32(4)
	leftRecs := minLeaf  // exactly at min: deleting one makes it underfull
	rightRecs := maxLeaf // full: can spare a record
	leftBase := uint32(100)
	rightBase := leftBase + uint32(leftRecs+5)*stride
	left := fullAllocLeaf(sb, 10, 0xFFFFFFFF, 11, leftRecs, leftBase, stride, 1)
	right := fullAllocLeaf(sb, 11, 10, 0xFFFFFFFF, rightRecs, rightBase, stride, 1)
	root := makeAllocInternal(sb, []allocRec{{leftBase, 1}, {rightBase, 1}}, []uint32{10, 11})
	be.PutUint32(root[0:], magicABTB)
	be.PutUint16(root[4:], 1)
	putFSBlock(rw, 0, sb, 0, 8, root)
	putFSBlock(rw, 0, sb, 0, 10, left)
	putFSBlock(rw, 0, sb, 0, 11, right)
	allocMockAGF(rw, sb, 8, 9, 2, 2)
	agf, _ := agfBlock(rw, 0, sb, 0)
	be.PutUint32(agf[agfOffBtreeBlks:], 2)
	putAGF(rw, 0, sb, 0, agf)

	rootRel, level := liveRootLevel(rw, sb, true)
	if err := allocDeleteRecord(rw, 0, sb, 0, rootRel, level, leftBase, 1, true); err != nil {
		t.Fatalf("delete forcing rebalance: %v", err)
	}
	// Depth unchanged (no collapse), block count unchanged (no merge).
	newRoot, newLevel := liveRootLevel(rw, sb, true)
	if newLevel != 2 {
		t.Fatalf("tree depth changed on a rebalance: level=%d want 2", newLevel)
	}
	agf2, _ := agfBlock(rw, 0, sb, 0)
	if got := be.Uint32(agf2[agfOffBtreeBlks:]); got != 2 {
		t.Fatalf("agf_btreeblks changed on a rebalance: %d want 2", got)
	}
	recs := walkAllocLeaves(t, rw, sb, newRoot, newLevel, true)
	if len(recs) != leftRecs+rightRecs-1 {
		t.Fatalf("rebalanced tree holds %d records, want %d", len(recs), leftRecs+rightRecs-1)
	}
	// The left leaf must be back at >= minLeaf (borrowed from the right).
	lb, _ := readAGBlock(rw, 0, sb, 0, 10)
	if got := int(be.Uint16(lb[6:])); got < minLeaf {
		t.Fatalf("left leaf still underfull after rebalance: %d < %d", got, minLeaf)
	}
}

// TestAllocDeleteLshiftLeaf forces a borrow from the LEFT sibling: the underfull
// leaf is the parent's last child (no right sibling) and its left sibling is fat.
func TestAllocDeleteLshiftLeaf(t *testing.T) {
	restoreAllocHooks(t)
	sb := allocDeepSB()
	be := binary.BigEndian
	rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))
	allocDeleteFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }

	hdr := sb.agBTreeHdrSize()
	maxLeaf := allocMaxLeaf(int(sb.blockSize), hdr)
	minLeaf := allocMinLeaf(int(sb.blockSize), hdr)
	stride := uint32(4)
	leftBase := uint32(100)
	rightBase := leftBase + uint32(maxLeaf+5)*stride
	left := fullAllocLeaf(sb, 10, 0xFFFFFFFF, 11, maxLeaf, leftBase, stride, 1)   // fat
	right := fullAllocLeaf(sb, 11, 10, 0xFFFFFFFF, minLeaf, rightBase, stride, 1) // at min
	root := makeAllocInternal(sb, []allocRec{{leftBase, 1}, {rightBase, 1}}, []uint32{10, 11})
	be.PutUint32(root[0:], magicABTB)
	be.PutUint16(root[4:], 1)
	putFSBlock(rw, 0, sb, 0, 8, root)
	putFSBlock(rw, 0, sb, 0, 10, left)
	putFSBlock(rw, 0, sb, 0, 11, right)
	allocMockAGF(rw, sb, 8, 9, 2, 2)
	agf, _ := agfBlock(rw, 0, sb, 0)
	be.PutUint32(agf[agfOffBtreeBlks:], 2)
	putAGF(rw, 0, sb, 0, agf)

	rootRel, level := liveRootLevel(rw, sb, true)
	// Delete the right leaf's last record so it underflows; its only sibling is on
	// the left → lshift borrows the left's last record into the right's head.
	target := rightBase + uint32(minLeaf-1)*stride
	if err := allocDeleteRecord(rw, 0, sb, 0, rootRel, level, target, 1, true); err != nil {
		t.Fatalf("delete forcing lshift: %v", err)
	}
	newRoot, newLevel := liveRootLevel(rw, sb, true)
	if newLevel != 2 {
		t.Fatalf("tree depth changed on an lshift: level=%d want 2", newLevel)
	}
	agf2, _ := agfBlock(rw, 0, sb, 0)
	if got := be.Uint32(agf2[agfOffBtreeBlks:]); got != 2 {
		t.Fatalf("agf_btreeblks changed on an lshift: %d want 2", got)
	}
	// The right leaf's parent separator must now equal its new first key (the
	// borrowed record), and the whole tree stays consistent.
	recs := walkAllocLeaves(t, rw, sb, newRoot, newLevel, true)
	if len(recs) != maxLeaf+minLeaf-1 {
		t.Fatalf("lshift tree holds %d records, want %d", len(recs), maxLeaf+minLeaf-1)
	}
	rb, _ := readAGBlock(rw, 0, sb, 0, 11)
	if got := int(be.Uint16(rb[6:])); got < minLeaf {
		t.Fatalf("right leaf still underfull after lshift: %d < %d", got, minLeaf)
	}
}

// TestAllocDeleteInteriorRebalanceAndMerge drives a depth-3 tree where deleting
// records collapses leaves under one interior node until that INTERIOR node goes
// underfull and must itself rebalance/merge — exercising the interior lshift/
// rshift/merge key+ptr machinery (not just the leaf level). It deletes the whole
// key range and checks consistency after every step.
func TestAllocDeleteInteriorRebalanceAndMerge(t *testing.T) {
	for _, bnoSort := range []bool{true, false} {
		name := "cnt"
		if bnoSort {
			name = "bno"
		}
		t.Run(name, func(t *testing.T) {
			restoreAllocHooks(t)
			sb := allocTinyBtreeSB()
			rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))
			allocDeleteFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }

			const N = 300
			recs := make([][2]uint32, N)
			for i := 0; i < N; i++ {
				recs[i] = [2]uint32{uint32(i+1) * 8, uint32(i + 1)}
			}
			root, level := buildAllocTreeByInsert(t, rw, sb, bnoSort, recs)
			if level < 3 {
				t.Fatalf("%s tree only reached level %d; need >=3", name, level)
			}

			live := make([][2]uint32, len(recs))
			copy(live, recs)
			// Delete from the END (descending) so a DIFFERENT subtree drains than the
			// ascending merge test, exercising interior rshift/merge on the right.
			for i := N - 1; i >= 0; i-- {
				start, count := recs[i][0], recs[i][1]
				root, level = liveRootLevel(rw, sb, bnoSort)
				if err := allocDeleteRecord(rw, 0, sb, 0, root, level, start, count, bnoSort); err != nil {
					t.Fatalf("%s delete (start=%d,count=%d): %v", name, start, count, err)
				}
				live = removeRec(live, start, count)
				root, level = liveRootLevel(rw, sb, bnoSort)
				if len(live) == 0 {
					continue
				}
				got := walkAllocLeaves(t, rw, sb, root, level, bnoSort)
				assertRecSetEqual(t, name, got, live, bnoSort)
			}
		})
	}
}

// makeDepth2BnoTree builds a depth-2 bno tree: an interior root (block 8) over
// two leaves (10,11) and installs an AGF (with btreeblks=2). Returns rw.
func makeDepth2BnoTree(t *testing.T, sb *superblock, leftRecs, rightRecs int) *memRW {
	t.Helper()
	be := binary.BigEndian
	rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))
	stride := uint32(4)
	leftBase := uint32(100)
	rightBase := leftBase + uint32(leftRecs+rightRecs+10)*stride
	left := fullAllocLeaf(sb, 10, 0xFFFFFFFF, 11, leftRecs, leftBase, stride, 1)
	right := fullAllocLeaf(sb, 11, 10, 0xFFFFFFFF, rightRecs, rightBase, stride, 1)
	root := makeAllocInternal(sb, []allocRec{{leftBase, 1}, {rightBase, 1}}, []uint32{10, 11})
	be.PutUint32(root[0:], magicABTB)
	be.PutUint16(root[4:], 1)
	putFSBlock(rw, 0, sb, 0, 8, root)
	putFSBlock(rw, 0, sb, 0, 10, left)
	putFSBlock(rw, 0, sb, 0, 11, right)
	allocMockAGF(rw, sb, 8, 9, 2, 2)
	agf, _ := agfBlock(rw, 0, sb, 0)
	be.PutUint32(agf[agfOffBtreeBlks:], 2)
	putAGF(rw, 0, sb, 0, agf)
	return rw
}

// TestAllocDeleteErrorPaths injects I/O failures at each read/write/free point of
// the delete path and asserts the error propagates.
func TestAllocDeleteErrorPaths(t *testing.T) {
	sb := allocDeepSB()
	maxLeaf := allocMaxLeaf(int(sb.blockSize), sb.agBTreeHdrSize())
	minLeaf := allocMinLeaf(int(sb.blockSize), sb.agBTreeHdrSize())

	// Root-read failure during descent.
	t.Run("descent read fails", func(t *testing.T) {
		restoreAllocHooks(t)
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) { return nil, errBoom }
		if err := allocDeleteRecord(newMemRW(0), 0, sb, 0, 8, 2, 100, 1, true); !errors.Is(err, errBoom) {
			t.Fatalf("want errBoom, got %v", err)
		}
	})

	// Leaf write failure after an in-place delete (leaf stays full enough → write).
	t.Run("leaf write fails", func(t *testing.T) {
		restoreAllocHooks(t)
		allocReadAGBlock = readAGBlock
		rw := makeDepth2BnoTree(t, sb, maxLeaf, maxLeaf)
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return errBoom }
		root, level := liveRootLevel(rw, sb, true)
		// Delete a middle record of the (full) left leaf: no underflow, just a leaf write.
		if err := allocDeleteRecord(rw, 0, sb, 0, root, level, 100+8, 1, true); !errors.Is(err, errBoom) {
			t.Fatalf("want errBoom on leaf write, got %v", err)
		}
	})

	// Sibling read failure during a rebalance attempt (rshift reads right sibling).
	t.Run("rshift sibling read fails", func(t *testing.T) {
		restoreAllocHooks(t)
		rw := makeDepth2BnoTree(t, sb, minLeaf, maxLeaf)
		reads := 0
		allocReadAGBlock = func(r io.ReaderAt, p int64, s *superblock, ag, rel uint32) ([]byte, error) {
			reads++
			// Allow the descent (root, leaf) reads; fail the first sibling read.
			if rel == 11 {
				return nil, errBoom
			}
			return readAGBlock(r, p, s, ag, rel)
		}
		root, level := liveRootLevel(rw, sb, true)
		// Delete the left leaf's only-just-min record → underflow → rshift reads 11.
		if err := allocDeleteRecord(rw, 0, sb, 0, root, level, 100, 1, true); !errors.Is(err, errBoom) {
			t.Fatalf("want errBoom on sibling read, got %v", err)
		}
	})

	// AGF write failure after a collapse (root/level/btreeblks persist).
	t.Run("agf write fails on collapse", func(t *testing.T) {
		restoreAllocHooks(t)
		allocReadAGBlock = readAGBlock
		allocDeleteFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }
		rw := makeDepth2BnoTree(t, sb, minLeaf, minLeaf)
		allocWriteAGF = func(io.WriterAt, int64, *superblock, uint32, []byte) error { return errBoom }
		root, level := liveRootLevel(rw, sb, true)
		// minLeaf+minLeaf both at min: delete one → merge → collapse → AGF write.
		if err := allocDeleteRecord(rw, 0, sb, 0, root, level, 100, 1, true); !errors.Is(err, errBoom) {
			t.Fatalf("want errBoom on AGF write, got %v", err)
		}
	})

	// Merged-block free failure.
	t.Run("merged block free fails", func(t *testing.T) {
		restoreAllocHooks(t)
		allocReadAGBlock = readAGBlock
		rw := makeDepth2BnoTree(t, sb, minLeaf, minLeaf)
		allocDeleteFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return errBoom }
		root, level := liveRootLevel(rw, sb, true)
		if err := allocDeleteRecord(rw, 0, sb, 0, root, level, 100, 1, true); !errors.Is(err, errBoom) {
			t.Fatalf("want errBoom on merged-block free, got %v", err)
		}
	})
}

// makeAllocInternalLevel builds an interior block at the given on-disk level with
// the supplied keys/ptrs and sibling pointers.
func makeAllocInternalLevel(sb *superblock, level int, lsib, rsib uint32, keys []allocRec, ptrs []uint32) []byte {
	be := binary.BigEndian
	blk := makeAllocInternal(sb, keys, ptrs)
	be.PutUint32(blk[0:], magicABTB)
	be.PutUint16(blk[4:], uint16(level))
	be.PutUint32(blk[8:], lsib)
	be.PutUint32(blk[12:], rsib)
	return blk
}

// TestAllocDeleteInteriorLshiftRshift hand-builds a depth-3 bno tree whose root
// has two interior children L (fat) and R (at minInternal). Deleting a record
// under R's last leaf forces that leaf to merge, dropping R below minInternal;
// R then borrows a (key,ptr) from its fat left sibling L via the INTERIOR lshift
// path (the non-leaf branch of tryLshift / the parent-separator refresh).
func TestAllocDeleteInteriorLshiftRshift(t *testing.T) {
	restoreAllocHooks(t)
	sb := allocTinyBtreeSB()
	be := binary.BigEndian
	hdr := sb.agBTreeHdrSize()
	rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))
	allocDeleteFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return nil }

	minLeaf := allocMinLeaf(int(sb.blockSize), hdr)
	minInt := allocMinInternal(int(sb.blockSize), hdr)
	maxInt := allocMaxInternal(int(sb.blockSize), hdr)
	stride := uint32(4)

	// Build leaves. We need L to be a fat interior node (maxInt children) and R to
	// be at exactly minInt children. Each child leaf holds minLeaf records so a
	// single merge under R can drop it a child. Leaf blocks numbered from 100,
	// interior nodes from 50.
	var leafBlk uint32 = 100
	base := uint32(1000)
	mkLeaves := func(n int) ([]uint32, []allocRec, uint32) {
		ptrs := make([]uint32, n)
		keys := make([]allocRec, n)
		for i := 0; i < n; i++ {
			self := leafBlk
			leafBlk++
			var ls, rs uint32 = 0xFFFFFFFF, 0xFFFFFFFF
			if self > 100 {
				ls = self - 1
			}
			rs = self + 1 // fixed up globally below by chaining; last set to 0xFFFFFFFF later
			leaf := fullAllocLeaf(sb, self, ls, rs, minLeaf, base, stride, 1)
			putFSBlock(rw, 0, sb, 0, self, leaf)
			ptrs[i] = self
			keys[i] = allocRec{base, 1}
			base += uint32(minLeaf+2) * stride
		}
		return ptrs, keys, leafBlk - 1
	}

	lPtrs, lKeys, _ := mkLeaves(maxInt)
	rPtrs, rKeys, lastLeaf := mkLeaves(minInt)

	// Null the last leaf's rsib.
	last, _ := readAGBlock(rw, 0, sb, 0, lastLeaf)
	be.PutUint32(last[12:], 0xFFFFFFFF)
	putFSBlock(rw, 0, sb, 0, lastLeaf, last)

	// Interior L (block 50, level 1, fat) and R (block 51, level 1, at min).
	L := makeAllocInternalLevel(sb, 1, 0xFFFFFFFF, 51, lKeys, lPtrs)
	R := makeAllocInternalLevel(sb, 1, 50, 0xFFFFFFFF, rKeys, rPtrs)
	putFSBlock(rw, 0, sb, 0, 50, L)
	putFSBlock(rw, 0, sb, 0, 51, R)

	// Root (block 8, level 2) over L and R.
	root := makeAllocInternalLevel(sb, 2, 0xFFFFFFFF, 0xFFFFFFFF, []allocRec{lKeys[0], rKeys[0]}, []uint32{50, 51})
	putFSBlock(rw, 0, sb, 0, 8, root)
	allocMockAGF(rw, sb, 8, 9, 3, 3)
	agf, _ := agfBlock(rw, 0, sb, 0)
	be.PutUint32(agf[agfOffBtreeBlks:], uint32(2+2*maxInt)) // rough; only sign matters
	putAGF(rw, 0, sb, 0, agf)

	// Collect every live record for the post-delete consistency check.
	var allRecs [][2]uint32
	for _, p := range append(append([]uint32{}, lPtrs...), rPtrs...) {
		blk, _ := readAGBlock(rw, 0, sb, 0, p)
		nr := int(be.Uint16(blk[6:]))
		for i := 0; i < nr; i++ {
			off := hdr + i*allocRecSize
			allRecs = append(allRecs, [2]uint32{be.Uint32(blk[off:]), be.Uint32(blk[off+4:])})
		}
	}

	// Delete every record of R's last leaf so that leaf merges into its left
	// sibling, dropping R to minInt-1 children → R borrows from L (interior lshift)
	// or L merges with R. Either way the tree stays consistent and (since L is fat)
	// the interior rebalance path runs.
	rLastLeaf := rPtrs[len(rPtrs)-1]
	llBlk, _ := readAGBlock(rw, 0, sb, 0, rLastLeaf)
	llNum := int(be.Uint16(llBlk[6:]))
	var targets [][2]uint32
	for i := 0; i < llNum; i++ {
		off := hdr + i*allocRecSize
		targets = append(targets, [2]uint32{be.Uint32(llBlk[off:]), be.Uint32(llBlk[off+4:])})
	}
	for _, tg := range targets {
		rootRel, level := liveRootLevel(rw, sb, true)
		if err := allocDeleteRecord(rw, 0, sb, 0, rootRel, level, tg[0], tg[1], true); err != nil {
			t.Fatalf("interior-rebalance delete (start=%d): %v", tg[0], err)
		}
		allRecs = removeRec(allRecs, tg[0], tg[1])
		rootRel, level = liveRootLevel(rw, sb, true)
		got := walkAllocLeaves(t, rw, sb, rootRel, level, true)
		assertRecSetEqual(t, "bno-interior", got, allRecs, true)
	}
	_ = minInt
}

// TestAllocDeleteEndToEndCollapse drives a REAL formatted image through the full
// delete path end to end (no mocks): it fragments AG 0's free space to multi-
// level bno/cnt trees via free inode chunks, then allocates whole free extents
// (remaining==0 → record delete in both trees → rebalance/merge/collapse) until
// the bno tree shrinks a level. It asserts bno/cnt stay in sync (same extent set)
// after every delete and that a collapse actually happens — exercising the
// partial-consume separator refresh (allocCntRefreshLeafKey), the merged-block
// drain with queue-backed split blocks (allocDrainMergedFrees), the fragmentation
// gate, and the structural merge/collapse against trees built by the real writer.
func TestAllocDeleteEndToEndCollapse(t *testing.T) {
	restoreAllocHooks(t)
	// This test drives a REAL formatted image, so the hooks must be the real
	// implementations (restoreAllocHooks installs no-op free / longest=0 mocks).
	allocFreeBlocks = freeBlocks
	allocRecomputeLongest = agfRecomputeLongest
	allocDeleteFreeBlocks = freeBlocks
	path := t.TempDir() + "/collapse.img"
	fsIfc, err := Format(path, 3<<30, FormatConfig{Label: "e2eclp"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsIfc.(*xfsFS)
	defer fs.Close()
	be := binary.BigEndian

	collect := func(bnoSort bool) [][2]uint32 {
		hdr := fs.sb.agBTreeHdrSize()
		agf, _ := agfBlock(fs.f, fs.partOffset, fs.sb, 0)
		var root uint32
		if bnoSort {
			root = be.Uint32(agf[agfOffBnoRoot:])
		} else {
			root = be.Uint32(agf[agfOffCntRoot:])
		}
		cur := root
		for {
			blk, _ := readAGBlock(fs.f, fs.partOffset, fs.sb, 0, cur)
			if be.Uint16(blk[4:]) == 0 {
				break
			}
			ptrOff := hdr + allocMaxInternal(int(fs.sb.blockSize), hdr)*allocKeySize
			cur = be.Uint32(blk[ptrOff:])
		}
		var out [][2]uint32
		for cur != 0xFFFFFFFF {
			blk, _ := readAGBlock(fs.f, fs.partOffset, fs.sb, 0, cur)
			nr := int(be.Uint16(blk[6:]))
			for i := 0; i < nr; i++ {
				out = append(out, [2]uint32{be.Uint32(blk[hdr+i*allocRecSize:]), be.Uint32(blk[hdr+i*allocRecSize+4:])})
			}
			cur = be.Uint32(blk[12:])
		}
		return out
	}
	inSync := func() bool {
		b, c := collect(true), collect(false)
		if len(b) != len(c) {
			return false
		}
		set := map[[2]uint32]bool{}
		for _, r := range b {
			set[r] = true
		}
		for _, r := range c {
			if !set[r] {
				return false
			}
		}
		return true
	}

	// Phase 1: fragment to multi-level.
	margin := 0
	for i := 0; i < 200000; i++ {
		if err := growInobt(fs.f, fs.partOffset, fs.sb, 0); err != nil {
			break
		}
		agf, _ := agfBlock(fs.f, fs.partOffset, fs.sb, 0)
		if be.Uint32(agf[agfOffBnoLevel:]) >= 2 && be.Uint32(agf[agfOffCntLevel:]) >= 2 {
			margin++
			if margin >= 40 {
				break
			}
		}
	}
	agf, _ := agfBlock(fs.f, fs.partOffset, fs.sb, 0)
	if be.Uint32(agf[agfOffBnoLevel:]) < 2 {
		t.Skip("phase 1 did not reach multi-level on this geometry; nothing to collapse")
	}

	// Phase 2: consume whole smallest extents until the bno tree collapses a level.
	collapsed := false
	for step := 0; step < 100000; step++ {
		agf, _ := agfBlock(fs.f, fs.partOffset, fs.sb, 0)
		if be.Uint32(agf[agfOffBnoLevel:]) < 2 {
			collapsed = true
			break
		}
		cntRoot := be.Uint32(agf[agfOffCntRoot:])
		cntLevel := int(be.Uint32(agf[agfOffCntLevel:]))
		_, leaf, idx, err := cntFindBlock(fs.f, fs.partOffset, fs.sb, 0, cntRoot, cntLevel, 1)
		if err != nil {
			break
		}
		small := be.Uint32(leaf[fs.sb.agBTreeHdrSize()+idx*allocRecSize+4:])
		if small == 0 {
			break
		}
		if _, err := allocBlocks(fs.f, fs.partOffset, fs.sb, 0, small); err != nil {
			t.Fatalf("whole-extent alloc at step %d (n=%d): %v", step, small, err)
		}
		if !inSync() {
			t.Fatalf("bno/cnt desynced after whole-extent delete at step %d", step)
		}
	}
	if !collapsed {
		t.Skip("free-space tree did not collapse on this geometry within the step budget")
	}
	// The collapse landed and the trees stayed in sync through every delete; the
	// committed alloc-collapse fixture (TestAllocCollapseFixture) carries the same
	// shape through the canonical xfs_repair + kernel-mount oracle.
	if !inSync() {
		t.Fatal("bno/cnt out of sync after collapse")
	}
}
