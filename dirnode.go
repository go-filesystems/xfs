package filesystem_xfs

// dirnode.go — node-form directory writer.
//
// Node form is reached when a directory's hash index no longer fits in a single
// leaf block. The index then spreads across several "leafn" blocks (a leaf
// block without the trailing bests/tail) linked into a da-btree by one internal
// node block, and the per-data-block free-space hints move out of the leaf into
// dedicated "free" blocks. Logical address-space layout (directory block units,
// our geometry uses one FS block per directory block):
//
//	[0, leafLogBlock)            data blocks (XDD3)
//	leafLogBlock                 da-btree internal node (root)
//	leafLogBlock+1 .. +L         leafn blocks (one per da-btree child)
//	[freeLogBlock, …)            free blocks (XDF3)
//
// As with leaf form the directory is laid out from a full entry list on each
// mutation so the data entries, hash index, da-btree and free-space bests stay
// mutually consistent.

import "encoding/binary"

// nodeFormRegions returns the leaf-space and free-space regions of a node-form
// directory for the sorted hash index `leaves` and per-data-block `bests`. The
// leaf-space region is one contiguous run holding the da-btree root node
// (logical leafLogBlock) followed by its leafn children; the free-space region
// holds the bests array. The data-block region is supplied separately by the
// caller.
func nodeFormRegions(sb *superblock, ownerIno uint64, leaves []leafIndexEnt, bests []int, nData int) []dirRegion {
	blkSize := int(sb.blockSize)
	leafLog := dirLeafLogBlock(sb)

	// Split the sorted hash index across leafn blocks. Force at least two
	// children so the da-btree is never a degenerate single-child node (which
	// mkfs.xfs never emits).
	leafnCap := (blkSize - dirLeafHdrSize(sb.hasCRC)) / 8
	nLeaf := (len(leaves) + leafnCap - 1) / leafnCap
	if nLeaf < 2 {
		nLeaf = 2
	}
	per := (len(leaves) + nLeaf - 1) / nLeaf

	freeCap := (blkSize - dirFreeHdrSize(sb.hasCRC)) / 2
	nFree := (nData + freeCap - 1) / freeCap
	if nFree < 1 {
		nFree = 1
	}

	leafRegion := dirRegion{
		logicalOff: leafLog,
		count:      uint32(1 + nLeaf),
		build: func(abs uint64) ([]byte, error) {
			out := make([]byte, (1+nLeaf)*blkSize)
			child0Abs := abs + 1
			nodeEnts := make([]nodeBtreeEnt, 0, nLeaf)
			for i := 0; i < nLeaf; i++ {
				lo, hi := i*per, (i+1)*per
				if hi > len(leaves) {
					hi = len(leaves)
				}
				slice := leaves[lo:hi]
				childLog := leafLog + 1 + uint64(i)
				var forw, back uint32
				if i > 0 {
					back = uint32(childLog - 1)
				}
				if i < nLeaf-1 {
					forw = uint32(childLog + 1)
				}
				blk := out[(1+i)*blkSize : (2+i)*blkSize]
				buildLeafnBlock(sb, blk, child0Abs+uint64(i), ownerIno, forw, back, slice)
				// Highest hash reachable through this child (entries sorted).
				nodeEnts = append(nodeEnts, nodeBtreeEnt{hash: slice[len(slice)-1].hash, before: uint32(childLog)})
			}
			copy(out[0:blkSize], buildDaNodeBlock(sb, abs, ownerIno, nodeEnts))
			return out, nil
		},
	}
	freeRegion := dirRegion{
		logicalOff: dirFreeLogBlock(sb),
		count:      uint32(nFree),
		build: func(abs uint64) ([]byte, error) {
			return buildFreeBlocks(sb, abs, ownerIno, bests, nData, nFree, freeCap), nil
		},
	}
	return []dirRegion{leafRegion, freeRegion}
}

// nodeBtreeEnt is one xfs_da_node_entry: the highest hash reachable through a
// child block, and that child's logical directory-block number.
type nodeBtreeEnt struct {
	hash   uint32
	before uint32
}

