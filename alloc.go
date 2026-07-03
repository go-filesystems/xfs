package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/go-volumes/safeio"
)

// allocBTreeMaxSteps is the global ceiling on the number of blocks any single
// AGF free-space B-tree traversal (descent + leaf-sibling walk) may visit
// before it is treated as corrupt. A real tree is far smaller; this only fires
// on a forged cyclic or pathologically deep chain (H3).
const allocBTreeMaxSteps = 1 << 20

// errInobtFull is returned by inobtFindFree when every record in the inobt
// has freecount==0. allocInode uses this sentinel to decide whether to
// trigger growInobt — other errors (I/O, corruption) propagate as-is so
// the caller can distinguish "out of inodes, grow the chunk pool" from
// "the on-disk inobt is unreadable".
var errInobtFull = errors.New("xfs: inobt has no free inodes")

// AGF v5 field offsets (big-endian unless noted). Layout mirrors
// struct xfs_agf from fs/xfs/libxfs/xfs_format.h.
const (
	agfOffMagic     = 0
	agfOffVersion   = 4 // agf_versionnum (= 1)
	agfOffSeqNo     = 8
	agfOffLength    = 12  // AG size in blocks
	agfOffBnoRoot   = 16  // agf_roots[0]: bno B-tree root (AG-relative block)
	agfOffCntRoot   = 20  // agf_roots[1]: cnt B-tree root
	agfOffBnoLevel  = 28  // agf_levels[0]: bno B-tree depth
	agfOffCntLevel  = 32  // agf_levels[1]: cnt B-tree depth
	agfOffFLFirst   = 40  // first valid AGFL index
	agfOffFLLast    = 44  // last valid AGFL index
	agfOffFLCount   = 48  // number of blocks in the free list
	agfOffFreeBlks  = 52  // free block count
	agfOffLongest   = 56  // longest free run
	agfOffBtreeBlks = 60  // blocks held by the free-space btrees
	agfOffUUID      = 64  // sb_uuid (16 bytes)
	agfOffCRC       = 216 // __le32 v5 CRC (covers the sector); agf_crc field after agf_lsn@208
	agfStructSize   = 224
)

// AGI v5 field offsets. Layout mirrors struct xfs_agi.
const (
	agiOffMagic     = 0
	agiOffVersion   = 4 // agi_versionnum (= 1)
	agiOffSeqNo     = 8
	agiOffLength    = 12
	agiOffCount     = 16  // allocated inode count
	agiOffRoot      = 20  // inobt root block (AG-relative)
	agiOffLevel     = 24  // inobt depth
	agiOffFreeCount = 28  // free inode count
	agiOffNewIno    = 32  // last allocated inode (AG-relative)
	agiOffDirIno    = 36  // agi_dirino (unused; null = 0xFFFFFFFF)
	agiOffUUID      = 296 // sb_uuid (16 bytes); after agi_unlinked[64]
	agiOffCRC       = 312 // __le32 v5 CRC; agi_crc field
	agiStructSize   = 344
)

// AGFL v5 field offsets (the free-list block at sector 3 of each AG).
// Layout mirrors struct xfs_agfl.
const (
	aglOffMagic = 0
	aglOffSeqNo = 4
	aglOffUUID  = 8  // sb_uuid (16 bytes)
	aglOffLSN   = 24 // __be64
	aglOffCRC   = 32 // __le32
	aglOffBno   = 36 // __be32 bno[]; (sectorSize-36)/4 entries
)

// B-tree block header sizes.
const (
	btreeHdrSizeV5 = 56 // short-form v5: magic+level+numrecs+leftsib+rightsib+blkno+lsn+uuid+owner+crc
	btreeHdrSizeV4 = 16 // short-form v4: magic+level+numrecs+leftsib+rightsib
	btreeCRCOff    = 52 // __le32, within btreeHdrSizeV5
)

// Inobt leaf record (16 bytes).
const (
	inobtRecSize = 16
	inobtKeySize = 4
	inobtPtrSize = 4
	allocRecSize = 8 // bno/cnt B-tree leaf record: startblock(4)+count(4)
	allocKeySize = 8
	allocPtrSize = 4
)

var allocAGFBlock = agfBlock
var allocWriteAGF = writeAGF
var allocAGIBlock = agiBlock
var allocWriteAGI = writeAGI
var allocReadAGBlock = readAGBlock
var allocWriteAGBTree = writeAGBTree
var allocCntFindBlock = cntFindBlock
var allocBtreeDeleteRecord = btreeDeleteRecord
var allocBnoDeleteRecord = bnoDeleteRecord
var allocBnoUpdateRecord = bnoUpdateRecord
var allocBnoFindRecord = bnoFindRecord
var allocBnoInsertRecord = bnoInsertRecord
var allocCntInsertRecord = cntInsertRecord
var allocInobtFindFree = inobtFindFree
var allocInobtFindRecord = inobtFindRecord
var allocGrowInobt = growInobt
var allocAllocBlocks = allocBlocks
var allocWriteRawBlock = writeRawBlock
var allocFreeBlocks = freeBlocks
var allocRecomputeLongest = agfRecomputeLongest

// inobtChunkInodes is the number of inodes per inobt record. The XFS spec
// always uses 64-inode chunks (the irFree bitmap is 64 bits wide). With
// blockSize=4096 and inopBlock=8 (inodeSize=512), a chunk occupies 8
// filesystem blocks. growInobt() always allocates an 8-block run and
// initialises all 64 inode slots so xfs_repair sees a well-formed chunk.
const inobtChunkInodes = 64

// agfBlock reads the AGF block for allocation group ag.
func agfBlock(r io.ReaderAt, partOff int64, sb *superblock, ag uint32) ([]byte, error) {
	buf := make([]byte, sb.sectSize())
	off := sb.agFByteOffset(partOff, ag)
	if err := readBytes(r, off, buf); err != nil {
		return nil, fmt.Errorf("xfs: read AGF ag=%d: %w", ag, err)
	}
	magic := binary.BigEndian.Uint32(buf[agfOffMagic:])
	if magic != magicAGF {
		return nil, fmt.Errorf("xfs: AG %d bad AGF magic 0x%08X", ag, magic)
	}
	return buf, nil
}

// writeAGF writes back a modified AGF block, updating its CRC if v5.
func writeAGF(rw io.WriterAt, partOff int64, sb *superblock, ag uint32, buf []byte) error {
	if sb.hasCRC {
		updateCRC(buf, agfOffCRC, 512)
	}
	off := sb.agFByteOffset(partOff, ag)
	if _, err := rw.WriteAt(buf, off); err != nil {
		return fmt.Errorf("xfs: write AGF ag=%d: %w", ag, err)
	}
	return nil
}

// agiBlock reads the AGI block for allocation group ag.
func agiBlock(r io.ReaderAt, partOff int64, sb *superblock, ag uint32) ([]byte, error) {
	buf := make([]byte, sb.sectSize())
	off := sb.agIByteOffset(partOff, ag)
	if err := readBytes(r, off, buf); err != nil {
		return nil, fmt.Errorf("xfs: read AGI ag=%d: %w", ag, err)
	}
	magic := binary.BigEndian.Uint32(buf[agiOffMagic:])
	if magic != magicAGI {
		return nil, fmt.Errorf("xfs: AG %d bad AGI magic 0x%08X", ag, magic)
	}
	return buf, nil
}

// writeAGI writes back a modified AGI block.
func writeAGI(rw io.WriterAt, partOff int64, sb *superblock, ag uint32, buf []byte) error {
	if sb.hasCRC {
		updateCRC(buf, agiOffCRC, 512)
	}
	off := sb.agIByteOffset(partOff, ag)
	if _, err := rw.WriteAt(buf, off); err != nil {
		return fmt.Errorf("xfs: write AGI ag=%d: %w", ag, err)
	}
	return nil
}

// agAbsBlock converts an AG number + AG-relative block to a packed on-disk
// XFS filesystem block number (fsbno = (agno<<agblklog)|agbno). This is the
// value stored in on-disk extent/bmbt records and B-tree pointers; translate it
// with fsbToPhysBlock before computing a byte offset or a 512-byte daddr.
// (When sb_agblocks is a power of two — as our own Format produces — packing is
// numerically identical to a flat block index.)
func (sb *superblock) agAbsBlock(ag, agRel uint32) uint64 {
	return uint64(ag)<<uint64(sb.agBlkLog) | uint64(agRel)
}

