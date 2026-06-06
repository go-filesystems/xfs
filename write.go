package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

var writePathLookup = pathLookup
var writeLookupInDir = lookupInDir
var writeReadInode = readInode
var writeInlineExtents = inlineExtents
var writeBtreeExtents = btreeExtents
var writeAllocBlocks = allocBlocks
var writeFreeBlocks = freeBlocks
var writeWriteInode = writeInode
var writeWriteExtentList = writeExtentList
var writeWriteBlocksData = writeBlocksData
var writeAllocInode = allocInode
var writeAddDirEntry = addDirEntry
var writeDirExtents = dirExtents
var writeReadRawBlock = readRawBlock
var writeWriteRawBlock = writeRawBlock
var writeConvertSFToBlock = convertSFToBlock
var writeSFReadDir = sfReadDir
var writeInsertIntoSlot = insertIntoSlot

// writeFile creates or overwrites the file at p within the XFS filesystem.
// The parent directory must already exist.
func writeFile(rw readerWriterAt, partOff int64, sb *superblock, p string, data []byte, perm os.FileMode) error {
	p = path.Clean(p)
	parentPath, name := path.Split(p)
	if name == "" {
		return fmt.Errorf("xfs: invalid path %q", p)
	}
	parentPath = strings.TrimSuffix(parentPath, "/")
	if parentPath == "" {
		parentPath = "/"
	}

	dirIn, err := writePathLookup(rw, partOff, sb, parentPath)
	if err != nil {
		return fmt.Errorf("xfs: parent %q: %w", parentPath, err)
	}
	if !dirIn.isDir() {
		return fmt.Errorf("xfs: %q is not a directory", parentPath)
	}

	// Check if the file already exists.
	existIno, err := writeLookupInDir(rw, partOff, sb, dirIn, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}

	if existIno != 0 {
		return overwriteFile(rw, partOff, sb, existIno, data)
	}
	return createFile(rw, partOff, sb, dirIn, name, data, perm)
}

// overwriteFile rewrites the content of an existing regular file.
func overwriteFile(rw readerWriterAt, partOff int64, sb *superblock, ino uint64, data []byte) error {
	in, err := writeReadInode(rw, partOff, sb, ino)
	if err != nil {
		return err
	}
	if !in.isRegular() {
		return fmt.Errorf("xfs: inode %d is not a regular file", ino)
	}

	newSize := uint64(len(data))
	blockSize := uint64(sb.blockSize)
	newBlocks := (newSize + blockSize - 1) / blockSize

	switch in.format {
	case inodeFmtLocal:
		// Data is inline in the data fork. Check capacity.
		forkCap := uint64(sb.inodeSize) - inodeCoreSize
		if in.forkOff != 0 {
			forkCap = uint64(in.forkOff) * 8
		}
		if newSize <= forkCap {
			clear(in.dataFork())
			copy(in.dataFork(), data)
			setInodeSize(in, newSize)
			return writeWriteInode(rw, partOff, sb, in)
		}
		// Fall through: data no longer fits inline — allocate a block.
		return promoteAndWrite(rw, partOff, sb, in, data)

	case inodeFmtExtents:
		exts, err := writeInlineExtents(in)
		if err != nil {
			return err
		}
		existingBlocks := uint64(0)
		for _, e := range exts {
			existingBlocks += uint64(e.count)
		}

		if newBlocks <= existingBlocks {
			// Write data into existing blocks; zero any extra space.
			return writeExtentsInPlace(rw, partOff, sb, in, exts, data)
		}
		// More blocks needed: free old, allocate new.
		return reallocAndWrite(rw, partOff, sb, in, exts, data)

	case inodeFmtBtree:
		exts, err := writeBtreeExtents(rw, partOff, sb, in)
		if err != nil {
			return err
		}
		existingBlocks := uint64(0)
		for _, e := range exts {
			existingBlocks += uint64(e.count)
		}
		if newBlocks <= existingBlocks {
			return writeExtentsInPlace(rw, partOff, sb, in, exts, data)
		}
		return reallocAndWrite(rw, partOff, sb, in, exts, data)

	default:
		return fmt.Errorf("xfs: inode %d unsupported format %d for write", ino, in.format)
	}
}

// writeExtentsInPlace writes data into the blocks described by exts and
// updates the inode size. Existing blocks beyond len(data) are zeroed.
func writeExtentsInPlace(rw readerWriterAt, partOff int64, sb *superblock, in *inode, exts []extent, data []byte) error {
	bs := int64(sb.blockSize)
	written := int64(0)
	total := int64(len(data))

	for _, e := range exts {
		physOff := partOff + int64(e.startBlock)*bs
		for b := uint32(0); b < e.count; b++ {
			blk := make([]byte, bs)
			start := written
			end := written + bs
			if start >= total {
				// Block entirely beyond new data — write zeros.
			} else {
				if end > total {
					end = total
				}
				copy(blk, data[start:end])
			}
			if _, err := rw.WriteAt(blk, physOff+int64(b)*bs); err != nil {
				return fmt.Errorf("xfs: write block %d: %w", e.startBlock+uint64(b), err)
			}
			written += bs
		}
	}

	setInodeSize(in, uint64(len(data)))
	return writeWriteInode(rw, partOff, sb, in)
}

