package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// inode is a parsed in-memory representation of an XFS inode.
type inode struct {
	num     uint64
	mode    uint16
	format  uint8 // di_format: 1=local, 2=extents, 3=btree
	size    uint64
	nBlocks uint64 // di_nblocks
	nExts   uint32 // di_nextents
	forkOff uint8  // di_forkoff (in 8-byte units from data-fork start)
	raw     []byte // full inode buffer (inodeSize bytes)
}

func (in *inode) isRegular() bool { return in.mode&0xF000 == 0x8000 }
func (in *inode) isDir() bool     { return in.mode&0xF000 == 0x4000 }
func (in *inode) isSymlink() bool { return in.mode&0xF000 == 0xA000 }

// dataForkOffset returns the byte offset of the data fork within the raw
// inode buffer. For v3 inodes the core is always 176 bytes.
const inodeCoreSize = 176

func (in *inode) dataFork() []byte {
	return in.raw[inodeCoreSize:]
}

// v3 inode field offsets. All fields are big-endian unless tagged otherwise.
// Layout mirrors `struct xfs_dinode` from fs/xfs/libxfs/xfs_format.h.
const (
	inoOffMagic    = 0
	inoOffMode     = 2
	inoOffVersion  = 4
	inoOffFormat   = 5
	inoOffUID      = 8  // __be32
	inoOffGID      = 12 // __be32
	inoOffNLink    = 16 // __be32
	inoOffATime    = 32 // legacy timespec: sec(be32) + nsec(be32), 8 bytes total
	inoOffMTime    = 40
	inoOffCTime    = 48
	inoOffSize     = 56
	inoOffNBlocks  = 64
	inoOffNExtents = 76
	inoOffForkOff  = 82
	inoOffGen      = 92  // __be32 inode generation
	inoOffCRC      = 100 // __le32, v3 only
	inoOffFlags2   = 120 // __be64 di_flags2, v3 only
	inoOffCRTime   = 144 // v3 only: legacy timespec (sec+nsec)
	inoOffIno      = 152 // uint64 absolute inode number, v3 only

	// di_big_nextents: 64-bit data-fork extent count present when the inode
	// carries the NREXT64 feature. It overlays the legacy v3 padding right
	// after di_projid_hi, NOT the di_nextents slot at offset 76.
	inoOffBigNExtents = 24

	// xfsDiflag2Nrext64 (XFS_DIFLAG2_NREXT64) marks an inode that stores its
	// data-fork extent count in di_big_nextents. Set by modern mkfs.xfs.
	xfsDiflag2Nrext64 = 0x10
)

// inodeByteOff returns the absolute byte offset of inode ino within the image.
func inodeByteOff(sb *superblock, partOff int64, ino uint64) int64 {
	agBlockLog := uint64(sb.agBlkLog)
	inopBLog := uint64(sb.inopBLog)
	ag := ino >> (agBlockLog + inopBLog)
	agRel := ino & ((1 << (agBlockLog + inopBLog)) - 1)
	agBlock := agRel >> inopBLog
	blkOff := agRel & ((1 << inopBLog) - 1)
	return partOff + int64(ag)*int64(sb.agBlocks)*int64(sb.blockSize) +
		int64(agBlock)*int64(sb.blockSize) +
		int64(blkOff)*int64(sb.inodeSize)
}

// inoAG returns the allocation group number that owns ino.
func inoAG(sb *superblock, ino uint64) uint32 {
	return uint32(ino >> (uint64(sb.agBlkLog) + uint64(sb.inopBLog)))
}

// inoAGRel returns the AG-relative inode number.
func inoAGRel(sb *superblock, ino uint64) uint32 {
	mask := (uint64(1) << (uint64(sb.agBlkLog) + uint64(sb.inopBLog))) - 1
	return uint32(ino & mask)
}

// inoFromAGRel computes an absolute inode number from ag + AG-relative inode.
func inoFromAGRel(sb *superblock, ag uint32, agRel uint32) uint64 {
	return uint64(ag)<<(uint64(sb.agBlkLog)+uint64(sb.inopBLog)) | uint64(agRel)
}