// syncSuperblockCounts recomputes the filesystem-wide free counters
// (sb_icount, sb_ifree, sb_fdblocks) from the per-AG AGI/AGF headers and
// rewrites the primary superblock. Allocation only updates the per-AG headers;
// without this the global SB counts drift and xfs_repair reports
// "sb_ifree X, counted Y" / "sb_fdblocks ..." and exits non-zero. Call after
// any operation that allocates or frees inodes or blocks.
//
// sb_fdblocks counts free-space-btree blocks plus the AGFL free-list blocks
// (agf_flcount), matching how XFS and xfs_repair account for free space.
func syncSuperblockCounts(rw readerWriterAt, partOff int64, sb *superblock) error {
	be := binary.BigEndian
	var icount, ifree, fdblocks uint64
	for ag := uint32(0); ag < sb.agCount; ag++ {
		agf, err := agfBlock(rw, partOff, sb, ag)
		if err != nil {
			return err
		}
		agi, err := agiBlock(rw, partOff, sb, ag)
		if err != nil {
			return err
		}
		// sb_fdblocks counts genuinely-free blocks (agf_freeblks) plus the AGFL
		// free-list blocks (agf_flcount) plus the free-space-btree blocks
		// (agf_btreeblks) — the last because those blocks return to the free pool
		// when the bno/cnt trees shrink, so XFS/xfs_repair include them in the
		// global free count. Omitting agf_btreeblks left sb_fdblocks low by the
		// number of blocks the multi-level free-space-tree splits consumed
		// ("sb_fdblocks X, counted X+btreeblks").
		fdblocks += uint64(be.Uint32(agf[agfOffFreeBlks:])) +
			uint64(be.Uint32(agf[agfOffFLCount:])) +
			uint64(be.Uint32(agf[agfOffBtreeBlks:]))
		icount += uint64(be.Uint32(agi[agiOffCount:]))
		ifree += uint64(be.Uint32(agi[agiOffFreeCount:]))
	}

	buf := make([]byte, 512)
	if err := readBytes(rw, partOff, buf); err != nil {
		return fmt.Errorf("xfs: sync SB counts: read SB: %w", err)
	}
	be.PutUint64(buf[sbOffIcount:], icount)
	be.PutUint64(buf[sbOffIfree:], ifree)
	be.PutUint64(buf[sbOffFdblocks:], fdblocks)
	if sb.hasCRC {
		updateCRC(buf, sbOffCRC, sbCRCLen)
	}
	if _, err := rw.WriteAt(buf, partOff); err != nil {
		return fmt.Errorf("xfs: sync SB counts: write SB: %w", err)
	}
	return nil
}

// readAGBlock reads an AG-relative block by converting to absolute first.
func readAGBlock(r io.ReaderAt, partOff int64, sb *superblock, ag, agRel uint32) ([]byte, error) {
	return readRawBlock(r, partOff, sb, sb.agAbsBlock(ag, agRel))
}

// writeAGBlock writes an AG-relative block, updating the v5 B-tree CRC.
func writeAGBTree(rw io.WriterAt, partOff int64, sb *superblock, ag, agRel uint32, blk []byte) error {
	if sb.hasCRC {
		updateCRC(blk, btreeCRCOff, len(blk))
	}
	absBlock := sb.agAbsBlock(ag, agRel)
	return writeRawBlock(rw, partOff, sb, absBlock, blk)
}

// agBTreeHdrSize returns the B-tree block header size.
func (sb *superblock) agBTreeHdrSize() int {
	return btreeHdrSize(sb.hasCRC)
}

// inobtMaxInternal returns the maximum number of key/ptr pairs an interior
// inobt block of length blockLen (with header hdrSize) can hold. XFS short-form
// btree blocks size the key and pointer arrays to this maxrecs, so an interior
// node's pointer array always begins at hdrSize + maxInternal*inobtKeySize —
// independent of how many records the node currently holds.
func inobtMaxInternal(blockLen, hdrSize int) int {
	return (blockLen - hdrSize) / (inobtKeySize + inobtPtrSize)
}

// ──────────────────── Block allocation ─────────────────────────────────────

// allocBlocks allocates nBlocks contiguous blocks from allocation group ag
// using the cnt (free-space-by-count) B-tree. Returns the absolute
// filesystem block number of the first allocated block.
//
// Strategy: walk the cnt B-tree to the rightmost leaf (largest extents)
// and take the first record with count >= nBlocks. This is a simplified
// allocator; it handles images with ≤1 level-deep trees (typical for
// newly formatted cloud images with a single large free extent).
func allocBlocks(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, nBlocks uint32) (retBlk uint64, retErr error) {
	// Enter an alloc/free operation. Merged-away btree blocks from any delete this
	// triggers are queued (allocFreeMergedBlocks) and drained only when the
	// OUTERMOST operation finishes — never mid-flight, where freeing them would
	// re-enter the trees still being rewritten. See allocDrainMergedFrees.
	allocOpDepth++
	defer func() {
		allocOpDepth--
		if allocOpDepth == 0 && retErr == nil {
			retErr = allocDrainMergedFrees(rw, partOff, sb)
		}
	}()

	agfBuf, err := allocAGFBlock(rw, partOff, sb, ag)
	if err != nil {
		return 0, err
	}
	be := binary.BigEndian

	cntRoot := be.Uint32(agfBuf[agfOffCntRoot:])
	cntLevel := be.Uint32(agfBuf[agfOffCntLevel:])
	bnoRoot := be.Uint32(agfBuf[agfOffBnoRoot:])
	bnoLevel := be.Uint32(agfBuf[agfOffBnoLevel:])

	// Navigate cnt B-tree to a leaf containing a large enough extent.
	cntLeafRel, cntLeaf, recIdx, err := allocCntFindBlock(rw, partOff, sb, ag, cntRoot, int(cntLevel), nBlocks)
	if err != nil {
		return 0, fmt.Errorf("xfs: allocBlocks ag=%d: %w", ag, err)
	}

	hdrSize := sb.agBTreeHdrSize()
	recOff := hdrSize + recIdx*allocRecSize
	startRel := be.Uint32(cntLeaf[recOff:])
	count := be.Uint32(cntLeaf[recOff+4:])

	if count < nBlocks {
		return 0, fmt.Errorf("xfs: allocBlocks ag=%d: largest free extent %d < %d", ag, count, nBlocks)
	}

	// Determine allocation: take the first nBlocks from this extent.
	allocStartRel := startRel
	remaining := count - nBlocks

	if remaining == 0 {
		// Fully consume the extent: delete its record from BOTH free-space trees
		// with full B+tree semantics — rebalance/merge an underfull leaf or interior
		// node and collapse the root when it loses its last child (alloc_btree_delete
		// .go). The cnt delete is keyed by (count,start); the bno delete by start.
		// _ = recIdx/cntLeaf/cntLeafRel: the structural delete re-descends from the
		// root to record the path it needs for rebalancing.
		_ = recIdx
		_ = cntLeaf
		_ = cntLeafRel
		// Use the Collect variant for BOTH trees and defer freeing the merged-away
		// btree blocks until AFTER both deletes complete. Freeing inserts into both
		// free-space trees; doing it between the cnt and bno deletes would mutate the
		// not-yet-deleted bno tree (moving the very record/keys the bno delete is
		// about to navigate), which corrupts the descent. Collecting first and
		// freeing once keeps each delete operating on a quiescent pair of trees.
		cntFreed, derr := allocDeleteRecordCollect(rw, partOff, sb, ag, cntRoot, int(cntLevel), allocStartRel, count, false)
		if derr != nil {
			return 0, fmt.Errorf("xfs: cnt B-tree delete: %w", derr)
		}
		// The cnt delete may have moved the bno root/level on disk (a collapse
		// rewrites the cnt fields only, but agf_btreeblks is shared); re-read.
		agfBuf, err = allocAGFBlock(rw, partOff, sb, ag)
		if err != nil {
			return 0, err
		}
		bnoRoot = be.Uint32(agfBuf[agfOffBnoRoot:])
		bnoLevel = be.Uint32(agfBuf[agfOffBnoLevel:])
		bnoFreed, derr := allocDeleteRecordCollect(rw, partOff, sb, ag, bnoRoot, int(bnoLevel), allocStartRel, 0, true)
		if derr != nil {
			return 0, fmt.Errorf("xfs: bno B-tree delete: %w", derr)
		}
		// Now both trees are quiescent: return the merged-away btree blocks to the
		// free-space trees. Each free inserts a single-block extent into both trees
		// (and updates agf_freeblks/longest/btreeblks/roots on disk).
		freed := append(cntFreed, bnoFreed...)
		if err := allocFreeMergedBlocks(rw, partOff, sb, ag, freed); err != nil {
			return 0, err
		}
		// Re-read so the free-count and longest update below builds on the
		// post-delete, post-free AGF instead of clobbering it. Refresh cntRoot/Level
		// for the longest walk.
		agfBuf, err = allocAGFBlock(rw, partOff, sb, ag)
		if err != nil {
			return 0, err
		}
		cntRoot = be.Uint32(agfBuf[agfOffCntRoot:])
		cntLevel = be.Uint32(agfBuf[agfOffCntLevel:])
	} else {
		// Update cnt record: new start+count pointing to the remainder. The count
		// SHRINKS (remaining < count), which moves the record earlier in the cnt
		// (count,start) order, so re-sort it leftward within the leaf, then refresh
		// any ancestor separator keys that drifted because the leaf's first record
		// changed. The leaf's record count is unchanged, so this never underflows the
		// node. Keeping the separators exact matters because the full-semantics
		// delete (alloc_btree_delete.go) navigates the cnt tree by separator key.
		newStart := allocStartRel + nBlocks
		be.PutUint32(cntLeaf[recOff:], newStart)
		be.PutUint32(cntLeaf[recOff+4:], remaining)
		// Re-sort the shrunk record leftward so the leaf stays in cnt (count,start)
		// order; numrecs is unchanged so the node cannot underflow. Without this a
		// multi-level cnt leaf accumulates out-of-order records (xfs_repair:
		// "out-of-order cnt btree record").
		allocCntBubbleLeft(cntLeaf, sb.agBTreeHdrSize(), recIdx)
		if err := allocWriteAGBTree(rw, partOff, sb, ag, cntLeafRel, cntLeaf); err != nil {
			return 0, err
		}
		// If the leaf's first record changed (the shrunk record bubbled to index 0,
		// or the deleted/edited record WAS index 0), the cnt ancestor separator keys
		// that point at this leaf must be refreshed to the new first key so the
		// separator-key-navigating delete stays accurate. allocCntRefreshLeafKey
		// re-descends to cntLeafRel and rewrites every ancestor separator that covers
		// it. Cheap (one root→leaf descent) and only when the first record moved.
		if err := allocCntRefreshLeafKey(rw, partOff, sb, ag, cntRoot, int(cntLevel), cntLeafRel); err != nil {
			return 0, fmt.Errorf("xfs: cnt B-tree separator refresh: %w", err)
		}
		// Update bno record for the same extent.
		if err := allocBnoUpdateRecord(rw, partOff, sb, ag, bnoRoot, int(bnoLevel), allocStartRel, newStart, remaining); err != nil {
			return 0, fmt.Errorf("xfs: bno B-tree update: %w", err)
		}
	}

	// Update AGF free block count.
	freeBlks := be.Uint32(agfBuf[agfOffFreeBlks:])
	be.PutUint32(agfBuf[agfOffFreeBlks:], freeBlks-nBlocks)
	// Recompute agf_longest from the (already-updated) cnt B-tree rather than
	// guessing from `remaining`: consuming the previously-longest extent can
	// promote a different extent to longest, and fully consuming it (remaining
	// == 0) must drop longest to the next-largest run. The old "shrink only when
	// remaining < longest" heuristic let agf_longest drift to 0 after a run of
	// allocations, which xfs_repair flags ("agf_longest N, counted M").
	longest, err := allocRecomputeLongest(rw, partOff, sb, ag, cntRoot, int(cntLevel))
	if err != nil {
		return 0, err
	}
	be.PutUint32(agfBuf[agfOffLongest:], longest)
	if err := allocWriteAGF(rw, partOff, sb, ag, agfBuf); err != nil {
		return 0, err
	}

	absBlock := sb.agAbsBlock(ag, allocStartRel)
	return absBlock, nil
}

