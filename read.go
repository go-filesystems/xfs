package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
	"io"
)

// readFileData reads and returns the full content of a regular file.
func readFileData(r io.ReaderAt, partOff int64, sb *superblock, in *inode) ([]byte, error) {
	switch in.format {
	case inodeFmtLocal:
		// Inline data: the file content is stored directly in the data fork.
		if uint64(len(in.dataFork())) < in.size {
			return nil, fmt.Errorf("xfs: inline inode %d size %d exceeds fork space %d",
				in.num, in.size, len(in.dataFork()))
		}
		out := make([]byte, in.size)
		copy(out, in.dataFork())
		return out, nil

	case inodeFmtExtents:
		exts, err := inlineExtents(in)
		if err != nil {
			return nil, fmt.Errorf("xfs: inode %d extents: %w", in.num, err)
		}
		return readExtents(r, partOff, sb, exts, in.size)

	case inodeFmtBtree:
		exts, err := btreeExtents(r, partOff, sb, in)
		if err != nil {
			return nil, fmt.Errorf("xfs: inode %d btree extents: %w", in.num, err)
		}
		return readExtents(r, partOff, sb, exts, in.size)

	default:
		return nil, fmt.Errorf("xfs: inode %d unsupported fork format %d", in.num, in.format)
	}
}

// xfsSymlinkHdrSize is the size of struct xfs_dsymlink_hdr that prefixes every
// data block of a remote (extent-based) symlink on a v5 (CRC) filesystem.
// xfsSymlinkMagic ("XSLM") is its leading sl_magic field.
const (
	xfsSymlinkHdrSize = 56
	xfsSymlinkMagic   = "XSLM"
)

// readSymlinkTarget reads the target path of a symbolic link inode.
//
// Short targets are stored inline in the data fork (local format). Longer
// targets are "remote": stored in data blocks. On a v5/CRC filesystem the
// kernel prefixes each remote block with a 56-byte xfs_dsymlink_hdr (magic
// "XSLM") that must be stripped before the target bytes; v4 filesystems (and
// this driver's own writer) store the bytes with no header. We detect the
// header per block by its magic so both layouts read correctly. readFileData
// cannot be used for remote symlinks because it would return the header bytes
// as part of the target.
func readSymlinkTarget(r io.ReaderAt, partOff int64, sb *superblock, in *inode) ([]byte, error) {
	if in.format == inodeFmtLocal {
		fork := in.dataFork()
		if uint64(len(fork)) < in.size {
			return nil, fmt.Errorf("xfs: symlink inode %d size %d exceeds fork space %d",
				in.num, in.size, len(fork))
		}
		out := make([]byte, in.size)
		copy(out, in.dataFork())
		return out, nil
	}

	var (
		exts []extent
		err  error
	)
	switch in.format {
	case inodeFmtExtents:
		exts, err = inlineExtents(in)
	case inodeFmtBtree:
		exts, err = btreeExtents(r, partOff, sb, in)
	default:
		return nil, fmt.Errorf("xfs: symlink inode %d unsupported fork format %d", in.num, in.format)
	}
	if err != nil {
		return nil, fmt.Errorf("xfs: symlink inode %d extents: %w", in.num, err)
	}

	out := make([]byte, 0, in.size)
	remaining := int(in.size)
	for _, e := range exts {
		for b := uint32(0); b < e.count && remaining > 0; b++ {
			blk, err := readRawBlock(r, partOff, sb, e.startBlock+uint64(b))
			if err != nil {
				return nil, err
			}
			hdr := 0
			if len(blk) >= len(xfsSymlinkMagic) && string(blk[:len(xfsSymlinkMagic)]) == xfsSymlinkMagic {
				hdr = xfsSymlinkHdrSize
			}
			if len(blk) <= hdr {
				continue
			}
			payload := blk[hdr:]
			if len(payload) > remaining {
				payload = payload[:remaining]
			}
			out = append(out, payload...)
			remaining -= len(payload)
		}
	}
	return out, nil
}

// inlineExtents returns the flat extent list stored in an inode data fork
// (format == inodeFmtExtents).
func inlineExtents(in *inode) ([]extent, error) {
	fork := in.dataFork()
	nExts := int(in.nExts)
	if len(fork) < nExts*16 {
		return nil, fmt.Errorf("data fork too small for %d extents", nExts)
	}
	exts := make([]extent, nExts)
	for i := range exts {
		exts[i] = decodeExtent(fork[i*16:])
	}
	return exts, nil
}

