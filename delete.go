package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path"
	"strings"
)

var deletePathLookup = pathLookup
var deleteLookupInDir = lookupInDir
var deleteReadInode = readInode
var deleteRemoveDirEntry = removeDirEntry
var deleteInlineExtents = inlineExtents
var deleteBtreeExtents = btreeExtents
var deleteFreeBlocks = freeBlocks
var deleteWriteInode = writeInode
var deleteFreeInode = freeInode
var deleteReadDir = readDir
var deleteDirDeleteDir func(readerWriterAt, int64, *superblock, string) error
var deleteDirDeleteFile func(readerWriterAt, int64, *superblock, string) error

func init() {
	deleteDirDeleteDir = deleteDir
	deleteDirDeleteFile = deleteFile
}

// deleteFile removes the regular file at p, freeing its data blocks and
// inode. Returns nil if the path does not exist (idempotent).
func deleteFile(rw readerWriterAt, partOff int64, sb *superblock, p string) error {
	p = path.Clean(p)
	parentPath, name := path.Split(p)
	parentPath = strings.TrimSuffix(parentPath, "/")
	if parentPath == "" {
		parentPath = "/"
	}

	dirIn, err := deletePathLookup(rw, partOff, sb, parentPath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}

	childIno, err := deleteLookupInDir(rw, partOff, sb, dirIn, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}

	// Read the file inode to get its extent list.
	in, err := deleteReadInode(rw, partOff, sb, childIno)
	if err != nil {
		return err
	}
	if !in.isRegular() {
		return fmt.Errorf("xfs: %q is not a regular file", p)
	}

	// Remove the directory entry first.
	if err := deleteRemoveDirEntry(rw, partOff, sb, dirIn, name); err != nil {
		return fmt.Errorf("xfs: remove dir entry %q: %w", name, err)
	}

	// Free data blocks.
	if in.format == inodeFmtExtents || in.format == inodeFmtBtree {
		var exts []extent
		if in.format == inodeFmtExtents {
			exts, err = deleteInlineExtents(in)
		} else {
			exts, err = deleteBtreeExtents(rw, partOff, sb, in)
		}
		if err != nil {
			return fmt.Errorf("xfs: get extents for inode %d: %w", childIno, err)
		}
		for _, e := range exts {
			if err := deleteFreeBlocks(rw, partOff, sb, e.startBlock, e.count); err != nil {
				return fmt.Errorf("xfs: free blocks for inode %d: %w", childIno, err)
			}
		}
	}

	// Zero the inode and mark it free.
	zeroInode(in)
	if err := deleteWriteInode(rw, partOff, sb, in); err != nil {
		return err
	}
	return deleteFreeInode(rw, partOff, sb, childIno)
}

// removeDirEntry finds and removes the named entry from the directory dirIn.
func removeDirEntry(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode, name string) error {
	switch dirIn.format {
	case inodeFmtLocal:
		return removeSFEntry(rw, partOff, sb, dirIn, name)
	case inodeFmtExtents, inodeFmtBtree:
		return removeBlockDirEntry(rw, partOff, sb, dirIn, name)
	default:
		return fmt.Errorf("xfs: dir inode %d unsupported format %d", dirIn.num, dirIn.format)
	}
}

// removeSFEntry removes name from a short-form directory inline in the inode.
func removeSFEntry(rw readerWriterAt, partOff int64, sb *superblock, in *inode, name string) error {
	fork := in.dataFork()
	if len(fork) < 2 {
		return ErrNotFound
	}
	count := int(fork[0])
	i8count := int(fork[1])
	parentSize := 4
	if i8count > 0 {
		parentSize = 8
	}
	inoSize := 4
	if i8count > 0 {
		inoSize = 8
	}

	hdr := 2 + parentSize
	off := hdr
	for i := 0; i < count; i++ {
		if off+3 >= len(fork) {
			return ErrNotFound
		}
		entStart := off
		namelen := int(fork[off])
		off += 3 // namelen + offset(2)
		if off+namelen > len(fork) {
			return ErrNotFound
		}
		n := string(fork[off : off+namelen])
		off += namelen
		if sb.hasFType {
			off++
		}
		off += inoSize
		if n == name {
			// Remove: shift everything after this entry left.
			entEnd := off
			copy(fork[entStart:], fork[entEnd:])
			clear(fork[len(fork)-(entEnd-entStart):])
			fork[0] = byte(count - 1)
			return deleteWriteInode(rw, partOff, sb, in)
		}
	}
	return ErrNotFound
}

