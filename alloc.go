package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

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
	agfOffLength    = 12 // AG size in blocks
	agfOffBnoRoot   = 16 // agf_roots[0]: bno B-tree root (AG-relative block)
	agfOffCntRoot   = 20 // agf_roots[1]: cnt B-tree root
	agfOffBnoLevel  = 28 // agf_levels[0]: bno B-tree depth
	agfOffCntLevel  = 32 // agf_levels[1]: cnt B-tree depth
	agfOffFLFirst   = 40 // first valid AGFL index
	agfOffFLLast    = 44 // last valid AGFL index
	agfOffFLCount   = 48 // number of blocks in the free list
	agfOffFreeBlks  = 52 // free block count
	agfOffLongest   = 56 // longest free run
	agfOffBtreeBlks = 60 // blocks held by the free-space btrees
	agfOffUUID      = 64 // sb_uuid (16 bytes)
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

// agAbsBlock converts an AG-relative block number to an absolute FS block.
func (sb *superblock) agAbsBlock(ag, agRel uint32) uint64 {
	return uint64(ag)*uint64(sb.agBlocks) + uint64(agRel)
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
		fdblocks += uint64(be.Uint32(agf[agfOffFreeBlks:])) + uint64(be.Uint32(agf[agfOffFLCount:]))
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

// ──────────────────── Block allocation ─────────────────────────────────────

// allocBlocks allocates nBlocks contiguous blocks from allocation group ag
// using the cnt (free-space-by-count) B-tree. Returns the absolute
// filesystem block number of the first allocated block.
//
// Strategy: walk the cnt B-tree to the rightmost leaf (largest extents)
// and take the first record with count >= nBlocks. This is a simplified
// allocator; it handles images with ≤1 level-deep trees (typical for
// newly formatted cloud images with a single large free extent).
func allocBlocks(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, nBlocks uint32) (uint64, error) {
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
		// Delete the record from both B-trees.
		if err := allocBtreeDeleteRecord(rw, partOff, sb, ag, cntRoot, int(cntLevel), recIdx, cntLeafRel, cntLeaf, allocRecSize, false); err != nil {
			return 0, fmt.Errorf("xfs: cnt B-tree delete: %w", err)
		}
		if err := allocBnoDeleteRecord(rw, partOff, sb, ag, bnoRoot, int(bnoLevel), allocStartRel); err != nil {
			return 0, fmt.Errorf("xfs: bno B-tree delete: %w", err)
		}
	} else {
		// Update cnt record: new start+count pointing to the remainder.
		newStart := allocStartRel + nBlocks
		be.PutUint32(cntLeaf[recOff:], newStart)
		be.PutUint32(cntLeaf[recOff+4:], remaining)
		if err := allocWriteAGBTree(rw, partOff, sb, ag, cntLeafRel, cntLeaf); err != nil {
			return 0, err
		}
		// Update bno record for the same extent.
		if err := allocBnoUpdateRecord(rw, partOff, sb, ag, bnoRoot, int(bnoLevel), allocStartRel, newStart, remaining); err != nil {
			return 0, fmt.Errorf("xfs: bno B-tree update: %w", err)
		}
	}

	// Update AGF free block count and recompute the true longest free extent.
	// Lowering agf_longest to this extent's remainder is wrong when a smaller
	// extent (e.g. an aligned-allocation head gap) is consumed while larger
	// extents remain — it would understate agf_longest, eventually reaching 0.
	freeBlks := be.Uint32(agfBuf[agfOffFreeBlks:])
	be.PutUint32(agfBuf[agfOffFreeBlks:], freeBlks-nBlocks)
	longest, err := allocRecomputeLongest(rw, partOff, sb, ag, bnoRoot, int(bnoLevel))
	if err != nil {
		return 0, err
	}
	be.PutUint32(agfBuf[agfOffLongest:], longest)
	if err := allocWriteAGF(rw, partOff, sb, ag, agfBuf); err != nil {
		return 0, err
	}

	return sb.agAbsBlock(ag, allocStartRel), nil
}

