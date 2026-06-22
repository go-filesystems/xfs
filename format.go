package filesystem_xfs

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
)

// ──────────────────── Layout constants ─────────────────────────────────────
//
// The on-disk geometry mirrors what mkfs.xfs produces for a v5 (CRC) image
// with 4 KiB blocks, 512-byte inodes, a 512-byte sector, and only the FTYPE
// incompat feature. Each allocation group begins with four sector-sized
// headers packed into block 0 — superblock, AGF, AGI, AGFL — followed by the
// free-space and inode B-tree roots at blocks 1/2/3. The empirically verified
// reference (xfs_repair 6.13 clean) is reproduced field-for-field below.

const (
	fmtBlockSize  = 4096
	fmtBlockLog   = 12 // log2(fmtBlockSize)
	fmtSectorSize = 512
	fmtSectorLog  = 9 // log2(fmtSectorSize)
	fmtInodeSize  = 512
	fmtInodeLog   = 9     // log2(fmtInodeSize)
	fmtInopBlock  = 8     // fmtBlockSize / fmtInodeSize
	fmtInopBLog   = 3     // log2(fmtInopBlock)
	fmtAgBlocks   = 16384 // blocks per AG (64 MiB)
	fmtAgBlkLog   = 14    // log2(fmtAgBlocks)

	// bb_blkno and other on-disk "disk addresses" are expressed in 512-byte
	// basic blocks regardless of the filesystem block size.
	fmtDaddrPerBlock = fmtBlockSize / fmtSectorSize // 8

	// Internal log. XFS requires a minimum internal-log size of 10 MiB. The
	// region is left zeroed; xfs_repair treats a zeroed log on a freshly
	// formatted fs as clean ("zero log").
	fmtLogBlocks = 2560 // 10 MiB / 4 KiB

	// Per-AG block assignment (sectors 0-3 of block 0 hold SB/AGF/AGI/AGFL):
	//   1 – bno B-tree root (free space by block)
	//   2 – cnt B-tree root (free space by count)
	//   3 – inobt root
	//  4+ – AGFL-reserved blocks / log / inode chunk / free space
	fmtBnoRoot   = 1
	fmtCntRoot   = 2
	fmtInobtRoot = 3
	fmtFirstData = 4 // first block past the B-tree roots

	// The free list reserves four blocks per AG, exactly as mkfs.xfs does.
	fmtAGFLCount = 4

	// One 64-inode chunk (8 blocks) is pre-allocated in AG 0; it holds the
	// root directory (inode rootino), the realtime bitmap/summary inodes
	// (rootino+1, rootino+2), and 61 free inodes.
	fmtInodesPerChunk = 64
	fmtChunkBlocks    = fmtInodesPerChunk / fmtInopBlock // 8

	// sb_inoalignmt for this geometry, as emitted by mkfs.xfs (inode cluster
	// alignment in filesystem blocks).
	fmtInoAlignmt = 4

	fmtMinSize = int64(fmtAgBlocks) * fmtBlockSize // 64 MiB minimum (1 AG)
)

// ──────────────────── AG layout ────────────────────────────────────────────

// agLayout describes where the variable-position structures land within one
// allocation group: the four reserved free-list blocks, the single free
// extent, and (where present) the internal log and the inode chunk.
type agLayout struct {
	agflBlocks      [fmtAGFLCount]uint32
	freeStart       uint32 // AG-relative first free block
	freeCount       uint32 // blocks in the single free extent
	hasLog          bool
	logStartRel     uint32 // AG-relative first log block
	hasInodeChunk   bool
	inodeChunkBlock uint32 // AG-relative first block of the inode chunk
}

// fmtLogAG returns the allocation group that hosts the internal log.
//
// We always place the log in AG 0 (immediately after the free-list blocks,
// with the inode chunk following it). mkfs.xfs prefers a middle AG, but
// pinning the log to AG 0 makes the log start and the root-inode location
// independent of the AG count — which is what lets Grow() append AGs without
// the superblock's logstart/rootino drifting away from the physical layout.
// xfs_repair accepts this: when the log lives in AG 0 it simply expects the
// root inode to follow the log, which is exactly where we put it.
func fmtLogAG(uint32) uint32 { return 0 }

