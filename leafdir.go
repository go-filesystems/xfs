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
// single leaf1 block with a da3-node btree over multiple leafN blocks plus one
// or more free-index blocks. The da-node btree and the free-index array both
// grow to arbitrary depth/width, so node form scales to directories of any
// size (millions of entries).

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
			be.PutUint64(blk[8:], sb.fsbToPhysBlock(abs)*fmtDaddrPerBlock)
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
	be.PutUint64(leaf[16:], sb.fsbToPhysBlock(leafBlock)*fmtDaddrPerBlock)
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
// hash index, a da3-node btree (0x3ebe) of one or more levels above them, and
// F free-index blocks (XDF3) carrying the per-data-block bests array. The leaf
// region (data-btree leaves + internal nodes) lives at the 32 GiB logical
// offset and the free region at 64 GiB.
//
// The da-node btree grows in levels: when the K leaf pointers fit a single
// node it is one level (the classic single-root case); when they do not, the
// node level above is itself split into ceil(K/maxNodeEnts) nodes and another
// node level is stacked on top, repeating until the top level holds one root
// node. The bests array is likewise split across ceil(D/bestsPerFree)
// free-index blocks, each tagged with its covering firstdb window. This lifts
// the previous single-level / single-free-block ceilings, so directories of any
// size (millions of entries) lay out correctly.
func writeNodeIndex(rw readerWriterAt, partOff int64, sb *superblock, in *inode, dataStart uint64, D int, blockEnd []int, lents []leafEnt) error {
	blkSize := int(sb.blockSize)
	leafLog := uint32(dirLeafByteOffset / uint64(sb.blockSize))
	freeLog := dirFreeByteOffset / uint64(sb.blockSize)

	perLeaf := (blkSize - dirLeafHdrSize) / 8
	K := (len(lents) + perLeaf - 1) / perLeaf
	if K < 1 {
		K = 1
	}

	// Lay out the da-node region: K leafN blocks at the bottom, then as many
	// internal-node levels as needed. dirNodeBuild returns the per-block buffers
	// (logical-block-ordered) and the logical block number of the root node.
	region, rootLog, err := dirNodeBuild(sb, in, leafLog, perLeaf, K, lents)
	if err != nil {
		return err
	}
	leafBlocks := uint32(len(region) / blkSize)

	// One free-index block holds bestsPerFree be16 bests after its header.
	bestsPerFree := (blkSize - dirFreeHdrSize) / 2
	F := (D + bestsPerFree - 1) / bestsPerFree
	if F < 1 {
		F = 1
	}

	ag := inoAG(sb, in.num)
	leafStart, err := writeAllocBlocks(rw, partOff, sb, ag, leafBlocks)
	if err != nil {
		return fmt.Errorf("xfs: node dir alloc %d leaf/node blocks: %w", leafBlocks, err)
	}
	freeStart, err := writeAllocBlocks(rw, partOff, sb, ag, uint32(F))
	if err != nil {
		return fmt.Errorf("xfs: node dir alloc %d free-index blocks: %w", F, err)
	}

	// Stamp each da-node block's self-blkno and CRC now that we know its
	// absolute placement (the buffers were built logical-block-relative).
	for b := uint32(0); b < leafBlocks; b++ {
		buf := region[int(b)*blkSize : (int(b)+1)*blkSize]
		abs := leafStart + uint64(b)
		binary.BigEndian.PutUint64(buf[16:], sb.fsbToPhysBlock(abs)*fmtDaddrPerBlock)
		if sb.hasCRC {
			updateCRC(buf, 12, blkSize) // da3_blkinfo.crc@12
		}
	}
	if err := writeWriteBlocksData(rw, partOff, sb, leafStart, leafBlocks, region); err != nil {
		return err
	}

	// Free-index blocks (XDF3). Each covers a window [firstdb, firstdb+nvalid)
	// of the data blocks; the be16 bests array starts at dirFreeHdrSize.
	if err := dirWriteFreeIndex(rw, partOff, sb, in, freeStart, D, F, bestsPerFree, blockEnd); err != nil {
		return err
	}

	// Inode: data region (logical 0) + leaf/node region (32 GiB) + free region
	// (64 GiB).
	exts := []extent{
		{startOff: 0, startBlock: dataStart, count: uint32(D)},
		{startOff: uint64(leafLog), startBlock: leafStart, count: leafBlocks},
		{startOff: freeLog, startBlock: freeStart, count: uint32(F)},
	}
	setInodeFormat(in, inodeFmtExtents)
	setInodeNBlocks(in, uint64(D)+uint64(leafBlocks)+uint64(F))
	setInodeNExtents(in, uint32(len(exts)))
	setInodeSize(in, uint64(D*blkSize))
	if err := writeWriteExtentList(in, exts); err != nil {
		return err
	}
	_ = rootLog // root placement is encoded via sibling/level fields, not the inode
	return writeWriteInode(rw, partOff, sb, in)
}

