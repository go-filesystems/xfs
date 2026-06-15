package filesystem_xfs

// dirleaf.go — leaf-form and node-form directory writer.
//
// A short-form directory lives inline in the inode; a "block-form" directory
// fits every entry plus its hash index in a single directory block (see
// dir.go / write.go). When a directory outgrows one block (~127 small
// entries) XFS promotes it to "leaf form": the directory entries spread across
// one or more dedicated *data* blocks (logical offsets [0, 32 GiB)) while the
// hash index moves into a single *leaf* block (logical offset 32 GiB). When
// the hash index itself outgrows one block the directory becomes "node form":
// the index splits across several *leafn* blocks indexed by a da-btree
// internal node, and the per-data-block free-space hints move into dedicated
// *free* blocks (logical offset 64 GiB).
//
// This file implements writing all three on-disk shapes. To keep the entry
// data, the hash index, the free-space bests array and the da-btree mutually
// consistent (which is what xfs_repair validates) the directory is laid out
// from scratch on every mutation via rewriteDirEntries — the same
// rebuild-the-whole-thing strategy buildBlockDirBlock already uses for block
// form, extended across multiple blocks.
//
// Only the directory geometry our own Format() produces is handled: a single
// filesystem block per directory block (sb_dirblklog == 0). Images created by
// mkfs.xfs with a larger directory block size are read fine elsewhere but are
// not promoted here.

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// Directory leaf/node/free on-disk magic numbers. Leaf and node blocks begin
// with an xfs_da{,3}_blkinfo header whose magic is a 16-bit field at byte
// offset 8; data and free blocks carry a 32-bit magic at offset 0.
const (
	magicDir3Leaf1 uint16 = 0x3DF1 // v5 leaf1 (single-leaf index)
	magicDir2Leaf1 uint16 = 0xD2F1 // v4 leaf1
	magicDir3Leafn uint16 = 0x3DFF // v5 leafn (node-form leaf)
	magicDir2Leafn uint16 = 0xD2FF // v4 leafn
	magicDa3Node   uint16 = 0x3EBE // v5 da-btree internal node
	magicDaNode    uint16 = 0xFEBE // v4 da-btree internal node

	magicDir3Free uint32 = 0x58444633 // "XDF3" — v5 free block
	magicDir2Free uint32 = 0x58443246 // "XD2F" — v4 free block

	// Sentinels.
	dir2NullDataPtr uint32 = 0      // stale/empty leaf-index slot
	dir2NullDataOff uint16 = 0xFFFF // absent data block in a bests array

	// Header sizes (v5 / v4) for the various directory block kinds.
	dir3LeafHdrSize = 64
	dir2LeafHdrSize = 16
	dir3FreeHdrSize = 64
	dir2FreeHdrSize = 16
	dir3NodeHdrSize = 64
	dir2NodeHdrSize = 16

	// XFS_DIR2_DATA_ALIGN_LOG: directory data pointers are byte offsets >> 3.
	dirDataAlignLog = 3
)

// dirLeafHdrSize returns the leaf-block header size (shared by leaf1 and leafn).
func dirLeafHdrSize(hasCRC bool) int {
	if hasCRC {
		return dir3LeafHdrSize
	}
	return dir2LeafHdrSize
}

// dirFreeHdrSize returns the free-block header size.
func dirFreeHdrSize(hasCRC bool) int {
	if hasCRC {
		return dir3FreeHdrSize
	}
	return dir2FreeHdrSize
}

// dirNodeHdrSize returns the da-btree node header size.
func dirNodeHdrSize(hasCRC bool) int {
	if hasCRC {
		return dir3NodeHdrSize
	}
	return dir2NodeHdrSize
}

// dirLeafLogBlock returns the logical (directory) block index at which the leaf
// address space begins: XFS_DIR2_LEAF_OFFSET (32 GiB) divided by the directory
// block size. For our geometry the directory block size equals the FS block.
func dirLeafLogBlock(sb *superblock) uint64 {
	return dirLeafByteOffset / uint64(sb.blockSize)
}

// dirFreeLogBlock returns the logical block index at which the free-block
// address space begins: XFS_DIR2_FREE_OFFSET (64 GiB) divided by the block size.
func dirFreeLogBlock(sb *superblock) uint64 {
	return (dirLeafByteOffset * 2) / uint64(sb.blockSize)
}

