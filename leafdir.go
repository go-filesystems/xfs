package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"sort"
)

// Leaf-form directories. When a directory outgrows a single block ("block
// form"), XFS spreads its entries across several dir3 DATA blocks (magic XDD3,
// at logical blocks 0..D-1) and moves the name→entry hash index into a
// separate "leaf" block (magic 0x3df1, at the logical 32 GiB offset). This file
// implements writing that form and the unified rebuild path that chooses
// between block, leaf and node form. Node form (~500+ entries) replaces the
// single leaf1 block with a da3-node btree over multiple leafN blocks plus a
// free-index block; only single-level da nodes and a single free-index block
// are emitted (enough for directories up to ~250k entries).

// gatherDirEntries returns every real entry (excluding "." and "..") of a
// directory plus its parent inode, working for short-form, block-form and
// leaf-form directories.
func gatherDirEntries(rw readerWriterAt, partOff int64, sb *superblock, in *inode) ([]dirEnt, uint64, error) {
	be := binary.BigEndian
	var ents []dirEnt
	var parent uint64

	if in.format == inodeFmtLocal {
		fork := in.dataFork()
		if len(fork) < 6 {
			return nil, 0, nil
		}
		if fork[1] > 0 {
			parent = be.Uint64(fork[2:])
		} else {
			parent = uint64(be.Uint32(fork[2:]))
		}
		des, err := writeSFReadDir(fork, sb.hasFType)
		if err != nil {
			return nil, 0, err
		}
		for _, d := range des {
			if d.Name != "." && d.Name != ".." {
				ents = append(ents, dirEnt{d.Name, d.Inode, d.FileType})
			}
		}
		return ents, parent, nil
	}

	exts, err := writeDirExtents(rw, partOff, sb, in)
	if err != nil {
		return nil, 0, err
	}
	leafLog := dirLeafByteOffset / uint64(sb.blockSize)
	firstData := true
	for _, e := range exts {
		if e.startOff >= leafLog {
			continue // leaf / free-index blocks, not data
		}
		for b := uint32(0); b < e.count; b++ {
			blk, err := writeReadRawBlock(rw, partOff, sb, e.startBlock+uint64(b))
			if err != nil {
				return nil, 0, err
			}
			if firstData {
				parent = blockDirParent(blk, sb.hasFType, sb.hasCRC)
				firstData = false
			}
			for _, d := range parseDirBlock(blk, sb.hasFType, sb.hasCRC) {
				ents = append(ents, dirEnt{d.Name, d.Inode, d.FileType})
			}
		}
	}
	return ents, parent, nil
}

// dirBlockLayout returns the directory's current data-block list and whether it
// has a leaf/index block, from its extent map.
func dirBlockLayout(rw readerWriterAt, partOff int64, sb *superblock, in *inode) (dataBlocks []uint64, hasLeaf bool, err error) {
	if in.format == inodeFmtLocal {
		return nil, false, nil
	}
	exts, err := writeDirExtents(rw, partOff, sb, in)
	if err != nil {
		return nil, false, err
	}
	leafLog := dirLeafByteOffset / uint64(sb.blockSize)
	for _, e := range exts {
		if e.startOff >= leafLog {
			hasLeaf = true
			continue
		}
		for b := uint32(0); b < e.count; b++ {
			dataBlocks = append(dataBlocks, e.startBlock+uint64(b))
		}
	}
	return dataBlocks, hasLeaf, nil
}

