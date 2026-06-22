package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
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
var writeReadDir = readDir
var writeWriteWholeDir = writeWholeDir

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
		physOff := partOff + int64(sb.fsbToPhysBlock(e.startBlock))*bs
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
	initInodeV3(inodeBuf, ino, mode, sb.inodeSize, 1, sb.uuid)

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

// dirEnt is a directory entry to be laid down in a block-form directory.
type dirEnt struct {
	name  string
	ino   uint64
	ftype uint8
}

// buildBlockDirBlock fills blk (a directory block of len blkSize) with a
// single-block ("block form") directory: the dir3/dir2 header, "." and ".."
// (always directories), the supplied entries, one free region recorded in
// bestfree, and the trailing leaf-entry array + xfs_dir2_block_tail. It is the
// single source of truth for the on-disk block layout, shared by short-form
// conversion and by subsequent adds. Returns an error if the entries don't fit
// one block (leaf form, not yet implemented).
func buildBlockDirBlock(sb *superblock, blk []byte, absBlock, ownerIno, parentIno uint64, entries []dirEnt) error {
	be := binary.BigEndian
	for i := range blk {
		blk[i] = 0
	}
	blkSize := len(blk)
	hdrSize := dirDataHdrSize(sb.hasCRC)

	if sb.hasCRC {
		// xfs_dir3_blk_hdr: magic@0, crc@4, blkno@8, lsn@16, uuid@24, owner@40.
		be.PutUint32(blk[0:], magicDir3Block)
		be.PutUint64(blk[8:], sb.fsbToPhysBlock(absBlock)*fmtDaddrPerBlock)
		copy(blk[24:40], sb.uuid[:])
		be.PutUint64(blk[40:], ownerIno)
	} else {
		be.PutUint32(blk[0:], magicDir2Block)
	}

	all := make([]dirEnt, 0, len(entries)+2)
	all = append(all, dirEnt{".", ownerIno, 2}, dirEnt{"..", parentIno, 2})
	all = append(all, entries...)

	// Tail (count+stale) is the last 8 bytes; the leaf array (8 bytes/entry)
	// precedes it.
	count := len(all)
	tailOff := blkSize - 8
	leafStart := tailOff - count*8

	type leafEnt struct{ hash, addr uint32 }
	leaves := make([]leafEnt, 0, count)
	off := hdrSize
	for _, e := range all {
		sz := dirEntrySize(len(e.name), sb.hasFType)
		if off+sz > leafStart {
			return fmt.Errorf("xfs: directory %d too large for single-block form (%d entries); leaf form not implemented", ownerIno, count)
		}
		writeDirEntry(blk, off, e.ino, e.name, e.ftype, sb.hasFType)
		leaves = append(leaves, leafEnt{xfsDirHash([]byte(e.name)), uint32(off) >> 3})
		off += sz
	}
	endData := off

	// The gap between the data entries and the leaf array is one free region,
	// recorded as an xfs_dir2_data_unused entry and in bestfree[0].
	if leafStart > endData {
		freeLen := leafStart - endData
		be.PutUint16(blk[endData:], 0xFFFF)                    // freetag
		be.PutUint16(blk[endData+2:], uint16(freeLen))         // length
		be.PutUint16(blk[endData+freeLen-2:], uint16(endData)) // tag = own offset
		bfOff := 48                                            // dir3 hdr: bestfree[3] at 48
		if !sb.hasCRC {
			bfOff = 4 // dir2 hdr: bestfree[3] right after magic
		}
		be.PutUint16(blk[bfOff:], uint16(endData))
		be.PutUint16(blk[bfOff+2:], uint16(freeLen))
	}

	// Leaf entries, sorted by hash (ties broken by address).
	sort.Slice(leaves, func(i, j int) bool {
		if leaves[i].hash != leaves[j].hash {
			return leaves[i].hash < leaves[j].hash
		}
		return leaves[i].addr < leaves[j].addr
	})
	for i, le := range leaves {
		p := leafStart + i*8
		be.PutUint32(blk[p:], le.hash)
		be.PutUint32(blk[p+4:], le.addr)
	}

	// xfs_dir2_block_tail.
	be.PutUint32(blk[tailOff:], uint32(count))
	be.PutUint32(blk[tailOff+4:], 0) // stale

	if sb.hasCRC {
		updateCRC(blk, 4, blkSize) // CRC at offset 4 in dir3_blk_hdr
	}
	return nil
}