// fmtAGLayoutFor computes the layout of allocation group ag.
func fmtAGLayoutFor(ag, agCount uint32) agLayout {
	var l agLayout
	logAG := fmtLogAG(agCount)
	setAGFL := func(first uint32) {
		for i := uint32(0); i < fmtAGFLCount; i++ {
			l.agflBlocks[i] = first + i
		}
	}

	switch {
	case ag == 0 && logAG == 0:
		// Single AG: B-tree roots, AGFL blocks, the log, then the inode chunk.
		setAGFL(fmtFirstData)
		l.hasLog = true
		l.logStartRel = fmtFirstData + fmtAGFLCount // 8
		l.hasInodeChunk = true
		l.inodeChunkBlock = l.logStartRel + fmtLogBlocks
		l.freeStart = l.inodeChunkBlock + fmtChunkBlocks
	case ag == 0:
		// AG 0 with the log elsewhere: AGFL blocks then the inode chunk.
		setAGFL(fmtFirstData)
		l.hasInodeChunk = true
		l.inodeChunkBlock = fmtFirstData + fmtAGFLCount // 8
		l.freeStart = l.inodeChunkBlock + fmtChunkBlocks
	case ag == logAG:
		// Log AG: the log sits right after the B-tree roots; the AGFL blocks
		// and free space follow the log.
		l.hasLog = true
		l.logStartRel = fmtFirstData // 4
		setAGFL(fmtFirstData + fmtLogBlocks)
		l.freeStart = fmtFirstData + fmtLogBlocks + fmtAGFLCount
	default:
		// Plain AG: B-tree roots then AGFL blocks.
		setAGFL(fmtFirstData)
		l.freeStart = fmtFirstData + fmtAGFLCount // 8
	}
	l.freeCount = fmtAgBlocks - l.freeStart
	return l
}

// fmtRootIno returns the absolute root-inode number for the given AG count.
func fmtRootIno(agCount uint32) uint64 {
	return uint64(fmtAGLayoutFor(0, agCount).inodeChunkBlock) * fmtInopBlock
}

// fmtLogStartAbs returns the absolute (filesystem-block) start of the log.
func fmtLogStartAbs(agCount uint32) uint64 {
	logAG := fmtLogAG(agCount)
	return uint64(logAG)*fmtAgBlocks + uint64(fmtAGLayoutFor(logAG, agCount).logStartRel)
}

// fmtTotalFreeBlocks returns sb_fdblocks: the sum over every AG of its
// free-extent blocks plus the four blocks held in its free list (which XFS
// counts as free/available space).
func fmtTotalFreeBlocks(agCount uint32) uint64 {
	var total uint64
	for ag := uint32(0); ag < agCount; ag++ {
		total += uint64(fmtAGLayoutFor(ag, agCount).freeCount) + fmtAGFLCount
	}
	return total
}

// ──────────────────── Public API ───────────────────────────────────────────

// FormatConfig holds optional parameters for Format.
type FormatConfig struct {
	// UUID is the filesystem UUID. A random UUID is generated when all bytes are zero.
	UUID [16]byte
	// Label is the volume label (up to 12 bytes; silently truncated).
	Label string
}

type fmtFile interface {
	WriteAt([]byte, int64) (int, error)
	Truncate(int64) error
	Close() error
}

var fmtOpenFile = func(path string) (fmtFile, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
}

var fmtRandRead = func(p []byte) (int, error) {
	return rand.Read(p)
}

var fmtOpenFS = Open