// placedEnt records where a single directory entry was laid down in the data
// address space: its data-block index and byte offset within that block.
type placedEnt struct {
	db  int
	off int
}

// placeDirEntries packs `all` (which must start with "." and "..") into
// fixed-size data blocks, returning each entry's placement, the number of data
// blocks used, and the trailing free-region length of every data block. Each
// block carries only entries (no trailing hash index); the gap after the last
// entry is the block's sole free region. Because the header, every entry and
// the block size are all 8-byte multiples, every free length is 0 or ≥ 8.
func placeDirEntries(all []dirEnt, hdrSize, blkSize int, hasFType bool) (placed []placedEnt, nData int, freeLen []int) {
	placed = make([]placedEnt, 0, len(all))
	db := 0
	off := hdrSize
	for _, e := range all {
		sz := dirEntrySize(len(e.name), hasFType)
		if off+sz > blkSize {
			freeLen = append(freeLen, blkSize-off)
			db++
			off = hdrSize
		}
		placed = append(placed, placedEnt{db: db, off: off})
		off += sz
	}
	freeLen = append(freeLen, blkSize-off)
	return placed, db + 1, freeLen
}

// dirEntDataptr converts a (data block, byte offset) pair into the dir2 data
// pointer stored in leaf-index entries: the byte offset within the whole data
// address space, divided by 8.
func dirEntDataptr(db, off, blkSize int) uint32 {
	return uint32((uint64(db)*uint64(blkSize) + uint64(off)) >> dirDataAlignLog)
}

// putDirDataHdr writes the header of a leaf/node-form *data* block (magic
// XDD3/XD2D, no trailing hash index).
func putDirDataHdr(sb *superblock, blk []byte, absBlock, ownerIno uint64) {
	be := binary.BigEndian
	if sb.hasCRC {
		be.PutUint32(blk[0:], magicDir3Data)
		be.PutUint64(blk[8:], absBlock*fmtDaddrPerBlock) // blkno (512-byte units)
		copy(blk[24:40], sb.uuid[:])
		be.PutUint64(blk[40:], ownerIno)
	} else {
		be.PutUint32(blk[0:], magicDir2Data)
	}
}

// putDataBestfree records a data block's single free region in bestfree[0].
func putDataBestfree(sb *superblock, blk []byte, freeOff, freeLen int) {
	if freeLen == 0 {
		return
	}
	be := binary.BigEndian
	bfOff := 48 // dir3 header: bestfree[3] at offset 48
	if !sb.hasCRC {
		bfOff = 4 // dir2 header: bestfree[3] right after the magic
	}
	be.PutUint16(blk[bfOff:], uint16(freeOff))
	be.PutUint16(blk[bfOff+2:], uint16(freeLen))
}

// markDataFreeRegion stamps the trailing free region of a data block as an
// xfs_dir2_data_unused entry (freetag + length + trailing tag).
func markDataFreeRegion(blk []byte, freeOff, freeLen int) {
	if freeLen == 0 {
		return
	}
	be := binary.BigEndian
	be.PutUint16(blk[freeOff:], dirFreeTag)
	be.PutUint16(blk[freeOff+2:], uint16(freeLen))
	be.PutUint16(blk[freeOff+freeLen-2:], uint16(freeOff)) // tag = own offset
}

// dirDataPlan is the placement of directory entries across data blocks,
// computed independently of where those blocks are physically allocated: the
// leaf-index records use logical data pointers and the bests are free lengths,
// so only the data-block bytes (their on-disk headers) actually depend on the
// physical block numbers.
type dirDataPlan struct {
	placed  []placedEnt
	nData   int
	freeLen []int          // per data block: trailing free length
	leaves  []leafIndexEnt // hash index over every entry (incl. "." / "..")
}

