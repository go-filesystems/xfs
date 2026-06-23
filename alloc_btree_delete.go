package filesystem_xfs

import (
	"encoding/binary"
	"fmt"

	"github.com/go-volumes/safeio"
)

// alloc_btree_delete.go — full B+tree DELETE for the AGF free-space trees
// (bnobt, keyed by start block; cntbt, keyed by (count, start block)). It mirrors
// xfsprogs' xfs_btree_delrec → xfs_btree_join / xfs_btree_lshift / xfs_btree_rshift
// → xfs_btree_killroot:
//
//   - delete the record from the leaf;
//   - if a leaf (or, after a merge, an interior node) drops below the btree
//     minimum-record count, REBALANCE by moving a record from a sibling
//     (lshift/rshift) or MERGE two siblings (join), re-stitching the sibling
//     chain and propagating the removed pointer / changed separator key up the
//     parent;
//   - if the root ends up an interior node with a single child, COLLAPSE the root
//     (depth N → N-1) and free the emptied root block.
//
// The complement to alloc_btree.go's multi-level insert/split/root-growth, this
// gives the free-space trees full B+tree DELETE semantics so they shrink levels
// again. It applies to BOTH bno and cnt (a free extent lives in both, keyed
// differently); the caller in allocBlocks deletes from each in turn.
//
// On-disk records/keys/ptrs are big-endian (binary.BigEndian); the header
// sibling/level/numrecs fields are likewise BE (alloc-btree convention). The
// LE on-disk header fields (CRC etc.) are handled by allocWriteAGBTree.
//
// xfs minrecs for a short-form btree block is maxrecs/2 (XFS_BTREE_BLOCK_MINRECS).
// The root is exempt from minrecs (it may hold as few as 1 record, or — for a
// single-leaf tree — 0). A merge that empties the root collapses it.

// allocMinLeaf / allocMinInternal return the minimum live record counts a
// non-root leaf / interior alloc-btree block must keep. Below this the block is
// underfull and must rebalance or merge. xfs uses maxrecs/2.
func allocMinLeaf(blockLen, hdrSize int) int { return allocMaxLeaf(blockLen, hdrSize) / 2 }

func allocMinInternal(blockLen, hdrSize int) int { return allocMaxInternal(blockLen, hdrSize) / 2 }

// allocPathNode records one block visited descending root→leaf during a delete:
// the block's AG-relative number, the in-memory bytes, and the child-pointer
// index this descent followed out of it (childIdx is meaningful only for the
// interior nodes, i.e. every entry except the last/leaf).
type allocPathNode struct {
	rel      uint32
	blk      []byte
	childIdx int
}

// allocDeleteRecord deletes the free-extent record identified by agStartRel
// (bno tree: keyed by start; cnt tree: keyed by (nBlocks,start)) from the tree
// rooted at rootRel, then rebalances/merges underfull blocks and collapses the
// root if it loses its last separator. It updates the AGF's root/level for this
// tree and agf_btreeblks for any block a merge/collapse freed. agf_freeblks and
// agf_longest are the caller's responsibility (unchanged here).
//
// It is the structural replacement for the old in-place btreeDeleteRecord /
// bnoDeleteRecord, which decremented numrecs without ever rebalancing — leaving
// permanently underfull leaves and a tree that could never shrink a level.
func allocDeleteRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, agStartRel, nBlocks uint32, bnoSort bool) (retErr error) {
	// A standalone delete is its own operation boundary: queue the merged-away
	// blocks, then drain them once the structural delete has settled. allocBlocks'
	// whole-extent consume instead uses allocDeleteRecordCollect directly (deleting
	// from BOTH trees before any free) and relies on its own outer op-depth drain.
	allocOpDepth++
	defer func() {
		allocOpDepth--
		if allocOpDepth == 0 && retErr == nil {
			retErr = allocDrainMergedFrees(rw, partOff, sb)
		}
	}()
	freed, err := allocDeleteRecordCollect(rw, partOff, sb, ag, rootRel, level, agStartRel, nBlocks, bnoSort)
	if err != nil {
		return err
	}
	return allocFreeMergedBlocks(rw, partOff, sb, ag, freed)
}

// allocMergedFreeQueue holds (ag-relative) btree blocks a delete merged away,
// pending return to the free-space trees. Freeing inserts into both trees and so
// must NOT happen while an allocBlocks/freeBlocks operation is mid-flight (it
// would re-enter the very trees being mutated). Instead the blocks are queued and
// drained by allocDrainMergedFrees at the OUTERMOST operation boundary, when the
// trees are quiescent. Single-writer image builder → a plain slice + depth
// counter suffice (not concurrency-safe, like allocReserveDepth).
type allocMergedFree struct {
	ag  uint32
	rel uint32
}

var allocMergedFreeQueue []allocMergedFree