// Format creates a new XFS v5 filesystem in the file at path.
// sizeBytes must be a multiple of 4096 and at least 64 MiB (one AG).
// On success the newly formatted filesystem is opened and returned.
func Format(path string, sizeBytes int64, cfg FormatConfig) (FS, error) {
	if sizeBytes%fmtBlockSize != 0 {
		return nil, fmt.Errorf("xfs: format: size %d is not a multiple of %d",
			sizeBytes, fmtBlockSize)
	}
	if sizeBytes < fmtMinSize {
		return nil, fmt.Errorf("xfs: format: size %d too small (minimum %d bytes)",
			sizeBytes, fmtMinSize)
	}

	agCount := uint32(sizeBytes / fmtMinSize)

	// Generate a random UUID when none supplied.
	uuid := cfg.UUID
	if uuid == [16]byte{} {
		if _, err := fmtRandRead(uuid[:]); err != nil {
			return nil, fmt.Errorf("xfs: format: random UUID: %w", err)
		}
	}

	// Truncate label to 12 bytes.
	label := cfg.Label
	if len(label) > 12 {
		label = label[:12]
	}

	// Create / truncate the backing file.
	f, err := fmtOpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("xfs: format: %w", err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		f.Close()
		return nil, fmt.Errorf("xfs: format: truncate: %w", err)
	}

	sb := &superblock{
		blockSize:  fmtBlockSize,
		sectorSize: fmtSectorSize,
		agBlocks:   fmtAgBlocks,
		agCount:    agCount,
		rootIno:    fmtRootIno(agCount),
		inodeSize:  fmtInodeSize,
		inopBlock:  fmtInopBlock,
		inopBLog:   fmtInopBLog,
		agBlkLog:   fmtAgBlkLog,
		dirBlkLog:  0,
		hasCRC:     true,
		hasFType:   true,
		uuid:       uuid,
		label:      label,
	}

	// Write per-AG structures (headers, B-trees, AGFL, log region).
	for ag := uint32(0); ag < agCount; ag++ {
		if err := fmtWriteAG(f, sb, ag, uuid); err != nil {
			f.Close()
			return nil, fmt.Errorf("xfs: format AG %d: %w", ag, err)
		}
	}

	// Write the AG 0 inode chunk (root directory + rt inodes + free inodes).
	if err := fmtWriteInodeChunk(f, sb, uuid); err != nil {
		f.Close()
		return nil, fmt.Errorf("xfs: format inode chunk: %w", err)
	}

	// Stamp a clean (unmounted) internal log so xfs_repair sees a valid tail
	// rather than a zeroed log it would want to reformat.
	if err := fmtWriteLog(f, sb, agCount); err != nil {
		f.Close()
		return nil, fmt.Errorf("xfs: format log: %w", err)
	}

	// Write the primary superblock at byte 0 and identical secondaries in
	// every other AG.
	if err := fmtWriteSuperblock(f, sb, agCount, uuid, label); err != nil {
		f.Close()
		return nil, fmt.Errorf("xfs: format superblock: %w", err)
	}

	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("xfs: format: close: %w", err)
	}
	return fmtOpenFS(path, -1)
}

// ──────────────────── Per-AG writer ────────────────────────────────────────