// buildLeafnBlock fills blk as an xfs_dir3_leafn block: header + sorted hash
// index, no bests array or tail. forw/back are the logical block numbers of the
// sibling leafn blocks (0 when absent).
func buildLeafnBlock(sb *superblock, blk []byte, absBlock, ownerIno uint64, forw, back uint32, entries []leafIndexEnt) {
	be := binary.BigEndian
	magic := magicDir3Leafn
	if !sb.hasCRC {
		magic = magicDir2Leafn
	}
	putDaBlkInfo(sb, blk, magic, absBlock, ownerIno)
	be.PutUint32(blk[0:], forw)
	be.PutUint32(blk[4:], back)

	cntOff := 12
	if sb.hasCRC {
		cntOff = 56
	}
	be.PutUint16(blk[cntOff:], uint16(len(entries)))
	be.PutUint16(blk[cntOff+2:], 0) // stale

	hdrSize := dirLeafHdrSize(sb.hasCRC)
	for i, le := range entries {
		p := hdrSize + i*8
		be.PutUint32(blk[p:], le.hash)
		be.PutUint32(blk[p+4:], le.addr)
	}
	if sb.hasCRC {
		updateCRC(blk, 12, len(blk)) // da3_blkinfo.crc
	}
}

// buildDaNodeBlock builds the da-btree internal node (root) pointing at the
// leafn children. level is 1: its children are leaf blocks.
func buildDaNodeBlock(sb *superblock, absBlock, ownerIno uint64, ents []nodeBtreeEnt) []byte {
	be := binary.BigEndian
	blkSize := int(sb.blockSize)
	blk := make([]byte, blkSize)

	magic := magicDa3Node
	if !sb.hasCRC {
		magic = magicDaNode
	}
	putDaBlkInfo(sb, blk, magic, absBlock, ownerIno)

	cntOff := 12
	if sb.hasCRC {
		cntOff = 56
	}
	be.PutUint16(blk[cntOff:], uint16(len(ents))) // __count
	be.PutUint16(blk[cntOff+2:], 1)               // __level (children are leaves)

	hdrSize := dirNodeHdrSize(sb.hasCRC)
	for i, e := range ents {
		p := hdrSize + i*8
		be.PutUint32(blk[p:], e.hash)
		be.PutUint32(blk[p+4:], e.before)
	}
	if sb.hasCRC {
		updateCRC(blk, 12, blkSize) // da3_blkinfo.crc
	}
	return blk
}

// buildFreeBlocks builds the free block(s) holding the per-data-block bests
// array. Block i covers data blocks [i*freeCap, …); firstdb/nvalid/nused index
// into the global data-block space.
func buildFreeBlocks(sb *superblock, freeAbs, ownerIno uint64, bests []int, nData, nFree, freeCap int) []byte {
	be := binary.BigEndian
	blkSize := int(sb.blockSize)
	hdrSize := dirFreeHdrSize(sb.hasCRC)
	out := make([]byte, nFree*blkSize)

	for f := 0; f < nFree; f++ {
		blk := out[f*blkSize : (f+1)*blkSize]
		firstdb := f * freeCap
		nvalid := nData - firstdb
		if nvalid > freeCap {
			nvalid = freeCap
		}

		if sb.hasCRC {
			be.PutUint32(blk[0:], magicDir3Free)
			be.PutUint64(blk[8:], (freeAbs+uint64(f))*fmtDaddrPerBlock) // blkno
			copy(blk[24:40], sb.uuid[:])
			be.PutUint64(blk[40:], ownerIno)
			be.PutUint32(blk[48:], uint32(firstdb))
			be.PutUint32(blk[52:], uint32(nvalid))
			be.PutUint32(blk[56:], uint32(nvalid)) // nused: all slots map a real block
		} else {
			be.PutUint32(blk[0:], magicDir2Free)
			be.PutUint32(blk[4:], uint32(firstdb))
			be.PutUint32(blk[8:], uint32(nvalid))
			be.PutUint32(blk[12:], uint32(nvalid))
		}

		for i := 0; i < nvalid; i++ {
			be.PutUint16(blk[hdrSize+i*2:], uint16(bests[firstdb+i]))
		}
		if sb.hasCRC {
			updateCRC(blk, 4, blkSize) // dir3_blk_hdr.crc
		}
	}
	return out
}