// readInode reads the inode with number ino.
func readInode(r io.ReaderAt, partOff int64, sb *superblock, ino uint64) (*inode, error) {
	off := inodeByteOff(sb, partOff, ino)
	buf := make([]byte, sb.inodeSize)
	if err := readBytes(r, off, buf); err != nil {
		return nil, fmt.Errorf("xfs: read inode %d: %w", ino, err)
	}
	be := binary.BigEndian
	magic := be.Uint16(buf[inoOffMagic:])
	if magic != magicInode {
		return nil, fmt.Errorf("xfs: inode %d bad magic 0x%04X", ino, magic)
	}
	in := &inode{
		num:     ino,
		mode:    be.Uint16(buf[inoOffMode:]),
		format:  buf[inoOffFormat],
		size:    be.Uint64(buf[inoOffSize:]),
		nBlocks: be.Uint64(buf[inoOffNBlocks:]),
		nExts:   be.Uint32(buf[inoOffNExtents:]),
		forkOff: buf[inoOffForkOff],
		raw:     buf,
	}
	// Modern mkfs.xfs sets the NREXT64 feature, which moves the data-fork
	// extent count into the 64-bit di_big_nextents field; the legacy
	// di_nextents slot reads as 0. Without this, every extent-based file,
	// directory, or symlink on a kernel-created image looks empty.
	if buf[inoOffVersion] >= 3 && be.Uint64(buf[inoOffFlags2:])&xfsDiflag2Nrext64 != 0 {
		in.nExts = uint32(be.Uint64(buf[inoOffBigNExtents:]))
	}
	return in, nil
}

// writeInode writes a (possibly modified) inode back to the image, updating
// the CRC for v5 inodes.
func writeInode(rw io.WriterAt, partOff int64, sb *superblock, in *inode) error {
	if sb.hasCRC {
		updateCRC(in.raw, inoOffCRC, int(sb.inodeSize))
	}
	off := inodeByteOff(sb, partOff, in.num)
	if _, err := rw.WriteAt(in.raw, off); err != nil {
		return fmt.Errorf("xfs: write inode %d: %w", in.num, err)
	}
	return nil
}

// zeroInode marks inode in as unlinked/free by zeroing its mode.
func zeroInode(in *inode) {
	binary.BigEndian.PutUint16(in.raw[inoOffMode:], 0)
}

// setInodeSize updates di_size in the raw inode buffer.
func setInodeSize(in *inode, sz uint64) {
	binary.BigEndian.PutUint64(in.raw[inoOffSize:], sz)
	in.size = sz
}

// setInodeNBlocks updates di_nblocks.
func setInodeNBlocks(in *inode, n uint64) {
	binary.BigEndian.PutUint64(in.raw[inoOffNBlocks:], n)
	in.nBlocks = n
}

// setInodeNExtents updates di_nextents.
func setInodeNExtents(in *inode, n uint32) {
	binary.BigEndian.PutUint32(in.raw[inoOffNExtents:], n)
	in.nExts = n
}

// setInodeFormat updates di_format.
func setInodeFormat(in *inode, fmt uint8) {
	in.raw[inoOffFormat] = fmt
	in.format = fmt
}

// initInodeV3 initialises a fresh v3 inode in the buffer (inodeSize bytes).
// The caller is responsible for filling in the data fork and calling
// writeInode.
//
// `nlink` is the initial link count: 1 for a regular file or symlink (the
// dir entry that will point at the inode), 2 for a fresh directory (parent
// entry + "." self-reference). All four timestamp fields — atime, mtime,
// ctime, crtime (v3 birth time) — are stamped with the current time so
// `ls -l` and `stat(1)` don't show 1970.
func initInodeV3(buf []byte, ino uint64, mode uint16, inodeSize uint16, nlink uint32) {
	clear(buf)
	be := binary.BigEndian
	be.PutUint16(buf[inoOffMagic:], magicInode)
	be.PutUint16(buf[inoOffMode:], mode)
	buf[inoOffVersion] = 3
	buf[inoOffFormat] = inodeFmtLocal // start as local; caller updates
	be.PutUint32(buf[inoOffNLink:], nlink)
	now := time.Now().UTC()
	sec := uint32(now.Unix())
	nsec := uint32(now.Nanosecond())
	stampLegacy := func(off int) {
		be.PutUint32(buf[off:], sec)
		be.PutUint32(buf[off+4:], nsec)
	}
	stampLegacy(inoOffATime)
	stampLegacy(inoOffMTime)
	stampLegacy(inoOffCTime)
	stampLegacy(inoOffCRTime)
	be.PutUint64(buf[inoOffIno:], ino)
}