// reallocAndWrite frees the old extents, allocates new ones, and writes data.
func reallocAndWrite(rw readerWriterAt, partOff int64, sb *superblock, in *inode, oldExts []extent, data []byte) error {
	// Free old blocks.
	for _, e := range oldExts {
		if err := writeFreeBlocks(rw, partOff, sb, e.startBlock, e.count); err != nil {
			return err
		}
	}

	blockSize := uint64(sb.blockSize)
	nBlocks := uint32((uint64(len(data)) + blockSize - 1) / blockSize)

	ag := inoAG(sb, in.num)
	absBlock, err := writeAllocBlocks(rw, partOff, sb, ag, nBlocks)
	if err != nil {
		// Try other AGs.
		for a := uint32(0); a < sb.agCount; a++ {
			if a == ag {
				continue
			}
			absBlock, err = writeAllocBlocks(rw, partOff, sb, a, nBlocks)
			if err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("xfs: realloc: no space for %d blocks", nBlocks)
		}
	}

	newExts := []extent{{startOff: 0, startBlock: absBlock, count: nBlocks}}
	if err := writeWriteExtentList(in, newExts); err != nil {
		return err
	}
	setInodeSize(in, uint64(len(data)))
	setInodeNBlocks(in, uint64(nBlocks))
	setInodeNExtents(in, 1)
	setInodeFormat(in, inodeFmtExtents)

	if err := writeWriteInode(rw, partOff, sb, in); err != nil {
		return err
	}
	return writeWriteBlocksData(rw, partOff, sb, absBlock, nBlocks, data)
}

// promoteAndWrite converts an inline file to block-based and writes data.
func promoteAndWrite(rw readerWriterAt, partOff int64, sb *superblock, in *inode, data []byte) error {
	blockSize := uint64(sb.blockSize)
	nBlocks := uint32((uint64(len(data)) + blockSize - 1) / blockSize)
	ag := inoAG(sb, in.num)
	absBlock, err := writeAllocBlocks(rw, partOff, sb, ag, nBlocks)
	if err != nil {
		return fmt.Errorf("xfs: promoteAndWrite: %w", err)
	}
	newExts := []extent{{startOff: 0, startBlock: absBlock, count: nBlocks}}
	if err := writeWriteExtentList(in, newExts); err != nil {
		return err
	}
	setInodeSize(in, uint64(len(data)))
	setInodeNBlocks(in, uint64(nBlocks))
	setInodeNExtents(in, 1)
	setInodeFormat(in, inodeFmtExtents)
	if err := writeWriteInode(rw, partOff, sb, in); err != nil {
		return err
	}
	return writeWriteBlocksData(rw, partOff, sb, absBlock, nBlocks, data)
}

// writeExtentList stores the extent list into the inode's data fork in-place.
// Supports only the extents format (a single flat array).
func writeExtentList(in *inode, exts []extent) error {
	fork := in.dataFork()
	need := len(exts) * 16
	if need > len(fork) {
		return fmt.Errorf("xfs: extent list (%d entries) too large for inode %d fork", len(exts), in.num)
	}
	clear(fork[:need])
	for i, e := range exts {
		rec := encodeExtent(e)
		copy(fork[i*16:], rec[:])
	}
	return nil
}

// writeBlocksData writes data into nBlocks contiguous filesystem blocks
// starting at absStartBlock.
func writeBlocksData(rw io.WriterAt, partOff int64, sb *superblock, absStartBlock uint64, nBlocks uint32, data []byte) error {
	bs := int64(sb.blockSize)
	for b := uint32(0); b < nBlocks; b++ {
		blk := make([]byte, bs)
		start := int64(b) * bs
		end := start + bs
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		if start < int64(len(data)) {
			copy(blk, data[start:end])
		}
		off := partOff + int64(absStartBlock+uint64(b))*bs
		if _, err := rw.WriteAt(blk, off); err != nil {
			return fmt.Errorf("xfs: write data block %d: %w", absStartBlock+uint64(b), err)
		}
	}
	return nil
}

// createFile allocates a new inode and data blocks, writes data, and adds an
// entry to the parent directory.
func createFile(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode, name string, data []byte, perm os.FileMode) error {
	mode := uint16(perm&0o777) | 0x8000 // regular file
	return createInodeWithData(rw, partOff, sb, dirIn, name, data, mode, 1 /* DT_REG */)
}

