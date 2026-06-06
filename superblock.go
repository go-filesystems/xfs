package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// Internal superblock representation — only the fields we use at runtime.
type superblock struct {
	blockSize uint32
	agBlocks  uint32 // blocks per allocation group
	agCount   uint32
	rootIno   uint64 // root directory inode number
	inodeSize uint16
	inopBlock uint16 // inodes per block
	inopBLog  uint8  // log2(inopBlock)
	agBlkLog  uint8  // ceil(log2(agBlocks))
	dirBlkLog uint8  // log2(dir block size in FS blocks)
	hasCRC    bool   // v5 CRC-enabled superblock
	hasFType  bool   // directory entries carry ftype byte
	features2 uint32 // v4 sb_features2 (v5 rarely needed)
	// UUID and Label are stored in the on-disk superblock but were not
	// previously retained in the in-memory representation. Grow operations
	// need the UUID to initialize new AG structures and the label when
	// rewriting the superblock.
	uuid  [16]byte
	label string
}

// Superblock field offsets (big-endian except CRC at 224 which is LE).
const (
	sbOffMagic        = 0
	sbOffBlockSize    = 4
	sbOffRootIno      = 56
	sbOffAgBlocks     = 84
	sbOffAgCount      = 88
	sbOffVersionNum   = 100
	sbOffInodeSize    = 104
	sbOffInopBlock    = 106
	sbOffBlockLog     = 120
	sbOffInodeLog     = 122
	sbOffInopBLog     = 123
	sbOffAgBlkLog     = 124
	sbOffDirBlkLog    = 192
	sbOffFeatures2    = 200
	sbOffFeatIncompat = 216
	sbOffCRC          = 224 // v5 only, __le32
	sbCRCLen          = 224 // bytes covered by CRC
)

// XFS version and feature bits.
const (
	xfsSBVersionBits = 0x000F
	xfsSBVersion5    = 5
	xfsSBFeatFType   = 0x00000008 // v5 sb_features_incompat ftype
	xfsSBv4FeatFType = 0x00000200 // v4 sb_features2 ftype (XFS_SB_VERSION2_FTYPE)
)

// readSuperblock reads and parses the XFS superblock from offset partOff.
func readSuperblock(r io.ReaderAt, partOff int64) (*superblock, error) {
	buf := make([]byte, 512)
	if err := readBytes(r, partOff, buf); err != nil {
		return nil, fmt.Errorf("xfs: read superblock: %w", err)
	}
	be := binary.BigEndian

	magic := be.Uint32(buf[sbOffMagic:])
	if magic != magicSB {
		return nil, fmt.Errorf("xfs: bad superblock magic 0x%08X at offset 0x%X", magic, partOff)
	}

	versionNum := be.Uint16(buf[sbOffVersionNum:])
	version := versionNum & xfsSBVersionBits
	hasCRC := version == xfsSBVersion5

	var hasFType bool
	if hasCRC {
		featIncompat := be.Uint32(buf[sbOffFeatIncompat:])
		hasFType = (featIncompat & xfsSBFeatFType) != 0
	} else {
		feat2 := be.Uint32(buf[sbOffFeatures2:])
		hasFType = (feat2 & xfsSBv4FeatFType) != 0
	}

	sb := &superblock{
		blockSize: be.Uint32(buf[sbOffBlockSize:]),
		agBlocks:  be.Uint32(buf[sbOffAgBlocks:]),
		agCount:   be.Uint32(buf[sbOffAgCount:]),
		rootIno:   be.Uint64(buf[sbOffRootIno:]),
		inodeSize: be.Uint16(buf[sbOffInodeSize:]),
		inopBlock: be.Uint16(buf[sbOffInopBlock:]),
		inopBLog:  buf[sbOffInopBLog],
		agBlkLog:  buf[sbOffAgBlkLog],
		dirBlkLog: buf[sbOffDirBlkLog],
		hasCRC:    hasCRC,
		hasFType:  hasFType,
		features2: be.Uint32(buf[sbOffFeatures2:]),
	}
	// Extract UUID (16 bytes at offset 32) and label (12 bytes at offset 108).
	copy(sb.uuid[:], buf[32:48])
	lbl := make([]byte, 12)
	copy(lbl, buf[108:120])
	sb.label = strings.TrimRight(string(lbl), "\x00 \t\n\r")
	if sb.blockSize == 0 || sb.agBlocks == 0 || sb.agCount == 0 || sb.inodeSize == 0 {
		return nil, fmt.Errorf("xfs: superblock has invalid geometry")
	}
	return sb, nil
}

// agByteOffset returns the byte offset of the first block of allocation group ag.
func (sb *superblock) agByteOffset(ag uint32) int64 {
	return int64(ag) * int64(sb.agBlocks) * int64(sb.blockSize)
}

// agFByteOffset returns the absolute byte offset of the AGF (block 1 of AG ag).
func (sb *superblock) agFByteOffset(partOff int64, ag uint32) int64 {
	return partOff + sb.agByteOffset(ag) + int64(sb.blockSize)
}

// agIByteOffset returns the absolute byte offset of the AGI (block 2 of AG ag).
func (sb *superblock) agIByteOffset(partOff int64, ag uint32) int64 {
	return partOff + sb.agByteOffset(ag) + 2*int64(sb.blockSize)
}

// dirBlocksPerBlock returns the number of filesystem blocks per directory
// "dirblock" (always a power of two, often 1).
func (sb *superblock) dirFSBlocks() uint32 {
	return 1 << sb.dirBlkLog
}