func fmtWriteAG(f fmtFile, sb *superblock, ag uint32, uuid [16]byte) error {
	be := binary.BigEndian
	agBase := int64(ag) * int64(sb.agBlocks) * int64(sb.blockSize)
	l := fmtAGLayoutFor(ag, sb.agCount)
	absBase := uint64(ag) * uint64(sb.agBlocks)

	// ── AGF (sector 1) ─────────────────────────────────────────────────────
	agf := make([]byte, fmtSectorSize)
	be.PutUint32(agf[agfOffMagic:], magicAGF)
	be.PutUint32(agf[agfOffVersion:], 1)
	be.PutUint32(agf[agfOffSeqNo:], ag)
	be.PutUint32(agf[agfOffLength:], sb.agBlocks)
	be.PutUint32(agf[agfOffBnoRoot:], fmtBnoRoot)
	be.PutUint32(agf[agfOffCntRoot:], fmtCntRoot)
	be.PutUint32(agf[agfOffBnoLevel:], 1)
	be.PutUint32(agf[agfOffCntLevel:], 1)
	be.PutUint32(agf[agfOffFLFirst:], 1)
	be.PutUint32(agf[agfOffFLLast:], fmtAGFLCount)
	be.PutUint32(agf[agfOffFLCount:], fmtAGFLCount)
	be.PutUint32(agf[agfOffFreeBlks:], l.freeCount)
	be.PutUint32(agf[agfOffLongest:], l.freeCount)
	be.PutUint32(agf[agfOffBtreeBlks:], 0)
	copy(agf[agfOffUUID:agfOffUUID+16], uuid[:])
	updateCRC(agf, agfOffCRC, fmtSectorSize)
	if _, err := f.WriteAt(agf, sb.agFByteOffset(0, ag)); err != nil {
		return fmt.Errorf("write AGF: %w", err)
	}

	// ── AGI (sector 2) ─────────────────────────────────────────────────────
	agi := make([]byte, fmtSectorSize)
	be.PutUint32(agi[agiOffMagic:], magicAGI)
	be.PutUint32(agi[agiOffVersion:], 1)
	be.PutUint32(agi[agiOffSeqNo:], ag)
	be.PutUint32(agi[agiOffLength:], sb.agBlocks)
	be.PutUint32(agi[agiOffRoot:], fmtInobtRoot)
	be.PutUint32(agi[agiOffLevel:], 1)
	be.PutUint32(agi[agiOffDirIno:], 0xFFFFFFFF) // null
	// agi_unlinked[64] (bytes 40..296): all buckets empty = NULLAGINO.
	for i := 40; i+4 <= agiOffUUID; i += 4 {
		be.PutUint32(agi[i:], 0xFFFFFFFF)
	}
	if l.hasInodeChunk {
		be.PutUint32(agi[agiOffCount:], fmtInodesPerChunk)
		be.PutUint32(agi[agiOffFreeCount:], fmtInodesPerChunk-3)
		be.PutUint32(agi[agiOffNewIno:], uint32(fmtRootIno(sb.agCount)))
	} else {
		be.PutUint32(agi[agiOffNewIno:], 0xFFFFFFFF) // null
	}
	copy(agi[agiOffUUID:agiOffUUID+16], uuid[:])
	updateCRC(agi, agiOffCRC, fmtSectorSize)
	if _, err := f.WriteAt(agi, sb.agIByteOffset(0, ag)); err != nil {
		return fmt.Errorf("write AGI: %w", err)
	}

	// ── AGFL (sector 3) ────────────────────────────────────────────────────
	agfl := make([]byte, fmtSectorSize)
	be.PutUint32(agfl[aglOffMagic:], magicAGFL)
	be.PutUint32(agfl[aglOffSeqNo:], ag)
	copy(agfl[aglOffUUID:aglOffUUID+16], uuid[:])
	// All slots null (0xFFFFFFFF) except the four reserved blocks at indices
	// 1..fmtAGFLCount (matching flfirst=1).
	for i := aglOffBno; i+4 <= fmtSectorSize; i += 4 {
		be.PutUint32(agfl[i:], 0xFFFFFFFF)
	}
	for i := uint32(0); i < fmtAGFLCount; i++ {
		be.PutUint32(agfl[aglOffBno+int(i+1)*4:], l.agflBlocks[i])
	}
	updateCRC(agfl, aglOffCRC, fmtSectorSize)
	if _, err := f.WriteAt(agfl, sb.agFLByteOffset(0, ag)); err != nil {
		return fmt.Errorf("write AGFL: %w", err)
	}

	// ── B-tree roots (blocks 1/2/3) ────────────────────────────────────────
	// bno and cnt each hold a single record covering the whole free extent.
	if err := fmtWriteFreeBtree(f, agBase, absBase, uuid, ag, magicABTB, fmtBnoRoot, l.freeStart, l.freeCount); err != nil {
		return err
	}
	if err := fmtWriteFreeBtree(f, agBase, absBase, uuid, ag, magicABTC, fmtCntRoot, l.freeStart, l.freeCount); err != nil {
		return err
	}

	// inobt: a single record in AG 0, empty elsewhere.
	inobt := fmtBtreeHeader(absBase, uuid, ag, magicIAB3, fmtInobtRoot)
	if l.hasInodeChunk {
		hdr := btreeHdrSizeV5
		be.PutUint16(inobt[6:], 1)                                // numrecs
		be.PutUint32(inobt[hdr:], uint32(fmtRootIno(sb.agCount))) // ir_startino (AG-relative in AG 0)
		be.PutUint32(inobt[hdr+4:], fmtInodesPerChunk-3)          // ir_freecount
		be.PutUint64(inobt[hdr+8:], 0xFFFFFFFFFFFFFFF8)           // ir_free: inodes 0,1,2 in use
	}
	updateCRC(inobt, btreeCRCOff, fmtBlockSize)
	if _, err := f.WriteAt(inobt, agBase+int64(fmtInobtRoot)*int64(sb.blockSize)); err != nil {
		return fmt.Errorf("write inobt: %w", err)
	}

	return nil
}