// btreeExtents walks the in-inode bmbt B-tree and collects all leaf extents.
//
// The in-inode root uses a compact 4-byte header (xfs_bmdr_block_t):
//
//	offset 0: level uint16 (big-endian)
//	offset 2: numrecs uint16 (big-endian)
//
// For a leaf root (level==0), numrecs extent records follow immediately.
// For an internal root (level>0), numrecs keys (16 bytes each) are followed
// by numrecs pointers (8 bytes each, absolute FS block numbers).
func btreeExtents(r io.ReaderAt, partOff int64, sb *superblock, in *inode) ([]extent, error) {
	fork := in.dataFork()
	if len(fork) < 4 {
		return nil, fmt.Errorf("inode %d btree fork too short", in.num)
	}
	be := binary.BigEndian
	level := be.Uint16(fork[0:])
	numrecs := be.Uint16(fork[2:])

	if level == 0 {
		// Leaf root: extent records directly in the fork.
		return btreeLeafExtents(fork[4:], int(numrecs)), nil
	}

	// Internal root: follow the leftmost pointer chain down to the leaves.
	// Keys are 16 bytes each; pointers are 8-byte absolute block numbers.
	ptrOff := 4 + int(numrecs)*16 // skip header + keys
	if len(fork) < ptrOff+int(numrecs)*8 {
		return nil, fmt.Errorf("inode %d btree root too small", in.num)
	}
	// Follow the leftmost pointer (first child) at each level.
	ptr := be.Uint64(fork[ptrOff:])
	for lvl := level - 1; lvl > 0; lvl-- {
		blk, err := readRawBlock(r, partOff, sb, ptr)
		if err != nil {
			return nil, err
		}
		hdrSize := btreeHdrSize(sb.hasCRC)
		nr := int(be.Uint16(blk[6:]))
		ptrStart := hdrSize + nr*16 // skip header + keys
		if len(blk) < ptrStart+8 {
			return nil, fmt.Errorf("xfs: btree block too small")
		}
		ptr = be.Uint64(blk[ptrStart:])
	}

	// Read all leaf blocks at level 0 by following right-sibling links.
	const maxLeaves = 65536
	var exts []extent
	for i := 0; i < maxLeaves; i++ {
		blk, err := readRawBlock(r, partOff, sb, ptr)
		if err != nil {
			return nil, err
		}
		hdrSize := btreeHdrSize(sb.hasCRC)
		nr := int(be.Uint16(blk[6:]))
		leaf := btreeLeafExtents(blk[hdrSize:], nr)
		exts = append(exts, leaf...)

		// Advance to the right sibling.
		rsib := be.Uint32(blk[12:])
		if rsib == 0xFFFFFFFF {
			break
		}
		ptr = uint64(rsib)
	}
	return exts, nil
}

func btreeLeafExtents(data []byte, n int) []extent {
	exts := make([]extent, 0, n)
	for i := 0; i < n && (i+1)*16 <= len(data); i++ {
		exts = append(exts, decodeExtent(data[i*16:]))
	}
	return exts
}

// btreeHdrSize returns the size of the on-disk B-tree block header.
// v5 uses a 56-byte long-form header; v4 uses a 16-byte short-form header.
func btreeHdrSize(hasCRC bool) int {
	if hasCRC {
		return 56
	}
	return 16
}

// readExtents reads file data from an extent list up to size bytes.
func readExtents(r io.ReaderAt, partOff int64, sb *superblock, exts []extent, size uint64) ([]byte, error) {
	out := make([]byte, size)
	for _, e := range exts {
		byteStart := e.startOff * uint64(sb.blockSize)
		if byteStart >= size {
			continue
		}
		byteEnd := byteStart + uint64(e.count)*uint64(sb.blockSize)
		if byteEnd > size {
			byteEnd = size
		}
		physOff := partOff + int64(e.startBlock)*int64(sb.blockSize)
		n := int(byteEnd - byteStart)
		if _, err := r.ReadAt(out[byteStart:byteStart+uint64(n)], physOff); err != nil {
			return nil, fmt.Errorf("xfs: read extent block %d: %w", e.startBlock, err)
		}
	}
	return out, nil
}
