package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"strings"
)

// Internal superblock representation — only the fields we use at runtime.
type superblock struct {
	blockSize  uint32
	sectorSize uint32 // sb_sectsize; AG headers (SB/AGF/AGI/AGFL) are sector-aligned
	agBlocks   uint32 // blocks per allocation group
	agCount    uint32
	rootIno    uint64 // root directory inode number
	inodeSize  uint16
	inopBlock  uint16 // inodes per block
	inopBLog   uint8  // log2(inopBlock)
	agBlkLog   uint8  // ceil(log2(agBlocks))
	dirBlkLog  uint8  // log2(dir block size in FS blocks)
	hasCRC     bool   // v5 CRC-enabled superblock
	hasFType   bool   // directory entries carry ftype byte
	features2  uint32 // v4 sb_features2 (v5 rarely needed)
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
	sbOffDBlocks      = 8
	sbOffRbmino       = 64  // realtime bitmap inode (rootino+1)
	sbOffRsumino      = 72  // realtime summary inode (rootino+2)
	sbOffSectSize     = 102 // sb_sectsize (__be16)
	sbOffSectLog      = 121 // sb_sectlog (__u8)
	sbOffLogStart     = 48
	sbOffRootIno      = 56
	sbOffRExtSize     = 80
	sbOffLogBlocks    = 96
	sbOffIcount       = 128
	sbOffIfree        = 136
	sbOffFdblocks     = 144
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
	sbOffImaxPct      = 127
	sbOffInoAlignmt   = 180
	sbOffFeatures2    = 200
	sbOffBadFeatures2 = 204
	sbOffFeatIncompat = 216
	sbOffCRC          = 224 // v5 only, __le32
	sbCRCLen          = 512 // CRC covers the whole superblock sector (sectsize)
)

// XFS version and feature bits.
const (
	xfsSBVersionBits = 0x000F
	xfsSBVersion5    = 5
	// xfsSBVersionV5 is the canonical v5 sb_versionnum: version 5 plus
	// MOREBITS|DIRV2|EXTFLG|LOGV2|ALIGN|NLINK — what mkfs.xfs writes (0xb4a5).
	xfsSBVersionV5   = 0xb4a5
	xfsSBFeatures2   = 0x0000018a // sb_features2 (LAZYSBCOUNT|ATTR2|PROJID32|FTYPE)
	xfsSBFeatFType   = 0x00000001 // v5 sb_features_incompat ftype (XFS_SB_FEAT_INCOMPAT_FTYPE)
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

	sectorSize := uint32(be.Uint16(buf[sbOffSectSize:]))
	if sectorSize == 0 {
		sectorSize = 512
	}
	sb := &superblock{
		blockSize:  be.Uint32(buf[sbOffBlockSize:]),
		sectorSize: sectorSize,
		agBlocks:   be.Uint32(buf[sbOffAgBlocks:]),
		agCount:    be.Uint32(buf[sbOffAgCount:]),
		rootIno:    be.Uint64(buf[sbOffRootIno:]),
		inodeSize:  be.Uint16(buf[sbOffInodeSize:]),
		inopBlock:  be.Uint16(buf[sbOffInopBlock:]),
		inopBLog:   buf[sbOffInopBLog],
		agBlkLog:   buf[sbOffAgBlkLog],
		dirBlkLog:  buf[sbOffDirBlkLog],
		hasCRC:     hasCRC,
		hasFType:   hasFType,
		features2:  be.Uint32(buf[sbOffFeatures2:]),
	}
	// Extract UUID (16 bytes at offset 32) and label (12 bytes at offset 108).
	copy(sb.uuid[:], buf[32:48])
	lbl := make([]byte, 12)
	copy(lbl, buf[108:120])
	sb.label = strings.TrimRight(string(lbl), "\x00 \t\n\r")
	if err := validateGeometry(sb); err != nil {
		return nil, err
	}
	return sb, nil
}

// Geometry bounds. XFS restricts these on disk; the kernel verifier
// (xfs_validate_sb_common) enforces the same. We reject anything outside
// them so an attacker cannot drive an out-of-bounds read or a bad allocation
// through corrupt geometry.
const (
	// minBlockSize / maxBlockSize bound sb_blocksize; XFS allows 512..65536.
	minBlockSize = 512
	maxBlockSize = 65536
	// inodeSize must be a power of two in this set and large enough to hold
	// the 176-byte v3 inode core plus a usable data fork.
	minInodeSize = 256
	maxInodeSize = 2048
)