// fmtBtreeHeader builds a v5 short-form B-tree block header with no records.
// bb_blkno is recorded in 512-byte basic blocks, as XFS expects.
func fmtBtreeHeader(absBase uint64, uuid [16]byte, ag uint32, magic uint32, agRelBlock uint32) []byte {
	be := binary.BigEndian
	blk := make([]byte, fmtBlockSize)
	be.PutUint32(blk[0:], magic)
	be.PutUint16(blk[4:], 0)           // level = 0 (leaf)
	be.PutUint16(blk[6:], 0)           // numrecs (caller overrides)
	be.PutUint32(blk[8:], 0xFFFFFFFF)  // left sibling: none
	be.PutUint32(blk[12:], 0xFFFFFFFF) // right sibling: none
	be.PutUint64(blk[16:], (absBase+uint64(agRelBlock))*fmtDaddrPerBlock)
	copy(blk[32:], uuid[:])    // bb_uuid
	be.PutUint32(blk[48:], ag) // bb_owner = AG number
	return blk
}

// fmtWriteFreeBtree writes a single-record bno or cnt B-tree leaf.
func fmtWriteFreeBtree(f fmtFile, agBase int64, absBase uint64, uuid [16]byte, ag, magic, agRelBlock, start, count uint32) error {
	be := binary.BigEndian
	blk := fmtBtreeHeader(absBase, uuid, ag, magic, agRelBlock)
	hdr := btreeHdrSizeV5
	be.PutUint16(blk[6:], 1)         // numrecs
	be.PutUint32(blk[hdr:], start)   // record: start block
	be.PutUint32(blk[hdr+4:], count) // record: block count
	updateCRC(blk, btreeCRCOff, fmtBlockSize)
	if _, err := f.WriteAt(blk, agBase+int64(agRelBlock)*fmtBlockSize); err != nil {
		return fmt.Errorf("write free B-tree (magic 0x%08X): %w", magic, err)
	}
	return nil
}

// ──────────────────── Inode chunk writer ───────────────────────────────────

// fmtWriteInodeChunk writes the 64-inode chunk in AG 0: the root directory
// (inode rootino), the realtime bitmap and summary inodes (rootino+1/+2,
// referenced by sb_rbmino/sb_rsumino), and 61 free inodes. Every slot carries
// a valid v5 inode header + CRC so xfs_repair can walk the cluster.
func fmtWriteInodeChunk(f fmtFile, sb *superblock, uuid [16]byte) error {
	rootIno := sb.rootIno
	for slot := uint64(0); slot < fmtInodesPerChunk; slot++ {
		ino := rootIno + slot
		buf := make([]byte, fmtInodeSize)
		switch slot {
		case 0:
			fmtBuildRootInode(buf, ino, uuid)
		case 1, 2:
			// Realtime bitmap / summary: empty extents-format regular files.
			fmtBuildEmptyReg(buf, ino, uuid)
		default:
			// Free inode: valid header, mode 0.
			initInodeV3(buf, ino, 0, fmtInodeSize, 0, uuid)
			buf[inoOffFormat] = inodeFmtExtents
		}
		updateCRC(buf, inoOffCRC, fmtInodeSize)
		if _, err := f.WriteAt(buf, inodeByteOff(sb, 0, ino)); err != nil {
			return fmt.Errorf("write inode %d: %w", ino, err)
		}
	}
	return nil
}

// fmtBuildRootInode fills buf with the root directory inode: an empty
// short-form directory whose only "entries" are the implicit "." and "..".
func fmtBuildRootInode(buf []byte, ino uint64, uuid [16]byte) {
	const mode = uint16(0o40755) // directory, rwxr-xr-x
	initInodeV3(buf, ino, mode, fmtInodeSize, 2, uuid)
	buf[inoOffFormat] = inodeFmtLocal

	// Short-form directory header: count=0, i8count=0, parent=self (4 bytes).
	fork := buf[inodeCoreSize:]
	fork[0] = 0
	fork[1] = 0
	binary.BigEndian.PutUint32(fork[2:], uint32(ino))
	binary.BigEndian.PutUint64(buf[inoOffSize:], 6) // 2 (header) + 4 (parent)
}

// fmtBuildEmptyReg fills buf with an empty extents-format regular file
// (used for the realtime bitmap/summary inodes, which carry no data on a
// filesystem without a realtime device).
func fmtBuildEmptyReg(buf []byte, ino uint64, uuid [16]byte) {
	const mode = uint16(0o100000) // S_IFREG, no permission bits (matches mkfs)
	initInodeV3(buf, ino, mode, fmtInodeSize, 1, uuid)
	buf[inoOffFormat] = inodeFmtExtents
	binary.BigEndian.PutUint64(buf[inoOffSize:], 0)
}