// writeWholeDir rebuilds directory in from the complete entry set (excluding
// "." and ".."), laying it out as block form when everything fits one block,
// otherwise leaf form. A block-form directory that stays single-block is
// rebuilt in place (no allocation); conversions, growth and shrink free the
// current blocks and allocate fresh.
func writeWholeDir(rw readerWriterAt, partOff int64, sb *superblock, in *inode, parentIno uint64, entries []dirEnt) error {
	blkSize := int(sb.blockSize)
	hdr := dirDataHdrSize(sb.hasCRC)
	// Block-form footprint: header + "."/".." + entries (8-aligned) + leaf
	// array (8 bytes/entry incl "."/"..") + block tail (8).
	sum := dirEntrySize(1, sb.hasFType) + dirEntrySize(2, sb.hasFType)
	for _, e := range entries {
		sum += dirEntrySize(len(e.name), sb.hasFType)
	}
	fitsOneBlock := hdr+sum+(len(entries)+2)*8+8 <= blkSize

	dataBlocks, hasLeaf, err := dirBlockLayout(rw, partOff, sb, in)
	if err != nil {
		return err
	}

	// Fast path: already a single block-form block and still fits — rebuild in
	// place, no allocation.
	if fitsOneBlock && len(dataBlocks) == 1 && !hasLeaf {
		blk := make([]byte, blkSize)
		if err := buildBlockDirBlock(sb, blk, dataBlocks[0], in.num, parentIno, entries); err != nil {
			return err
		}
		return writeWriteRawBlock(rw, partOff, sb, dataBlocks[0], blk)
	}

	// Otherwise free the current blocks and lay the directory out fresh.
	if err := freeDirBlocks(rw, partOff, sb, in); err != nil {
		return err
	}
	if fitsOneBlock {
		ag := inoAG(sb, in.num)
		absBlock, err := writeAllocBlocks(rw, partOff, sb, ag, 1)
		if err != nil {
			return fmt.Errorf("xfs: dir block alloc: %w", err)
		}
		blk := make([]byte, blkSize)
		if err := buildBlockDirBlock(sb, blk, absBlock, in.num, parentIno, entries); err != nil {
			return err
		}
		if err := writeWriteBlocksData(rw, partOff, sb, absBlock, 1, blk); err != nil {
			return err
		}
		setInodeFormat(in, inodeFmtExtents)
		setInodeNBlocks(in, 1)
		setInodeNExtents(in, 1)
		setInodeSize(in, uint64(blkSize))
		if err := writeWriteExtentList(in, []extent{{startOff: 0, startBlock: absBlock, count: 1}}); err != nil {
			return err
		}
		return writeWriteInode(rw, partOff, sb, in)
	}
	return buildLeafDir(rw, partOff, sb, in, parentIno, entries)
}

// freeDirBlocks releases every block backing a non-short-form directory.
func freeDirBlocks(rw readerWriterAt, partOff int64, sb *superblock, in *inode) error {
	if in.format == inodeFmtLocal {
		return nil
	}
	exts, err := writeDirExtents(rw, partOff, sb, in)
	if err != nil {
		return err
	}
	for _, e := range exts {
		if err := writeFreeBlocks(rw, partOff, sb, e.startBlock, e.count); err != nil {
			return err
		}
	}
	return nil
}

// placedEnt is a directory entry pinned to its data-block byte position.
type placedEnt struct {
	ent dirEnt
	blk int
	off int
}

// leafEnt is a directory hash-index entry: a name hash and the entry's
// directory data pointer (byte offset within the data region >> 3).
type leafEnt struct{ hash, addr uint32 }

// packDirEntries places "." ".." and entries into D contiguous dir3 DATA
// blocks, returning each entry's (block, offset), the first-free offset of
// each block, and D.
func packDirEntries(sb *superblock, in *inode, parentIno uint64, entries []dirEnt) ([]placedEnt, []int, int) {
	blkSize := int(sb.blockSize)
	hdr := dirDataHdrSize(sb.hasCRC)
	all := make([]dirEnt, 0, len(entries)+2)
	all = append(all, dirEnt{".", in.num, 2}, dirEnt{"..", parentIno, 2})
	all = append(all, entries...)

	placed := make([]placedEnt, 0, len(all))
	var blockEnd []int
	curBlk, off := 0, hdr
	for _, e := range all {
		sz := dirEntrySize(len(e.name), sb.hasFType)
		if off+sz > blkSize {
			blockEnd = append(blockEnd, off)
			curBlk++
			off = hdr
		}
		placed = append(placed, placedEnt{e, curBlk, off})
		off += sz
	}
	blockEnd = append(blockEnd, off)
	return placed, blockEnd, curBlk + 1
}

// leafEntries returns the hash→dataptr index for placed entries, sorted by
// (hash, addr) as XFS requires for both leaf and node forms.
func leafEntries(sb *superblock, placed []placedEnt) []leafEnt {
	shift := uint(bits.TrailingZeros32(sb.blockSize)) - 3 // dataptr = (block<<shift)|(off>>3)
	lents := make([]leafEnt, 0, len(placed))
	for _, p := range placed {
		addr := uint32((uint64(p.blk) << shift) | (uint64(p.off) >> 3))
		lents = append(lents, leafEnt{xfsDirHash([]byte(p.ent.name)), addr})
	}
	sort.Slice(lents, func(i, j int) bool {
		if lents[i].hash != lents[j].hash {
			return lents[i].hash < lents[j].hash
		}
		return lents[i].addr < lents[j].addr
	})
	return lents
}