// createSymlink creates a new symlink inode named `name` inside `dirIn`,
// whose target is the bytes of `target`. POSIX symlinks always have mode
// 0o777 on the link itself (chmod is a no-op), so we don't take a perm
// argument.
func createSymlink(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode, name string, target []byte) error {
	mode := uint16(0o777) | 0xA000 // symbolic link
	return createInodeWithData(rw, partOff, sb, dirIn, name, target, mode, 7 /* DT_LNK */)
}

// createInodeWithData is the shared body for createFile and createSymlink:
// it allocates a fresh inode, optionally allocates a single extent run for
// `data`, writes the data + inode, and inserts the dir entry. The type bits
// in `mode` and the directory ftype byte are the only things that differ
// between regular files and symlinks.
func createInodeWithData(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode, name string, data []byte, mode uint16, ftype uint8) error {
	ag := inoAG(sb, dirIn.num)

	// Allocate a new inode.
	ino, err := writeAllocInode(rw, partOff, sb, ag)
	if err != nil {
		// Try other AGs.
		for a := uint32(0); a < sb.agCount; a++ {
			if a == ag {
				continue
			}
			ino, err = writeAllocInode(rw, partOff, sb, a)
			if err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("xfs: createInode: no free inode: %w", err)
		}
	}

	// Allocate data blocks (none for an empty payload).
	blockSize := uint64(sb.blockSize)
	nBlocks := uint32((uint64(len(data)) + blockSize - 1) / blockSize)

	// A freshly created regular file or symlink has nlink=1 (the dir entry
	// about to point at it).
	inodeBuf := make([]byte, sb.inodeSize)
	initInodeV3(inodeBuf, ino, mode, sb.inodeSize, 1)

	newIn := &inode{
		num:  ino,
		mode: mode,
		raw:  inodeBuf,
	}

	if len(data) == 0 {
		// Empty payload: leave as local format with zero size.
		setInodeFormat(newIn, inodeFmtLocal)
		setInodeSize(newIn, 0)
	} else {
		absBlock, err := writeAllocBlocks(rw, partOff, sb, ag, nBlocks)
		if err != nil {
			for a := uint32(0); a < sb.agCount; a++ {
				if a == ag {
					continue
				}
				absBlock, err = writeAllocBlocks(rw, partOff, sb, a, nBlocks)
				if err == nil {
					break
				}
			}
			if err != nil {
				return fmt.Errorf("xfs: createInode: no space: %w", err)
			}
		}

		exts := []extent{{startOff: 0, startBlock: absBlock, count: nBlocks}}
		setInodeFormat(newIn, inodeFmtExtents)
		setInodeSize(newIn, uint64(len(data)))
		setInodeNBlocks(newIn, uint64(nBlocks))
		setInodeNExtents(newIn, 1)
		if err := writeWriteExtentList(newIn, exts); err != nil {
			return err
		}
		if err := writeWriteBlocksData(rw, partOff, sb, absBlock, nBlocks, data); err != nil {
			return err
		}
	}

	if err := writeWriteInode(rw, partOff, sb, newIn); err != nil {
		return err
	}

	return writeAddDirEntry(rw, partOff, sb, dirIn, ino, name, ftype)
}

