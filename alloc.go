package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
	"io"
)

// AGF v5 field offsets (big-endian unless noted).
const (
	agfOffMagic    = 0
	agfOffSeqNo    = 8
	agfOffLength   = 12  // AG size in blocks
	agfOffBnoRoot  = 16  // bno B-tree root (AG-relative block)
	agfOffCntRoot  = 20  // cnt B-tree root
	agfOffBnoLevel = 28  // bno B-tree depth
	agfOffCntLevel = 32  // cnt B-tree depth
	agfOffFreeBlks = 52  // free block count
	agfOffLongest  = 56  // longest free run
	agfOffCRC      = 104 // __le32 v5 CRC
	agfStructSize  = 112
)

// AGI v5 field offsets.
const (
	agiOffMagic     = 0
	agiOffSeqNo     = 8
	agiOffLength    = 12
	agiOffCount     = 16  // allocated inode count
	agiOffRoot      = 20  // inobt root block (AG-relative)
	agiOffLevel     = 24  // inobt depth
	agiOffFreeCount = 28  // free inode count
	agiOffNewIno    = 32  // last allocated inode (AG-relative)
	agiOffCRC       = 320 // __le32 v5 CRC
	agiStructSize   = 348
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

// agfBlock reads the AGF block for allocation group ag.
func agfBlock(r io.ReaderAt, partOff int64, sb *superblock, ag uint32) ([]byte, error) {
	buf := make([]byte, sb.blockSize)
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
		updateCRC(buf, agfOffCRC, agfStructSize)
	}
	off := sb.agFByteOffset(partOff, ag)
	if _, err := rw.WriteAt(buf, off); err != nil {
		return fmt.Errorf("xfs: write AGF ag=%d: %w", ag, err)
	}
	return nil
}

// agiBlock reads the AGI block for allocation group ag.
func agiBlock(r io.ReaderAt, partOff int64, sb *superblock, ag uint32) ([]byte, error) {
	buf := make([]byte, sb.blockSize)
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
		updateCRC(buf, agiOffCRC, agiStructSize)
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

	// Update AGF free block count.
	freeBlks := be.Uint32(agfBuf[agfOffFreeBlks:])
	be.PutUint32(agfBuf[agfOffFreeBlks:], freeBlks-nBlocks)
	if remaining < be.Uint32(agfBuf[agfOffLongest:]) {
		be.PutUint32(agfBuf[agfOffLongest:], remaining)
	}
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

	// Update AGF free count.
	freeBlks := be.Uint32(agfBuf[agfOffFreeBlks:])
	be.PutUint32(agfBuf[agfOffFreeBlks:], freeBlks+nBlocks)
	longest := be.Uint32(agfBuf[agfOffLongest:])
	if nBlocks > longest {
		be.PutUint32(agfBuf[agfOffLongest:], nBlocks)
	}
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
		return 0, fmt.Errorf("xfs: allocInode ag=%d: %w", ag, err)
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
				return 0, nil, 0, fmt.Errorf("no free inodes in AG %d inobt", ag)
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
