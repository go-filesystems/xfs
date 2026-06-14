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
// between block and leaf form. Node form (a multi-block leaf index + free
// blocks, ~500+ entries) is not yet implemented and is rejected cleanly.

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

// buildLeafDir lays a directory out in leaf form: the entries (with "." and
// "..") packed into D contiguous dir3 DATA blocks at logical 0..D-1, plus one
// leaf index block at the logical 32 GiB offset holding the sorted hash→entry
// index and the per-data-block free-space "bests" array.
func buildLeafDir(rw readerWriterAt, partOff int64, sb *superblock, in *inode, parentIno uint64, entries []dirEnt) error {
	be := binary.BigEndian
	blkSize := int(sb.blockSize)
	hdr := dirDataHdrSize(sb.hasCRC)
	shift := uint(bits.TrailingZeros32(sb.blockSize)) - 3 // dataptr = (block<<shift)|(off>>3)

	all := make([]dirEnt, 0, len(entries)+2)
	all = append(all, dirEnt{".", in.num, 2}, dirEnt{"..", parentIno, 2})
	all = append(all, entries...)

	// Pack entries into data blocks; record each entry's (block, offset).
	type placed struct {
		ent dirEnt
		blk int
		off int
	}
	placedEnts := make([]placed, 0, len(all))
	blockEnd := []int{} // first free offset in each data block
	curBlk, off := 0, hdr
	for _, e := range all {
		sz := dirEntrySize(len(e.name), sb.hasFType)
		if off+sz > blkSize {
			blockEnd = append(blockEnd, off)
			curBlk++
			off = hdr
		}
		placedEnts = append(placedEnts, placed{e, curBlk, off})
		off += sz
	}
	blockEnd = append(blockEnd, off)
	D := curBlk + 1

	// The leaf index (header + lents + bests + tail) must fit one block, else
	// node form would be required.
	if hdr+len(all)*8+D*2+4 > blkSize {
		return fmt.Errorf("xfs: directory %d too large for leaf form (%d entries, %d data blocks); node form not implemented", in.num, len(all), D)
	}

	ag := inoAG(sb, in.num)
	dataStart, err := writeAllocBlocks(rw, partOff, sb, ag, uint32(D))
	if err != nil {
		return fmt.Errorf("xfs: leaf dir alloc %d data blocks: %w", D, err)
	}
	leafBlock, err := writeAllocBlocks(rw, partOff, sb, ag, 1)
	if err != nil {
		return fmt.Errorf("xfs: leaf dir alloc index block: %w", err)
	}

	// --- data blocks (XDD3) ---
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
		// Trailing free region (xfs_dir2_data_unused) + bestfree[0].
		if free := blkSize - blockEnd[b]; free > 0 {
			fo := blockEnd[b]
			be.PutUint16(blk[fo:], 0xFFFF)
			be.PutUint16(blk[fo+2:], uint16(free))
			be.PutUint16(blk[fo+free-2:], uint16(fo))
			be.PutUint16(blk[48:], uint16(fo))
			be.PutUint16(blk[50:], uint16(free))
		}
	}
	for _, p := range placedEnts {
		blk := dataBuf[p.blk*blkSize : (p.blk+1)*blkSize]
		writeDirEntry(blk, p.off, p.ent.ino, p.ent.name, p.ent.ftype, sb.hasFType)
	}
	if sb.hasCRC {
		for b := 0; b < D; b++ {
			updateCRC(dataBuf[b*blkSize:(b+1)*blkSize], 4, blkSize)
		}
	}
	if err := writeWriteBlocksData(rw, partOff, sb, dataStart, uint32(D), dataBuf); err != nil {
		return err
	}

	// --- leaf index block (0x3df1) ---
	leaf := make([]byte, blkSize)
	// xfs_dir3_leaf_hdr: da3_blkinfo (magic@8, crc@12, blkno@16, uuid@32,
	// owner@48) + count@56 + stale@58 + pad@60; lents start at 64.
	be.PutUint16(leaf[8:], magicDir3Leaf1)
	be.PutUint64(leaf[16:], leafBlock*fmtDaddrPerBlock)
	copy(leaf[32:48], sb.uuid[:])
	be.PutUint64(leaf[48:], in.num)
	be.PutUint16(leaf[56:], uint16(len(all)))

	type lent struct{ hash, addr uint32 }
	lents := make([]lent, 0, len(all))
	for _, p := range placedEnts {
		addr := uint32((uint64(p.blk) << shift) | (uint64(p.off) >> 3))
		lents = append(lents, lent{xfsDirHash([]byte(p.ent.name)), addr})
	}
	sort.Slice(lents, func(i, j int) bool {
		if lents[i].hash != lents[j].hash {
			return lents[i].hash < lents[j].hash
		}
		return lents[i].addr < lents[j].addr
	})
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

	// --- inode: data extents [0..D-1] + leaf extent at the 32 GiB offset ---
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
