package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
)

// alloc_btree.go — general multi-level insert for the AGF free-space B-trees
// (bnobt, keyed by start block; cntbt, keyed by (count, start block)). It
// mirrors the inobt insert/split/newroot machinery in alloc.go
// (inobtInsertChunkRecord → inobtInsertInto → inobtSplitLeaf/Node) but for the
// 8-byte alloc record (ar_startblock(4) + ar_blockcount(4)), the 8-byte alloc
// key (the leftmost record's {startblock, blockcount}) and the 4-byte child
// pointer.
//
// Before this, the writer kept each free-space tree as a single root leaf, so
// once the free-extent count exceeded one leaf (~540 records at blockSize=4096)
// every allocation/free failed ("bno/cnt B-tree leaf full"). That ceiling
// blocked generating large/fragmented images and the depth-3 inobt fixture (the
// inobt's own 8-block chunk allocations fragment the free space, overflowing the
// single free-space leaf at ~540 chunks — see f16b4a8's note). With recursive
// node split + key propagation + root growth the free-space trees now grow to
// arbitrary depth, exactly as xfsprogs' xfs_alloc_insrec → xfs_btree_split does.
//
// On-disk records/keys/ptrs are big-endian (binary.BigEndian); the LE on-disk
// header fields (CRC etc.) are handled by writeAGBTree as before.

// allocMaxInternal returns the maximum number of key/ptr pairs an interior
// alloc-btree block of length blockLen (header hdrSize) can hold. As with the
// inobt, XFS short-form btree blocks size the key and pointer arrays to this
// maxrecs, so an interior node's pointer array always begins at
// hdrSize + maxInternal*allocKeySize regardless of the live record count.
func allocMaxInternal(blockLen, hdrSize int) int {
	return (blockLen - hdrSize) / (allocKeySize + allocPtrSize)
}

// allocMaxLeaf returns the maximum number of records an alloc-btree leaf of
// length blockLen (header hdrSize) can hold.
func allocMaxLeaf(blockLen, hdrSize int) int {
	return (blockLen - hdrSize) / allocRecSize
}

// allocSplit carries a node split back up the recursion, identically to
// inobtSplit: the lower half stays in the visited block, the upper half moves to
// a freshly-allocated right sibling at rightRel, and rightKeyStart/rightKeyCount
// are that sibling's first record's {startblock, blockcount} so the parent can
// insert the separator key. When split is false the insert was absorbed in
// place.
type allocSplit struct {
	split         bool
	rightKeyStart uint32
	rightKeyCount uint32
	rightRel      uint32
}

// allocLess reports whether record/key a sorts strictly before b in the given
// tree order. bno orders by startblock alone (start is unique across free
// extents, so no tiebreak is needed); cnt orders by (blockcount, startblock) so
// equal-length extents stay strictly ordered and the allocator's "largest on the
// right" invariant holds. This is the single source of truth for both descent
// and leaf insertion, so the two never disagree.
func allocLess(bnoSort bool, aStart, aCount, bStart, bCount uint32) bool {
	if bnoSort {
		return aStart < bStart
	}
	if aCount != bCount {
		return aCount < bCount
	}
	return aStart < bStart
}