// allocOpDepth counts nested allocBlocks/freeBlocks operations so the merged-free
// queue is drained only at the outermost one (depth returns to 0), never mid-
// operation where a free would corrupt a tree still being rewritten.
var allocOpDepth int

// allocDraining is set while allocDrainMergedFrees is running so the freeBlocks it
// invokes (and any allocBlocks those trigger) do NOT recursively re-enter the
// drain from their own op-depth-0 boundary. The single in-flight drain loop owns
// the queue and re-checks it after every free, so blocks enqueued by a nested
// alloc during the drain are still picked up — without unbounded re-entrancy.
var allocDraining bool

// allocFreeMergedBlocks ENQUEUES each merged-away block for deferred freeing. The
// queue is drained at the outermost operation boundary (allocDrainMergedFrees),
// so a merge that happens deep inside an allocBlocks never re-enters the trees.
func allocFreeMergedBlocks(_ readerWriterAt, _ int64, _ *superblock, ag uint32, freed []uint32) error {
	for _, fb := range freed {
		allocMergedFreeQueue = append(allocMergedFreeQueue, allocMergedFree{ag: ag, rel: fb})
	}
	return nil
}

// allocDrainMergedFrees returns every queued merged-away btree block to the
// free-space trees. It is called only at the outermost allocBlocks/freeBlocks
// boundary, where both trees are consistent. Each free is a single-block insert
// (via the lazily-bound hook; see allocDeleteFreeBlocks). A free may itself merge
// or split and enqueue/realloc more blocks, so the loop re-checks the queue.
func allocDrainMergedFrees(rw readerWriterAt, partOff int64, sb *superblock) error {
	if allocDraining {
		return nil // a drain is already in flight; it owns the queue.
	}
	allocDraining = true
	defer func() { allocDraining = false }()

	// While draining, redirect the btree split-block hook so any split a free
	// triggers takes a block from the queue ITSELF (a free metadata block) instead
	// of carving from the free-space trees — which would re-enter allocBlocks
	// mid-rewrite (the deepest sub-case). A queued block reused this way stays a
	// btree block: it is removed from the queue, never freed back, and its
	// agf_btreeblks credit (debited when the merge freed it) is restored.
	prevHook := allocMetaAllocBlock
	allocMetaAllocBlock = func(_ readerWriterAt, _ int64, _ *superblock, ag uint32, nBlocks uint32) (uint64, error) {
		if nBlocks != 1 {
			return 0, fmt.Errorf("xfs: drain meta alloc: only single-block requests, got %d", nBlocks)
		}
		// Prefer a queued block in the SAME ag; fall back to any queued block.
		idx := -1
		for i := range allocMergedFreeQueue {
			if allocMergedFreeQueue[i].ag == ag {
				idx = i
				break
			}
		}
		if idx < 0 && len(allocMergedFreeQueue) > 0 {
			idx = 0
		}
		if idx < 0 {
			// Queue empty mid-split: fall back to the normal carve. This is rare (the
			// drain pops the block it is about to free first), and any merge it
			// triggers is suppressed (allocDraining), so it cannot recurse here.
			return prevHook(rw, partOff, sb, ag, nBlocks)
		}
		item := allocMergedFreeQueue[idx]
		allocMergedFreeQueue = append(allocMergedFreeQueue[:idx], allocMergedFreeQueue[idx+1:]...)
		// The block becomes a btree node again. The split helper that requested it
		// already bumps agf_btreeblks (+1) for the new node, and the merge that
		// queued it already debited agf_btreeblks (-1) when freeing it — so the net
		// is correct and we must NOT adjust agf_btreeblks here. The block was also
		// never returned to agf_freeblks (it was popped from the queue before its
		// freeBlocks ran), so the global free count stays consistent too.
		return sb.agAbsBlock(item.ag, item.rel), nil
	}
	defer func() { allocMetaAllocBlock = prevHook }()

	guard := safeio.NewLoopGuard(allocBTreeMaxSteps)
	for len(allocMergedFreeQueue) > 0 {
		if err := guard.Next(); err != nil {
			return fmt.Errorf("xfs: drain merged frees: %w", err)
		}
		item := allocMergedFreeQueue[0]
		allocMergedFreeQueue = allocMergedFreeQueue[1:]
		abs := sb.agAbsBlock(item.ag, item.rel)
		if err := allocDeleteFreeBlocks(rw, partOff, sb, abs, 1); err != nil {
			return fmt.Errorf("xfs: free merged btree block %d ag=%d: %w", item.rel, item.ag, err)
		}
	}
	return nil
}