// validateGeometry rejects superblocks whose geometry would let a corrupt or
// hostile image drive an out-of-bounds slice, a bad allocation, or a
// nonsensical shift. Every field an inode/extent read indexes against is
// constrained to a sane range here, once, at open time.
func validateGeometry(sb *superblock) error {
	if sb.agBlocks == 0 || sb.agCount == 0 {
		return fmt.Errorf("xfs: superblock has invalid geometry")
	}
	// C2: blockSize must be a power of two in [512,65536] and >= sectorSize.
	// Sub-512 or non-power-of-two blocks make block-relative reads
	// (blk[6:], blk[12:], directory/btree headers) index past the buffer and
	// corrupt every geometry computation.
	if sb.blockSize < minBlockSize || sb.blockSize > maxBlockSize ||
		bits.OnesCount32(sb.blockSize) != 1 {
		return fmt.Errorf("xfs: invalid block size %d (must be a power of two in [%d,%d])",
			sb.blockSize, minBlockSize, maxBlockSize)
	}
	if sb.blockSize < sb.sectorSize {
		return fmt.Errorf("xfs: block size %d smaller than sector size %d",
			sb.blockSize, sb.sectorSize)
	}
	// C1: inodeSize must be a power of two in {256,512,1024,2048} and at least
	// the 176-byte inode core so dataFork() (raw[176:]) and every fixed-offset
	// core read stay in bounds. Values 1..183 made the root-inode read OOB.
	if sb.inodeSize < minInodeSize || sb.inodeSize > maxInodeSize ||
		bits.OnesCount16(sb.inodeSize) != 1 || int(sb.inodeSize) < inodeCoreSize {
		return fmt.Errorf("xfs: invalid inode size %d (must be a power of two in [%d,%d])",
			sb.inodeSize, minInodeSize, maxInodeSize)
	}
	// H2: inopBlock / inopBLog / agBlkLog must be self-consistent and small
	// enough that the inode-number shifts in inodeByteOff cannot exceed 63.
	if sb.inopBlock == 0 || bits.OnesCount16(sb.inopBlock) != 1 {
		return fmt.Errorf("xfs: invalid inodes-per-block %d", sb.inopBlock)
	}
	if uint(sb.inopBLog) >= 16 || (uint16(1)<<sb.inopBLog) != sb.inopBlock {
		return fmt.Errorf("xfs: inopblog %d inconsistent with inopblock %d", sb.inopBLog, sb.inopBlock)
	}
	// agBlkLog must be ceil(log2(agBlocks)): the smallest n with 1<<n >= agBlocks.
	wantAgBlkLog := uint8(bits.Len32(sb.agBlocks - 1))
	if sb.agBlkLog != wantAgBlkLog {
		return fmt.Errorf("xfs: agblklog %d inconsistent with agblocks %d (want %d)",
			sb.agBlkLog, sb.agBlocks, wantAgBlkLog)
	}
	// The combined inode-number shift (ag << (agBlkLog+inopBLog)) must stay
	// below 64 or the shift is undefined / wraps.
	if uint(sb.agBlkLog)+uint(sb.inopBLog) >= 64 {
		return fmt.Errorf("xfs: agblklog %d + inopblog %d shift overflows", sb.agBlkLog, sb.inopBLog)
	}
	// dirBlkLog (1<<dirBlkLog FS blocks per dir block) must not overflow.
	if sb.dirBlkLog >= 32 {
		return fmt.Errorf("xfs: dirblklog %d too large", sb.dirBlkLog)
	}
	return nil
}

// agByteOffset returns the byte offset of the first block of allocation group ag.
func (sb *superblock) agByteOffset(ag uint32) int64 {
	return int64(ag) * int64(sb.agBlocks) * int64(sb.blockSize)
}

// sectSize returns the on-disk sector size, defaulting to 512 for an
// in-memory superblock built before the field was populated.
func (sb *superblock) sectSize() int64 {
	if sb.sectorSize == 0 {
		return 512
	}
	return int64(sb.sectorSize)
}

// agFByteOffset returns the absolute byte offset of the AGF, which lives at
// sector 1 of the AG (immediately after the superblock sector, inside block 0).
func (sb *superblock) agFByteOffset(partOff int64, ag uint32) int64 {
	return partOff + sb.agByteOffset(ag) + sb.sectSize()
}

// agIByteOffset returns the absolute byte offset of the AGI (sector 2 of the AG).
func (sb *superblock) agIByteOffset(partOff int64, ag uint32) int64 {
	return partOff + sb.agByteOffset(ag) + 2*sb.sectSize()
}

// agFLByteOffset returns the absolute byte offset of the AGFL (sector 3 of the AG).
func (sb *superblock) agFLByteOffset(partOff int64, ag uint32) int64 {
	return partOff + sb.agByteOffset(ag) + 3*sb.sectSize()
}

// dirBlocksPerBlock returns the number of filesystem blocks per directory
// "dirblock" (always a power of two, often 1).
func (sb *superblock) dirFSBlocks() uint32 {
	return 1 << sb.dirBlkLog
}
