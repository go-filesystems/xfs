package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path"
	"strings"
)

var renamePathLookup = pathLookup
var renameLookupInDir = lookupInDir
var renameReadInode = readInode
var renameDeleteDir = deleteDir
var renameDeleteFile = deleteFile
var renameAddDirEntry = addDirEntry
var renameRemoveDirEntry = removeDirEntry
var renameUpdateDotDot = xfsUpdateDotDot
var renameInlineExtents = inlineExtents
var renameBtreeExtents = btreeExtents
var renameReadRawBlock = readRawBlock
var renameWriteRawBlock = writeRawBlock
var renameWriteInode = writeInode

// renameEntry moves the filesystem object at oldPath to newPath.
//
// Semantics:
//   - If newPath does not exist the entry is simply moved.
//   - If newPath exists and matches the type of oldPath it is replaced
//     (directories must be empty).
//   - Moving a directory across parents updates the ".." entry in the
//     moved directory.
func renameEntry(rw readerWriterAt, partOff int64, sb *superblock, oldPath, newPath string) error {
	oldPath = path.Clean(oldPath)
	newPath = path.Clean(newPath)

	oldParPath, oldName := path.Split(oldPath)
	oldParPath = strings.TrimSuffix(oldParPath, "/")
	if oldParPath == "" {
		oldParPath = "/"
	}
	newParPath, newName := path.Split(newPath)
	newParPath = strings.TrimSuffix(newParPath, "/")
	if newParPath == "" {
		newParPath = "/"
	}

	oldParIn, err := renamePathLookup(rw, partOff, sb, oldParPath)
	if err != nil {
		return fmt.Errorf("xfs: rename: source parent %q: %w", oldParPath, err)
	}
	newParIn, err := renamePathLookup(rw, partOff, sb, newParPath)
	if err != nil {
		return fmt.Errorf("xfs: rename: destination parent %q: %w", newParPath, err)
	}

	// Locate source inode number (without following the last component as a symlink).
	srcInoNum, err := renameLookupInDir(rw, partOff, sb, oldParIn, oldName)
	if err != nil {
		return fmt.Errorf("xfs: rename: source %q: %w", oldPath, err)
	}
	srcIn, err := renameReadInode(rw, partOff, sb, srcInoNum)
	if err != nil {
		return err
	}

	// No-op.
	if oldParIn.num == newParIn.num && oldName == newName {
		return nil
	}

	// Handle existing destination.
	dstInoNum, err := renameLookupInDir(rw, partOff, sb, newParIn, newName)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err == nil {
		dstIn, err := renameReadInode(rw, partOff, sb, dstInoNum)
		if err != nil {
			return err
		}
		if srcIn.isDir() && !dstIn.isDir() {
			return fmt.Errorf("xfs: rename: cannot replace non-directory %q with a directory", newPath)
		}
		if !srcIn.isDir() && dstIn.isDir() {
			return fmt.Errorf("xfs: rename: cannot replace directory %q with a non-directory", newPath)
		}
		if dstIn.isDir() {
			if err := renameDeleteDir(rw, partOff, sb, newPath); err != nil {
				return err
			}
		} else {
			if err := renameDeleteFile(rw, partOff, sb, newPath); err != nil {
				return err
			}
		}
	}

	// Determine file type byte.
	ftype := uint8(1) // DT_REG
	if srcIn.isDir() {
		ftype = 2 // DT_DIR
	} else if srcIn.isSymlink() {
		ftype = 7 // DT_LNK
	}

	// Re-read parent inodes in case they were modified by the delete above.
	oldParIn, err = renamePathLookup(rw, partOff, sb, oldParPath)
	if err != nil {
		return err
	}
	newParIn, err = renamePathLookup(rw, partOff, sb, newParPath)
	if err != nil {
		return err
	}

	// Add new entry and remove old entry.
	if err := renameAddDirEntry(rw, partOff, sb, newParIn, srcInoNum, newName, ftype); err != nil {
		return err
	}
	// Re-read old parent after addDirEntry may have converted it.
	oldParIn, err = renamePathLookup(rw, partOff, sb, oldParPath)
	if err != nil {
		return err
	}
	if err := renameRemoveDirEntry(rw, partOff, sb, oldParIn, oldName); err != nil {
		return err
	}

	// For a cross-parent directory move, update ".." in the moved directory.
	if srcIn.isDir() && oldParIn.num != newParIn.num {
		// Re-read srcIn (it was not modified, but we need fresh data).
		srcIn, err = renameReadInode(rw, partOff, sb, srcInoNum)
		if err != nil {
			return err
		}
		if err := renameUpdateDotDot(rw, partOff, sb, srcIn, newParIn.num); err != nil {
			return err
		}
	}

	return nil
}

// xfsUpdateDotDot rewrites the parent-inode reference inside the directory
// dirIn so it points to newParentIno.
//
// For short-form (local) directories the parent is stored in the sf header.
// For block/extent directories the ".." entry is an explicit data record.
func xfsUpdateDotDot(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode, newParentIno uint64) error {
	switch dirIn.format {
	case inodeFmtLocal:
		// SF header: [count(1)][i8count(1)][parent(4 or 8)]
		fork := dirIn.dataFork()
		if len(fork) < 6 {
			return fmt.Errorf("xfs: updateDotDot: short fork in inode %d", dirIn.num)
		}
		i8count := int(fork[1])
		if i8count > 0 {
			// 8-byte parent.
			if len(fork) < 10 {
				return fmt.Errorf("xfs: updateDotDot: fork too short for 8-byte parent")
			}
			binary.BigEndian.PutUint64(fork[2:], newParentIno)
		} else {
			binary.BigEndian.PutUint32(fork[2:], uint32(newParentIno))
		}
		return renameWriteInode(rw, partOff, sb, dirIn)

	case inodeFmtExtents, inodeFmtBtree:
		var exts []extent
		var err error
		if dirIn.format == inodeFmtExtents {
			exts, err = renameInlineExtents(dirIn)
		} else {
			exts, err = renameBtreeExtents(rw, partOff, sb, dirIn)
		}
		if err != nil {
			return err
		}
		leafLogBlock := dirLeafByteOffset / uint64(sb.blockSize)
		be := binary.BigEndian
		for _, e := range exts {
			if e.startOff >= leafLogBlock {
				continue
			}
			for b := uint32(0); b < e.count; b++ {
				blk, err := renameReadRawBlock(rw, partOff, sb, e.startBlock+uint64(b))
				if err != nil {
					return err
				}
				hdrSize := dirDataHdrSize(sb.hasCRC)
				off := hdrSize
				modified := false
				for off+10 <= len(blk) {
					freetag := be.Uint16(blk[off:])
					if freetag == dirFreeTag {
						l := int(be.Uint16(blk[off+2:]))
						if l < 8 {
							break
						}
						off += l
						continue
					}
					ino := be.Uint64(blk[off:])
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
					n := string(blk[off+9 : nameEnd])
					if n == ".." {
						be.PutUint64(blk[off:], newParentIno)
						modified = true
						break
					}
					off += dirEntrySize(namelen, sb.hasFType)
				}
				if modified {
					if sb.hasCRC && len(blk) >= dir3DataHdrSize {
						updateCRC(blk, 4, len(blk))
					}
					return renameWriteRawBlock(rw, partOff, sb, e.startBlock+uint64(b), blk)
				}
			}
		}
		return nil

	default:
		return fmt.Errorf("xfs: updateDotDot: unsupported dir format %d for inode %d", dirIn.format, dirIn.num)
	}
}