// dirNodeBuild lays out the da-node hash btree for a node-form directory and
// returns (region, rootLog). region is a logical-block-ordered byte buffer of
// every da-node block in the leaf address space; rootLog is the logical block
// of the top node.
//
// XFS requires the da-btree root to sit at the very first block of the leaf
// address space (logical leafLog, the 32 GiB offset), so this routine assigns
// logical block leafLog+0 to the root and places the remaining nodes (and the K
// leafN blocks) after it. The tree is built bottom-up to learn its shape, then
// numbered top-down so the root lands first; leaf sibling links and node
// child-pointers are emitted in those final logical-block numbers. Each block's
// da3_blkinfo header is filled except for bb_blkno and the CRC, which the
// caller stamps once absolute placement is known.
func dirNodeBuild(sb *superblock, in *inode, leafLog uint32, perLeaf, K int, lents []leafEnt) ([]byte, uint32, error) {
	be := binary.BigEndian
	blkSize := int(sb.blockSize)
	maxNodeEnts := (blkSize - dirNodeHdrSize) / 8 // da3_node_entry = hashval(4)+before(4)

	// A planned node records its kind, its highest hash (its parent's key), and
	// the indices (into the flat plan) of its children. Leaves carry a slice of
	// the sorted hash index instead.
	type plan struct {
		isLeaf   bool
		hash     uint32    // highest hash covered (key in the parent)
		children []int     // plan indices of children (internal nodes only)
		chunk    []leafEnt // hash entries (leaves only)
	}
	var nodes []plan

	// Each level is the hash-ordered list of node plan-indices at that height.
	// We keep every level so the same-level sibling chain (forw/back) can be
	// emitted for the internal-node levels as well as the leaves — xfs_repair
	// walks those sibling links when it exhausts a node's children.
	var levels [][]int

	// Level 0: the K leafN blocks, in hash order.
	level := make([]int, 0, K)
	for j := 0; j < K; j++ {
		lo := j * perLeaf
		hi := lo + perLeaf
		if hi > len(lents) {
			hi = len(lents)
		}
		chunk := lents[lo:hi]
		idx := len(nodes)
		nodes = append(nodes, plan{isLeaf: true, hash: chunk[len(chunk)-1].hash, chunk: chunk})
		level = append(level, idx)
	}
	levels = append(levels, level)

	// Internal levels: group the current level's nodes under parent nodes until
	// a single root remains. Children are distributed as evenly as possible
	// across ceil(len/maxNodeEnts) parents rather than greedily packing the
	// first parents full and leaving a tiny tail: xfs_repair's verify_da_path
	// mishandles an interior node that holds a single entry, so even splitting
	// (every parent gets floor or ceil of the average, all >= 2 when len >= 2)
	// keeps the tree xfs_repair-clean.
	for len(level) > 1 {
		m := (len(level) + maxNodeEnts - 1) / maxNodeEnts // parents needed
		base := len(level) / m                            // min children per parent
		extra := len(level) % m                           // first `extra` parents get one more
		next := make([]int, 0, m)
		pos := 0
		for p := 0; p < m; p++ {
			cnt := base
			if p < extra {
				cnt++
			}
			children := append([]int(nil), level[pos:pos+cnt]...)
			pos += cnt
			lastHash := nodes[children[len(children)-1]].hash
			idx := len(nodes)
			nodes = append(nodes, plan{hash: lastHash, children: children})
			next = append(next, idx)
		}
		level = next
		levels = append(levels, level)
	}
	rootIdx := level[0]

	// Per-node same-level siblings (logical block numbers), filled once logical
	// numbers are assigned below. 0 = no sibling (XFS uses 0 for "none" here).
	siblForw := make([]uint32, len(nodes))
	siblBack := make([]uint32, len(nodes))

	// Assign final logical block numbers: root first (leafLog+0), then a
	// breadth-first walk so every block has a stable number before we emit the
	// pointers that reference it.
	logOf := make([]uint32, len(nodes))
	order := make([]int, 0, len(nodes))
	logOf[rootIdx] = leafLog
	order = append(order, rootIdx)
	nextLog := leafLog + 1
	for qi := 0; qi < len(order); qi++ {
		p := nodes[order[qi]]
		for _, c := range p.children {
			logOf[c] = nextLog
			nextLog++
			order = append(order, c)
		}
	}

	// Now that every node has a logical block number, derive the same-level
	// forw/back sibling links (in hash order) for every level — leaves and
	// internal nodes alike. xfs_repair follows these when it walks off the end
	// of a node's children, so an unset internal-node forw would send it to
	// "block 0".
	for _, lv := range levels {
		for i, idx := range lv {
			if i > 0 {
				siblBack[idx] = logOf[lv[i-1]]
			}
			if i < len(lv)-1 {
				siblForw[idx] = logOf[lv[i+1]]
			}
		}
	}

	// Emit each block into its logical slot.
	total := len(nodes)
	region := make([]byte, total*blkSize)
	for idx, p := range nodes {
		buf := region[int(logOf[idx]-leafLog)*blkSize : (int(logOf[idx]-leafLog)+1)*blkSize]
		be.PutUint32(buf[0:], siblForw[idx]) // da3_blkinfo.forw
		be.PutUint32(buf[4:], siblBack[idx]) // da3_blkinfo.back
		copy(buf[32:48], sb.uuid[:])
		be.PutUint64(buf[48:], in.num)
		if p.isLeaf {
			be.PutUint16(buf[8:], magicDir3LeafN)
			be.PutUint16(buf[56:], uint16(len(p.chunk)))
			o := dirLeafHdrSize
			for _, l := range p.chunk {
				be.PutUint32(buf[o:], l.hash)
				be.PutUint32(buf[o+4:], l.addr)
				o += 8
			}
			continue
		}
		be.PutUint16(buf[8:], magicDa3Node)
		be.PutUint16(buf[56:], uint16(len(p.children)))
		// btree level: leaves are level 0, so a node's level is 1 + the level
		// of its children. All children of a node share the same level.
		childLevel := 1
		if !nodes[p.children[0]].isLeaf {
			childLevel = int(be.Uint16(region[int(logOf[p.children[0]]-leafLog)*blkSize+58:])) + 1
		}
		be.PutUint16(buf[58:], uint16(childLevel))
		o := dirNodeHdrSize
		for _, c := range p.children {
			be.PutUint32(buf[o:], nodes[c].hash)
			be.PutUint32(buf[o+4:], logOf[c])
			o += 8
		}
	}
	return region, leafLog, nil
}