// allocDeleteRecordCollect performs the structural delete (rebalance/merge/
// collapse + AGF root/level/btreeblks update) and RETURNS the AG-relative btree
// blocks a merge/collapse emptied, WITHOUT freeing them — so the caller controls
// when the free (which re-enters both free-space trees) happens.
func allocDeleteRecordCollect(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, agStartRel, nBlocks uint32, bnoSort bool) ([]uint32, error) {
	be := binary.BigEndian
	hdrSize := sb.agBTreeHdrSize()

	// Fragmentation gate (deepest sub-case bound). Merging frees a btree block back
	// to the trees, which may itself need to SPLIT and so pre-allocate a small
	// contiguous reserve from the AG's longest free extent. If the AG is so
	// fragmented that no extent ≥ 2 blocks exists, that reserve cannot be allocated
	// and the free would fall back to a re-entrant block carve that rewrites the
	// trees mid-operation — the one path that desynced bno/cnt. To stay strictly
	// correct, suppress merging (and thus block-freeing) when agf_longest < 2: the
	// delete still removes the record, leaving an underfull-but-valid leaf that
	// xfs_repair tolerates. (When draining merged frees, suppression is already on.)
	suppressMerge := allocDraining
	if !suppressMerge {
		if agfBuf, aerr := allocAGFBlock(rw, partOff, sb, ag); aerr == nil {
			if be.Uint32(agfBuf[agfOffLongest:]) < 2 {
				suppressMerge = true
			}
		}
	}

	// Descend root→leaf recording the path. At each interior node pick the last
	// child whose key sorts <= the target (same rule as insert/find).
	var path []allocPathNode
	curRel := rootRel
	for {
		blk, err := allocReadAGBlock(rw, partOff, sb, ag, curRel)
		if err != nil {
			return nil, err
		}
		lvl := int(be.Uint16(blk[4:]))
		numrecs := int(be.Uint16(blk[6:]))
		if lvl == 0 {
			path = append(path, allocPathNode{rel: curRel, blk: blk})
			break
		}
		maxInternal := allocMaxInternal(len(blk), hdrSize)
		ptrBase := hdrSize + maxInternal*allocKeySize
		child := 0
		for i := 0; i < numrecs; i++ {
			kOff := hdrSize + i*allocKeySize
			kStart := be.Uint32(blk[kOff:])
			kCount := be.Uint32(blk[kOff+4:])
			if !allocLess(bnoSort, agStartRel, nBlocks, kStart, kCount) {
				child = i
			} else {
				break
			}
		}
		path = append(path, allocPathNode{rel: curRel, blk: blk, childIdx: child})
		curRel = be.Uint32(blk[ptrBase+child*allocPtrSize:])
	}

	// Find the record in the leaf. For bno the key is start; for cnt the leaf may
	// hold several equal-(count) records so match on both start and count.
	leaf := &path[len(path)-1]
	numrecs := int(be.Uint16(leaf.blk[6:]))
	recIdx := -1
	for i := 0; i < numrecs; i++ {
		off := hdrSize + i*allocRecSize
		s := be.Uint32(leaf.blk[off:])
		c := be.Uint32(leaf.blk[off+4:])
		if s == agStartRel && (bnoSort || c == nBlocks) {
			recIdx = i
			break
		}
	}
	if recIdx < 0 {
		tree := "cnt"
		if bnoSort {
			tree = "bno"
		}
		return nil, fmt.Errorf("xfs: %s B-tree delete: start %d (count %d) not found in AG %d leaf %d", tree, agStartRel, nBlocks, ag, leaf.rel)
	}

	// Remove the record from the leaf in memory.
	allocRemoveRecAt(leaf.blk, hdrSize, recIdx, allocRecSize)

	// freedBlocks collects btree blocks emptied by a merge or root collapse; they
	// are returned to the free-space trees after the structural delete settles so
	// the free never re-enters a tree that is still mid-rewrite.
	var freedBlocks []uint32

	// Rebalance from the leaf up. dctx carries everything the helpers need.
	d := &allocDelCtx{
		rw: rw, partOff: partOff, sb: sb, ag: ag, hdrSize: hdrSize,
		bnoSort: bnoSort, be: be, freed: &freedBlocks,
		// Suppress merges when draining (to stop an unbounded
		// free→split→alloc→delete→merge cascade) or when the AG is too fragmented to
		// safely reabsorb a freed block (the fragmentation gate above).
		suppressMerge: suppressMerge,
	}
	if err := d.fixupFrom(path, len(path)-1); err != nil {
		return nil, err
	}

	// The leftmost separator key of an ancestor may have changed when the leaf's
	// first record was deleted (or shifted by a rebalance). Re-derive every
	// ancestor separator from its first child's first key, bottom-up, so interior
	// keys stay equal to the smallest key in their subtree (xfs_repair checks
	// this). fixupFrom already rewrote the structurally-changed blocks; this pass
	// only corrects separator drift on the surviving path.
	if err := d.refreshKeysAlongPath(path); err != nil {
		return nil, err
	}

	// Collapse the root while it is an interior node with a single child.
	newRootRel := rootRel
	newLevel := level
	for {
		rootBlk, err := allocReadAGBlock(rw, partOff, sb, ag, newRootRel)
		if err != nil {
			return nil, err
		}
		rootLvl := int(be.Uint16(rootBlk[4:]))
		rootNum := int(be.Uint16(rootBlk[6:]))
		if rootLvl == 0 || rootNum != 1 {
			break
		}
		maxInternal := allocMaxInternal(len(rootBlk), hdrSize)
		onlyChild := be.Uint32(rootBlk[hdrSize+maxInternal*allocKeySize:])
		freedBlocks = append(freedBlocks, newRootRel)
		newRootRel = onlyChild
		newLevel--
		// The promoted child becomes the root: its sibling pointers must be null.
		childBlk, err := allocReadAGBlock(rw, partOff, sb, ag, newRootRel)
		if err != nil {
			return nil, err
		}
		be.PutUint32(childBlk[8:], 0xFFFFFFFF)
		be.PutUint32(childBlk[12:], 0xFFFFFFFF)
		if err := allocWriteAGBTree(rw, partOff, sb, ag, newRootRel, childBlk); err != nil {
			return nil, err
		}
	}

	// Persist the (possibly new) root/level into the AGF, and account every freed
	// btree block in agf_btreeblks. Re-read so we don't clobber a field a sibling
	// step touched.
	if newRootRel != rootRel || newLevel != level || len(freedBlocks) > 0 {
		agfBuf, err := allocAGFBlock(rw, partOff, sb, ag)
		if err != nil {
			return nil, err
		}
		if bnoSort {
			be.PutUint32(agfBuf[agfOffBnoRoot:], newRootRel)
			be.PutUint32(agfBuf[agfOffBnoLevel:], uint32(newLevel))
		} else {
			be.PutUint32(agfBuf[agfOffCntRoot:], newRootRel)
			be.PutUint32(agfBuf[agfOffCntLevel:], uint32(newLevel))
		}
		allocBumpBtreeBlocks(agfBuf, -int32(len(freedBlocks)))
		if err := allocWriteAGF(rw, partOff, sb, ag, agfBuf); err != nil {
			return nil, err
		}
	}

	// The emptied btree blocks are returned to the caller to free once the whole
	// logical delete operation has settled (see allocDeleteRecord / allocBlocks):
	// freeing here, mid-operation, would re-enter both free-space trees and could
	// corrupt a sibling tree the same operation has not yet finished deleting from.
	return freedBlocks, nil
}