// cntFindBlock navigates the cnt B-tree to a leaf containing a record with
// count >= need. Returns (AG-relative leaf block, block data, record index, err).
func cntFindBlock(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, need uint32) (uint32, []byte, int, error) {
	hdrSize := sb.agBTreeHdrSize()
	agRel := rootRel
	var blk []byte
	var err error

	// H3: bound the whole traversal and reject a cyclic block chain so a forged
	// rsib loop or a descent that never reaches a leaf terminates with an error
	// instead of spinning forever. A B-tree has at most ~level + leaf-chain
	// blocks; allocCntFindBlockMaxSteps is a generous global ceiling.
	guard := safeio.NewLoopGuard(allocBTreeMaxSteps)
	var seen safeio.VisitSet
	// prevLvl tracks the on-disk level of the last internal node descended
	// through; each child must have a strictly smaller level so a corrupt tree
	// cannot loop between two non-leaf blocks. Seed it above any legal level.
	prevLvl := 1 << 30

	for {
		if err := guard.Next(); err != nil {
			return 0, nil, 0, fmt.Errorf("xfs: cnt B-tree AG %d traversal: %w", ag, err)
		}
		if err := seen.Check(uint64(agRel)); err != nil {
			return 0, nil, 0, fmt.Errorf("xfs: cnt B-tree AG %d cycle: %w", ag, err)
		}
		blk, err = allocReadAGBlock(rw, partOff, sb, ag, agRel)
		if err != nil {
			return 0, nil, 0, err
		}
		if len(blk) < hdrSize {
			return 0, nil, 0, fmt.Errorf("xfs: cnt B-tree block too small")
		}
		lvl := int(binary.BigEndian.Uint16(blk[4:]))
		numrecs := int(binary.BigEndian.Uint16(blk[6:]))

		if lvl == 0 {
			// At a leaf: find the first record with count >= need.
			for i := 0; i < numrecs; i++ {
				off := hdrSize + i*allocRecSize
				if off+8 > len(blk) {
					break
				}
				count := binary.BigEndian.Uint32(blk[off+4:])
				if count >= need {
					return agRel, blk, i, nil
				}
			}
			// Try right sibling if no suitable record found.
			rsib := binary.BigEndian.Uint32(blk[12:])
			if rsib == 0xFFFFFFFF {
				return 0, nil, 0, fmt.Errorf("no free extent >= %d blocks in AG %d", need, ag)
			}
			agRel = rsib
			continue
		}
		// Internal node: the on-disk level must strictly decrease as we
		// descend, or a corrupt tree could loop between two non-leaf blocks.
		if lvl >= prevLvl {
			return 0, nil, 0, fmt.Errorf("xfs: cnt B-tree AG %d non-decreasing level %d", ag, lvl)
		}
		prevLvl = lvl
		// Follow the rightmost pointer (largest counts on right). XFS short-form
		// btree blocks size the key/ptr arrays to maxrecs, so the pointer array
		// begins at the maxrecs boundary regardless of the live record count.
		if numrecs <= 0 {
			return 0, nil, 0, fmt.Errorf("xfs: cnt B-tree internal node with no records")
		}
		ptrOff := hdrSize + allocMaxInternal(len(blk), hdrSize)*allocKeySize
		lastPtr := ptrOff + (numrecs-1)*allocPtrSize
		if lastPtr < 0 || lastPtr+4 > len(blk) {
			return 0, nil, 0, fmt.Errorf("cnt B-tree block too small")
		}
		agRel = binary.BigEndian.Uint32(blk[lastPtr:])
	}
}

// agfRecomputeLongest walks the AGF free-space B-tree rooted at rootRel
// (level deep) and returns the true maximum free-extent count, regardless of
// the order records are stored in. It descends internal nodes to the leftmost
// leaf, then follows the right-sibling chain across all leaves, taking the
// largest count field. Used to refresh agf_longest after edits that may have
// left a stale value (the simple allocator only ever lowers it).
func agfRecomputeLongest(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int) (uint32, error) {
	hdrSize := sb.agBTreeHdrSize()
	be := binary.BigEndian

	// Descend internal nodes to the leftmost leaf. level is the tree depth
	// (number of B-tree levels); level==1 means the root is already a leaf,
	// so descend level-1 times. H3: bound the descent and reject revisited
	// blocks so a corrupt pointer chain cannot spin forever.
	descend := safeio.NewLoopGuard(allocBTreeMaxSteps)
	var descendSeen safeio.VisitSet
	agRel := rootRel
	for level > 1 {
		if err := descend.Next(); err != nil {
			return 0, fmt.Errorf("xfs: agfRecomputeLongest ag=%d descent: %w", ag, err)
		}
		if err := descendSeen.Check(uint64(agRel)); err != nil {
			return 0, fmt.Errorf("xfs: agfRecomputeLongest ag=%d descent cycle: %w", ag, err)
		}
		blk, err := allocReadAGBlock(rw, partOff, sb, ag, agRel)
		if err != nil {
			return 0, err
		}
		if len(blk) < hdrSize {
			return 0, fmt.Errorf("xfs: agfRecomputeLongest ag=%d: btree block too small", ag)
		}
		// Pointer array begins at the maxrecs boundary (XFS short-form layout).
		ptrOff := hdrSize + allocMaxInternal(len(blk), hdrSize)*allocKeySize
		if ptrOff < 0 || ptrOff+allocPtrSize > len(blk) {
			return 0, fmt.Errorf("xfs: agfRecomputeLongest ag=%d: btree block too small", ag)
		}
		agRel = be.Uint32(blk[ptrOff:]) // leftmost child pointer
		level--
	}

	// Walk the leaf chain left-to-right, keeping the largest count. H3: bound
	// the iteration count and reject a cyclic sibling chain.
	walk := safeio.NewLoopGuard(allocBTreeMaxSteps)
	var leafSeen safeio.VisitSet
	var longest uint32
	for {
		if err := walk.Next(); err != nil {
			return 0, fmt.Errorf("xfs: agfRecomputeLongest ag=%d leaf walk: %w", ag, err)
		}
		if err := leafSeen.Check(uint64(agRel)); err != nil {
			return 0, fmt.Errorf("xfs: agfRecomputeLongest ag=%d sibling cycle: %w", ag, err)
		}
		blk, err := allocReadAGBlock(rw, partOff, sb, ag, agRel)
		if err != nil {
			return 0, err
		}
		if len(blk) < hdrSize {
			return 0, fmt.Errorf("xfs: agfRecomputeLongest ag=%d: btree block too small", ag)
		}
		numrecs := int(be.Uint16(blk[6:]))
		for i := 0; i < numrecs; i++ {
			off := hdrSize + i*allocRecSize
			if off+8 > len(blk) {
				break
			}
			if count := be.Uint32(blk[off+4:]); count > longest {
				longest = count
			}
		}
		rsib := be.Uint32(blk[12:])
		if rsib == 0xFFFFFFFF {
			return longest, nil
		}
		agRel = rsib
	}
}