// symlinkInode creates a new symlink at `linkPath` whose target is the
// literal string `target`. Parent must exist; `linkPath` must not.
func symlinkInode(rw readerWriterAt, partOff int64, sb *superblock, target, linkPath string) error {
	linkPath = path.Clean(linkPath)
	parentPath, name := path.Split(linkPath)
	if name == "" {
		return fmt.Errorf("xfs: invalid symlink path %q", linkPath)
	}
	parentPath = strings.TrimSuffix(parentPath, "/")
	if parentPath == "" {
		parentPath = "/"
	}

	dirIn, err := writePathLookup(rw, partOff, sb, parentPath)
	if err != nil {
		return fmt.Errorf("xfs: parent %q: %w", parentPath, err)
	}
	if !dirIn.isDir() {
		return fmt.Errorf("xfs: %q is not a directory", parentPath)
	}

	if _, err := writeLookupInDir(rw, partOff, sb, dirIn, name); err == nil {
		return fmt.Errorf("xfs: %q already exists", linkPath)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	return createSymlink(rw, partOff, sb, dirIn, name, []byte(target))
}

// addDirEntry inserts a new directory entry into dirIn.
func addDirEntry(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode, childIno uint64, name string, ftype uint8) error {
	switch dirIn.format {
	case inodeFmtLocal:
		if addEntryToSFDir(dirIn, childIno, name, ftype, sb) {
			return writeWriteInode(rw, partOff, sb, dirIn)
		}
		// Short-form is full; convert to block form.
		return writeConvertSFToBlock(rw, partOff, sb, dirIn, childIno, name, ftype)

	case inodeFmtExtents, inodeFmtBtree:
		return addEntryToBlockDir(rw, partOff, sb, dirIn, childIno, name, ftype)

	default:
		return fmt.Errorf("xfs: dir inode %d unsupported format %d", dirIn.num, dirIn.format)
	}
}

// convertSFToBlock allocates a directory data block, copies all sf entries
// into it, plus the new entry, and updates the directory inode.
func convertSFToBlock(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode, newIno uint64, newName string, newFtype uint8) error {
	// Collect existing sf entries.
	existing, err := writeSFReadDir(dirIn.dataFork(), sb.hasFType)
	if err != nil {
		return err
	}

	// Allocate one directory data block (dirFSBlocks() filesystem blocks).
	dirBlocks := sb.dirFSBlocks()
	ag := inoAG(sb, dirIn.num)
	absBlock, err := writeAllocBlocks(rw, partOff, sb, ag, dirBlocks)
	if err != nil {
		return fmt.Errorf("xfs: convertSFToBlock: %w", err)
	}

	// Build the directory data block.
	blkSize := int(sb.blockSize) * int(dirBlocks)
	blk := make([]byte, blkSize)

	// Write header.
	hdrSize := dirDataHdrSize(sb.hasCRC)
	if sb.hasCRC {
		binary.BigEndian.PutUint32(blk[0:], magicDir3Block)
		// blk_lsn, uuid, owner, blkno — left as zero / will be set if needed
	} else {
		binary.BigEndian.PutUint32(blk[0:], magicDir2Block)
	}

	// Write dot and dotdot entries.
	off := hdrSize
	off += writeDirEntry(blk, off, dirIn.num, ".", newFtype, sb.hasFType)
	// For dotdot we'd need the parent inode. Use dirIn.num as placeholder.
	off += writeDirEntry(blk, off, dirIn.num, "..", newFtype, sb.hasFType)

	// Write existing entries.
	for _, e := range existing {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		off += writeDirEntry(blk, off, e.Inode, e.Name, e.FileType, sb.hasFType)
	}

	// Write the new entry.
	off += writeDirEntry(blk, off, newIno, newName, newFtype, sb.hasFType)

	// Mark remaining space as free.
	// For block-form, we need a tail at the end before the leaf entries.
	// Simple block-form tail: 8-byte xfs_dir2_block_tail at blkSize-8.
	tailOff := blkSize - 8
	if off < tailOff {
		markSlotFree(blk, off, tailOff-off)
	}
	// Block tail: count=0 (no leaf entries), stale=0.
	binary.BigEndian.PutUint32(blk[tailOff:], 0)
	binary.BigEndian.PutUint32(blk[tailOff+4:], 0)

	if sb.hasCRC {
		updateCRC(blk, 4, blkSize) // CRC at offset 4 in dir3_blk_hdr
	}

	if err := writeWriteBlocksData(rw, partOff, sb, absBlock, dirBlocks, blk); err != nil {
		return err
	}

	// Update inode to point at the new block via an extent.
	exts := []extent{{startOff: 0, startBlock: absBlock, count: dirBlocks}}
	setInodeFormat(dirIn, inodeFmtExtents)
	setInodeNBlocks(dirIn, uint64(dirBlocks))
	setInodeNExtents(dirIn, 1)
	setInodeSize(dirIn, uint64(blkSize))
	if err := writeWriteExtentList(dirIn, exts); err != nil {
		return err
	}
	return writeWriteInode(rw, partOff, sb, dirIn)
}

// addEntryToBlockDir wraps the dir.go helper, adapting the interface.
func addEntryToBlockDir(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode, childIno uint64, name string, ftype uint8) error {
	exts, err := writeDirExtents(rw, partOff, sb, dirIn)
	if err != nil {
		return err
	}
	need := dirEntrySize(len(name), sb.hasFType)
	leafLogBlock := dirLeafByteOffset / uint64(sb.blockSize)

	for _, e := range exts {
		if e.startOff >= leafLogBlock {
			continue
		}
		for b := uint32(0); b < e.count; b++ {
			absBlock := e.startBlock + uint64(b)
			blk, err := writeReadRawBlock(rw, partOff, sb, absBlock)
			if err != nil {
				return err
			}
			if offs := findFreeSlot(blk, need, sb.hasFType, sb.hasCRC); offs >= 0 {
				if err := writeInsertIntoSlot(blk, offs, need, childIno, name, ftype, sb.hasFType, sb.hasCRC, absBlock); err != nil {
					return err
				}
				return writeWriteRawBlock(rw, partOff, sb, absBlock, blk)
			}
		}
	}
	return fmt.Errorf("xfs: no free slot found in directory inode %d for %q; consider expanding the directory", dirIn.num, name)
}