// allocAlignedBlocks allocates need contiguous blocks in AG ag whose
// AG-relative start block is a multiple of align. Inode chunks need this:
// the chunk's start inode is startBlock*inopBlock, and xfs_repair / the kernel
// require it to be a multiple of XFS_INODES_PER_CHUNK (64). allocBlocks alone
// returns the first blocks of a free extent, which are only inopBlock-aligned,
// so a chunk could start at e.g. block 2637 → startino 21096 (21096 % 64 = 8),
// which xfs_repair rejects (it then maps inodes onto file-data blocks).
//
// It finds a free extent large enough to contain an aligned run, removes it
// from the bno/cnt B-trees, and reinserts the unaligned head and the trailing
// remainder as smaller free extents.
func allocAlignedBlocks(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, need, align uint32) (uint64, error) {
	if align <= 1 {
		return allocBlocks(rw, partOff, sb, ag, need)
	}
	be := binary.BigEndian
	agfBuf, err := allocAGFBlock(rw, partOff, sb, ag)
	if err != nil {
		return 0, err
	}
	cntRoot := be.Uint32(agfBuf[agfOffCntRoot:])
	cntLevel := be.Uint32(agfBuf[agfOffCntLevel:])
	bnoRoot := be.Uint32(agfBuf[agfOffBnoRoot:])
	bnoLevel := be.Uint32(agfBuf[agfOffBnoLevel:])

	// Slack of align-1 guarantees an aligned start exists inside the extent.
	want := need + align - 1
	cntLeafRel, cntLeaf, recIdx, err := allocCntFindBlock(rw, partOff, sb, ag, cntRoot, int(cntLevel), want)
	if err != nil {
		return 0, fmt.Errorf("xfs: allocAlignedBlocks ag=%d: %w", ag, err)
	}
	hdrSize := sb.agBTreeHdrSize()
	recOff := hdrSize + recIdx*allocRecSize
	startRel := be.Uint32(cntLeaf[recOff:])
	count := be.Uint32(cntLeaf[recOff+4:])

	alignedStart := ((startRel + align - 1) / align) * align
	headGap := alignedStart - startRel
	tailStart := alignedStart + need
	tailLen := (startRel + count) - tailStart // count >= need+align-1 >= headGap+need

	// Remove the whole extent from both B-trees.
	if err := allocBtreeDeleteRecord(rw, partOff, sb, ag, cntRoot, int(cntLevel), recIdx, cntLeafRel, cntLeaf, allocRecSize, false); err != nil {
		return 0, fmt.Errorf("xfs: aligned cnt delete: %w", err)
	}
	if err := allocBnoDeleteRecord(rw, partOff, sb, ag, bnoRoot, int(bnoLevel), startRel); err != nil {
		return 0, fmt.Errorf("xfs: aligned bno delete: %w", err)
	}
	// Reinsert the unaligned head and the trailing remainder as free extents.
	if headGap > 0 {
		if err := allocBnoInsertRecord(rw, partOff, sb, ag, bnoRoot, int(bnoLevel), startRel, headGap); err != nil {
			return 0, err
		}
		if err := allocCntInsertRecord(rw, partOff, sb, ag, cntRoot, int(cntLevel), startRel, headGap); err != nil {
			return 0, err
		}
	}
	if tailLen > 0 {
		if err := allocBnoInsertRecord(rw, partOff, sb, ag, bnoRoot, int(bnoLevel), tailStart, tailLen); err != nil {
			return 0, err
		}
		if err := allocCntInsertRecord(rw, partOff, sb, ag, cntRoot, int(cntLevel), tailStart, tailLen); err != nil {
			return 0, err
		}
	}

	// AGF: drop need blocks and recompute the true longest free extent.
	// Splitting an extent into head+tail can leave a remainder smaller than
	// some other free extent, so the conservative "lower-only" update is wrong
	// here (it once drove agf_longest to 0); scan the cnt B-tree for the real
	// maximum instead.
	freeBlks := be.Uint32(agfBuf[agfOffFreeBlks:])
	be.PutUint32(agfBuf[agfOffFreeBlks:], freeBlks-need)
	longest, err := allocRecomputeLongest(rw, partOff, sb, ag, cntRoot, int(cntLevel))
	if err != nil {
		return 0, err
	}
	be.PutUint32(agfBuf[agfOffLongest:], longest)
	if err := allocWriteAGF(rw, partOff, sb, ag, agfBuf); err != nil {
		return 0, err
	}

	return sb.agAbsBlock(ag, alignedStart), nil
}

