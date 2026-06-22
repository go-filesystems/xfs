package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"path"
	"strings"
)

var dirReadInode = readInode
var dirLookupInDirHook = lookupInDir
var dirReadSymlinkTarget = readSymlinkTarget
var dirBlockDirLookup = blockDirLookup
var dirInlineExtents = inlineExtents
var dirBtreeExtents = btreeExtents
var dirReadRawBlock = readRawBlock
var dirBlockReadDir = blockReadDir

// dirPathLookup is the recursive symlink-resolution seam. It carries the
// running symlink depth so the C4 loop bound holds across hops without a
// shared global (which would race under the FS read lock). Tests may replace
// it to intercept recursive resolution.
var dirPathLookup func(r io.ReaderAt, partOff int64, sb *superblock, p string, depth int) (*inode, error)

func init() {
	dirPathLookup = pathLookupResolve
}

// v5 directory data block header size (xfs_dir3_data_hdr).
const (
	dir3DataHdrSize  = 64 // 48-byte blk_hdr + 3×4-byte best_free + 4-byte pad
	dir2DataHdrSize  = 16 // v4 xfs_dir2_data_hdr: magic(4)+bestfree[3]×4+pad(0)=16
	dir3BlockHdrSize = 64 // same as dir3DataHdrSize for single-block form
	dir2BlockHdrSize = 16
)

// ErrNotFound is returned when a path component is not found.
var ErrNotFound = errors.New("not found")

// maxSymlinkDepth bounds the chain of symlinks pathLookup will follow before
// giving up, matching the Linux MAXSYMLINKS limit. C4: a symlink that points
// at itself (or a cycle of symlinks) would otherwise recurse without bound and
// overflow the stack on a hostile image.
const maxSymlinkDepth = 40

// ErrSymlinkLoop is returned when symlink resolution exceeds maxSymlinkDepth.
var ErrSymlinkLoop = errors.New("xfs: too many levels of symbolic links")

// pathLookup resolves a slash-separated path and returns the inode. It begins a
// fresh symlink-resolution chain (depth 0).
func pathLookup(r io.ReaderAt, partOff int64, sb *superblock, p string) (*inode, error) {
	return pathLookupResolve(r, partOff, sb, p, 0)
}