// allocCntRefreshLeafKey re-descends the alloc tree rooted at rootRel to the leaf
// block targetLeafRel and rewrites every ancestor separator key that points along
// that path to its child's current first key. It is used after an in-place leaf
// edit (the allocBlocks partial-consume cnt update) that may have changed the
// leaf's first record, so separator keys stay exact and the separator-key-
// navigating delete path stays accurate.
//
// The path to a specific leaf block is found by a bounded depth-first search over
// child pointers (NOT by separator-key comparison — the very thing that may have
// drifted), so it is robust to a stale separator. The tree is shallow and nodes
// hold few children, so the search is cheap.
func allocCntRefreshLeafKey(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, targetLeafRel uint32) error {
	be := binary.BigEndian
	hdrSize := sb.agBTreeHdrSize()
	if level <= 1 {
		return nil // single-leaf root: no separators to refresh.
	}

	// findPath returns the chain of (block rel, child index) from rootRel down to
	// the interior node whose child is the leaf, or false if not found.
	type step struct {
		rel      uint32
		childIdx int
	}
	guard := safeio.NewLoopGuard(allocBTreeMaxSteps)
	var search func(rel uint32) ([]step, bool, error)
	search = func(rel uint32) ([]step, bool, error) {
		if err := guard.Next(); err != nil {
			return nil, false, fmt.Errorf("xfs: cnt separator refresh ag=%d: %w", ag, err)
		}
		blk, err := allocReadAGBlock(rw, partOff, sb, ag, rel)
		if err != nil {
			return nil, false, err
		}
		if be.Uint16(blk[4:]) == 0 {
			return nil, false, nil // a leaf is never an interior we descend from
		}
		numrecs := int(be.Uint16(blk[6:]))
		ptrBase := hdrSize + allocMaxInternal(len(blk), hdrSize)*allocKeySize
		for i := 0; i < numrecs; i++ {
			child := be.Uint32(blk[ptrBase+i*allocPtrSize:])
			if child == targetLeafRel {
				return []step{{rel: rel, childIdx: i}}, true, nil
			}
			sub, ok, err := search(child)
			if err != nil {
				return nil, false, err
			}
			if ok {
				return append([]step{{rel: rel, childIdx: i}}, sub...), true, nil
			}
		}
		return nil, false, nil
	}

	path, ok, err := search(rootRel)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("xfs: cnt separator refresh ag=%d: leaf %d not reachable from root %d", ag, targetLeafRel, rootRel)
	}

	// Refresh separators bottom-up: each ancestor's separator for the descended
	// child becomes that child's first key.
	for i := len(path) - 1; i >= 0; i-- {
		parentRel := path[i].rel
		ci := path[i].childIdx
		parent, err := allocReadAGBlock(rw, partOff, sb, ag, parentRel)
		if err != nil {
			return err
		}
		childRel := be.Uint32(parent[hdrSize+allocMaxInternal(len(parent), hdrSize)*allocKeySize+ci*allocPtrSize:])
		child, err := allocReadAGBlock(rw, partOff, sb, ag, childRel)
		if err != nil {
			return err
		}
		// First key/record of the child (leaf record and interior key share layout).
		kStart := be.Uint32(child[hdrSize:])
		kCount := be.Uint32(child[hdrSize+4:])
		cur0 := be.Uint32(parent[hdrSize+ci*allocKeySize:])
		cur1 := be.Uint32(parent[hdrSize+ci*allocKeySize+4:])
		if cur0 == kStart && cur1 == kCount {
			continue
		}
		be.PutUint32(parent[hdrSize+ci*allocKeySize:], kStart)
		be.PutUint32(parent[hdrSize+ci*allocKeySize+4:], kCount)
		if err := allocWriteAGBTree(rw, partOff, sb, ag, parentRel, parent); err != nil {
			return err
		}
	}
	return nil
}