var allocAlignedAllocBlocks = allocAlignedBlocks

// agfRecomputeLongest returns the size of the largest free extent in AG ag by
// scanning every leaf of the cnt B-tree. Used to keep agf_longest accurate
// after operations (like aligned allocation) that can split a free extent into
// remainders smaller than other free extents.
func agfRecomputeLongest(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, cntRoot uint32, cntLevel int) (uint32, error) {
	hdrSize := sb.agBTreeHdrSize()
	agRel := cntRoot
	// Descend leftmost pointers to the first leaf.
	for {
		blk, err := allocReadAGBlock(rw, partOff, sb, ag, agRel)
		if err != nil {
			return 0, err
		}
		if int(binary.BigEndian.Uint16(blk[4:])) == 0 {
			// Leaf: scan this block and all right siblings for the max count.
			var longest uint32
			for {
				numrecs := int(binary.BigEndian.Uint16(blk[6:]))
				for i := 0; i < numrecs; i++ {
					c := binary.BigEndian.Uint32(blk[hdrSize+i*allocRecSize+4:])
					if c > longest {
						longest = c
					}
				}
				rsib := binary.BigEndian.Uint32(blk[12:])
				if rsib == 0xFFFFFFFF {
					return longest, nil
				}
				blk, err = allocReadAGBlock(rw, partOff, sb, ag, rsib)
				if err != nil {
					return 0, err
				}
			}
		}
		// Internal node: follow the leftmost child pointer.
		numrecs := int(binary.BigEndian.Uint16(blk[6:]))
		agRel = binary.BigEndian.Uint32(blk[hdrSize+numrecs*allocKeySize:])
	}
}