// btreeDeleteRecord removes the record at recIdx from a leaf block, then
// decrements numrecs in place. Does not handle tree restructuring (merges).
func btreeDeleteRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, _ uint32, _ int, recIdx int, leafRel uint32, leaf []byte, recSize int, useBE bool) error {
	hdrSize := sb.agBTreeHdrSize()
	numrecs := int(binary.BigEndian.Uint16(leaf[6:]))
	if recIdx < 0 || recIdx >= numrecs {
		return fmt.Errorf("btree delete: invalid record index %d/%d", recIdx, numrecs)
	}
	// Shift remaining records left.
	recStart := hdrSize + recIdx*recSize
	copy(leaf[recStart:], leaf[recStart+recSize:hdrSize+numrecs*recSize])
	// Zero the now-unused last slot.
	lastOff := hdrSize + (numrecs-1)*recSize
	clear(leaf[lastOff : lastOff+recSize])
	binary.BigEndian.PutUint16(leaf[6:], uint16(numrecs-1))
	return allocWriteAGBTree(rw, partOff, sb, ag, leafRel, leaf)
}

// bnoDeleteRecord deletes the bno B-tree record that starts at agStartRel with
// full B+tree semantics (rebalance/merge underfull blocks, collapse the root).
// The bno tree is keyed by start alone, so count is irrelevant to the lookup;
// passing 0 keeps allocDeleteRecord's signature uniform with the cnt path.
func bnoDeleteRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, agStartRel uint32) error {
	return allocDeleteRecord(rw, partOff, sb, ag, rootRel, level, agStartRel, 0, true)
}

// bnoUpdateRecord replaces the bno B-tree record at oldStart with
// (newStart, newCount).
func bnoUpdateRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, oldStart, newStart, newCount uint32) error {
	leafRel, leaf, recIdx, err := allocBnoFindRecord(rw, partOff, sb, ag, rootRel, level, oldStart)
	if err != nil {
		return err
	}
	hdrSize := sb.agBTreeHdrSize()
	off := hdrSize + recIdx*allocRecSize
	binary.BigEndian.PutUint32(leaf[off:], newStart)
	binary.BigEndian.PutUint32(leaf[off+4:], newCount)
	// The bno B-tree sorts by startblock, and newStart > oldStart, so there
	// may be a need to re-sort. For a simple pre-image with a single big extent
	// this is a no-op. For correctness, do an in-place insertion sort on the
	// one changed record:
	numrecs := int(binary.BigEndian.Uint16(leaf[6:]))
	for i := recIdx; i+1 < numrecs; i++ {
		cur := hdrSize + i*allocRecSize
		nxt := cur + allocRecSize
		curStart := binary.BigEndian.Uint32(leaf[cur:])
		nxtStart := binary.BigEndian.Uint32(leaf[nxt:])
		if curStart <= nxtStart {
			break
		}
		// Swap records.
		var tmp [8]byte
		copy(tmp[:], leaf[cur:cur+8])
		copy(leaf[cur:], leaf[nxt:nxt+8])
		copy(leaf[nxt:], tmp[:])
	}
	return allocWriteAGBTree(rw, partOff, sb, ag, leafRel, leaf)
}

// bnoFindRecord walks the bno B-tree and returns the leaf block and record
// index for the record with startblock == agStartRel.
func bnoFindRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, agStartRel uint32) (uint32, []byte, int, error) {
	hdrSize := sb.agBTreeHdrSize()
	agRel := rootRel
	for {
		blk, err := allocReadAGBlock(rw, partOff, sb, ag, agRel)
		if err != nil {
			return 0, nil, 0, err
		}
		lvl := int(binary.BigEndian.Uint16(blk[4:]))
		numrecs := int(binary.BigEndian.Uint16(blk[6:]))

		if lvl == 0 {
			for i := 0; i < numrecs; i++ {
				off := hdrSize + i*allocRecSize
				start := binary.BigEndian.Uint32(blk[off:])
				if start == agStartRel {
					return agRel, blk, i, nil
				}
			}
			return 0, nil, 0, fmt.Errorf("bno B-tree: startblock %d not found in AG %d", agStartRel, ag)
		}
		// Internal node: binary search for key <= agStartRel.
		child := 0
		for i := 0; i < numrecs; i++ {
			kOff := hdrSize + i*allocKeySize
			k := binary.BigEndian.Uint32(blk[kOff:])
			if k <= agStartRel {
				child = i
			} else {
				break
			}
		}
		ptrOff := hdrSize + allocMaxInternal(len(blk), hdrSize)*allocKeySize + child*allocPtrSize
		agRel = binary.BigEndian.Uint32(blk[ptrOff:])
		level--
	}
}