// ──────────────────── Log writer ───────────────────────────────────────────

// Log on-disk magic and field offsets (xlog_rec_header).
const (
	xlogHeaderMagic = 0xFEEDBABE
	xlogUnmountType = 0x556E // "Un" — xfs_unmount_log_format.magic
)

// fmtWriteLog stamps a clean, unmounted internal log: a single unmount record
// (an xlog_rec_header at the first log basic-block followed by a one-op data
// block) at cycle 1, mirroring mkfs.xfs. The remainder of the log stays zeroed
// — the head/tail point at this record so there is nothing to recover. Without
// it, xfs_repair sees a "totally zeroed log" and exits non-zero ("would format
// log").
func fmtWriteLog(f fmtFile, sb *superblock, agCount uint32) error {
	be := binary.BigEndian
	logByte := int64(fmtLogStartAbs(agCount)) * int64(sb.blockSize)

	// Basic block 0: the log record header.
	hdr := make([]byte, fmtSectorSize)
	be.PutUint32(hdr[0:], xlogHeaderMagic) // h_magicno
	be.PutUint32(hdr[4:], 1)               // h_cycle
	be.PutUint32(hdr[8:], 2)               // h_version
	be.PutUint32(hdr[12:], fmtSectorSize)  // h_len (data length = 1 BB)
	be.PutUint64(hdr[16:], 1<<32)          // h_lsn      = (cycle 1, block 0)
	be.PutUint64(hdr[24:], 1<<32)          // h_tail_lsn = (cycle 1, block 0)
	// h_crc (offset 32) stays 0, as mkfs.xfs writes it for the seed record.
	be.PutUint32(hdr[36:], 0xFFFFFFFF) // h_prev_block = -1
	be.PutUint32(hdr[40:], 1)          // h_num_logops = 1
	be.PutUint32(hdr[44:], 0xB0C0D0D0) // h_cycle_data[0]: displaced first word of BB1
	if _, err := f.WriteAt(hdr, logByte); err != nil {
		return fmt.Errorf("write log header: %w", err)
	}

	// Basic block 1: the unmount op. Its first word is overwritten by the
	// cycle stamp (1); the original (oh_tid = 0xB0C0D0D0) is saved in
	// h_cycle_data[0] above.
	data := make([]byte, fmtSectorSize)
	be.PutUint32(data[0:], 1) // cycle stamp (displaces oh_tid)
	be.PutUint32(data[4:], 8) // oh_len = 8 (unmount record payload)
	data[8] = 0xAA            // oh_clientid = XFS_LOG
	data[9] = 0x20            // oh_flags
	// xfs_unmount_log_format.magic at byte 12 (native/host order, as mkfs writes).
	data[12] = byte(xlogUnmountType & 0xFF)
	data[13] = byte(xlogUnmountType >> 8)
	if _, err := f.WriteAt(data, logByte+fmtSectorSize); err != nil {
		return fmt.Errorf("write log unmount record: %w", err)
	}
	return nil
}

// ──────────────────── Superblock writer ────────────────────────────────────

func fmtWriteSuperblock(f fmtFile, sb *superblock, agCount uint32, uuid [16]byte, label string) error {
	buf := buildSuperblockBuffer(sb, agCount, uuid, label)
	// Write the primary SB at byte 0 and an identical secondary SB at block 0
	// of every other AG; xfs_repair cross-checks secondaries against the primary.
	for ag := uint32(0); ag < agCount; ag++ {
		off := int64(ag) * int64(sb.agBlocks) * int64(sb.blockSize)
		if _, err := f.WriteAt(buf, off); err != nil {
			return fmt.Errorf("write superblock AG %d: %w", ag, err)
		}
	}
	return nil
}