// allocDeleteFreeBlocks is the hook the delete path uses to return a merged-away
// btree block to the free-space trees. It is bound in init() (not a package-var
// initializer) so it does not participate in the var-init dependency graph that
// would otherwise form a cycle through freeBlocks → insert → allocBlocks → the
// delete hooks. Tests may override it.
var allocDeleteFreeBlocks func(rw readerWriterAt, partOff int64, sb *superblock, absStartBlock uint64, nBlocks uint32) error

func init() { allocDeleteFreeBlocks = freeBlocks }

// allocDelCtx bundles the immutable context the rebalance/merge helpers share so
// their signatures stay readable.
type allocDelCtx struct {
	rw            readerWriterAt
	partOff       int64
	sb            *superblock
	ag            uint32
	hdrSize       int
	bnoSort       bool
	be            binary.ByteOrder
	freed         *[]uint32
	suppressMerge bool
}

// fixupFrom rebalances the block at path[idx] if it went underfull, recursing up
// to its parent when a merge removes the parent's pointer. idx==0 is the root,
// which is exempt from minrecs (handled by the collapse loop in the caller).
func (d *allocDelCtx) fixupFrom(path []allocPathNode, idx int) error {
	node := &path[idx]
	be := d.be
	numrecs := int(be.Uint16(node.blk[6:]))
	isLeaf := be.Uint16(node.blk[4:]) == 0

	var minRecs int
	if isLeaf {
		minRecs = allocMinLeaf(len(node.blk), d.hdrSize)
	} else {
		minRecs = allocMinInternal(len(node.blk), d.hdrSize)
	}

	if idx == 0 {
		// Root: no minrecs. Just persist whatever in-memory edit happened.
		return d.write(node.rel, node.blk)
	}
	if numrecs >= minRecs || d.suppressMerge {
		// Full enough, or merging is suppressed for this delete: persist in place and
		// stop. A leaf left below minrecs is still a valid (if underfull) B-tree node
		// — xfs_repair tolerates underfull non-root blocks. suppressMerge is used only
		// while draining merged-block frees, where triggering further merges (and
		// thus further frees) would feed an unbounded free→split→alloc→delete→merge
		// cascade; the trees stay consistent, just occasionally underfull.
		return d.write(node.rel, node.blk)
	}

	parent := &path[idx-1]
	ci := parent.childIdx
	recSize := allocRecSize
	if !isLeaf {
		recSize = -1 // sentinel: interior uses key+ptr layout, handled in helpers
	}

	// Try borrowing from a sibling under the SAME parent. Prefer the right
	// sibling, then the left (xfs tries right then left in xfs_btree_delrec).
	if ci+1 <= int(be.Uint16(parent.blk[6:]))-1 {
		rightRel := d.childPtr(parent.blk, ci+1)
		moved, err := d.tryRshift(node, rightRel, isLeaf, recSize)
		if err != nil {
			return err
		}
		if moved {
			if err := d.write(node.rel, node.blk); err != nil {
				return err
			}
			// Parent separator for the right sibling changed (its first key moved).
			return d.refreshParentKey(parent, ci+1, rightRel)
		}
	}
	if ci-1 >= 0 {
		leftRel := d.childPtr(parent.blk, ci-1)
		moved, err := d.tryLshift(node, leftRel, isLeaf, recSize)
		if err != nil {
			return err
		}
		if moved {
			if err := d.write(node.rel, node.blk); err != nil {
				return err
			}
			// This node's own separator (its first key) changed.
			return d.refreshParentKey(parent, ci, node.rel)
		}
	}

	// No sibling can spare a record → MERGE. Persist node's in-memory state first
	// (its target record is already removed) so merge sees the post-delete record
	// set on disk; merge re-reads the blocks it joins.
	if err := d.write(node.rel, node.blk); err != nil {
		return err
	}
	// Join with the right sibling if there is one (keep node, absorb right), else
	// join into the left sibling.
	if ci+1 <= int(be.Uint16(parent.blk[6:]))-1 {
		rightRel := d.childPtr(parent.blk, ci+1)
		if err := d.merge(node.rel, node.blk, rightRel, isLeaf); err != nil {
			return err
		}
		// The parent loses the pointer/key for the right sibling at ci+1.
		allocRemoveKeyPtrAt(parent.blk, d.hdrSize, ci+1, allocMaxInternal(len(parent.blk), d.hdrSize))
	} else {
		// No right sibling: this node is the parent's last child. Merge it into its
		// left sibling, then the parent loses THIS node's pointer at ci.
		leftRel := d.childPtr(parent.blk, ci-1)
		leftBlk, err := allocReadAGBlock(d.rw, d.partOff, d.sb, d.ag, leftRel)
		if err != nil {
			return err
		}
		if err := d.merge(leftRel, leftBlk, node.rel, isLeaf); err != nil {
			return err
		}
		allocRemoveKeyPtrAt(parent.blk, d.hdrSize, ci, allocMaxInternal(len(parent.blk), d.hdrSize))
	}
	be.PutUint16(parent.blk[6:], uint16(int(be.Uint16(parent.blk[6:]))-1))

	// Recurse: the parent may now be underfull.
	return d.fixupFrom(path, idx-1)
}