// freeBlocks returns a run of nBlocks blocks starting at absStartBlock back
// to the free space B-trees. Only handles the simple case where the freed
// range does not merge with adjacent extents.
func freeBlocks(rw readerWriterAt, partOff int64, sb *superblock, absStartBlock uint64, nBlocks uint32) (retErr error) {
	// Participate in the same op-depth bookkeeping as allocBlocks so a freeBlocks
	// nested inside an alloc (e.g. a reserve release, or the merged-free drain
	// itself) does not drain the merged-free queue prematurely; only the outermost
	// operation drains it. A standalone top-level free drains any queue too.
	allocOpDepth++
	defer func() {
		allocOpDepth--
		if allocOpDepth == 0 && retErr == nil {
			retErr = allocDrainMergedFrees(rw, partOff, sb)
		}
	}()

	// absStartBlock is a packed fsbno (every caller derives it from agAbsBlock or
	// from a decoded bmbt extent's startBlock — both packed). Decode it with the
	// packing inverse, NOT a /sb_agblocks division: division only matches on a
	// power-of-two sb_agblocks (our own Format), and would mis-compute ag/agbno on
	// a real mkfs.xfs image (non-power-of-two sb_agblocks) — corrupting free-space
	// accounting on a read-modify-write. See superblock.fsbToAgAgbno / agAbsBlock.
	ag, agStartRel := sb.fsbToAgAgbno(absStartBlock)

	agfBuf, err := allocAGFBlock(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	be := binary.BigEndian

	bnoRoot := be.Uint32(agfBuf[agfOffBnoRoot:])
	cntRoot := be.Uint32(agfBuf[agfOffCntRoot:])
	bnoLevel := int(be.Uint32(agfBuf[agfOffBnoLevel:]))
	cntLevel := int(be.Uint32(agfBuf[agfOffCntLevel:]))

	// Insert into bno B-tree. A multi-level insert may split nodes and grow the
	// tree root; allocInsertWithReserve pre-allocates the split blocks (via
	// allocBlocks, while the trees are consistent) so a split never allocates
	// against a tree it is mid-rewrite — and never re-enters / mis-sorts the
	// free-space trees (see alloc_btree.go).
	if err := allocInsertWithReserve(rw, partOff, sb, ag, bnoLevel, func() error {
		return allocBnoInsertRecord(rw, partOff, sb, ag, bnoRoot, bnoLevel, agStartRel, nBlocks)
	}); err != nil {
		return fmt.Errorf("xfs: freeBlocks bno insert: %w", err)
	}
	// Re-read the cnt root/level in case a split (or reserve alloc/free) moved it.
	agfBuf, err = allocAGFBlock(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	cntRoot = be.Uint32(agfBuf[agfOffCntRoot:])
	cntLevel = int(be.Uint32(agfBuf[agfOffCntLevel:]))
	// Insert into cnt B-tree (same reserve discipline).
	if err := allocInsertWithReserve(rw, partOff, sb, ag, cntLevel, func() error {
		return allocCntInsertRecord(rw, partOff, sb, ag, cntRoot, cntLevel, agStartRel, nBlocks)
	}); err != nil {
		return fmt.Errorf("xfs: freeBlocks cnt insert: %w", err)
	}

	// Re-read the AGF: an insert that grew a tree root rewrote agf_*_root /
	// agf_*_level (and agf_btreeblks) on disk. The agfBuf read above is now
	// stale; writing it back would clobber the new depth. Re-read so the freeblks
	// / longest update below builds on the post-split AGF. Refresh cntRoot/Level
	// too so agfRecomputeLongest walks the (possibly deeper) cnt tree.
	agfBuf, err = allocAGFBlock(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	cntRoot = be.Uint32(agfBuf[agfOffCntRoot:])
	cntLevel = int(be.Uint32(agfBuf[agfOffCntLevel:]))

	// Update AGF free count.
	freeBlks := be.Uint32(agfBuf[agfOffFreeBlks:])
	be.PutUint32(agfBuf[agfOffFreeBlks:], freeBlks+nBlocks)
	// Recompute agf_longest from the cnt B-tree: a freed run can coalesce with
	// adjacent free extents into something larger than nBlocks, so the simple
	// "nBlocks > longest" bump under-counts after a merge.
	longest, err := allocRecomputeLongest(rw, partOff, sb, ag, cntRoot, cntLevel)
	if err != nil {
		return err
	}
	be.PutUint32(agfBuf[agfOffLongest:], longest)
	return allocWriteAGF(rw, partOff, sb, ag, agfBuf)
}

// bnoInsertRecord inserts (agStartRel, nBlocks) into the bno B-tree leaf.
// Fails if the leaf is full (would require a split).
func bnoInsertRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, agStartRel, nBlocks uint32) error {
	return allocInsertRecord(rw, partOff, sb, ag, rootRel, level, agStartRel, nBlocks, true)
}

// cntInsertRecord inserts (agStartRel, nBlocks) into the cnt B-tree leaf,
// sorted by count.
func cntInsertRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, agStartRel, nBlocks uint32) error {
	return allocInsertRecord(rw, partOff, sb, ag, rootRel, level, agStartRel, nBlocks, false)
}

// allocBTreeInsert navigates to the appropriate leaf of a bno or cnt B-tree
// and inserts a record in sorted order. bnoSort=true sorts by startblock;
// bnoSort=false sorts by blockcount.
func allocBTreeInsert(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, agStartRel, nBlocks uint32, bnoSort bool) error {
	hdrSize := sb.agBTreeHdrSize()
	agRel := rootRel

	for {
		blk, err := allocReadAGBlock(rw, partOff, sb, ag, agRel)
		if err != nil {
			return err
		}
		lvl := int(binary.BigEndian.Uint16(blk[4:]))
		numrecs := int(binary.BigEndian.Uint16(blk[6:]))

		if lvl == 0 {
			// Leaf: find insertion point and insert.
			maxRecs := (len(blk) - hdrSize) / allocRecSize
			if numrecs >= maxRecs {
				return fmt.Errorf("bno/cnt B-tree leaf full (%d records); cannot insert without a tree split", numrecs)
			}
			// Find insertion position (sorted).
			insertAt := numrecs
			for i := 0; i < numrecs; i++ {
				off := hdrSize + i*allocRecSize
				var key uint32
				if bnoSort {
					key = binary.BigEndian.Uint32(blk[off:])
				} else {
					key = binary.BigEndian.Uint32(blk[off+4:])
				}
				var newKey uint32
				if bnoSort {
					newKey = agStartRel
				} else {
					newKey = nBlocks
				}
				if newKey < key {
					insertAt = i
					break
				}
			}
			// Shift records right to make room.
			insertOff := hdrSize + insertAt*allocRecSize
			copy(blk[insertOff+allocRecSize:], blk[insertOff:hdrSize+numrecs*allocRecSize])
			binary.BigEndian.PutUint32(blk[insertOff:], agStartRel)
			binary.BigEndian.PutUint32(blk[insertOff+4:], nBlocks)
			binary.BigEndian.PutUint16(blk[6:], uint16(numrecs+1))
			return allocWriteAGBTree(rw, partOff, sb, ag, agRel, blk)
		}

		// Internal node: descend.
		var child int
		for i := 0; i < numrecs; i++ {
			kOff := hdrSize + i*allocKeySize
			var k uint32
			if bnoSort {
				k = binary.BigEndian.Uint32(blk[kOff:])
			} else {
				k = binary.BigEndian.Uint32(blk[kOff+4:])
			}
			var newKey uint32
			if bnoSort {
				newKey = agStartRel
			} else {
				newKey = nBlocks
			}
			if newKey >= k {
				child = i
			} else {
				break
			}
		}
		ptrOff := hdrSize + allocMaxInternal(len(blk), hdrSize)*allocKeySize + child*allocPtrSize
		agRel = binary.BigEndian.Uint32(blk[ptrOff:])
	}
}

// ──────────────────── Inode allocation ─────────────────────────────────────

// allocInode allocates a free inode from allocation group ag using the inobt
// B-tree. Returns the absolute inode number.
//
// When every inobt record in AG ag is full, allocInode grows the inobt by
// allocating a fresh 64-inode chunk (8 filesystem blocks) via the bno/cnt
// B-trees, pre-initialising every inode slot with a valid v3 header, and
// inserting a new record in the inobt leaf. The newly-added chunk's first
// inode is then returned to the caller. This lifts the writer's previous
// 7-live-files cap (which came from the single 8-inode chunk Format()
// seeds) to ~127 chunks per AG = ~8128 live inodes per AG.
func allocInode(rw readerWriterAt, partOff int64, sb *superblock, ag uint32) (uint64, error) {
	agiBuf, err := allocAGIBlock(rw, partOff, sb, ag)
	if err != nil {
		return 0, err
	}
	be := binary.BigEndian

	inobtRoot := be.Uint32(agiBuf[agiOffRoot:])
	inobtLevel := int(be.Uint32(agiBuf[agiOffLevel:]))

	leafRel, leaf, recIdx, err := allocInobtFindFree(rw, partOff, sb, ag, inobtRoot, inobtLevel)
	if err != nil {
		// Only grow when find-free reports the inobt is full. Other errors
		// (I/O, corruption) propagate directly so the caller doesn't mask a
		// real failure as a grow attempt.
		if !errors.Is(err, errInobtFull) {
			return 0, fmt.Errorf("xfs: allocInode ag=%d: %w", ag, err)
		}
		// Every chunk is full: grow the inobt with a new 64-inode chunk and
		// retry. growInobt persists the updated AGI/inobt to disk, so we
		// re-read both before retrying.
		if gerr := allocGrowInobt(rw, partOff, sb, ag); gerr != nil {
			return 0, fmt.Errorf("xfs: allocInode ag=%d: grow inobt: %w", ag, gerr)
		}
		// Re-read AGI: growInobt rewrote agiBuf on disk.
		agiBuf, err = allocAGIBlock(rw, partOff, sb, ag)
		if err != nil {
			return 0, err
		}
		inobtRoot = be.Uint32(agiBuf[agiOffRoot:])
		inobtLevel = int(be.Uint32(agiBuf[agiOffLevel:]))
		leafRel, leaf, recIdx, err = allocInobtFindFree(rw, partOff, sb, ag, inobtRoot, inobtLevel)
		if err != nil {
			return 0, fmt.Errorf("xfs: allocInode ag=%d after grow: %w", ag, err)
		}
	}

	hdrSize := sb.agBTreeHdrSize()
	recOff := hdrSize + recIdx*inobtRecSize
	startIno := be.Uint32(leaf[recOff:]) // AG-relative inode number

	// ir_free is a 64-bit bitmask at recOff+8 (freecount at recOff+4 for v4,
	// or recOff+6 for v5 sparse); for simplicity handle v4-layout which is
	// the common case (even in v5 without "sparse inodes" feature).
	irFree := be.Uint64(leaf[recOff+8:])
	freeCount := bits64(irFree)
	if freeCount == 0 {
		return 0, fmt.Errorf("xfs: inobt record has no free inodes")
	}

	// Find the first free bit (bit 0 = lowest inode in the chunk).
	var bit int
	for bit = 0; bit < 64; bit++ {
		if (irFree>>bit)&1 == 1 {
			break
		}
	}

	// Clear the bit and decrement freecount.
	irFree &^= 1 << bit
	be.PutUint64(leaf[recOff+8:], irFree)
	// Decrement freecount — stored at recOff+4 (4 bytes) for non-sparse inodes.
	fc := be.Uint32(leaf[recOff+4:])
	be.PutUint32(leaf[recOff+4:], fc-1)

	if err := allocWriteAGBTree(rw, partOff, sb, ag, leafRel, leaf); err != nil {
		return 0, err
	}

	agRelIno := startIno + uint32(bit)
	// Update AGI free count.
	fc2 := be.Uint32(agiBuf[agiOffFreeCount:])
	be.PutUint32(agiBuf[agiOffFreeCount:], fc2-1)
	be.PutUint32(agiBuf[agiOffNewIno:], agRelIno)
	if err := allocWriteAGI(rw, partOff, sb, ag, agiBuf); err != nil {
		return 0, err
	}

	return inoFromAGRel(sb, ag, agRelIno), nil
}

// bits64 counts set bits in v (popcount).
func bits64(v uint64) int {
	n := 0
	for v != 0 {
		n += int(v & 1)
		v >>= 1
	}
	return n
}

// inobtFindFree walks the inobt B-tree and returns the first leaf block
// containing a record with freecount > 0.
func inobtFindFree(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int) (uint32, []byte, int, error) {
	hdrSize := sb.agBTreeHdrSize()
	agRel := rootRel
	for {
		blk, err := allocReadAGBlock(rw, partOff, sb, ag, agRel)
		if err != nil {
			return 0, nil, 0, err
		}
		lvl := int(binary.BigEndian.Uint16(blk[4:]))
		numrecs := int(binary.BigEndian.Uint16(blk[6:]))

		if lvl == 0 {
			for i := 0; i < numrecs; i++ {
				off := hdrSize + i*inobtRecSize
				fc := binary.BigEndian.Uint32(blk[off+4:])
				if fc > 0 {
					return agRel, blk, i, nil
				}
			}
			rsib := binary.BigEndian.Uint32(blk[12:])
			if rsib == 0xFFFFFFFF {
				return 0, nil, 0, fmt.Errorf("AG %d: %w", ag, errInobtFull)
			}
			agRel = rsib
			continue
		}
		// Follow leftmost child. XFS short-form btree blocks size the key and
		// pointer arrays to the block's maxrecs, so pointers start at the maxrecs
		// boundary (not numrecs).
		ptrOff := hdrSize + inobtMaxInternal(len(blk), hdrSize)*inobtKeySize
		agRel = binary.BigEndian.Uint32(blk[ptrOff:])
		level--
	}
}

// freeInode marks inode ino as free in the AGI inobt B-tree.
func freeInode(rw readerWriterAt, partOff int64, sb *superblock, ino uint64) error {
	ag := inoAG(sb, ino)
	agRel := inoAGRel(sb, ino)

	agiBuf, err := allocAGIBlock(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	be := binary.BigEndian

	inobtRoot := be.Uint32(agiBuf[agiOffRoot:])
	inobtLevel := int(be.Uint32(agiBuf[agiOffLevel:]))

	leafRel, leaf, recIdx, err := allocInobtFindRecord(rw, partOff, sb, ag, inobtRoot, inobtLevel, agRel)
	if err != nil {
		return fmt.Errorf("xfs: freeInode %d: %w", ino, err)
	}

	hdrSize := sb.agBTreeHdrSize()
	recOff := hdrSize + recIdx*inobtRecSize
	startIno := be.Uint32(leaf[recOff:])
	bit := uint(agRel - startIno)
	irFree := be.Uint64(leaf[recOff+8:])
	irFree |= 1 << bit
	be.PutUint64(leaf[recOff+8:], irFree)
	fc := be.Uint32(leaf[recOff+4:])
	be.PutUint32(leaf[recOff+4:], fc+1)

	if err := allocWriteAGBTree(rw, partOff, sb, ag, leafRel, leaf); err != nil {
		return err
	}

	// Update AGI freecount.
	freeCount := be.Uint32(agiBuf[agiOffFreeCount:])
	be.PutUint32(agiBuf[agiOffFreeCount:], freeCount+1)
	return allocWriteAGI(rw, partOff, sb, ag, agiBuf)
}

// inobtFindRecord walks the inobt and returns the leaf block + record index
// that contains agRelIno.
func inobtFindRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, agRelIno uint32) (uint32, []byte, int, error) {
	hdrSize := sb.agBTreeHdrSize()
	agRel := rootRel
	for {
		blk, err := allocReadAGBlock(rw, partOff, sb, ag, agRel)
		if err != nil {
			return 0, nil, 0, err
		}
		lvl := int(binary.BigEndian.Uint16(blk[4:]))
		numrecs := int(binary.BigEndian.Uint16(blk[6:]))

		if lvl == 0 {
			for i := 0; i < numrecs; i++ {
				off := hdrSize + i*inobtRecSize
				startIno := binary.BigEndian.Uint32(blk[off:])
				if agRelIno >= startIno && agRelIno < startIno+64 {
					return agRel, blk, i, nil
				}
			}
			return 0, nil, 0, fmt.Errorf("inobt: inode %d not found in AG %d", agRelIno, ag)
		}
		// Descend into the correct child.
		child := 0
		for i := 0; i < numrecs; i++ {
			kOff := hdrSize + i*inobtKeySize
			k := binary.BigEndian.Uint32(blk[kOff:])
			if agRelIno >= k {
				child = i
			} else {
				break
			}
		}
		ptrOff := hdrSize + inobtMaxInternal(len(blk), hdrSize)*inobtKeySize + child*inobtPtrSize
		agRel = binary.BigEndian.Uint32(blk[ptrOff:])
		level--
	}
}

// growInobt extends the inobt of AG ag by one fresh 64-inode chunk. It:
//  1. allocates 8 contiguous blocks (= inobtChunkInodes/inopBlock) via the
//     bno/cnt B-trees;
//  2. initialises all 64 inode slots in those blocks with valid v3 headers
//     (mode=0 means "free") so xfs_repair can walk the chunk;
//  3. inserts a new inobt record (startIno, freeCount=64, irFree=all-ones)
//     into the inobt leaf in sorted order;
//  4. updates the AGI count + freeCount fields to reflect the +64 inodes.
//
// growInobt does NOT take an inode for the caller — allocInode will do a
// fresh find-free pass on the now-non-empty inobt and return the first
// inode of the new chunk.
//
// When the single root leaf fills (~252 chunks at blockSize=4096), the record
// insert promotes the inobt from one level to two: the full leaf is split in
// half into two leaf blocks and a fresh internal root (level 1) is written
// above them, with the AGI root/level updated. The insert generalises to any
// depth: a full leaf under an already-deep tree is split and the new key/ptr
// propagated up; a full interior node is split in turn, and if the root itself
// splits the tree grows one level deeper (depth 2→3, …) with a fresh root.
func growInobt(rw readerWriterAt, partOff int64, sb *superblock, ag uint32) error {
	be := binary.BigEndian

	// blocksPerChunk = 64 / inopBlock (typically 64/8 = 8). Guard inopBlock
	// up front: division by zero would panic, and any inopBlock > 64 would
	// produce blocksPerChunk == 0 (so we can't back the chunk at all).
	if sb.inopBlock == 0 || sb.inopBlock > inobtChunkInodes {
		return fmt.Errorf("xfs: growInobt: invalid inopBlock %d", sb.inopBlock)
	}
	blocksPerChunk := uint32(inobtChunkInodes) / uint32(sb.inopBlock)

	// An inode chunk's starting inode number must be a multiple of
	// XFS_INODES_PER_CHUNK (64); otherwise xfs_repair maps the chunk's inodes
	// onto unrelated blocks. startIno = agBlockBase * inopBlock, so agBlockBase
	// must be a multiple of blocksPerChunk (= 64/inopBlock). The bno/cnt
	// allocator hands out whatever block fits, so over-allocate by up to one
	// chunk's worth, carve out the aligned blocksPerChunk-block run, and free
	// the unaligned head/tail slack back to the free-space B-trees.
	// Fast path: ask for exactly blocksPerChunk. The cnt allocator carves from
	// the front of the largest free extent, so once that extent's start is
	// chunk-aligned every subsequent exact allocation is aligned too — yielding
	// zero slack and, crucially, no slack-free fragmentation of the bno/cnt
	// trees (a steady stream of unaligned slack frees would otherwise overflow a
	// free-space leaf). Only when the returned run is misaligned do we fall back
	// to the over-allocate-and-trim path below.
	probeStart, err := allocAllocBlocks(rw, partOff, sb, ag, blocksPerChunk)
	if err != nil {
		return fmt.Errorf("growInobt: alloc %d blocks: %w", blocksPerChunk, err)
	}
	// probeStart is a packed fsbno (allocAllocBlocks returns agAbsBlock). Decode
	// it with the agblklog shift/mask, NOT /sb_agblocks: the two agree only on a
	// power-of-two sb_agblocks (our Format), so division would mis-derive the
	// AG-relative base — and thus the chunk alignment — on a real mkfs.xfs image.
	_, probeBase := sb.fsbToAgAgbno(probeStart)

	var absStart uint64
	var agBlockBase uint32
	if probeBase%blocksPerChunk == 0 {
		absStart = probeStart
		agBlockBase = probeBase
	} else {
		// Misaligned: hand the probe run back and over-allocate enough slack to
		// contain an aligned blocksPerChunk-block run, then free the unaligned
		// head/tail so xfs_repair sees no leaked blocks.
		if err := allocFreeBlocks(rw, partOff, sb, probeStart, blocksPerChunk); err != nil {
			return fmt.Errorf("growInobt: free probe run: %w", err)
		}
		wantBlocks := 2*blocksPerChunk - 1
		rawStart, err := allocAllocBlocks(rw, partOff, sb, ag, wantBlocks)
		if err != nil {
			return fmt.Errorf("growInobt: alloc %d blocks: %w", wantBlocks, err)
		}
		// Packed fsbno -> AG-relative via agblklog shift/mask, not /sb_agblocks.
		_, rawBase := sb.fsbToAgAgbno(rawStart)
		alignedBase := (rawBase + blocksPerChunk - 1) / blocksPerChunk * blocksPerChunk
		headSlack := alignedBase - rawBase
		absStart = rawStart + uint64(headSlack)
		tailSlack := wantBlocks - headSlack - blocksPerChunk
		if headSlack > 0 {
			if err := allocFreeBlocks(rw, partOff, sb, rawStart, headSlack); err != nil {
				return fmt.Errorf("growInobt: free head slack: %w", err)
			}
		}
		if tailSlack > 0 {
			if err := allocFreeBlocks(rw, partOff, sb, absStart+uint64(blocksPerChunk), tailSlack); err != nil {
				return fmt.Errorf("growInobt: free tail slack: %w", err)
			}
		}
		agBlockBase = alignedBase
	}

	startIno := agBlockBase * uint32(sb.inopBlock)

	// Pre-initialise every inode in the chunk. xfs_repair walks the chunk's
	// inodes by physical block + slot, so each slot must hold a v3 inode
	// header with the correct on-disk inode number and a valid CRC. mode=0
	// marks the inode as free (kernel: "inode is not in use").
	for slot := uint32(0); slot < inobtChunkInodes; slot++ {
		buf := make([]byte, sb.inodeSize)
		ino := inoFromAGRel(sb, ag, startIno+slot)
		initInodeV3(buf, ino, 0 /* mode = free */, sb.inodeSize, 0 /* nlink */, sb.uuid)
		// Set di_format to a sane default so any reader sees something
		// well-formed. The kernel uses XFS_DINODE_FMT_EXTENTS on freshly
		// inited free inodes.
		buf[inoOffFormat] = inodeFmtExtents
		if sb.hasCRC {
			updateCRC(buf, inoOffCRC, int(sb.inodeSize))
		}
		// Write the inode slot directly to its physical location.
		blockIdx := slot / uint32(sb.inopBlock)
		slotInBlock := slot % uint32(sb.inopBlock)
		off := partOff +
			int64(absStart+uint64(blockIdx))*int64(sb.blockSize) +
			int64(slotInBlock)*int64(sb.inodeSize)
		if _, err := rw.WriteAt(buf, off); err != nil {
			return fmt.Errorf("growInobt: write inode slot %d: %w", slot, err)
		}
	}

	// Insert the new record into the inobt.
	agiBuf, err := allocAGIBlock(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	inobtRoot := be.Uint32(agiBuf[agiOffRoot:])
	inobtLevel := be.Uint32(agiBuf[agiOffLevel:])

	if err := inobtInsertChunkRecord(rw, partOff, sb, ag, agiBuf, inobtRoot, inobtLevel, startIno); err != nil {
		return err
	}

	// Update AGI: +64 to count + freeCount. inobtInsertChunkRecord may have
	// already rewritten the AGI root/level (on a split); those updates live in
	// agiBuf, so the single AGI write below persists them together with the
	// count bump.
	agiCount := be.Uint32(agiBuf[agiOffCount:])
	agiFree := be.Uint32(agiBuf[agiOffFreeCount:])
	be.PutUint32(agiBuf[agiOffCount:], agiCount+inobtChunkInodes)
	be.PutUint32(agiBuf[agiOffFreeCount:], agiFree+inobtChunkInodes)
	return allocWriteAGI(rw, partOff, sb, ag, agiBuf)
}

// inobtSplit carries the result of a node split back up the recursion: when a
// child block at relative position selfRel overflows it is split, and the lower
// half stays in selfRel while the upper half moves to a freshly-allocated block
// at rightRel whose first key is rightKey. The parent (or, at the root, the
// caller) must insert the (rightKey → rightRel) separator next to the pointer
// it followed to reach selfRel. When split is false the insert was absorbed in
// place and the other fields are unset.
type inobtSplit struct {
	split    bool
	rightKey uint32 // first ir_startino in the new right sibling
	rightRel uint32 // AG-relative block number of the new right sibling
}

// inobtInsertChunkRecord inserts one freshly-allocated 64-inode chunk record
// (startIno, freecount=64, ir_free=all-ones) into the inobt of AG ag, keeping
// records sorted by ir_startino. It mirrors xfsprogs' xfs_inobt_insert →
// xfs_btree_insrec → xfs_btree_split: the tree is descended from the root, the
// record is inserted into the target leaf, and when a block fills it is split
// and the new separator key/ptr propagated up. If the root itself splits, a
// fresh root one level higher is written and the AGI root/level fields are
// updated in place (so the caller persists them with its AGI write). This grows
// the tree depth-1→2, depth-2→3, … on demand.
func inobtInsertChunkRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, agiBuf []byte, rootRel uint32, rootLevel, startIno uint32) error {
	be := binary.BigEndian

	// Build the new record once: startIno, freecount=64, ir_free=all-free.
	var rec [inobtRecSize]byte
	be.PutUint32(rec[0:], startIno)
	be.PutUint32(rec[4:], inobtChunkInodes)
	be.PutUint64(rec[8:], 0xFFFFFFFFFFFFFFFF)

	res, err := inobtInsertInto(rw, partOff, sb, ag, rootRel, startIno, rec[:])
	if err != nil {
		return err
	}
	if !res.split {
		return nil
	}

	// The root split. Allocate a fresh root one level higher that points at the
	// old root (now the left child) and its new right sibling, then repoint the
	// AGI. This is the only place tree depth grows.
	newRootAbs, err := allocAllocBlocks(rw, partOff, sb, ag, 1)
	if err != nil {
		return fmt.Errorf("growInobt split: alloc root: %w", err)
	}
	// Packed fsbno -> AG-relative via agblklog shift/mask, not /sb_agblocks.
	_, newRootRel := sb.fsbToAgAgbno(newRootAbs)

	hdrSize := sb.agBTreeHdrSize()
	// Re-read the old root for two things: its on-disk block-level (the new root
	// sits exactly one above it — note this "level" field is the leaf-relative
	// height, 0 for leaves, distinct from the AGI's 1-based depth) and its first
	// key, which is the smallest key reachable through the whole left subtree.
	oldRoot, err := allocReadAGBlock(rw, partOff, sb, ag, rootRel)
	if err != nil {
		return err
	}
	oldBlockLevel := int(be.Uint16(oldRoot[4:]))
	root := newInobtBlock(sb, ag, newRootRel, oldBlockLevel+1, 2)
	leftKey := be.Uint32(oldRoot[hdrSize:])
	be.PutUint32(root[hdrSize+0*inobtKeySize:], leftKey)
	be.PutUint32(root[hdrSize+1*inobtKeySize:], res.rightKey)
	maxInternal := inobtMaxInternal(len(root), hdrSize)
	ptrBase := hdrSize + maxInternal*inobtKeySize
	be.PutUint32(root[ptrBase+0*inobtPtrSize:], rootRel)
	be.PutUint32(root[ptrBase+1*inobtPtrSize:], res.rightRel)
	if err := allocWriteAGBTree(rw, partOff, sb, ag, newRootRel, root); err != nil {
		return err
	}

	be.PutUint32(agiBuf[agiOffRoot:], newRootRel)
	be.PutUint32(agiBuf[agiOffLevel:], rootLevel+1)
	return nil
}

// inobtInsertInto recursively inserts rec (keyed by startIno) into the inobt
// subtree rooted at selfRel and returns whether selfRel split. On a leaf it
// inserts the record; on an interior node it descends into the correct child,
// then absorbs (or splits to propagate) the child's separator key/ptr. The
// short-form inobt packs all keys first, then all pointers, at the block's
// maxrecs boundary, so node updates must preserve that layout.
func inobtInsertInto(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, selfRel, startIno uint32, rec []byte) (inobtSplit, error) {
	be := binary.BigEndian
	hdrSize := sb.agBTreeHdrSize()

	blk, err := allocReadAGBlock(rw, partOff, sb, ag, selfRel)
	if err != nil {
		return inobtSplit{}, err
	}
	lvl := int(be.Uint16(blk[4:]))
	numrecs := int(be.Uint16(blk[6:]))

	if lvl == 0 {
		// Leaf: find the sorted insertion slot, rejecting duplicates.
		maxRecs := (len(blk) - hdrSize) / inobtRecSize
		insertAt := numrecs
		for i := 0; i < numrecs; i++ {
			existingStart := be.Uint32(blk[hdrSize+i*inobtRecSize:])
			if startIno < existingStart {
				insertAt = i
				break
			}
			if startIno == existingStart {
				return inobtSplit{}, fmt.Errorf("growInobt: inobt already has a record at startIno %d", startIno)
			}
		}
		if numrecs < maxRecs {
			insertOff := hdrSize + insertAt*inobtRecSize
			copy(blk[insertOff+inobtRecSize:], blk[insertOff:hdrSize+numrecs*inobtRecSize])
			copy(blk[insertOff:], rec)
			be.PutUint16(blk[6:], uint16(numrecs+1))
			return inobtSplit{}, allocWriteAGBTree(rw, partOff, sb, ag, selfRel, blk)
		}
		return inobtSplitLeaf(rw, partOff, sb, ag, selfRel, blk, insertAt, rec)
	}

	// Interior node: choose the child whose key range covers startIno (the last
	// child with key <= startIno), descend, then handle any propagated split.
	child := 0
	for i := 0; i < numrecs; i++ {
		k := be.Uint32(blk[hdrSize+i*inobtKeySize:])
		if startIno >= k {
			child = i
		} else {
			break
		}
	}
	maxInternal := inobtMaxInternal(len(blk), hdrSize)
	ptrBase := hdrSize + maxInternal*inobtKeySize
	childRel := be.Uint32(blk[ptrBase+child*inobtPtrSize:])

	res, err := inobtInsertInto(rw, partOff, sb, ag, childRel, startIno, rec)
	if err != nil {
		return inobtSplit{}, err
	}
	if !res.split {
		return inobtSplit{}, nil
	}

	// Child split: insert the new (rightKey → rightRel) separator just after the
	// pointer we followed (index child+1). Re-read in case the descent rewrote
	// this very block (it can't — a child lives in a different block — but the
	// child write is already durable; this block is untouched in memory).
	insertAt := child + 1
	if numrecs < maxInternal {
		// Shift keys right from insertAt.
		copy(blk[hdrSize+(insertAt+1)*inobtKeySize:], blk[hdrSize+insertAt*inobtKeySize:hdrSize+numrecs*inobtKeySize])
		be.PutUint32(blk[hdrSize+insertAt*inobtKeySize:], res.rightKey)
		// Shift pointers right from insertAt.
		copy(blk[ptrBase+(insertAt+1)*inobtPtrSize:], blk[ptrBase+insertAt*inobtPtrSize:ptrBase+numrecs*inobtPtrSize])
		be.PutUint32(blk[ptrBase+insertAt*inobtPtrSize:], res.rightRel)
		be.PutUint16(blk[6:], uint16(numrecs+1))
		return inobtSplit{}, allocWriteAGBTree(rw, partOff, sb, ag, selfRel, blk)
	}
	return inobtSplitNode(rw, partOff, sb, ag, selfRel, blk, insertAt, res.rightKey, res.rightRel)
}

// inobtSplitLeaf splits a full leaf selfRel in two. The full sorted record set
// is the existing leaf records with rec inserted at insertAt; the lower half
// stays in selfRel and the upper half moves to a newly-allocated right sibling,
// with the leaf sibling chain re-stitched. Returns the right sibling's first
// key so the parent can insert the separator.
func inobtSplitLeaf(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, selfRel uint32, leaf []byte, insertAt int, rec []byte) (inobtSplit, error) {
	be := binary.BigEndian
	hdrSize := sb.agBTreeHdrSize()
	numrecs := int(be.Uint16(leaf[6:]))

	// Materialise the full, sorted record set (existing + new).
	total := numrecs + 1
	recs := make([][]byte, total)
	for i := 0; i < numrecs; i++ {
		r := make([]byte, inobtRecSize)
		copy(r, leaf[hdrSize+i*inobtRecSize:])
		recs[i] = r
	}
	nr := make([]byte, inobtRecSize)
	copy(nr, rec)
	copy(recs[insertAt+1:], recs[insertAt:])
	recs[insertAt] = nr

	half := total / 2 // left keeps the lower half, right gets the upper half

	rightAbs, err := allocAllocBlocks(rw, partOff, sb, ag, 1)
	if err != nil {
		return inobtSplit{}, fmt.Errorf("growInobt split: alloc right leaf: %w", err)
	}
	// Packed fsbno -> AG-relative via agblklog shift/mask, not /sb_agblocks.
	_, rightRel := sb.fsbToAgAgbno(rightAbs)

	oldRight := be.Uint32(leaf[12:]) // selfRel's current right sibling

	// Left half stays in selfRel; rightsib now points at the new block.
	left := leaf
	be.PutUint32(left[12:], rightRel)
	be.PutUint16(left[6:], uint16(half))
	for i := 0; i < half; i++ {
		copy(left[hdrSize+i*inobtRecSize:], recs[i])
	}
	for i := half; i < numrecs; i++ {
		clear(left[hdrSize+i*inobtRecSize : hdrSize+(i+1)*inobtRecSize])
	}
	if err := allocWriteAGBTree(rw, partOff, sb, ag, selfRel, left); err != nil {
		return inobtSplit{}, err
	}

	// Right half: a fresh level-0 block spliced between selfRel and oldRight.
	right := newInobtBlock(sb, ag, rightRel, 0, total-half)
	be.PutUint32(right[8:], selfRel)
	be.PutUint32(right[12:], oldRight)
	for i := half; i < total; i++ {
		copy(right[hdrSize+(i-half)*inobtRecSize:], recs[i])
	}
	if err := allocWriteAGBTree(rw, partOff, sb, ag, rightRel, right); err != nil {
		return inobtSplit{}, err
	}

	// Fix the old right sibling's leftsib to point at the new block.
	if oldRight != 0xFFFFFFFF {
		sib, err := allocReadAGBlock(rw, partOff, sb, ag, oldRight)
		if err != nil {
			return inobtSplit{}, err
		}
		be.PutUint32(sib[8:], rightRel)
		if err := allocWriteAGBTree(rw, partOff, sb, ag, oldRight, sib); err != nil {
			return inobtSplit{}, err
		}
	}

	return inobtSplit{split: true, rightKey: be.Uint32(recs[half][0:]), rightRel: rightRel}, nil
}

// inobtSplitNode splits a full interior node selfRel in two. The full key/ptr
// set is the node's existing separators with (newKey → newPtr) inserted at
// insertAt; the lower half stays in selfRel and the upper half moves to a
// newly-allocated right sibling. Returns the right sibling's first key so the
// parent can insert the separator.
func inobtSplitNode(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, selfRel uint32, node []byte, insertAt int, newKey, newPtr uint32) (inobtSplit, error) {
	be := binary.BigEndian
	hdrSize := sb.agBTreeHdrSize()
	numrecs := int(be.Uint16(node[6:]))
	maxInternal := inobtMaxInternal(len(node), hdrSize)
	ptrBase := hdrSize + maxInternal*inobtKeySize

	// Materialise the full, sorted key/ptr set (existing + new separator).
	total := numrecs + 1
	keys := make([]uint32, total)
	ptrs := make([]uint32, total)
	for i := 0; i < numrecs; i++ {
		keys[i] = be.Uint32(node[hdrSize+i*inobtKeySize:])
		ptrs[i] = be.Uint32(node[ptrBase+i*inobtPtrSize:])
	}
	copy(keys[insertAt+1:], keys[insertAt:])
	copy(ptrs[insertAt+1:], ptrs[insertAt:])
	keys[insertAt] = newKey
	ptrs[insertAt] = newPtr

	half := total / 2 // left keeps the lower half, right gets the upper half

	rightAbs, err := allocAllocBlocks(rw, partOff, sb, ag, 1)
	if err != nil {
		return inobtSplit{}, fmt.Errorf("growInobt split: alloc right node: %w", err)
	}
	// Packed fsbno -> AG-relative via agblklog shift/mask, not /sb_agblocks.
	_, rightRel := sb.fsbToAgAgbno(rightAbs)

	oldRight := be.Uint32(node[12:])

	// Left half stays in selfRel. Rewrite its keys/ptrs compactly and clear the
	// stale tail so xfs_repair doesn't see leftover separators.
	left := node
	be.PutUint32(left[12:], rightRel)
	be.PutUint16(left[6:], uint16(half))
	for i := 0; i < half; i++ {
		be.PutUint32(left[hdrSize+i*inobtKeySize:], keys[i])
		be.PutUint32(left[ptrBase+i*inobtPtrSize:], ptrs[i])
	}
	for i := half; i < numrecs; i++ {
		be.PutUint32(left[hdrSize+i*inobtKeySize:], 0)
		be.PutUint32(left[ptrBase+i*inobtPtrSize:], 0)
	}
	if err := allocWriteAGBTree(rw, partOff, sb, ag, selfRel, left); err != nil {
		return inobtSplit{}, err
	}

	// Right half: a fresh interior block at the same level, spliced into the
	// sibling chain.
	right := newInobtBlock(sb, ag, rightRel, int(be.Uint16(node[4:])), total-half)
	be.PutUint32(right[8:], selfRel)
	be.PutUint32(right[12:], oldRight)
	for i := half; i < total; i++ {
		be.PutUint32(right[hdrSize+(i-half)*inobtKeySize:], keys[i])
		be.PutUint32(right[ptrBase+(i-half)*inobtPtrSize:], ptrs[i])
	}
	if err := allocWriteAGBTree(rw, partOff, sb, ag, rightRel, right); err != nil {
		return inobtSplit{}, err
	}

	if oldRight != 0xFFFFFFFF {
		sib, err := allocReadAGBlock(rw, partOff, sb, ag, oldRight)
		if err != nil {
			return inobtSplit{}, err
		}
		be.PutUint32(sib[8:], rightRel)
		if err := allocWriteAGBTree(rw, partOff, sb, ag, oldRight, sib); err != nil {
			return inobtSplit{}, err
		}
	}

	return inobtSplit{split: true, rightKey: keys[half], rightRel: rightRel}, nil
}

// newInobtBlock returns a zeroed v5 inobt B-tree block (magic IAB3) with its
// header filled in: level, numrecs, null siblings, self blkno, uuid and owner.
// Callers that chain leaf blocks overwrite the sibling fields at blk[8:]/blk[12:].
func newInobtBlock(sb *superblock, ag, agRel uint32, level, numrecs int) []byte {
	be := binary.BigEndian
	blk := make([]byte, sb.blockSize)
	be.PutUint32(blk[0:], magicIAB3)
	be.PutUint16(blk[4:], uint16(level))
	be.PutUint16(blk[6:], uint16(numrecs))
	be.PutUint32(blk[8:], 0xFFFFFFFF)
	be.PutUint32(blk[12:], 0xFFFFFFFF)
	be.PutUint64(blk[16:], sb.fsbToPhysBlock(sb.agAbsBlock(ag, agRel))*fmtDaddrPerBlock)
	copy(blk[32:48], sb.uuid[:])
	be.PutUint32(blk[48:], ag)
	return blk
}