// writeDataRegion builds and writes D dir3 DATA blocks (XDD3) at absolute
// block dataStart, placing the given entries and a trailing free region whose
// length is recorded in bestfree[0].
func writeDataRegion(rw readerWriterAt, partOff int64, sb *superblock, in *inode, dataStart uint64, D int, placed []placedEnt, blockEnd []int) error {
	be := binary.BigEndian
	blkSize := int(sb.blockSize)
	dataBuf := make([]byte, D*blkSize)
	for b := 0; b < D; b++ {
		blk := dataBuf[b*blkSize : (b+1)*blkSize]
		abs := dataStart + uint64(b)
		if sb.hasCRC {
			be.PutUint32(blk[0:], magicDir3Data)
			be.PutUint64(blk[8:], abs*fmtDaddrPerBlock)
			copy(blk[24:40], sb.uuid[:])
			be.PutUint64(blk[40:], in.num)
		} else {
			be.PutUint32(blk[0:], magicDir2Data)
		}
		if free := blkSize - blockEnd[b]; free > 0 {
			fo := blockEnd[b]
			be.PutUint16(blk[fo:], 0xFFFF)
			be.PutUint16(blk[fo+2:], uint16(free))
			be.PutUint16(blk[fo+free-2:], uint16(fo))
			be.PutUint16(blk[48:], uint16(fo))
			be.PutUint16(blk[50:], uint16(free))
		}
	}
	for _, p := range placed {
		blk := dataBuf[p.blk*blkSize : (p.blk+1)*blkSize]
		writeDirEntry(blk, p.off, p.ent.ino, p.ent.name, p.ent.ftype, sb.hasFType)
	}
	if sb.hasCRC {
		for b := 0; b < D; b++ {
			updateCRC(dataBuf[b*blkSize:(b+1)*blkSize], 4, blkSize)
		}
	}
	return writeWriteBlocksData(rw, partOff, sb, dataStart, uint32(D), dataBuf)
}

// buildLeafDir lays a directory out in leaf or node form: the entries (with
// "." and "..") packed into D contiguous dir3 DATA blocks at logical 0..D-1,
// plus a name-hash index at the 32 GiB offset. When the whole index fits one
// block it is a single leaf1 block (0x3df1) carrying the bests array; when it
// does not, it becomes a da3-node btree (0x3ebe) over multiple leafN blocks
// (0x3dff) with the bests array moved to a free-index block (XDF3) at the
// 64 GiB offset.
func buildLeafDir(rw readerWriterAt, partOff int64, sb *superblock, in *inode, parentIno uint64, entries []dirEnt) error {
	blkSize := int(sb.blockSize)
	hdr := dirDataHdrSize(sb.hasCRC)

	placed, blockEnd, D := packDirEntries(sb, in, parentIno, entries)
	lents := leafEntries(sb, placed)

	ag := inoAG(sb, in.num)
	dataStart, err := writeAllocBlocks(rw, partOff, sb, ag, uint32(D))
	if err != nil {
		return fmt.Errorf("xfs: leaf dir alloc %d data blocks: %w", D, err)
	}
	if err := writeDataRegion(rw, partOff, sb, in, dataStart, D, placed, blockEnd); err != nil {
		return err
	}

	// leaf1 holds the lents, the bests array (be16/data block) and a be32 tail.
	if hdr+len(lents)*8+D*2+4 <= blkSize {
		return writeLeaf1Index(rw, partOff, sb, in, dataStart, D, blockEnd, lents)
	}
	return writeNodeIndex(rw, partOff, sb, in, dataStart, D, blockEnd, lents)
}