// childPtr returns the i-th child pointer of an interior block.
func (d *allocDelCtx) childPtr(blk []byte, i int) uint32 {
	ptrBase := d.hdrSize + allocMaxInternal(len(blk), d.hdrSize)*allocKeySize
	return d.be.Uint32(blk[ptrBase+i*allocPtrSize:])
}

// write persists a block via the btree writer (CRC etc.).
func (d *allocDelCtx) write(rel uint32, blk []byte) error {
	return allocWriteAGBTree(d.rw, d.partOff, d.sb, d.ag, rel, blk)
}

// tryRshift moves the right sibling's FIRST record into node's tail if the right
// sibling can spare one (stays >= minrecs). Returns whether a record moved.
func (d *allocDelCtx) tryRshift(node *allocPathNode, rightRel uint32, isLeaf bool, _ int) (bool, error) {
	be := d.be
	right, err := allocReadAGBlock(d.rw, d.partOff, d.sb, d.ag, rightRel)
	if err != nil {
		return false, err
	}
	rNum := int(be.Uint16(right[6:]))
	var rMin int
	if isLeaf {
		rMin = allocMinLeaf(len(right), d.hdrSize)
	} else {
		rMin = allocMinInternal(len(right), d.hdrSize)
	}
	if rNum-1 < rMin {
		return false, nil
	}
	nNum := int(be.Uint16(node.blk[6:]))
	if isLeaf {
		// Append right[0] to node, then drop right[0].
		src := d.hdrSize
		dst := d.hdrSize + nNum*allocRecSize
		copy(node.blk[dst:dst+allocRecSize], right[src:src+allocRecSize])
		allocRemoveRecAt(right, d.hdrSize, 0, allocRecSize)
	} else {
		// Interior: append (right key[0], right ptr[0]) to node.
		maxN := allocMaxInternal(len(node.blk), d.hdrSize)
		nPtrBase := d.hdrSize + maxN*allocKeySize
		rMaxN := allocMaxInternal(len(right), d.hdrSize)
		rPtrBase := d.hdrSize + rMaxN*allocKeySize
		copy(node.blk[d.hdrSize+nNum*allocKeySize:d.hdrSize+nNum*allocKeySize+allocKeySize], right[d.hdrSize:d.hdrSize+allocKeySize])
		be.PutUint32(node.blk[nPtrBase+nNum*allocPtrSize:], be.Uint32(right[rPtrBase:]))
		allocRemoveKeyPtrAt(right, d.hdrSize, 0, rMaxN)
	}
	be.PutUint16(node.blk[6:], uint16(nNum+1))
	be.PutUint16(right[6:], uint16(rNum-1))
	return true, d.write(rightRel, right)
}