// allocInsertRecord inserts (agStartRel, nBlocks) into the bno or cnt B-tree of
// AG ag, growing the tree (leaf split → node split → new root) as needed. It is
// the general replacement for the old single-leaf allocBTreeInsert. On a root
// split it re-reads the AGF, repoints the relevant root/level pair (bno_root +
// bno_level when bnoSort, else cnt_root + cnt_level), and writes the AGF back so
// the new depth is durable before the caller's own AGF update.
func allocInsertRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, agStartRel, nBlocks uint32, bnoSort bool) (retErr error) {
	be := binary.BigEndian

	var rec [allocRecSize]byte
	be.PutUint32(rec[0:], agStartRel)
	be.PutUint32(rec[4:], nBlocks)

	// Split blocks come from allocMetaAllocBlock. When the caller (freeBlocks via
	// allocInsertWithReserve) has installed a pre-allocated reserve, a split pops a
	// block carved while the trees were consistent, so it never allocates against a
	// tree that is mid-rewrite. Otherwise the default hook (allocMetaBlock →
	// allocBlocks) is used; that path is only reached by single-leaf trees that
	// never split, or by tests that install their own deterministic hook.

	res, err := allocInsertInto(rw, partOff, sb, ag, rootRel, agStartRel, nBlocks, bnoSort, rec[:])
	if err != nil {
		return err
	}
	if !res.split {
		return nil
	}

	// The root split: grow a fresh root one level higher pointing at the old
	// root (left child) and its new right sibling. This is the only place the
	// free-space tree depth grows.
	newRootAbs, err := allocMetaAllocBlock(rw, partOff, sb, ag, 1)
	if err != nil {
		return fmt.Errorf("xfs: alloc btree split: alloc root: %w", err)
	}
	newRootRel := uint32(newRootAbs % uint64(sb.agBlocks))

	hdrSize := sb.agBTreeHdrSize()
	oldRoot, err := allocReadAGBlock(rw, partOff, sb, ag, rootRel)
	if err != nil {
		return err
	}
	oldBlockLevel := int(be.Uint16(oldRoot[4:]))
	magic := be.Uint32(oldRoot[0:])
	root := newAllocBlock(sb, ag, newRootRel, oldBlockLevel+1, 2, magic)

	// The left separator key is the old root's first record/key verbatim (the
	// smallest key reachable through the whole left subtree); for a leaf that is
	// its first record's {start,count}, for an interior node its first key.
	leftStart := be.Uint32(oldRoot[hdrSize:])
	leftCount := be.Uint32(oldRoot[hdrSize+4:])
	be.PutUint32(root[hdrSize+0*allocKeySize:], leftStart)
	be.PutUint32(root[hdrSize+0*allocKeySize+4:], leftCount)
	be.PutUint32(root[hdrSize+1*allocKeySize:], res.rightKeyStart)
	be.PutUint32(root[hdrSize+1*allocKeySize+4:], res.rightKeyCount)

	maxInternal := allocMaxInternal(len(root), hdrSize)
	ptrBase := hdrSize + maxInternal*allocKeySize
	be.PutUint32(root[ptrBase+0*allocPtrSize:], rootRel)
	be.PutUint32(root[ptrBase+1*allocPtrSize:], res.rightRel)
	if err := allocWriteAGBTree(rw, partOff, sb, ag, newRootRel, root); err != nil {
		return err
	}

	// Persist the new root/level into the AGF. Re-read so we don't clobber any
	// other field a concurrent step touched; allocBlocks/freeBlocks re-read the
	// AGF after the inserts so this durable update is not lost.
	agfBuf, err := allocAGFBlock(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	if bnoSort {
		be.PutUint32(agfBuf[agfOffBnoRoot:], newRootRel)
		be.PutUint32(agfBuf[agfOffBnoLevel:], uint32(level+1))
	} else {
		be.PutUint32(agfBuf[agfOffCntRoot:], newRootRel)
		be.PutUint32(agfBuf[agfOffCntLevel:], uint32(level+1))
	}
	// Account the extra btree block the split consumed.
	allocBumpBtreeBlocks(agfBuf, 1)
	return allocWriteAGF(rw, partOff, sb, ag, agfBuf)
}

// allocInsertInto recursively inserts rec into the alloc subtree rooted at
// selfRel and returns whether selfRel split. On a leaf it inserts in sorted
// order; on an interior node it descends into the covering child, then absorbs
// (or splits to propagate) the child's separator key/ptr. Layout matches the
// short-form btree: all keys first, then all pointers, at the maxrecs boundary.
func allocInsertInto(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, selfRel, agStartRel, nBlocks uint32, bnoSort bool, rec []byte) (allocSplit, error) {
	be := binary.BigEndian
	hdrSize := sb.agBTreeHdrSize()

	blk, err := allocReadAGBlock(rw, partOff, sb, ag, selfRel)
	if err != nil {
		return allocSplit{}, err
	}
	lvl := int(be.Uint16(blk[4:]))
	numrecs := int(be.Uint16(blk[6:]))

	if lvl == 0 {
		maxRecs := allocMaxLeaf(len(blk), hdrSize)
		insertAt := numrecs
		for i := 0; i < numrecs; i++ {
			off := hdrSize + i*allocRecSize
			exStart := be.Uint32(blk[off:])
			exCount := be.Uint32(blk[off+4:])
			if allocLess(bnoSort, agStartRel, nBlocks, exStart, exCount) {
				insertAt = i
				break
			}
		}
		if numrecs < maxRecs {
			insertOff := hdrSize + insertAt*allocRecSize
			copy(blk[insertOff+allocRecSize:], blk[insertOff:hdrSize+numrecs*allocRecSize])
			copy(blk[insertOff:], rec)
			be.PutUint16(blk[6:], uint16(numrecs+1))
			return allocSplit{}, allocWriteAGBTree(rw, partOff, sb, ag, selfRel, blk)
		}
		return allocSplitLeaf(rw, partOff, sb, ag, selfRel, bnoSort, agStartRel, nBlocks, rec)
	}

	// Interior node: choose the last child whose key sorts <= the new record.
	maxInternal := allocMaxInternal(len(blk), hdrSize)
	ptrBase := hdrSize + maxInternal*allocKeySize
	child := 0
	for i := 0; i < numrecs; i++ {
		kOff := hdrSize + i*allocKeySize
		kStart := be.Uint32(blk[kOff:])
		kCount := be.Uint32(blk[kOff+4:])
		// child i covers the new record if the new record is NOT strictly less
		// than key i (i.e. key i <= new record in tree order).
		if !allocLess(bnoSort, agStartRel, nBlocks, kStart, kCount) {
			child = i
		} else {
			break
		}
	}
	childRel := be.Uint32(blk[ptrBase+child*allocPtrSize:])

	res, err := allocInsertInto(rw, partOff, sb, ag, childRel, agStartRel, nBlocks, bnoSort, rec)
	if err != nil {
		return allocSplit{}, err
	}
	if !res.split {
		return allocSplit{}, nil
	}

	insertAt := child + 1
	if numrecs < maxInternal {
		// Shift keys right from insertAt.
		copy(blk[hdrSize+(insertAt+1)*allocKeySize:], blk[hdrSize+insertAt*allocKeySize:hdrSize+numrecs*allocKeySize])
		be.PutUint32(blk[hdrSize+insertAt*allocKeySize:], res.rightKeyStart)
		be.PutUint32(blk[hdrSize+insertAt*allocKeySize+4:], res.rightKeyCount)
		// Shift pointers right from insertAt.
		copy(blk[ptrBase+(insertAt+1)*allocPtrSize:], blk[ptrBase+insertAt*allocPtrSize:ptrBase+numrecs*allocPtrSize])
		be.PutUint32(blk[ptrBase+insertAt*allocPtrSize:], res.rightRel)
		be.PutUint16(blk[6:], uint16(numrecs+1))
		return allocSplit{}, allocWriteAGBTree(rw, partOff, sb, ag, selfRel, blk)
	}
	return allocSplitNode(rw, partOff, sb, ag, selfRel, blk, insertAt, res.rightKeyStart, res.rightKeyCount, res.rightRel)
}

// allocSplitLeaf splits a full leaf in two and re-stitches the sibling chain.
// The lower half stays in selfRel, the upper half moves to a new right sibling;
// the new record (bnoSort-keyed by agStartRel/nBlocks) is merged in sorted
// order. Returns the right sibling's first record as the separator key.
//
// It allocates the right sibling FIRST (allocMetaAllocBlock), then RE-READS
// selfRel: the carve may have shrunk the longest free extent whose record lives
// in this very leaf, so the in-memory copy the caller passed could be stale.
// Re-reading and re-deriving the sorted record set from the fresh on-disk block
// keeps the split consistent with that edit.
func allocSplitLeaf(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, selfRel uint32, bnoSort bool, agStartRel, nBlocks uint32, rec []byte) (allocSplit, error) {
	be := binary.BigEndian
	hdrSize := sb.agBTreeHdrSize()

	rightAbs, err := allocMetaAllocBlock(rw, partOff, sb, ag, 1)
	if err != nil {
		return allocSplit{}, fmt.Errorf("xfs: alloc btree split: alloc right leaf: %w", err)
	}
	rightRel := uint32(rightAbs % uint64(sb.agBlocks))

	// Re-read selfRel after the carve, then rebuild the sorted record set.
	leaf, err := allocReadAGBlock(rw, partOff, sb, ag, selfRel)
	if err != nil {
		return allocSplit{}, err
	}
	numrecs := int(be.Uint16(leaf[6:]))

	insertAt := numrecs
	for i := 0; i < numrecs; i++ {
		off := hdrSize + i*allocRecSize
		if allocLess(bnoSort, agStartRel, nBlocks, be.Uint32(leaf[off:]), be.Uint32(leaf[off+4:])) {
			insertAt = i
			break
		}
	}

	total := numrecs + 1
	recs := make([][]byte, total)
	for i := 0; i < numrecs; i++ {
		r := make([]byte, allocRecSize)
		copy(r, leaf[hdrSize+i*allocRecSize:])
		recs[i] = r
	}
	nr := make([]byte, allocRecSize)
	copy(nr, rec)
	copy(recs[insertAt+1:], recs[insertAt:])
	recs[insertAt] = nr

	half := total / 2

	magic := be.Uint32(leaf[0:])

	oldRight := be.Uint32(leaf[12:])

	left := leaf
	be.PutUint32(left[12:], rightRel)
	be.PutUint16(left[6:], uint16(half))
	for i := 0; i < half; i++ {
		copy(left[hdrSize+i*allocRecSize:], recs[i])
	}
	for i := half; i < numrecs; i++ {
		clear(left[hdrSize+i*allocRecSize : hdrSize+(i+1)*allocRecSize])
	}
	if err := allocWriteAGBTree(rw, partOff, sb, ag, selfRel, left); err != nil {
		return allocSplit{}, err
	}

	right := newAllocBlock(sb, ag, rightRel, 0, total-half, magic)
	be.PutUint32(right[8:], selfRel)
	be.PutUint32(right[12:], oldRight)
	for i := half; i < total; i++ {
		copy(right[hdrSize+(i-half)*allocRecSize:], recs[i])
	}
	if err := allocWriteAGBTree(rw, partOff, sb, ag, rightRel, right); err != nil {
		return allocSplit{}, err
	}

	if oldRight != 0xFFFFFFFF {
		sib, err := allocReadAGBlock(rw, partOff, sb, ag, oldRight)
		if err != nil {
			return allocSplit{}, err
		}
		be.PutUint32(sib[8:], rightRel)
		if err := allocWriteAGBTree(rw, partOff, sb, ag, oldRight, sib); err != nil {
			return allocSplit{}, err
		}
	}

	// One new btree block was consumed by the split leaf.
	if err := allocBumpAGFBtreeBlocks(rw, partOff, sb, ag, 1); err != nil {
		return allocSplit{}, err
	}

	return allocSplit{
		split:         true,
		rightKeyStart: be.Uint32(recs[half][0:]),
		rightKeyCount: be.Uint32(recs[half][4:]),
		rightRel:      rightRel,
	}, nil
}

// allocSplitNode splits a full interior node in two. The full key/ptr set is the
// node's separators with (newKeyStart/Count → newPtr) inserted at insertAt; the
// lower half stays in selfRel, the upper half moves to a new right sibling.
// Returns the right sibling's first key as the separator.
func allocSplitNode(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, selfRel uint32, node []byte, insertAt int, newKeyStart, newKeyCount, newPtr uint32) (allocSplit, error) {
	be := binary.BigEndian
	hdrSize := sb.agBTreeHdrSize()
	numrecs := int(be.Uint16(node[6:]))
	maxInternal := allocMaxInternal(len(node), hdrSize)
	ptrBase := hdrSize + maxInternal*allocKeySize

	total := numrecs + 1
	keyStart := make([]uint32, total)
	keyCount := make([]uint32, total)
	ptrs := make([]uint32, total)
	for i := 0; i < numrecs; i++ {
		keyStart[i] = be.Uint32(node[hdrSize+i*allocKeySize:])
		keyCount[i] = be.Uint32(node[hdrSize+i*allocKeySize+4:])
		ptrs[i] = be.Uint32(node[ptrBase+i*allocPtrSize:])
	}
	copy(keyStart[insertAt+1:], keyStart[insertAt:])
	copy(keyCount[insertAt+1:], keyCount[insertAt:])
	copy(ptrs[insertAt+1:], ptrs[insertAt:])
	keyStart[insertAt] = newKeyStart
	keyCount[insertAt] = newKeyCount
	ptrs[insertAt] = newPtr

	half := total / 2

	rightAbs, err := allocMetaAllocBlock(rw, partOff, sb, ag, 1)
	if err != nil {
		return allocSplit{}, fmt.Errorf("xfs: alloc btree split: alloc right node: %w", err)
	}
	rightRel := uint32(rightAbs % uint64(sb.agBlocks))
	magic := be.Uint32(node[0:])
	blockLevel := int(be.Uint16(node[4:]))

	oldRight := be.Uint32(node[12:])

	left := node
	be.PutUint32(left[12:], rightRel)
	be.PutUint16(left[6:], uint16(half))
	for i := 0; i < half; i++ {
		be.PutUint32(left[hdrSize+i*allocKeySize:], keyStart[i])
		be.PutUint32(left[hdrSize+i*allocKeySize+4:], keyCount[i])
		be.PutUint32(left[ptrBase+i*allocPtrSize:], ptrs[i])
	}
	for i := half; i < numrecs; i++ {
		be.PutUint32(left[hdrSize+i*allocKeySize:], 0)
		be.PutUint32(left[hdrSize+i*allocKeySize+4:], 0)
		be.PutUint32(left[ptrBase+i*allocPtrSize:], 0)
	}
	if err := allocWriteAGBTree(rw, partOff, sb, ag, selfRel, left); err != nil {
		return allocSplit{}, err
	}

	right := newAllocBlock(sb, ag, rightRel, blockLevel, total-half, magic)
	be.PutUint32(right[8:], selfRel)
	be.PutUint32(right[12:], oldRight)
	for i := half; i < total; i++ {
		be.PutUint32(right[hdrSize+(i-half)*allocKeySize:], keyStart[i])
		be.PutUint32(right[hdrSize+(i-half)*allocKeySize+4:], keyCount[i])
		be.PutUint32(right[ptrBase+(i-half)*allocPtrSize:], ptrs[i])
	}
	if err := allocWriteAGBTree(rw, partOff, sb, ag, rightRel, right); err != nil {
		return allocSplit{}, err
	}

	if oldRight != 0xFFFFFFFF {
		sib, err := allocReadAGBlock(rw, partOff, sb, ag, oldRight)
		if err != nil {
			return allocSplit{}, err
		}
		be.PutUint32(sib[8:], rightRel)
		if err := allocWriteAGBTree(rw, partOff, sb, ag, oldRight, sib); err != nil {
			return allocSplit{}, err
		}
	}

	if err := allocBumpAGFBtreeBlocks(rw, partOff, sb, ag, 1); err != nil {
		return allocSplit{}, err
	}

	return allocSplit{
		split:         true,
		rightKeyStart: keyStart[half],
		rightKeyCount: keyCount[half],
		rightRel:      rightRel,
	}, nil
}

// newAllocBlock returns a zeroed v5 alloc-btree block with the given magic
// (AB3B/AB3C) and header filled in: level, numrecs, null siblings, self blkno,
// uuid and owner. Mirrors newInobtBlock for the free-space trees.
func newAllocBlock(sb *superblock, ag, agRel uint32, level, numrecs int, magic uint32) []byte {
	be := binary.BigEndian
	blk := make([]byte, sb.blockSize)
	be.PutUint32(blk[0:], magic)
	be.PutUint16(blk[4:], uint16(level))
	be.PutUint16(blk[6:], uint16(numrecs))
	be.PutUint32(blk[8:], 0xFFFFFFFF)
	be.PutUint32(blk[12:], 0xFFFFFFFF)
	be.PutUint64(blk[16:], sb.fsbToPhysBlock(sb.agAbsBlock(ag, agRel))*fmtDaddrPerBlock)
	copy(blk[32:48], sb.uuid[:])
	be.PutUint32(blk[48:], ag)
	return blk
}

// allocInsertWithReserve runs insertFn (a bno or cnt record insert into a tree
// of the given depth) with a pre-allocated split reserve installed on
// allocMetaAllocBlock. It pre-allocates level+1 contiguous blocks via allocBlocks
// while the trees are consistent — enough for one new node per level plus a new
// root — so any split insertFn triggers pops a pre-allocated block instead of
// allocating against a mid-rewrite tree. Unused blocks are freed back afterwards.
//
// If the reserve can't be allocated (a degenerate/tiny tree, or a unit test with
// no real free-space backing), insertFn still runs against whatever
// allocMetaAllocBlock is currently set to — single-leaf trees never split, and
// tests install their own deterministic hook.
//
// This wrapper lives between freeBlocks and the insert so allocInsertRecord
// itself never references freeBlocks (which would form a package-init cycle
// through the allocBnoInsertRecord/allocCntInsertRecord func-value hooks).
func allocInsertWithReserve(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, level int, insertFn func() error) (retErr error) {
	// Re-entrancy guard: a reserve's release frees its unused tail via freeBlocks,
	// which re-enters this wrapper. A nested call must NOT install another reserve
	// (that would recurse without bound). Inside a release the outer reserve's
	// already-allocated blocks back any split the nested free triggers — and the
	// nested free of a single contiguous tail run inserts at most one record, so it
	// rarely splits at all. When it does and the outer reserve is exhausted, the
	// default allocMetaBlock (→ allocBlocks) backs it; that allocation consumes
	// from the longest extent without inserting, so it does not recurse here.
	if allocReserveDepth > 0 {
		return insertFn()
	}
	// During a merged-free drain, do NOT install a reserve: the drain has already
	// redirected allocMetaAllocBlock to hand out blocks from the merged-free queue
	// (free metadata blocks), so a split a drained free triggers takes a queued
	// block and never carves from the free-space trees — which a reserve's
	// allocBlocks would do, re-entering the trees mid-rewrite (the deepest sub-case).
	if allocDraining {
		return insertFn()
	}
	reserve, rerr := allocReserveSplitBlocks(rw, partOff, sb, ag, level+1)
	if rerr == nil {
		prevHook := allocMetaAllocBlock
		allocMetaAllocBlock = reserve.alloc
		allocReserveDepth++
		defer func() {
			// Keep allocReserveDepth incremented across release so the nested
			// freeBlocks it calls does NOT install its own reserve; decrement only
			// once the tail is returned.
			allocMetaAllocBlock = prevHook
			if cerr := reserve.release(rw, partOff, sb, ag); cerr != nil && retErr == nil {
				retErr = cerr
			}
			allocReserveDepth--
		}()
	}
	return insertFn()
}

// allocReserveDepth guards against unbounded reserve re-entrancy when a reserve
// release frees its tail through freeBlocks (which re-enters
// allocInsertWithReserve). It is not concurrency-safe, matching the rest of this
// single-writer image builder (one *xfsFS mutates one image at a time).
var allocReserveDepth int

// allocMetaAllocBlock is the hook the split helpers use to obtain a fresh block
// for a new btree node. It defaults to allocMetaBlock, which carves one block
// from the longest free extent via the proven allocBlocks path. During a
// free-space-tree insert allocInsertRecord redirects it to a pre-allocated
// reserve (allocSplitReserve) so a split never allocates by reading a tree that
// is mid-rewrite. Tests override the hook to hand out deterministic blocks.
//
// Bound in init() rather than as a package-var initializer so it does not pull
// allocMetaBlock (→ allocBlocks → allocDrainMergedFrees → this var) into a var-
// init dependency cycle; allocDrainMergedFrees reassigns this hook while draining.
var allocMetaAllocBlock func(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, nBlocks uint32) (uint64, error)

func init() { allocMetaAllocBlock = allocMetaBlock }

// allocMetaBlock carves a single block for a btree node from allocation group
// ag's free space via allocBlocks. It is the default allocMetaAllocBlock hook,
// used when no split reserve is installed (e.g. the inobt root split, whose
// single-block allocation never itself splits the free-space trees).
func allocMetaBlock(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, nBlocks uint32) (uint64, error) {
	if nBlocks != 1 {
		return 0, fmt.Errorf("xfs: allocMetaBlock: only single-block btree-node allocations supported, got %d", nBlocks)
	}
	return allocBlocks(rw, partOff, sb, ag, 1)
}

// allocSplitReserve is a contiguous run of blocks pre-allocated for one
// free-space-tree insert. The run is allocated via allocBlocks BEFORE the insert
// begins (trees fully consistent), so a split — which must not re-enter the very
// trees it is rewriting — pops a pre-allocated block instead of allocating one
// against a mid-rewrite tree. After the insert, the unused tail is freed back.
//
// allocBlocks/freeBlocks correctly maintain both the bno and cnt trees
// (including the cnt resort a shrunk/grown extent needs), so the reserve never
// leaves the trees mis-sorted the way an in-place edit would.
type allocSplitReserve struct {
	runStart uint64 // absolute fsbno of the run start
	n        uint32 // blocks allocated
	used     uint32 // blocks handed out
}

// allocReserveSplitBlocks pre-allocates a contiguous run of n blocks for the
// split helpers. Allocating the run never splits a free-space tree (it consumes
// from the front of the longest extent: an in-place record update when a
// remainder is left, or a record delete when the extent is exactly consumed —
// neither inserts). On any allocation failure it returns an error and the caller
// falls back to the default block hook.
func allocReserveSplitBlocks(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, n int) (*allocSplitReserve, error) {
	if n <= 0 {
		return &allocSplitReserve{}, nil
	}
	runStart, err := allocBlocks(rw, partOff, sb, ag, uint32(n))
	if err != nil {
		return nil, fmt.Errorf("xfs: reserve %d split blocks ag=%d: %w", n, ag, err)
	}
	return &allocSplitReserve{runStart: runStart, n: uint32(n)}, nil
}

// alloc hands out the next pre-allocated block as an absolute filesystem block.
func (r *allocSplitReserve) alloc(_ readerWriterAt, _ int64, _ *superblock, ag uint32, nBlocks uint32) (uint64, error) {
	if nBlocks != 1 {
		return 0, fmt.Errorf("xfs: split reserve: only single-block allocations supported, got %d", nBlocks)
	}
	if r.used >= r.n {
		return 0, fmt.Errorf("xfs: split reserve ag=%d exhausted (%d blocks)", ag, r.n)
	}
	b := r.runStart + uint64(r.used)
	r.used++
	return b, nil
}

// release frees the unused tail of the reserve back to the free-space trees via
// freeBlocks. Because the tail is contiguous with (and immediately follows) the
// blocks actually consumed, this is a single free of the leftover run.
func (r *allocSplitReserve) release(rw readerWriterAt, partOff int64, sb *superblock, ag uint32) error {
	leftover := r.n - r.used
	if leftover == 0 {
		return nil
	}
	freeStart := r.runStart + uint64(r.used)
	return freeBlocks(rw, partOff, sb, freeStart, leftover)
}

// allocCntBubbleLeft restores cnt (count,start) order after the record at idx had
// its count reduced in place. It swaps the record leftward past any now-greater
// predecessor. Only a shrink is handled (the record can only move earlier), which
// is all the allocBlocks remainder update needs. It leaves the leaf's record
// count unchanged, so it cannot underflow the node.
func allocCntBubbleLeft(leaf []byte, hdrSize, idx int) {
	be := binary.BigEndian
	for i := idx; i > 0; i-- {
		cur := hdrSize + i*allocRecSize
		prev := cur - allocRecSize
		if !allocLess(false, be.Uint32(leaf[cur:]), be.Uint32(leaf[cur+4:]), be.Uint32(leaf[prev:]), be.Uint32(leaf[prev+4:])) {
			break
		}
		var tmp [allocRecSize]byte
		copy(tmp[:], leaf[cur:cur+allocRecSize])
		copy(leaf[cur:], leaf[prev:prev+allocRecSize])
		copy(leaf[prev:], tmp[:])
	}
}

// allocBumpBtreeBlocks adds delta to the AGF agf_btreeblks field in agfBuf
// (in-memory; the caller persists the AGF). agf_btreeblks counts blocks held by
// the two free-space btrees beyond their roots; xfs_repair cross-checks it.
func allocBumpBtreeBlocks(agfBuf []byte, delta int32) {
	be := binary.BigEndian
	cur := int32(be.Uint32(agfBuf[agfOffBtreeBlks:]))
	be.PutUint32(agfBuf[agfOffBtreeBlks:], uint32(cur+delta))
}

// allocBumpAGFBtreeBlocks reads the AGF, adds delta to agf_btreeblks, and writes
// it back. Used by the split helpers, which each consume one new btree block.
func allocBumpAGFBtreeBlocks(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, delta int32) error {
	agfBuf, err := allocAGFBlock(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	allocBumpBtreeBlocks(agfBuf, delta)
	return allocWriteAGF(rw, partOff, sb, ag, agfBuf)
}