// writeLeaf1Index writes a single leaf1 index block (0x3df1) and a 2-extent
// inode (data region + leaf block at the 32 GiB offset).
func writeLeaf1Index(rw readerWriterAt, partOff int64, sb *superblock, in *inode, dataStart uint64, D int, blockEnd []int, lents []leafEnt) error {
	be := binary.BigEndian
	blkSize := int(sb.blockSize)
	ag := inoAG(sb, in.num)
	leafBlock, err := writeAllocBlocks(rw, partOff, sb, ag, 1)
	if err != nil {
		return fmt.Errorf("xfs: leaf dir alloc index block: %w", err)
	}

	leaf := make([]byte, blkSize)
	// xfs_dir3_leaf_hdr: da3_blkinfo (magic@8, crc@12, blkno@16, uuid@32,
	// owner@48) + count@56 + stale@58 + pad@60; lents start at 64.
	be.PutUint16(leaf[8:], magicDir3Leaf1)
	be.PutUint64(leaf[16:], leafBlock*fmtDaddrPerBlock)
	copy(leaf[32:48], sb.uuid[:])
	be.PutUint64(leaf[48:], in.num)
	be.PutUint16(leaf[56:], uint16(len(lents)))
	lo := 64
	for _, l := range lents {
		be.PutUint32(leaf[lo:], l.hash)
		be.PutUint32(leaf[lo+4:], l.addr)
		lo += 8
	}
	// xfs_dir2_leaf_tail.bestcount (be32) at the very end; the bests array
	// (one be16 per data block = that block's largest free run) precedes it.
	be.PutUint32(leaf[blkSize-4:], uint32(D))
	bestsOff := blkSize - 4 - D*2
	for b := 0; b < D; b++ {
		be.PutUint16(leaf[bestsOff+b*2:], uint16(blkSize-blockEnd[b]))
	}
	if sb.hasCRC {
		updateCRC(leaf, 12, blkSize) // da3_blkinfo.crc @ 12
	}
	if err := writeWriteBlocksData(rw, partOff, sb, leafBlock, 1, leaf); err != nil {
		return err
	}

	leafLog := dirLeafByteOffset / uint64(sb.blockSize)
	exts := []extent{
		{startOff: 0, startBlock: dataStart, count: uint32(D)},
		{startOff: leafLog, startBlock: leafBlock, count: 1},
	}
	setInodeFormat(in, inodeFmtExtents)
	setInodeNBlocks(in, uint64(D+1))
	setInodeNExtents(in, uint32(len(exts)))
	setInodeSize(in, uint64(D*blkSize))
	if err := writeWriteExtentList(in, exts); err != nil {
		return err
	}
	return writeWriteInode(rw, partOff, sb, in)
}

// Directory da-btree / leaf / free block header sizes (v5). Each starts with a
// 56-byte xfs_da3_blkinfo (leaf/node) or a 48-byte xfs_dir3_blk_hdr (free),
// followed by a small fixed header; index arrays start at these offsets.
const (
	dirLeafHdrSize = 64 // xfs_dir3_leaf_hdr: blkinfo(56)+count(2)+stale(2)+pad(4)
	dirNodeHdrSize = 64 // xfs_da3_node_hdr:  blkinfo(56)+count(2)+level(2)+pad(4)
	dirFreeHdrSize = 64 // xfs_dir3_free_hdr: blk_hdr(48)+firstdb(4)+nvalid(4)+nused(4)+pad(4)
)