// tryLshift moves the left sibling's LAST record into node's head if the left
// sibling can spare one. Returns whether a record moved.
func (d *allocDelCtx) tryLshift(node *allocPathNode, leftRel uint32, isLeaf bool, _ int) (bool, error) {
	be := d.be
	left, err := allocReadAGBlock(d.rw, d.partOff, d.sb, d.ag, leftRel)
	if err != nil {
		return false, err
	}
	lNum := int(be.Uint16(left[6:]))
	var lMin int
	if isLeaf {
		lMin = allocMinLeaf(len(left), d.hdrSize)
	} else {
		lMin = allocMinInternal(len(left), d.hdrSize)
	}
	if lNum-1 < lMin {
		return false, nil
	}
	nNum := int(be.Uint16(node.blk[6:]))
	if isLeaf {
		// Shift node's records right by one, then prepend left's last.
		copy(node.blk[d.hdrSize+allocRecSize:], node.blk[d.hdrSize:d.hdrSize+nNum*allocRecSize])
		src := d.hdrSize + (lNum-1)*allocRecSize
		copy(node.blk[d.hdrSize:d.hdrSize+allocRecSize], left[src:src+allocRecSize])
		clear(left[src : src+allocRecSize])
	} else {
		maxN := allocMaxInternal(len(node.blk), d.hdrSize)
		nPtrBase := d.hdrSize + maxN*allocKeySize
		lMaxN := allocMaxInternal(len(left), d.hdrSize)
		lPtrBase := d.hdrSize + lMaxN*allocKeySize
		// Shift node's keys/ptrs right by one.
		copy(node.blk[d.hdrSize+allocKeySize:], node.blk[d.hdrSize:d.hdrSize+nNum*allocKeySize])
		copy(node.blk[nPtrBase+allocPtrSize:], node.blk[nPtrBase:nPtrBase+nNum*allocPtrSize])
		// Prepend left's last key/ptr.
		copy(node.blk[d.hdrSize:d.hdrSize+allocKeySize], left[d.hdrSize+(lNum-1)*allocKeySize:d.hdrSize+lNum*allocKeySize])
		be.PutUint32(node.blk[nPtrBase:], be.Uint32(left[lPtrBase+(lNum-1)*allocPtrSize:]))
		// Clear left's vacated slot.
		clear(left[d.hdrSize+(lNum-1)*allocKeySize : d.hdrSize+lNum*allocKeySize])
		be.PutUint32(left[lPtrBase+(lNum-1)*allocPtrSize:], 0)
	}
	be.PutUint16(node.blk[6:], uint16(nNum+1))
	be.PutUint16(left[6:], uint16(lNum-1))
	return true, d.write(leftRel, left)
}

// merge joins the block at rightRel into the block at leftRel (left absorbs
// right's records/keys/ptrs), re-stitches the sibling chain to skip right, frees
// right's block, and persists left. Both blocks are at the same level.
func (d *allocDelCtx) merge(leftRel uint32, left []byte, rightRel uint32, isLeaf bool) error {
	be := d.be
	right, err := allocReadAGBlock(d.rw, d.partOff, d.sb, d.ag, rightRel)
	if err != nil {
		return err
	}
	lNum := int(be.Uint16(left[6:]))
	rNum := int(be.Uint16(right[6:]))

	if isLeaf {
		for i := 0; i < rNum; i++ {
			src := d.hdrSize + i*allocRecSize
			dst := d.hdrSize + (lNum+i)*allocRecSize
			copy(left[dst:dst+allocRecSize], right[src:src+allocRecSize])
		}
	} else {
		lMaxN := allocMaxInternal(len(left), d.hdrSize)
		lPtrBase := d.hdrSize + lMaxN*allocKeySize
		rMaxN := allocMaxInternal(len(right), d.hdrSize)
		rPtrBase := d.hdrSize + rMaxN*allocKeySize
		for i := 0; i < rNum; i++ {
			copy(left[d.hdrSize+(lNum+i)*allocKeySize:d.hdrSize+(lNum+i+1)*allocKeySize], right[d.hdrSize+i*allocKeySize:d.hdrSize+(i+1)*allocKeySize])
			be.PutUint32(left[lPtrBase+(lNum+i)*allocPtrSize:], be.Uint32(right[rPtrBase+i*allocPtrSize:]))
		}
	}
	be.PutUint16(left[6:], uint16(lNum+rNum))

	// Re-stitch the sibling chain: left.rsib = right.rsib, and right.rsib.lsib =
	// left (if it exists).
	rRsib := be.Uint32(right[12:])
	be.PutUint32(left[12:], rRsib)
	if err := d.write(leftRel, left); err != nil {
		return err
	}
	if rRsib != 0xFFFFFFFF {
		nxt, err := allocReadAGBlock(d.rw, d.partOff, d.sb, d.ag, rRsib)
		if err != nil {
			return err
		}
		be.PutUint32(nxt[8:], leftRel)
		if err := d.write(rRsib, nxt); err != nil {
			return err
		}
	}
	*d.freed = append(*d.freed, rightRel)
	return nil
}