// dirParentIno reads the parent inode (the ".." entry) of a non-short-form
// directory. The ".." entry always lives in logical data block 0, so this
// reads just that block regardless of whether the directory is in block, leaf
// or node form.
func dirParentIno(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode) (uint64, error) {
	exts, err := writeDirExtents(rw, partOff, sb, dirIn)
	if err != nil {
		return 0, err
	}
	leafLogBlock := dirLeafByteOffset / uint64(sb.blockSize)
	for _, e := range exts {
		if e.startOff != 0 || e.startOff >= leafLogBlock {
			continue
		}
		blk, err := writeReadRawBlock(rw, partOff, sb, e.startBlock)
		if err != nil {
			return 0, err
		}
		return blockDirParent(blk, sb.hasFType, sb.hasCRC), nil
	}
	return 0, fmt.Errorf("xfs: directory inode %d has no data block", dirIn.num)
}

// blockDirParent returns the inode number stored in a block-form directory's
// ".." entry (its parent), or 0 if not found.
func blockDirParent(blk []byte, hasFType, hasCRC bool) uint64 {
	off := dirDataHdrSize(hasCRC)
	for off+10 <= len(blk) {
		if binary.BigEndian.Uint16(blk[off:]) == dirFreeTag {
			length := int(binary.BigEndian.Uint16(blk[off+2:]))
			if length < 8 {
				break
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
		if off+9+namelen <= len(blk) && string(blk[off+9:off+9+namelen]) == ".." {
			return ino
		}
		off += dirEntrySize(namelen, hasFType)
	}
	return 0
}

// convertSFToBlock promotes a short-form directory to block form: it allocates
// a directory block and lays down all existing entries plus the new one.
func convertSFToBlock(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode, newIno uint64, newName string, newFtype uint8) error {
	be := binary.BigEndian

	// The parent inode lives in the short-form header (after count + i8count).
	fork := dirIn.dataFork()
	var parentIno uint64
	if fork[1] > 0 {
		parentIno = be.Uint64(fork[2:])
	} else {
		parentIno = uint64(be.Uint32(fork[2:]))
	}

	existing, err := writeSFReadDir(fork, sb.hasFType)
	if err != nil {
		return err
	}

	dirBlocks := sb.dirFSBlocks()
	ag := inoAG(sb, dirIn.num)
	absBlock, err := writeAllocBlocks(rw, partOff, sb, ag, dirBlocks)
	if err != nil {
		return fmt.Errorf("xfs: convertSFToBlock: %w", err)
	}

	entries := make([]dirEnt, 0, len(existing)+1)
	for _, e := range existing {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		entries = append(entries, dirEnt{e.Name, e.Inode, e.FileType})
	}
	entries = append(entries, dirEnt{newName, newIno, newFtype})

	blkSize := int(sb.blockSize) * int(dirBlocks)
	blk := make([]byte, blkSize)
	if err := buildBlockDirBlock(sb, blk, absBlock, dirIn.num, parentIno, entries); err != nil {
		return err
	}
	if err := writeWriteBlocksData(rw, partOff, sb, absBlock, dirBlocks, blk); err != nil {
		return err
	}

	// Point the inode at the new block via a single extent.
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

// addEntryToBlockDir adds an entry to an existing block/leaf/node-form
// directory by reading all of its current entries, appending the new one, and
// re-laying-out the whole directory via writeWholeDir. writeWholeDir picks the
// on-disk form (block when everything fits one block, otherwise leaf or node),
// so this single path drives block→leaf→node growth as entries accumulate.
func addEntryToBlockDir(rw readerWriterAt, partOff int64, sb *superblock, dirIn *inode, childIno uint64, name string, ftype uint8) error {
	parentIno, err := dirParentIno(rw, partOff, sb, dirIn)
	if err != nil {
		return err
	}
	existing, err := writeReadDir(rw, partOff, sb, dirIn) // excludes "." / ".."
	if err != nil {
		return err
	}
	entries := make([]dirEnt, 0, len(existing)+1)
	for _, e := range existing {
		if e.Name == name {
			return fmt.Errorf("xfs: %q already exists", name)
		}
		entries = append(entries, dirEnt{e.Name, e.Inode, e.FileType})
	}
	entries = append(entries, dirEnt{name, childIno, ftype})
	return writeWriteWholeDir(rw, partOff, sb, dirIn, parentIno, entries)
}