// planDirData packs `all` (which must begin with "." and "..") into data blocks
// and builds the hash index, without touching physical block numbers.
func planDirData(sb *superblock, all []dirEnt) dirDataPlan {
	blkSize := int(sb.blockSize)
	placed, nData, freeLen := placeDirEntries(all, dirDataHdrSize(sb.hasCRC), blkSize, sb.hasFType)
	leaves := make([]leafIndexEnt, len(all))
	for i, e := range all {
		leaves[i] = leafIndexEnt{
			hash: xfsDirHash([]byte(e.name)),
			addr: dirEntDataptr(placed[i].db, placed[i].off, blkSize),
		}
	}
	return dirDataPlan{placed: placed, nData: nData, freeLen: freeLen, leaves: leaves}
}

// renderDataBlocks materialises the nData data blocks for a plan given the
// absolute block number of the first (data blocks are contiguous).
func renderDataBlocks(sb *superblock, ownerIno, dataAbs uint64, all []dirEnt, plan dirDataPlan) []byte {
	blkSize := int(sb.blockSize)
	data := make([]byte, plan.nData*blkSize)
	for db := 0; db < plan.nData; db++ {
		blk := data[db*blkSize : (db+1)*blkSize]
		putDirDataHdr(sb, blk, dataAbs+uint64(db), ownerIno)
		fl := plan.freeLen[db]
		freeOff := blkSize - fl
		markDataFreeRegion(blk, freeOff, fl)
		putDataBestfree(sb, blk, freeOff, fl)
	}
	for i, e := range all {
		p := plan.placed[i]
		writeDirEntry(data[p.db*blkSize:(p.db+1)*blkSize], p.off, e.ino, e.name, e.ftype, sb.hasFType)
	}
	if sb.hasCRC {
		for db := 0; db < plan.nData; db++ {
			updateCRC(data[db*blkSize:(db+1)*blkSize], 4, blkSize) // dir3_blk_hdr.crc
		}
	}
	return data
}

// buildDirDataBlocks renders the data blocks plus the (unsorted) hash index and
// per-block best-free lengths for a leaf/node-form directory. `all` must begin
// with "." and ".."; dataAbs is the first data block's absolute FS block number.
func buildDirDataBlocks(sb *superblock, ownerIno, dataAbs uint64, all []dirEnt) (data []byte, leaves []leafIndexEnt, bests []int) {
	plan := planDirData(sb, all)
	return renderDataBlocks(sb, ownerIno, dataAbs, all, plan), plan.leaves, plan.freeLen
}

// leafIndexEnt is one xfs_dir2_leaf_entry: a name hash and the data pointer of
// the entry it indexes.
type leafIndexEnt struct {
	hash uint32
	addr uint32
}

// sortLeafIndex orders leaf-index entries by hash, ties broken by address —
// the ordering the kernel and xfs_repair require.
func sortLeafIndex(leaves []leafIndexEnt) {
	sort.Slice(leaves, func(i, j int) bool {
		if leaves[i].hash != leaves[j].hash {
			return leaves[i].hash < leaves[j].hash
		}
		return leaves[i].addr < leaves[j].addr
	})
}

// putDaBlkInfo writes the xfs_da{,3}_blkinfo header common to leaf and node
// blocks (forw/back left zero) and returns the byte offset just past it.
func putDaBlkInfo(sb *superblock, blk []byte, magic uint16, absBlock, ownerIno uint64) {
	be := binary.BigEndian
	be.PutUint16(blk[8:], magic) // magic at offset 8 in both v4 and v5
	if sb.hasCRC {
		be.PutUint64(blk[16:], absBlock*fmtDaddrPerBlock) // blkno
		copy(blk[32:48], sb.uuid[:])
		be.PutUint64(blk[48:], ownerIno) // owner
	}
}