// buildSuperblockBuffer assembles the 512-byte on-disk superblock buffer
// (v5, CRC-stamped). Shared by the primary-SB writer and the per-AG
// secondary-SB writer so the two never drift.
func buildSuperblockBuffer(sb *superblock, agCount uint32, uuid [16]byte, label string) []byte {
	buf := make([]byte, 512)
	be := binary.BigEndian

	rootIno := fmtRootIno(agCount)

	be.PutUint32(buf[sbOffMagic:], magicSB)
	be.PutUint32(buf[sbOffBlockSize:], sb.blockSize)
	be.PutUint64(buf[sbOffRootIno:], rootIno)
	be.PutUint64(buf[sbOffRbmino:], rootIno+1)
	be.PutUint64(buf[sbOffRsumino:], rootIno+2)
	be.PutUint32(buf[sbOffAgBlocks:], sb.agBlocks)
	be.PutUint32(buf[sbOffAgCount:], agCount)
	be.PutUint16(buf[sbOffVersionNum:], uint16(xfsSBVersionV5))
	be.PutUint16(buf[sbOffSectSize:], fmtSectorSize)
	be.PutUint16(buf[sbOffInodeSize:], sb.inodeSize)
	be.PutUint16(buf[sbOffInopBlock:], sb.inopBlock)
	buf[sbOffSectLog] = fmtSectorLog
	buf[sbOffBlockLog] = fmtBlockLog
	buf[sbOffInodeLog] = fmtInodeLog
	buf[sbOffInopBLog] = sb.inopBLog
	buf[sbOffAgBlkLog] = sb.agBlkLog
	buf[sbOffDirBlkLog] = 0
	be.PutUint32(buf[sbOffFeatIncompat:], xfsSBFeatFType)
	be.PutUint32(buf[sbOffFeatures2:], xfsSBFeatures2)
	be.PutUint32(buf[sbOffBadFeatures2:], xfsSBFeatures2) // historical duplicate
	be.PutUint32(buf[sbOffInoAlignmt:], fmtInoAlignmt)
	buf[sbOffImaxPct] = 25

	// Filesystem-wide totals that xfs_repair cross-checks against AG metadata.
	dblocks := uint64(agCount) * uint64(sb.agBlocks)
	be.PutUint64(buf[sbOffDBlocks:], dblocks)
	be.PutUint64(buf[sbOffIcount:], fmtInodesPerChunk)
	be.PutUint64(buf[sbOffIfree:], fmtInodesPerChunk-3)
	be.PutUint64(buf[sbOffFdblocks:], fmtTotalFreeBlocks(agCount))

	// Internal log (zeroed region; the kernel formats it on first mount).
	be.PutUint64(buf[sbOffLogStart:], fmtLogStartAbs(agCount))
	be.PutUint32(buf[sbOffLogBlocks:], fmtLogBlocks)

	// No realtime device: rblocks/rextents stay 0, but rextsize must be a
	// valid non-zero extent size (1 block) or xfs_repair flags "inconsistent
	// realtime geometry".
	be.PutUint32(buf[sbOffRExtSize:], 1)

	copy(buf[32:], uuid[:]) // sb_uuid
	copy(buf[108:], label)  // sb_fname

	// CRC (v5 only, __le32 at offset 224, covers bytes [0, 224)).
	updateCRC(buf, sbOffCRC, sbCRCLen)
	return buf
}

// writerAtOnly is the minimal writer interface fmtWriteSecondarySB needs.
// Both fmtFile (used by Format) and the blockBackend used by xfsFS.Grow
// satisfy it without requiring the lifecycle methods.
type writerAtOnly interface {
	WriteAt(p []byte, off int64) (int, error)
}

// fmtWriteSecondarySB writes a secondary superblock at block 0 of AG
// `ag` (absolute byte offset partOff + ag×agBlocks×blockSize). XFS keeps
// a secondary SB in every AG so xfs_repair and the kernel can recover
// from a corrupted primary, and SetLabel iterates them to keep the label
// consistent. Grow uses this for each newly-appended AG.
//
// Calling with ag == 0 is a no-op: AG 0's "secondary" SB is the primary
// itself, written by fmtWriteSuperblock.
func fmtWriteSecondarySB(w writerAtOnly, partOff int64, sb *superblock, ag uint32, agCount uint32, uuid [16]byte, label string) error {
	if ag == 0 {
		return nil
	}
	buf := buildSuperblockBuffer(sb, agCount, uuid, label)
	off := partOff + int64(ag)*int64(sb.agBlocks)*int64(sb.blockSize)
	if _, err := w.WriteAt(buf, off); err != nil {
		return fmt.Errorf("write secondary SB AG %d: %w", ag, err)
	}
	return nil
}