// pathLookupResolve is the production symlink-following resolver bound to the
// dirPathLookup hook. depth is the number of symlinks already followed to reach
// this call; once it reaches maxSymlinkDepth the lookup fails with
// ErrSymlinkLoop instead of recursing into a stack overflow (C4). depth is a
// plain parameter, so concurrent lookups under the FS read lock do not race.
func pathLookupResolve(r io.ReaderAt, partOff int64, sb *superblock, p string, depth int) (*inode, error) {
	if depth >= maxSymlinkDepth {
		return nil, ErrSymlinkLoop
	}
	p = path.Clean(p)
	root, err := dirReadInode(r, partOff, sb, sb.rootIno)
	if err != nil {
		return nil, fmt.Errorf("xfs: read root inode: %w", err)
	}
	if p == "/" || p == "." {
		return root, nil
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	cur := root
	for _, name := range parts {
		childIno, err := dirLookupInDirHook(r, partOff, sb, cur, name)
		if err != nil {
			return nil, fmt.Errorf("xfs: %q in path %q: %w", name, p, err)
		}
		child, err := dirReadInode(r, partOff, sb, childIno)
		if err != nil {
			return nil, fmt.Errorf("xfs: inode %d: %w", childIno, err)
		}
		// Follow symlinks, bounding the chain depth (C4).
		if child.isSymlink() {
			target, err := dirReadSymlinkTarget(r, partOff, sb, child)
			if err != nil {
				return nil, err
			}
			cur, err = dirPathLookup(r, partOff, sb, string(target), depth+1)
			if err != nil {
				return nil, err
			}
			continue
		}
		cur = child
	}
	return cur, nil
}

// lookupInDir finds the inode number of name inside directory in.
// Returns (0, ErrNotFound) when not found.
func lookupInDir(r io.ReaderAt, partOff int64, sb *superblock, in *inode, name string) (uint64, error) {
	switch in.format {
	case inodeFmtLocal:
		return sfLookup(in.dataFork(), name, sb.hasFType)
	case inodeFmtExtents, inodeFmtBtree:
		return dirBlockDirLookup(r, partOff, sb, in, name)
	default:
		return 0, fmt.Errorf("xfs: inode %d unsupported dir format %d", in.num, in.format)
	}
}

// sfLookup searches a short-form (inline) directory for name.
// Short-form layout:
//
//	[0] count       – number of entries
//	[1] i8count     – entries whose inode number needs 8 bytes
//	[2..5 or 2..9]  – parent inode (4 or 8 bytes when i8count > 0)
//	per entry:  namelen(1) + offset(2) + name(namelen) + ftype?(1) + ino(4 or 8)
func sfLookup(fork []byte, name string, hasFType bool) (uint64, error) {
	if len(fork) < 6 {
		return 0, ErrNotFound
	}
	count := int(fork[0])
	i8count := int(fork[1])
	parentSize := 4
	if i8count > 0 {
		parentSize = 8
	}
	off := 2 + parentSize // skip header
	for i := 0; i < count; i++ {
		if off+3 >= len(fork) {
			break
		}
		namelen := int(fork[off])
		off += 3 // namelen + 2-byte offset
		if off+namelen > len(fork) {
			break
		}
		n := string(fork[off : off+namelen])
		off += namelen
		if hasFType {
			off++ // skip ftype
		}
		inoSize := 4
		if i8count > 0 {
			inoSize = 8
		}
		if off+inoSize > len(fork) {
			break
		}
		var ino uint64
		if inoSize == 8 {
			ino = binary.BigEndian.Uint64(fork[off:])
		} else {
			ino = uint64(binary.BigEndian.Uint32(fork[off:]))
		}
		off += inoSize
		if n == name {
			return ino, nil
		}
	}
	return 0, ErrNotFound
}

// blockDirLookup searches a block/leaf/node-form directory for name.
func blockDirLookup(r io.ReaderAt, partOff int64, sb *superblock, in *inode, name string) (uint64, error) {
	exts, err := dirExtents(r, partOff, sb, in)
	if err != nil {
		return 0, err
	}
	// Only iterate data blocks (logical offset < dirLeafByteOffset / blockSize).
	leafLogBlock := dirLeafByteOffset / uint64(sb.blockSize)
	for _, e := range exts {
		if e.startOff >= leafLogBlock {
			continue
		}
		for b := uint32(0); b < e.count; b++ {
			blk, err := dirReadRawBlock(r, partOff, sb, e.startBlock+uint64(b))
			if err != nil {
				return 0, err
			}
			ino, found := searchDirBlock(blk, name, sb.hasFType, sb.hasCRC, true)
			if found {
				return ino, nil
			}
		}
	}
	return 0, ErrNotFound
}

// dirExtents returns the extent list for a directory inode.
func dirExtents(r io.ReaderAt, partOff int64, sb *superblock, in *inode) ([]extent, error) {
	switch in.format {
	case inodeFmtExtents:
		return dirInlineExtents(in)
	case inodeFmtBtree:
		return dirBtreeExtents(r, partOff, sb, in)
	default:
		return nil, fmt.Errorf("xfs: inode %d unsupported format for dir extents: %d", in.num, in.format)
	}
}

// searchDirBlock scans a single directory data block for entries matching
// name. Returns (inode, true) if found. If isBlock is true the block also
// contains a leaf section at its end which we skip.
func searchDirBlock(blk []byte, name string, hasFType, hasCRC bool, _ bool) (uint64, bool) {
	hdrSize := dirDataHdrSize(hasCRC)
	if len(blk) <= hdrSize {
		return 0, false
	}
	off := hdrSize
	for off+8 < len(blk) {
		freetag := binary.BigEndian.Uint16(blk[off:])
		if freetag == dirFreeTag {
			// Unused slot: skip over it.
			length := int(binary.BigEndian.Uint16(blk[off+2:]))
			if length < 8 {
				break
			}
			off += length
			continue
		}
		// Check there is enough room for the minimum entry fields.
		if off+10 > len(blk) {
			break
		}
		ino := binary.BigEndian.Uint64(blk[off:])
		if ino == 0 {
			// Sentinel / padding — skip 8 bytes.
			off += 8
			continue
		}
		namelen := int(blk[off+8])
		nameEnd := off + 9 + namelen
		if nameEnd > len(blk) {
			break
		}
		n := string(blk[off+9 : nameEnd])
		entrySize := dirEntrySize(namelen, hasFType)
		if n == name {
			return ino, true
		}
		off += entrySize
	}
	return 0, false
}

// dirDataHdrSize returns the header size for a directory data block.
func dirDataHdrSize(hasCRC bool) int {
	if hasCRC {
		return dir3DataHdrSize
	}
	return dir2DataHdrSize
}

// dirEntrySize returns the padded byte size of a used directory entry.
// (inode:8 + namelen:1 + name + ftype:0or1 + tag:2) rounded up to 8.
func dirEntrySize(namelen int, hasFType bool) int {
	base := 8 + 1 + namelen + 2 // ino + namelen field + name + tag
	if hasFType {
		base++
	}
	return (base + 7) &^ 7
}

// readDir returns all entries in a directory.
func readDir(r io.ReaderAt, partOff int64, sb *superblock, in *inode) ([]DirEntry, error) {
	switch in.format {
	case inodeFmtLocal:
		return sfReadDir(in.dataFork(), sb.hasFType)
	case inodeFmtExtents, inodeFmtBtree:
		return dirBlockReadDir(r, partOff, sb, in)
	default:
		return nil, fmt.Errorf("xfs: inode %d unsupported dir format %d", in.num, in.format)
	}
}

// sfReadDir reads all entries from a short-form directory.
func sfReadDir(fork []byte, hasFType bool) ([]DirEntry, error) {
	if len(fork) < 6 {
		return nil, nil
	}
	count := int(fork[0])
	i8count := int(fork[1])
	parentSize := 4
	if i8count > 0 {
		parentSize = 8
	}
	off := 2 + parentSize
	entries := make([]DirEntry, 0, count)
	for i := 0; i < count; i++ {
		if off+3 >= len(fork) {
			break
		}
		namelen := int(fork[off])
		off += 3 // namelen + 2-byte offset
		if off+namelen > len(fork) {
			break
		}
		n := string(fork[off : off+namelen])
		off += namelen
		var ft uint8
		if hasFType {
			if off >= len(fork) {
				break
			}
			ft = fork[off]
			off++
		}
		inoSize := 4
		if i8count > 0 {
			inoSize = 8
		}
		if off+inoSize > len(fork) {
			break
		}
		var ino uint64
		if inoSize == 8 {
			ino = binary.BigEndian.Uint64(fork[off:])
		} else {
			ino = uint64(binary.BigEndian.Uint32(fork[off:]))
		}
		off += inoSize
		entries = append(entries, DirEntry{Inode: ino, Name: n, FileType: ft})
	}
	return entries, nil
}

// blockReadDir reads all entries from block/leaf/node-form directory.
func blockReadDir(r io.ReaderAt, partOff int64, sb *superblock, in *inode) ([]DirEntry, error) {
	exts, err := dirExtents(r, partOff, sb, in)
	if err != nil {
		return nil, err
	}
	leafLogBlock := dirLeafByteOffset / uint64(sb.blockSize)
	var entries []DirEntry
	for _, e := range exts {
		if e.startOff >= leafLogBlock {
			continue
		}
		for b := uint32(0); b < e.count; b++ {
			blk, err := dirReadRawBlock(r, partOff, sb, e.startBlock+uint64(b))
			if err != nil {
				return nil, err
			}
			blockEntries := parseDirBlock(blk, sb.hasFType, sb.hasCRC)
			entries = append(entries, blockEntries...)
		}
	}
	return entries, nil
}

// parseDirBlock extracts all directory entries from a data block.
func parseDirBlock(blk []byte, hasFType, hasCRC bool) []DirEntry {
	hdrSize := dirDataHdrSize(hasCRC)
	if len(blk) <= hdrSize {
		return nil
	}
	off := hdrSize
	var entries []DirEntry
	for off+10 <= len(blk) {
		freetag := binary.BigEndian.Uint16(blk[off:])
		if freetag == dirFreeTag {
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
			// Leaf section or padding — we are done with data entries.
			break
		}
		nameEnd := off + 9 + namelen
		if nameEnd > len(blk) {
			break
		}
		n := string(blk[off+9 : nameEnd])
		var ft uint8
		if hasFType {
			if nameEnd >= len(blk) {
				break
			}
			ft = blk[nameEnd]
		}
		// Skip . and .. entries in the listing.
		if n != "." && n != ".." {
			entries = append(entries, DirEntry{Inode: ino, Name: n, FileType: ft})
		}
		off += dirEntrySize(namelen, hasFType)
	}
	return entries
}

// writeDirEntry writes a new entry into a directory data block at the given
// free-slot offset. The caller must ensure the slot is large enough.
// Returns the byte size of the written entry.
func writeDirEntry(blk []byte, offset int, ino uint64, name string, ftype uint8, hasFType bool) int {
	be := binary.BigEndian
	be.PutUint64(blk[offset:], ino)
	namelen := len(name)
	blk[offset+8] = byte(namelen)
	copy(blk[offset+9:], name)
	if hasFType {
		blk[offset+9+namelen] = ftype
	}
	sz := dirEntrySize(namelen, hasFType)
	// Write the tag (byte offset from block start) at the end of the entry.
	binary.BigEndian.PutUint16(blk[offset+sz-2:], uint16(offset))
	return sz
}

// markSlotFree converts the slot at offset within blk to an unused entry with
// the given total byte length.
func markSlotFree(blk []byte, offset, length int) {
	// Zero the slot first so no stale inode numbers remain.
	clear(blk[offset : offset+length])
	binary.BigEndian.PutUint16(blk[offset:], dirFreeTag)
	binary.BigEndian.PutUint16(blk[offset+2:], uint16(length))
	// Tag at end.
	binary.BigEndian.PutUint16(blk[offset+length-2:], uint16(offset))
}

// findFreeSlot returns the offset of the first unused slot in blk large
// enough to hold need bytes, or -1 if none found.
func findFreeSlot(blk []byte, need int, hasFType, hasCRC bool) int {
	hdrSize := dirDataHdrSize(hasCRC)
	off := hdrSize
	for off+4 <= len(blk) {
		freetag := binary.BigEndian.Uint16(blk[off:])
		if freetag == dirFreeTag {
			length := int(binary.BigEndian.Uint16(blk[off+2:]))
			if length < 8 {
				return -1
			}
			if length >= need {
				return off
			}
			off += length
			continue
		}
		ino := binary.BigEndian.Uint64(blk[off:])
		if ino == 0 {
			off += 8
			continue
		}
		if off+9 > len(blk) {
			break
		}
		namelen := int(blk[off+8])
		if namelen == 0 {
			break
		}
		off += dirEntrySize(namelen, hasFType)
	}
	return -1
}

// insertIntoSlot writes a new directory entry into the free slot at offset.
// If the slot is larger than needed, the remainder becomes a new free chunk.
func insertIntoSlot(blk []byte, offset, need int, ino uint64, name string, ftype uint8, hasFType, hasCRC bool, absBlock uint64) error {
	totalFree := int(binary.BigEndian.Uint16(blk[offset+2:]))
	writeDirEntry(blk, offset, ino, name, ftype, hasFType)
	remain := totalFree - need
	if remain >= 8 { // minimum unused-entry size
		markSlotFree(blk, offset+need, remain)
	}
	// Update directory data header CRC if v5.
	if hasCRC {
		if len(blk) >= dir3DataHdrSize {
			updateCRC(blk, 4, len(blk)) // CRC field is at offset 4 in dir3_blk_hdr
		}
	}
	return nil
}

// addEntryToSFDir appends a new entry to a short-form directory inode.
// Returns false when the inode's data fork has no room and a format upgrade
// (sf → block) would be needed.
func addEntryToSFDir(in *inode, childIno uint64, name string, ftype uint8, sb *superblock) bool {
	fork := in.dataFork()
	if len(fork) < 2 {
		return false
	}
	count := int(fork[0])
	i8count := int(fork[1])
	parentSize := 4
	if i8count > 0 {
		parentSize = 8
	}
	inoSize := 4
	// Will the new inode number require 8 bytes?
	if childIno > 0xFFFFFFFF {
		inoSize = 8
	}

	// New entry byte size.
	extra := 1 + 2 + len(name) + inoSize // namelen + offset + name + ino
	if sb.hasFType {
		extra++
	}

	// Calculate current sf block size, and the dir2 "data block" offset the
	// new entry will occupy. Each sf entry's offset field is the byte offset
	// the entry *would* have in the equivalent dir2 data block — not its
	// position in the short-form fork. It starts past the data-block header
	// and the synthetic "." / ".." entries, then advances by each real
	// entry's 8-byte-aligned data-entry size. xfs_repair validates these
	// offsets, so they must match the canonical dir2 layout.
	dataOff := dirDataHdrSize(sb.hasCRC) +
		dir2DataEntSize(1, sb.hasFType) + // "."
		dir2DataEntSize(2, sb.hasFType) // ".."
	sfSize := 2 + parentSize // header
	pos := sfSize
	for i := 0; i < count; i++ {
		if pos >= len(fork) {
			return false
		}
		nl := int(fork[pos])
		pos += 3 + nl // namelen + offset(2) + name
		if sb.hasFType {
			pos++
		}
		entInoSize := 4
		if i8count > 0 {
			entInoSize = 8
		}
		pos += entInoSize
		dataOff += dir2DataEntSize(nl, sb.hasFType)
	}
	sfSize = pos

	// Check if there is room in the data fork — data fork ends at either
	// the attr fork or the end of the inode.
	forkCap := int(sb.inodeSize) - inodeCoreSize
	if in.forkOff != 0 {
		forkCap = int(in.forkOff) * 8 // attr fork at forkoff*8 bytes into data fork
	}
	if sfSize+extra > forkCap {
		return false
	}

	// Append the entry into the raw inode buffer.
	rawFork := in.raw[inodeCoreSize:]
	// Adjust inoSize all the existing entries if we need to upgrade to i8.
	// For simplicity: only handle the case where existing entries are i4 and
	// the new entry is also i4 (covers 99.9% of cloud images).
	if inoSize == 8 {
		// We'd need to rewrite all existing entries to i8; skip for now.
		return false
	}

	// Build new entry at sfSize.
	e := rawFork[sfSize:]
	e[0] = byte(len(name))
	binary.BigEndian.PutUint16(e[1:], uint16(dataOff)) // dir2 data-block offset
	copy(e[3:], name)
	p := 3 + len(name)
	if sb.hasFType {
		e[p] = ftype
		p++
	}
	binary.BigEndian.PutUint32(e[p:], uint32(childIno))

	// Update header entry count and the inode's di_size to the new
	// short-form length, or xfs_repair sees a header claiming more entries
	// than di_size accounts for and flags the data fork corrupt.
	rawFork[0] = byte(count + 1)
	setInodeSize(in, uint64(sfSize+extra))

	return true
}

// xfsDirHash computes the XFS directory name hash (xfs_da_hashname) used as
// the key in directory leaf entries. Verified against mkfs: "." → 0x2e,
// ".." → 0x172e.
func xfsDirHash(name []byte) uint32 {
	var hash uint32
	i, n := 0, len(name)
	for ; n >= 4; n -= 4 {
		hash = uint32(name[i])<<21 ^ uint32(name[i+1])<<14 ^ uint32(name[i+2])<<7 ^
			uint32(name[i+3]) ^ bits.RotateLeft32(hash, 7*4)
		i += 4
	}
	switch n {
	case 3:
		return uint32(name[i])<<14 ^ uint32(name[i+1])<<7 ^ uint32(name[i+2]) ^ bits.RotateLeft32(hash, 7*3)
	case 2:
		return uint32(name[i])<<7 ^ uint32(name[i+1]) ^ bits.RotateLeft32(hash, 7*2)
	case 1:
		return uint32(name[i]) ^ bits.RotateLeft32(hash, 7)
	default:
		return hash
	}
}

// dir2DataEntSize returns the 8-byte-aligned size of an xfs_dir2_data_entry
// for a name of the given length: inumber(8) + namelen(1) + name + tag(2),
// plus a file-type byte when ftype is enabled.
func dir2DataEntSize(namelen int, hasFType bool) int {
	sz := 8 + 1 + namelen + 2
	if hasFType {
		sz++
	}
	return (sz + 7) &^ 7
}
