package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
)

// On-disk quota record (struct xfs_dqblk) offsets. The disk dquot
// (xfs_disk_dquot) occupies the first 104 bytes; the v5 wrapper adds a CRC,
// LSN and UUID. Layout mirrors fs/xfs/libxfs/xfs_format.h.
const (
	dqMagic      = 0x4451  // 'DQ' — XFS_DQUOT_MAGIC (__be16 at offset 0)
	dqVersion    = 1       // XFS_DQUOT_VERSION
	dqBlkSize    = 136     // sizeof(struct xfs_dqblk)
	dqOffMagic   = 0       // d_magic  (__be16)
	dqOffVersion = 2       // d_version (__u8)
	dqOffType    = 3       // d_type/d_flags (__u8)
	dqOffID      = 4       // d_id     (__be32)
	dqOffBCount  = 40      // d_bcount (__be64) blocks charged to this id
	dqOffICount  = 48      // d_icount (__be64) inodes charged to this id
	dqOffCRC     = 104 + 4 // dd_crc  (__le32) — after 104-byte disk dquot + 4-byte fill
	dqOffUUID    = 120     // dd_uuid  (16 bytes)

	// d_type quota-type flags (XFS_DQTYPE_*/XFS_DQ_*).
	dqTypeUser  = 0x01
	dqTypeProj  = 0x02
	dqTypeGroup = 0x04
)

// injectable seams for tests.
var (
	quotaAllocInode  = allocInode
	quotaAllocBlocks = allocBlocks
	quotaWriteInode  = writeInode
	quotaWriteBlocks = writeBlocksData
	quotaSyncCounts  = syncSuperblockCounts
)

// buildDquot writes a single on-disk dquot for identity id of the given type
// into dst (must be >= dqBlkSize bytes) with the supplied block/inode usage
// counts, CRC-stamped for v5.
func buildDquot(dst []byte, sb *superblock, dqType uint8, id uint32, bcount, icount uint64) {
	be := binary.BigEndian
	clear(dst[:dqBlkSize])
	be.PutUint16(dst[dqOffMagic:], dqMagic)
	dst[dqOffVersion] = dqVersion
	dst[dqOffType] = dqType
	be.PutUint32(dst[dqOffID:], id)
	be.PutUint64(dst[dqOffBCount:], bcount)
	be.PutUint64(dst[dqOffICount:], icount)
	// Limits stay zero (no enforcement thresholds configured).
	if sb.hasCRC {
		copy(dst[dqOffUUID:dqOffUUID+16], sb.uuid[:])
		updateCRC(dst[:dqBlkSize], dqOffCRC, dqBlkSize)
	}
}

// dquotsPerBlock is the number of on-disk dquots that fit in one filesystem
// block (the dquot "cluster" the kernel initialises in bulk).
func dquotsPerBlock(sb *superblock) int {
	return int(sb.blockSize) / dqBlkSize
}

// buildDquotBlock fills a whole block with valid zero-usage dquots for the
// consecutive identities [0, dquotsPerBlock), exactly as the kernel's
// xfs_qm_init_dquot_blk does. Every slot must carry a valid dquot (magic +
// CRC) or the buffer verifier rejects the block — a block with only slot 0
// populated fails on the zeroed slots.
func buildDquotBlock(block []byte, sb *superblock, dqType uint8) {
	n := dquotsPerBlock(sb)
	for i := 0; i < n; i++ {
		buildDquot(block[i*dqBlkSize:], sb, dqType, uint32(i), 0, 0)
	}
}

// setupQuota creates the classic (non-metadir) quota inodes selected by cfg,
// seeds each with the id-0 dquot, sets the superblock quota fields, and
// reconciles the free counters. It runs on the freshly-formatted, open
// filesystem so it can reuse the standard inode/block allocators.
func (fs *xfsFS) setupQuota(cfg QuotaConfig) error {
	rw := fs.f
	partOff := fs.partOffset
	sb := fs.sb

	// mkQuotaInode allocates a quota inode (a regular file holding one block of
	// dquots) and returns its inode number.
	mkQuotaInode := func(dqType uint8) (uint64, error) {
		ino, err := quotaAllocInode(rw, partOff, sb, 0)
		if err != nil {
			return 0, fmt.Errorf("alloc quota inode: %w", err)
		}
		blk, err := quotaAllocBlocks(rw, partOff, sb, 0, 1)
		if err != nil {
			return 0, fmt.Errorf("alloc quota block: %w", err)
		}
		// Quota inodes are S_IFREG with no permission bits and di_size 0 (the
		// size field is unused — dquots are indexed by block, matching what the
		// kernel writes for a classic quota inode).
		const mode = uint16(0o100000) // S_IFREG
		buf := make([]byte, sb.inodeSize)
		initInodeV3(buf, ino, mode, sb.inodeSize, 1, sb.uuid)
		in := &inode{num: ino, mode: mode, raw: buf}
		setInodeFormat(in, inodeFmtExtents)
		setInodeSize(in, 0)
		setInodeNBlocks(in, 1)
		setInodeNExtents(in, 1)
		if err := writeExtentList(in, []extent{{startOff: 0, startBlock: blk, count: 1}}); err != nil {
			return 0, err
		}
		if err := quotaWriteInode(rw, partOff, sb, in); err != nil {
			return 0, err
		}
		// One block of dquots: every slot in the cluster is initialised with a
		// valid dquot for consecutive ids, matching the kernel's bulk
		// initialisation (a partially-populated block fails the verifier).
		data := make([]byte, sb.blockSize)
		buildDquotBlock(data, sb, dqType)
		if err := quotaWriteBlocks(rw, partOff, sb, blk, 1, data); err != nil {
			return 0, err
		}
		return ino, nil
	}

	if cfg.User {
		ino, err := mkQuotaInode(dqTypeUser)
		if err != nil {
			return err
		}
		sb.uQuotino = ino
	}
	if cfg.Group {
		ino, err := mkQuotaInode(dqTypeGroup)
		if err != nil {
			return err
		}
		sb.gQuotino = ino
	}
	if cfg.Project {
		ino, err := mkQuotaInode(dqTypeProj)
		if err != nil {
			return err
		}
		sb.pQuotino = ino
	}
	sb.quotaFlags = cfg.qflags()

	// Write the quota inode numbers + qflags to the PRIMARY superblock only.
	// The secondary superblocks are static geometry snapshots that predate the
	// quota inodes (the kernel leaves their quota fields zeroed); propagating
	// quota state to them makes xfs_repair distrust the primary's quota fields
	// and report the quota inodes as disconnected.
	buf := buildSuperblockBuffer(sb, sb.agCount, sb.uuid, sb.label)
	if _, err := rw.WriteAt(buf, partOff); err != nil {
		return fmt.Errorf("write primary SB: %w", err)
	}
	// Reconcile icount/ifree/fdblocks from the AG headers the allocators
	// updated. syncSuperblockCounts rewrites only the primary and preserves the
	// quota fields just written.
	if err := quotaSyncCounts(rw, partOff, sb); err != nil {
		return err
	}
	// Charge the actual on-disk usage into the dquots so the quota accounting
	// matches what xfs_repair recomputes.
	return quotaRecompute(rw, partOff, sb)
}