// buildLeaf1Block builds the single leaf-index block (xfs_dir3_leaf1) for a
// leaf-form directory: header, the sorted hash index, the per-data-block bests
// array and the xfs_dir2_leaf_tail.
func buildLeaf1Block(sb *superblock, ownerIno, leafAbs uint64, leaves []leafIndexEnt, bests []int) []byte {
	be := binary.BigEndian
	blkSize := int(sb.blockSize)
	blk := make([]byte, blkSize)

	magic := magicDir3Leaf1
	if !sb.hasCRC {
		magic = magicDir2Leaf1
	}
	putDaBlkInfo(sb, blk, magic, leafAbs, ownerIno)

	hdrSize := dirLeafHdrSize(sb.hasCRC)
	// count + stale live right after the blkinfo header.
	cntOff := hdrSize - 8 // v5: 56, v4: 8
	if sb.hasCRC {
		cntOff = 56
	} else {
		cntOff = 12
	}
	be.PutUint16(blk[cntOff:], uint16(len(leaves)))
	be.PutUint16(blk[cntOff+2:], 0) // stale

	sortLeafIndex(leaves)
	for i, le := range leaves {
		p := hdrSize + i*8
		be.PutUint32(blk[p:], le.hash)
		be.PutUint32(blk[p+4:], le.addr)
	}

	// xfs_dir2_leaf_tail.bestcount is the last 4 bytes; the bests array (one
	// __be16 per data block) sits immediately before it.
	bestcount := len(bests)
	tailOff := blkSize - 4
	be.PutUint32(blk[tailOff:], uint32(bestcount))
	bestsOff := tailOff - bestcount*2
	for i, b := range bests {
		be.PutUint16(blk[bestsOff+i*2:], uint16(b))
	}

	if sb.hasCRC {
		updateCRC(blk, 12, blkSize) // da3_blkinfo.crc at offset 12
	}
	return blk
}

// allocDirRun allocates nBlocks contiguous FS blocks for directory metadata,
// preferring the directory inode's own AG and falling back to the others.
func allocDirRun(rw readerWriterAt, partOff int64, sb *superblock, ino uint64, nBlocks uint32) (uint64, error) {
	ag := inoAG(sb, ino)
	abs, err := writeAllocBlocks(rw, partOff, sb, ag, nBlocks)
	if err == nil {
		return abs, nil
	}
	for a := uint32(0); a < sb.agCount; a++ {
		if a == ag {
			continue
		}
		if abs, err = writeAllocBlocks(rw, partOff, sb, a, nBlocks); err == nil {
			return abs, nil
		}
	}
	return 0, fmt.Errorf("xfs: no space for %d directory blocks", nBlocks)
}

// dirRegion is one contiguous run of directory blocks at a fixed logical
// offset, with a builder that renders the run's bytes once its physical block
// number is known.
type dirRegion struct {
	logicalOff uint64
	count      uint32
	abs        uint64
	build      func(abs uint64) ([]byte, error)
}

// setDirInode points a directory inode at a fresh extent layout and updates its
// size/block/extent counts. di_size for a dir2 directory counts only the data
// address space (nData data blocks); nblocks counts every block.
func setDirInode(dirIn *inode, exts []extent, nData, nBlocks uint32, blkSize int) error {
	// Clear the whole extent area so stale records from a larger previous
	// layout don't linger past di_nextents.
	clear(dirIn.dataFork())
	setInodeFormat(dirIn, inodeFmtExtents)
	setInodeSize(dirIn, uint64(nData)*uint64(blkSize))
	setInodeNBlocks(dirIn, uint64(nBlocks))
	setInodeNExtents(dirIn, uint32(len(exts)))
	return writeWriteExtentList(dirIn, exts)
}