// writeNodeIndex writes node form: K leafN blocks (0x3dff) holding the sorted
// hash index, a single da3-node root (0x3ebe) above them, a free-index block
// (XDF3) carrying the per-data-block bests array, and a 3-extent inode (data +
// leaf region at 32 GiB + free region at 64 GiB). Multi-level da-node trees
// and multi-block free indexes are rejected (single-block of each suffices for
// directories up to ~250k entries).
func writeNodeIndex(rw readerWriterAt, partOff int64, sb *superblock, in *inode, dataStart uint64, D int, blockEnd []int, lents []leafEnt) error {
	be := binary.BigEndian
	blkSize := int(sb.blockSize)
	leafLog := uint32(dirLeafByteOffset / uint64(sb.blockSize))
	freeLog := dirFreeByteOffset / uint64(sb.blockSize)

	perLeaf := (blkSize - dirLeafHdrSize) / 8
	K := (len(lents) + perLeaf - 1) / perLeaf
	if K < 1 {
		K = 1
	}
	if max := (blkSize - dirNodeHdrSize) / 8; K > max {
		return fmt.Errorf("xfs: directory %d too large: %d leaf blocks exceed a single-level da node (max %d); multi-level not implemented", in.num, K, max)
	}
	if max := (blkSize - dirFreeHdrSize) / 2; D > max {
		return fmt.Errorf("xfs: directory %d too large: %d data blocks exceed one free-index block (max %d); multi-block free index not implemented", in.num, D, max)
	}

	ag := inoAG(sb, in.num)
	leafStart, err := writeAllocBlocks(rw, partOff, sb, ag, uint32(K+1)) // node root + K leafN
	if err != nil {
		return fmt.Errorf("xfs: node dir alloc %d leaf blocks: %w", K+1, err)
	}
	freeStart, err := writeAllocBlocks(rw, partOff, sb, ag, 1)
	if err != nil {
		return fmt.Errorf("xfs: node dir alloc free-index block: %w", err)
	}

	// Leaf region: logical leafLog = da3 node root (abs leafStart); logical
	// leafLog+1+j = leafN block j (abs leafStart+1+j), linked in hash order.
	region := make([]byte, (K+1)*blkSize)
	type nodeBtree struct{ hash, before uint32 }
	nodeEnts := make([]nodeBtree, K)
	for j := 0; j < K; j++ {
		lo := j * perLeaf
		hi := lo + perLeaf
		if hi > len(lents) {
			hi = len(lents)
		}
		chunk := lents[lo:hi]
		absBlk := leafStart + uint64(1+j)
		logBlk := leafLog + uint32(1+j)
		buf := region[(1+j)*blkSize : (2+j)*blkSize]

		var forw, back uint32
		if j > 0 {
			back = leafLog + uint32(j)
		}
		if j < K-1 {
			forw = leafLog + uint32(2+j)
		}
		be.PutUint32(buf[0:], forw)
		be.PutUint32(buf[4:], back)
		be.PutUint16(buf[8:], magicDir3LeafN)
		be.PutUint64(buf[16:], absBlk*fmtDaddrPerBlock)
		copy(buf[32:48], sb.uuid[:])
		be.PutUint64(buf[48:], in.num)
		be.PutUint16(buf[56:], uint16(len(chunk))) // count; stale@58=0, pad@60=0
		o := dirLeafHdrSize
		for _, l := range chunk {
			be.PutUint32(buf[o:], l.hash)
			be.PutUint32(buf[o+4:], l.addr)
			o += 8
		}
		nodeEnts[j] = nodeBtree{chunk[len(chunk)-1].hash, logBlk}
	}

	// da3 node root at region block 0 (logical leafLog).
	root := region[0:blkSize]
	be.PutUint16(root[8:], magicDa3Node)
	be.PutUint64(root[16:], leafStart*fmtDaddrPerBlock)
	copy(root[32:48], sb.uuid[:])
	be.PutUint64(root[48:], in.num)
	be.PutUint16(root[56:], uint16(K)) // count
	be.PutUint16(root[58:], 1)         // level
	o := dirNodeHdrSize
	for _, ne := range nodeEnts {
		be.PutUint32(root[o:], ne.hash)
		be.PutUint32(root[o+4:], ne.before)
		o += 8
	}
	if sb.hasCRC {
		for b := 0; b <= K; b++ {
			updateCRC(region[b*blkSize:(b+1)*blkSize], 12, blkSize) // da3_blkinfo.crc@12
		}
	}
	if err := writeWriteBlocksData(rw, partOff, sb, leafStart, uint32(K+1), region); err != nil {
		return err
	}

	// Free-index block (XDF3): xfs_dir3_blk_hdr (magic@0, crc@4, blkno@8,
	// uuid@24, owner@40) + firstdb@48 + nvalid@52 + nused@56 + pad@60; the
	// be16 bests array (one per data block) starts at 64.
	free := make([]byte, blkSize)
	be.PutUint32(free[0:], magicDir3Free)
	be.PutUint64(free[8:], freeStart*fmtDaddrPerBlock)
	copy(free[24:40], sb.uuid[:])
	be.PutUint64(free[40:], in.num)
	be.PutUint32(free[48:], 0)         // firstdb: covers data blocks from 0
	be.PutUint32(free[52:], uint32(D)) // nvalid
	be.PutUint32(free[56:], uint32(D)) // nused (no NULLDATAOFF entries are written)
	for b := 0; b < D; b++ {
		be.PutUint16(free[dirFreeHdrSize+b*2:], uint16(blkSize-blockEnd[b]))
	}
	if sb.hasCRC {
		updateCRC(free, 4, blkSize) // xfs_dir3_blk_hdr.crc @ 4
	}
	if err := writeWriteBlocksData(rw, partOff, sb, freeStart, 1, free); err != nil {
		return err
	}

	// Inode: data region + leaf region (32 GiB) + free region (64 GiB).
	exts := []extent{
		{startOff: 0, startBlock: dataStart, count: uint32(D)},
		{startOff: uint64(leafLog), startBlock: leafStart, count: uint32(K + 1)},
		{startOff: freeLog, startBlock: freeStart, count: 1},
	}
	setInodeFormat(in, inodeFmtExtents)
	setInodeNBlocks(in, uint64(D+K+1+1))
	setInodeNExtents(in, uint32(len(exts)))
	setInodeSize(in, uint64(D*blkSize))
	if err := writeWriteExtentList(in, exts); err != nil {
		return err
	}
	return writeWriteInode(rw, partOff, sb, in)
}
