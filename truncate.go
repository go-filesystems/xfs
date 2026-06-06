package filesystem_xfs

import (
	"fmt"
	"time"
)

// bumpMTimeCTime stamps both mtime and ctime with the current time, the
// POSIX behavior for operations that modify the file body (truncate, write).
func bumpMTimeCTime(in *inode) {
	now := time.Now().UTC()
	writeXfsLegacyTimespec(in.raw[inoOffMTime:], now)
	writeXfsLegacyTimespec(in.raw[inoOffCTime:], now)
}

// truncateInode resizes the regular file at p to newSize bytes.
//
//   - newSize == in.size: no-op resize; mtime + ctime are still bumped per
//     POSIX truncate(2).
//   - newSize > in.size: extension. The inode size is bumped; the new region
//     reads back as zeros via readExtents' implicit hole-fill (no disk
//     allocation, no extent insertion).
//   - newSize < in.size: shrink. Extents (or local-format inline bytes)
//     past newSize are freed or trimmed; nBlocks and nExtents are
//     recomputed.
//
// Only the local + extents fork formats are supported on shrink. Inodes
// stored in the in-inode bmbt B-tree (inodeFmtBtree) are out of scope for
// this round.
func truncateInode(rw readerWriterAt, partOff int64, sb *superblock, p string, newSize uint64) error {
	in, err := pathLookup(rw, partOff, sb, p)
	if err != nil {
		return fmt.Errorf("xfs truncate: %q: %w", p, err)
	}
	if !in.isRegular() {
		return fmt.Errorf("xfs truncate: %q is not a regular file", p)
	}

	if newSize == in.size {
		bumpMTimeCTime(in)
		return writeInode(rw, partOff, sb, in)
	}

	if newSize > in.size {
		setInodeSize(in, newSize)
		bumpMTimeCTime(in)
		return writeInode(rw, partOff, sb, in)
	}

	// Shrink path.
	switch in.format {
	case inodeFmtLocal:
		// Inline data: truncating just lowers the size. Stale bytes past
		// newSize in the fork are harmless — readers honour in.size.
		setInodeSize(in, newSize)
		bumpMTimeCTime(in)
		return writeInode(rw, partOff, sb, in)

	case inodeFmtExtents:
		blockSize := uint64(sb.blockSize)
		exts, err := inlineExtents(in)
		if err != nil {
			return fmt.Errorf("xfs truncate: read extents: %w", err)
		}
		kept := exts[:0]
		var totalBlocks uint64
		for _, e := range exts {
			extStart := e.startOff * blockSize
			extEnd := extStart + uint64(e.count)*blockSize
			if extEnd <= newSize {
				kept = append(kept, e)
				totalBlocks += uint64(e.count)
				continue
			}
			if extStart >= newSize {
				if err := freeBlocks(rw, partOff, sb, e.startBlock, e.count); err != nil {
					return fmt.Errorf("xfs truncate: free extent: %w", err)
				}
				continue
			}
			// Straddles: keep the prefix, free the suffix.
			keepBytes := newSize - extStart
			keepBlocks := uint32((keepBytes + blockSize - 1) / blockSize)
			if keepBlocks < e.count {
				if err := freeBlocks(rw, partOff, sb, e.startBlock+uint64(keepBlocks), e.count-keepBlocks); err != nil {
					return fmt.Errorf("xfs truncate: free extent suffix: %w", err)
				}
			}
			trimmed := e
			trimmed.count = keepBlocks
			kept = append(kept, trimmed)
			totalBlocks += uint64(keepBlocks)
		}

		setInodeSize(in, newSize)
		setInodeNBlocks(in, totalBlocks)
		setInodeNExtents(in, uint32(len(kept)))
		if len(kept) == 0 {
			// Empty file: mirror createFile's empty path — local format, size 0.
			setInodeFormat(in, inodeFmtLocal)
		} else {
			if err := writeExtentList(in, kept); err != nil {
				return err
			}
		}
		bumpMTimeCTime(in)
		return writeInode(rw, partOff, sb, in)

	default:
		return fmt.Errorf("xfs truncate: inode %d unsupported fork format %d", in.num, in.format)
	}
}