// rewriteDirEntries lays directory `dirIn` out so it holds exactly `entries`
// (synthetic "." and ".." are added here), choosing block, leaf or node form by
// how large the entry data and hash index grow. When the layout shape is
// unchanged from the current one the existing blocks are reused in place;
// otherwise they are freed and fresh runs allocated. The caller guarantees
// `entries` excludes "." and "..".
func rewriteDirEntries(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode, parentIno uint64, entries []dirEnt) error {
	if sb.dirFSBlocks() != 1 {
		return fmt.Errorf("xfs: directory %d: leaf-form writer requires single-block dir geometry", dirIn.num)
	}
	blkSize := int(sb.blockSize)

	all := make([]dirEnt, 0, len(entries)+2)
	all = append(all, dirEnt{".", dirIn.num, 2}, dirEnt{"..", parentIno, 2})
	all = append(all, entries...)

	plan := planDirData(sb, all)
	nData := plan.nData

	// Collapse to a single block when everything (entries + hash index + tail)
	// fits one block again — matches how XFS shrinks a directory on deletes.
	if nData == 1 && blockFormFits(sb, all, blkSize) {
		region := dirRegion{logicalOff: 0, count: 1, build: func(abs uint64) ([]byte, error) {
			blk := make([]byte, blkSize)
			return blk, buildBlockDirBlock(sb, blk, abs, dirIn.num, parentIno, entries)
		}}
		return layoutDir(rw, partOff, sb, dirIn, []dirRegion{region}, 1, blkSize)
	}

	sortLeafIndex(plan.leaves)
	dataRegion := dirRegion{logicalOff: 0, count: uint32(nData), build: func(abs uint64) ([]byte, error) {
		return renderDataBlocks(sb, dirIn.num, abs, all, plan), nil
	}}

	leafCap := blkSize - dirLeafHdrSize(sb.hasCRC) - 4 /* tail */ - nData*2 /* bests */
	if len(plan.leaves)*8 <= leafCap {
		leafRegion := dirRegion{logicalOff: dirLeafLogBlock(sb), count: 1, build: func(abs uint64) ([]byte, error) {
			return buildLeaf1Block(sb, dirIn.num, abs, plan.leaves, plan.freeLen), nil
		}}
		return layoutDir(rw, partOff, sb, dirIn, []dirRegion{dataRegion, leafRegion}, nData, blkSize)
	}

	regions := append([]dirRegion{dataRegion}, nodeFormRegions(sb, dirIn.num, plan.leaves, plan.freeLen, nData)...)
	return layoutDir(rw, partOff, sb, dirIn, regions, nData, blkSize)
}

// blockFormFits reports whether `all` (entries including "." / "..") fits a
// single block-form directory block: header + data entries + leaf index + tail.
func blockFormFits(sb *superblock, all []dirEnt, blkSize int) bool {
	need := dirDataHdrSize(sb.hasCRC) + 8 /* xfs_dir2_block_tail */ + len(all)*8 /* leaf */
	for _, e := range all {
		need += dirEntrySize(len(e.name), sb.hasFType)
	}
	return need <= blkSize
}

// layoutDir resolves physical blocks for `regions`, writes their bytes, and
// repoints the inode. Reuse: when the directory's current extents exactly match
// the requested {logicalOff,count} runs the same physical blocks are rewritten
// in place — this keeps the AG free-space B-trees from churning (and eventually
// overflowing) as a directory grows one entry at a time. Otherwise every
// current block is freed and fresh runs allocated.
func layoutDir(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode, regions []dirRegion, nData, blkSize int) error {
	cur, err := writeDirExtents(rw, partOff, sb, dirIn)
	if err != nil {
		return err
	}
	reuse := len(cur) == len(regions)
	if reuse {
		for i := range regions {
			abs, ok := matchExtent(cur, regions[i])
			if !ok {
				reuse = false
				break
			}
			regions[i].abs = abs
		}
	}
	if !reuse {
		for _, e := range cur {
			if err := writeFreeBlocks(rw, partOff, sb, e.startBlock, e.count); err != nil {
				return err
			}
		}
		for i := range regions {
			abs, err := allocDirRun(rw, partOff, sb, dirIn.num, regions[i].count)
			if err != nil {
				return err
			}
			regions[i].abs = abs
		}
	}

	exts := make([]extent, len(regions))
	var nBlocks uint32
	for i := range regions {
		bytes, err := regions[i].build(regions[i].abs)
		if err != nil {
			return err
		}
		if err := writeWriteBlocksData(rw, partOff, sb, regions[i].abs, regions[i].count, bytes); err != nil {
			return err
		}
		exts[i] = extent{startOff: regions[i].logicalOff, startBlock: regions[i].abs, count: regions[i].count}
		nBlocks += regions[i].count
	}
	if err := setDirInode(dirIn, exts, uint32(nData), nBlocks, blkSize); err != nil {
		return err
	}
	return writeWriteInode(rw, partOff, sb, dirIn)
}

// matchExtent finds a current extent matching a region's logical offset and
// block count, returning its physical start block.
func matchExtent(cur []extent, r dirRegion) (uint64, bool) {
	for _, e := range cur {
		if e.startOff == r.logicalOff && e.count == r.count {
			return e.startBlock, true
		}
	}
	return 0, false
}

// nodeFormRegions is implemented in dirnode.go.