// dirWriteFreeIndex writes the F free-index (XDF3) blocks covering the D data
// blocks' bests (largest free run per block). Each block covers a contiguous
// window [firstdb, firstdb+nvalid) of data blocks; together they tile [0, D).
func dirWriteFreeIndex(rw readerWriterAt, partOff int64, sb *superblock, in *inode, freeStart uint64, D, F, bestsPerFree int, blockEnd []int) error {
	be := binary.BigEndian
	blkSize := int(sb.blockSize)
	region := make([]byte, F*blkSize)
	for fi := 0; fi < F; fi++ {
		first := fi * bestsPerFree
		n := bestsPerFree
		if first+n > D {
			n = D - first
		}
		buf := region[fi*blkSize : (fi+1)*blkSize]
		abs := freeStart + uint64(fi)
		be.PutUint32(buf[0:], magicDir3Free)
		be.PutUint64(buf[8:], sb.fsbToPhysBlock(abs)*fmtDaddrPerBlock)
		copy(buf[24:40], sb.uuid[:])
		be.PutUint64(buf[40:], in.num)
		be.PutUint32(buf[48:], uint32(first)) // firstdb
		be.PutUint32(buf[52:], uint32(n))     // nvalid
		be.PutUint32(buf[56:], uint32(n))     // nused (no NULLDATAOFF entries)
		for b := 0; b < n; b++ {
			be.PutUint16(buf[dirFreeHdrSize+b*2:], uint16(blkSize-blockEnd[first+b]))
		}
		if sb.hasCRC {
			updateCRC(buf, 4, blkSize) // xfs_dir3_blk_hdr.crc @ 4
		}
	}
	return writeWriteBlocksData(rw, partOff, sb, freeStart, uint32(F), region)
}