// cntFindBlock navigates the cnt B-tree to a leaf containing a record with
// count >= need. Returns (AG-relative leaf block, block data, record index, err).
func cntFindBlock(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, need uint32) (uint32, []byte, int, error) {
	hdrSize := sb.agBTreeHdrSize()
	agRel := rootRel
	var blk []byte
	var err error

	for {
		blk, err = allocReadAGBlock(rw, partOff, sb, ag, agRel)
		if err != nil {
			return 0, nil, 0, err
		}
		lvl := int(binary.BigEndian.Uint16(blk[4:]))
		numrecs := int(binary.BigEndian.Uint16(blk[6:]))

		if lvl == 0 {
			// At a leaf: find the first record with count >= need.
			for i := 0; i < numrecs; i++ {
				off := hdrSize + i*allocRecSize
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
		// Internal node: follow the rightmost pointer (largest counts on right).
		ptrOff := hdrSize + numrecs*allocKeySize
		lastPtr := ptrOff + (numrecs-1)*allocPtrSize
		if lastPtr+4 > len(blk) {
			return 0, nil, 0, fmt.Errorf("cnt B-tree block too small")
		}
		agRel = binary.BigEndian.Uint32(blk[lastPtr:])
		level--
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

// bnoDeleteRecord walks the bno B-tree and deletes the record that starts at
// agStartRel.
func bnoDeleteRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, agStartRel uint32) error {
	leafRel, leaf, recIdx, err := allocBnoFindRecord(rw, partOff, sb, ag, rootRel, level, agStartRel)
	if err != nil {
		return err
	}
	return allocBtreeDeleteRecord(rw, partOff, sb, ag, rootRel, level, recIdx, leafRel, leaf, allocRecSize, false)
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
		ptrOff := hdrSize + numrecs*allocKeySize + child*allocPtrSize
		agRel = binary.BigEndian.Uint32(blk[ptrOff:])
		level--
	}
}

// freeBlocks returns a run of nBlocks blocks starting at absStartBlock back
// to the free space B-trees. Only handles the simple case where the freed
// range does not merge with adjacent extents.
func freeBlocks(rw readerWriterAt, partOff int64, sb *superblock, absStartBlock uint64, nBlocks uint32) error {
	ag := uint32(absStartBlock / uint64(sb.agBlocks))
	agStartRel := uint32(absStartBlock % uint64(sb.agBlocks))

	agfBuf, err := allocAGFBlock(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	be := binary.BigEndian

	bnoRoot := be.Uint32(agfBuf[agfOffBnoRoot:])
	cntRoot := be.Uint32(agfBuf[agfOffCntRoot:])
	bnoLevel := int(be.Uint32(agfBuf[agfOffBnoLevel:]))
	cntLevel := int(be.Uint32(agfBuf[agfOffCntLevel:]))

	// Insert into bno B-tree.
	if err := allocBnoInsertRecord(rw, partOff, sb, ag, bnoRoot, bnoLevel, agStartRel, nBlocks); err != nil {
		return fmt.Errorf("xfs: freeBlocks bno insert: %w", err)
	}
	// Insert into cnt B-tree.
	if err := allocCntInsertRecord(rw, partOff, sb, ag, cntRoot, cntLevel, agStartRel, nBlocks); err != nil {
		return fmt.Errorf("xfs: freeBlocks cnt insert: %w", err)
	}

	// Update AGF free count and recompute the longest free extent. (This code
	// path does not merge with adjacent free extents, so a recompute keeps
	// agf_longest exact rather than a lower bound.)
	freeBlks := be.Uint32(agfBuf[agfOffFreeBlks:])
	be.PutUint32(agfBuf[agfOffFreeBlks:], freeBlks+nBlocks)
	longest, err := allocRecomputeLongest(rw, partOff, sb, ag, bnoRoot, bnoLevel)
	if err != nil {
		return err
	}
	be.PutUint32(agfBuf[agfOffLongest:], longest)
	return allocWriteAGF(rw, partOff, sb, ag, agfBuf)
}

// bnoInsertRecord inserts (agStartRel, nBlocks) into the bno B-tree leaf.
// Fails if the leaf is full (would require a split).
func bnoInsertRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, agStartRel, nBlocks uint32) error {
	return allocBTreeInsert(rw, partOff, sb, ag, rootRel, level, agStartRel, nBlocks, true)
}

// cntInsertRecord inserts (agStartRel, nBlocks) into the cnt B-tree leaf,
// sorted by count.
func cntInsertRecord(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, rootRel uint32, level int, agStartRel, nBlocks uint32) error {
	return allocBTreeInsert(rw, partOff, sb, ag, rootRel, level, agStartRel, nBlocks, false)
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
		ptrOff := hdrSize + numrecs*allocKeySize + child*allocPtrSize
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
		// Follow leftmost child.
		ptrOff := hdrSize + numrecs*inobtKeySize
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
		ptrOff := hdrSize + numrecs*inobtKeySize + child*inobtPtrSize
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
// Limitations of this initial implementation: only single-leaf inobts are
// supported (inobtLevel must be 1, i.e. the root is the leaf). A future
// extension will split the leaf when it fills (~252 chunks/AG, plenty of
// headroom before that ceiling).
func growInobt(rw readerWriterAt, partOff int64, sb *superblock, ag uint32) error {
	be := binary.BigEndian

	// blocksPerChunk = 64 / inopBlock (typically 64/8 = 8). Guard inopBlock
	// up front: division by zero would panic, and any inopBlock > 64 would
	// produce blocksPerChunk == 0 (so we can't back the chunk at all).
	if sb.inopBlock == 0 || sb.inopBlock > inobtChunkInodes {
		return fmt.Errorf("xfs: growInobt: invalid inopBlock %d", sb.inopBlock)
	}
	blocksPerChunk := uint32(inobtChunkInodes) / uint32(sb.inopBlock)

	// Allocate the backing blocks, aligned so the chunk's start inode is a
	// multiple of XFS_INODES_PER_CHUNK (64). startIno = startBlock*inopBlock,
	// so the start block must be a multiple of blocksPerChunk (= 64/inopBlock);
	// an unaligned chunk yields a non-64-aligned startino that xfs_repair
	// rejects. Best-effort same-AG; the caller (allocInode) retries other AGs.
	absStart, err := allocAlignedAllocBlocks(rw, partOff, sb, ag, blocksPerChunk, blocksPerChunk)
	if err != nil {
		return fmt.Errorf("growInobt: alloc %d blocks: %w", blocksPerChunk, err)
	}

	// Compute the chunk's AG-relative starting inode number. startIno must
	// not overlap the existing root chunk's 64-wide window; in practice
	// the bno/cnt allocator hands out blocks well past the root inode block
	// so startIno >= 56 ≥ 48+8 always, but the absolute spec window is 64.
	agBlockBase := uint32(absStart % uint64(sb.agBlocks))
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

	// Insert the new record into the inobt leaf.
	agiBuf, err := allocAGIBlock(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	inobtRoot := be.Uint32(agiBuf[agiOffRoot:])
	inobtLevel := int(be.Uint32(agiBuf[agiOffLevel:]))
	if inobtLevel != 1 {
		return fmt.Errorf("growInobt: inobt level %d (>1) not supported yet", inobtLevel)
	}

	leafRel := inobtRoot
	leaf, err := allocReadAGBlock(rw, partOff, sb, ag, leafRel)
	if err != nil {
		return err
	}
	hdrSize := sb.agBTreeHdrSize()
	numrecs := int(be.Uint16(leaf[6:]))
	maxRecs := (len(leaf) - hdrSize) / inobtRecSize
	if numrecs >= maxRecs {
		return fmt.Errorf("growInobt: inobt leaf full (%d records); leaf-split not implemented", numrecs)
	}

	// Find the insertion position (sorted by startIno).
	insertAt := numrecs
	for i := 0; i < numrecs; i++ {
		off := hdrSize + i*inobtRecSize
		existingStart := be.Uint32(leaf[off:])
		if startIno < existingStart {
			insertAt = i
			break
		}
		if startIno == existingStart {
			return fmt.Errorf("growInobt: inobt already has a record at startIno %d", startIno)
		}
	}

	// Shift records right.
	insertOff := hdrSize + insertAt*inobtRecSize
	copy(leaf[insertOff+inobtRecSize:], leaf[insertOff:hdrSize+numrecs*inobtRecSize])

	// Write the new record: startIno, freeCount=64, irFree=all-ones.
	be.PutUint32(leaf[insertOff:], startIno)
	be.PutUint32(leaf[insertOff+4:], inobtChunkInodes)
	be.PutUint64(leaf[insertOff+8:], 0xFFFFFFFFFFFFFFFF)
	be.PutUint16(leaf[6:], uint16(numrecs+1))

	if err := allocWriteAGBTree(rw, partOff, sb, ag, leafRel, leaf); err != nil {
		return err
	}

	// Update AGI: +64 to count + freeCount.
	agiCount := be.Uint32(agiBuf[agiOffCount:])
	agiFree := be.Uint32(agiBuf[agiOffFreeCount:])
	be.PutUint32(agiBuf[agiOffCount:], agiCount+inobtChunkInodes)
	be.PutUint32(agiBuf[agiOffFreeCount:], agiFree+inobtChunkInodes)
	return allocWriteAGI(rw, partOff, sb, ag, agiBuf)
}