// refreshParentKey rewrites the separator key for child ci in parent to match
// that child's current first key, then persists the parent.
func (d *allocDelCtx) refreshParentKey(parent *allocPathNode, ci int, childRel uint32) error {
	be := d.be
	child, err := allocReadAGBlock(d.rw, d.partOff, d.sb, d.ag, childRel)
	if err != nil {
		return err
	}
	kStart, kCount := d.firstKey(child)
	be.PutUint32(parent.blk[d.hdrSize+ci*allocKeySize:], kStart)
	be.PutUint32(parent.blk[d.hdrSize+ci*allocKeySize+4:], kCount)
	return d.write(parent.rel, parent.blk)
}

// firstKey returns a block's first separator key: for a leaf its first record's
// {start,count}, for an interior node its first stored key.
func (d *allocDelCtx) firstKey(blk []byte) (uint32, uint32) {
	be := d.be
	// Leaf record and interior key share the same {u32,u32} layout at hdrSize.
	return be.Uint32(blk[d.hdrSize:]), be.Uint32(blk[d.hdrSize+4:])
}

// refreshKeysAlongPath re-derives every interior separator key on the surviving
// descent path from its child's current first key, bottom-up, so an ancestor key
// stays equal to the smallest key reachable through its subtree even after the
// leaf's first record was deleted or shifted. It re-reads each block from disk so
// it reflects any rebalance/merge fixupFrom already wrote.
func (d *allocDelCtx) refreshKeysAlongPath(path []allocPathNode) error {
	be := d.be
	for i := len(path) - 2; i >= 0; i-- {
		parentRel := path[i].rel
		parent, err := allocReadAGBlock(d.rw, d.partOff, d.sb, d.ag, parentRel)
		if err != nil {
			return err
		}
		ci := path[i].childIdx
		// The child the path descended through may have merged away; clamp ci to
		// the parent's live record count so we refresh an existing separator.
		pNum := int(be.Uint16(parent[6:]))
		if pNum == 0 {
			continue
		}
		if ci >= pNum {
			ci = pNum - 1
		}
		childRel := d.childPtr(parent, ci)
		child, err := allocReadAGBlock(d.rw, d.partOff, d.sb, d.ag, childRel)
		if err != nil {
			return err
		}
		kStart, kCount := d.firstKey(child)
		cur0 := be.Uint32(parent[d.hdrSize+ci*allocKeySize:])
		cur1 := be.Uint32(parent[d.hdrSize+ci*allocKeySize+4:])
		if cur0 == kStart && cur1 == kCount {
			continue
		}
		be.PutUint32(parent[d.hdrSize+ci*allocKeySize:], kStart)
		be.PutUint32(parent[d.hdrSize+ci*allocKeySize+4:], kCount)
		if err := d.write(parentRel, parent); err != nil {
			return err
		}
	}
	return nil
}

// allocRemoveRecAt deletes the record at idx from a leaf block (shift left, zero
// the vacated tail slot) WITHOUT touching numrecs — the caller decrements it, or
// it is left to fixupFrom which reads numrecs from the block before the helper.
// Here it DOES decrement numrecs so a stand-alone leaf delete is self-contained.
func allocRemoveRecAt(blk []byte, hdrSize, idx, recSize int) {
	be := binary.BigEndian
	numrecs := int(be.Uint16(blk[6:]))
	start := hdrSize + idx*recSize
	copy(blk[start:], blk[start+recSize:hdrSize+numrecs*recSize])
	last := hdrSize + (numrecs-1)*recSize
	clear(blk[last : last+recSize])
	be.PutUint16(blk[6:], uint16(numrecs-1))
}

// allocRemoveKeyPtrAt deletes the (key,ptr) pair at idx from an interior block:
// shift the key array and the ptr array left over idx, zero the vacated tail
// slots. It does NOT touch numrecs (the caller adjusts it once).
func allocRemoveKeyPtrAt(blk []byte, hdrSize, idx, maxInternal int) {
	be := binary.BigEndian
	numrecs := int(be.Uint16(blk[6:]))
	ptrBase := hdrSize + maxInternal*allocKeySize
	// Keys.
	kStart := hdrSize + idx*allocKeySize
	copy(blk[kStart:], blk[kStart+allocKeySize:hdrSize+numrecs*allocKeySize])
	clear(blk[hdrSize+(numrecs-1)*allocKeySize : hdrSize+numrecs*allocKeySize])
	// Ptrs.
	pStart := ptrBase + idx*allocPtrSize
	copy(blk[pStart:], blk[pStart+allocPtrSize:ptrBase+numrecs*allocPtrSize])
	clear(blk[ptrBase+(numrecs-1)*allocPtrSize : ptrBase+numrecs*allocPtrSize])
}