// removeBlockDirEntry scans block/leaf/node-form directory data blocks and
// marks the entry for name as a free slot.
func removeBlockDirEntry(rw readerWriterAt, partOff int64, sb *superblock, in *inode, name string) error {
	exts, err := dirExtents(rw, partOff, sb, in)
	if err != nil {
		return err
	}
	leafLogBlock := dirLeafByteOffset / uint64(sb.blockSize)

	for _, e := range exts {
		if e.startOff >= leafLogBlock {
			continue
		}
		for b := uint32(0); b < e.count; b++ {
			absBlock := e.startBlock + uint64(b)
			blk, err := readRawBlock(rw, partOff, sb, absBlock)
			if err != nil {
				return err
			}
			off, sz := findEntryInBlock(blk, name, sb.hasFType, sb.hasCRC)
			if off < 0 {
				continue
			}
			// Mark the entry as a free slot.
			// Attempt to merge with an immediately following free slot.
			totalFree := sz
			nextOff := off + sz
			if nextOff+4 <= len(blk) {
				nextFreeTag := binary.BigEndian.Uint16(blk[nextOff:])
				if nextFreeTag == dirFreeTag {
					nextLen := int(binary.BigEndian.Uint16(blk[nextOff+2:]))
					totalFree += nextLen
				}
			}
			markSlotFree(blk, off, totalFree)
			if sb.hasCRC {
				updateCRC(blk, 4, len(blk))
			}
			return writeRawBlock(rw, partOff, sb, absBlock, blk)
		}
	}
	return ErrNotFound
}

// findEntryInBlock returns the byte offset and size of the directory entry
// named name within blk, or (-1, 0) if not found.
func findEntryInBlock(blk []byte, name string, hasFType, hasCRC bool) (int, int) {
	hdrSize := dirDataHdrSize(hasCRC)
	off := hdrSize
	for off+10 <= len(blk) {
		freetag := binary.BigEndian.Uint16(blk[off:])
		if freetag == dirFreeTag {
			length := int(binary.BigEndian.Uint16(blk[off+2:]))
			if length < 8 {
				return -1, 0
			}
			off += length
			continue
		}
		ino := binary.BigEndian.Uint64(blk[off:])
		if ino == 0 {
			off += 8
			continue
		}
		namelen := int(blk[off+8])
		if namelen == 0 {
			break
		}
		nameEnd := off + 9 + namelen
		if nameEnd > len(blk) {
			break
		}
		entSize := dirEntrySize(namelen, hasFType)
		if string(blk[off+9:nameEnd]) == name {
			return off, entSize
		}
		off += entSize
	}
	return -1, 0
}

// deleteDir removes the directory at p and all of its contents recursively.
// Returns nil if p does not exist (idempotent).
func deleteDir(rw readerWriterAt, partOff int64, sb *superblock, p string) error {
	p = path.Clean(p)
	parentPath, name := path.Split(p)
	parentPath = strings.TrimSuffix(parentPath, "/")
	if parentPath == "" {
		parentPath = "/"
	}

	dirIn, err := deletePathLookup(rw, partOff, sb, parentPath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}

	childInoNum, err := deleteLookupInDir(rw, partOff, sb, dirIn, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}

	targetIn, err := deleteReadInode(rw, partOff, sb, childInoNum)
	if err != nil {
		return err
	}
	if !targetIn.isDir() {
		return fmt.Errorf("xfs: DeleteDir: %q is not a directory", p)
	}

	// Recursively remove all children.
	children, err := deleteReadDir(rw, partOff, sb, targetIn)
	if err != nil {
		return err
	}
	for _, e := range children {
		childPath := p + "/" + e.Name
		if e.FileType == 2 { // DT_DIR
			if err := deleteDirDeleteDir(rw, partOff, sb, childPath); err != nil {
				return err
			}
		} else {
			if err := deleteDirDeleteFile(rw, partOff, sb, childPath); err != nil {
				return err
			}
		}
	}

	// Re-read parent (children deletions may have changed it).
	dirIn, err = deletePathLookup(rw, partOff, sb, parentPath)
	if err != nil {
		return err
	}

	// Remove the directory's entry from the parent.
	if err := deleteRemoveDirEntry(rw, partOff, sb, dirIn, name); err != nil {
		return fmt.Errorf("xfs: deleteDir remove parent entry: %w", err)
	}

	// Re-read to free the right inode.
	targetIn, err = deleteReadInode(rw, partOff, sb, childInoNum)
	if err != nil {
		return err
	}

	// Free directory data blocks (block/extent-form directories).
	if targetIn.format == inodeFmtExtents || targetIn.format == inodeFmtBtree {
		var exts []extent
		if targetIn.format == inodeFmtExtents {
			exts, err = deleteInlineExtents(targetIn)
		} else {
			exts, err = deleteBtreeExtents(rw, partOff, sb, targetIn)
		}
		if err != nil {
			return err
		}
		for _, e := range exts {
			if err := deleteFreeBlocks(rw, partOff, sb, e.startBlock, e.count); err != nil {
				return err
			}
		}
	}

	// Zero and free the inode.
	zeroInode(targetIn)
	if err := deleteWriteInode(rw, partOff, sb, targetIn); err != nil {
		return err
	}
	return deleteFreeInode(rw, partOff, sb, childInoNum)
}
