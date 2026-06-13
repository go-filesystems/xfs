package filesystem_xfs

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
)

// ──────────────────── Layout constants ─────────────────────────────────────

const (
	fmtBlockSize = 4096
	fmtBlockLog  = 12 // log2(fmtBlockSize)
	fmtInodeSize = 512
	fmtInodeLog  = 9    // log2(fmtInodeSize)
	fmtInopBlock = 8    // fmtBlockSize / fmtInodeSize
	fmtInopBLog  = 3    // log2(fmtInopBlock)
	fmtAgBlocks  = 16384 // blocks per AG (64 MiB) — large enough to host the log in AG 0
	fmtAgBlkLog  = 14    // log2(fmtAgBlocks)

	// Internal log: a contiguous run in AG 0 immediately after the metadata
	// blocks. XFS requires a minimum internal-log size of XFS_MIN_LOG_BYTES
	// (10 MiB). The region is left zeroed — mkfs.xfs does the same, and
	// xfs_repair treats a zeroed log on a freshly-formatted fs as clean.
	fmtLogBlocks = 2560 // 10 MiB / 4 KiB
	fmtLogStart  = fmtMetaBlocksAG0

	// Per-AG block assignment:
	//   0  – primary/secondary superblock
	//   1  – AGF (free-space header)
	//   2  – AGI (inode-B-tree header)
	//   3  – bno B-tree leaf
	//   4  – cnt B-tree leaf
	//   5  – inobt leaf
	//   6  – root inode block (AG 0 only; holds inodes 48–55; inode 48 = rootIno)
	//  7+  – free in AG 0 (6+ free in every other AG)
	fmtBnoRoot        = 3
	fmtCntRoot        = 4
	fmtInobtRoot      = 5
	fmtRootInodeBlock = 6  // agRel block of the root inode block
	fmtRootInodeAgRel = 48 // AG-relative inode number = block 6 × 8 + 0
	fmtMetaBlocksAG0  = 7  // first free AG-relative block in AG 0
	fmtMetaBlocksAGN  = 6  // first free AG-relative block in AG 1+

	fmtMinSize = int64(fmtAgBlocks) * fmtBlockSize // 4 MiB minimum
)

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
// sizeBytes must be a multiple of 4096 and at least 4 MiB.
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
		blockSize: fmtBlockSize,
		agBlocks:  fmtAgBlocks,
		agCount:   agCount,
		rootIno:   fmtRootInodeAgRel, // ag=0, agRel=48 → absolute ino = 48
		inodeSize: fmtInodeSize,
		inopBlock: fmtInopBlock,
		inopBLog:  fmtInopBLog,
		agBlkLog:  fmtAgBlkLog,
		dirBlkLog: 0,
		hasCRC:    true,
		hasFType:  true,
	}

	// Write per-AG structures.
	for ag := uint32(0); ag < agCount; ag++ {
		if err := fmtWriteAG(f, sb, ag, uuid); err != nil {
			f.Close()
			return nil, fmt.Errorf("xfs: format AG %d: %w", ag, err)
		}
	}

	// Write root directory inode (AG 0, inode 48).
	if err := fmtWriteRootInode(f, sb); err != nil {
		f.Close()
		return nil, fmt.Errorf("xfs: format root inode: %w", err)
	}

	// Write the primary superblock at byte 0.
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
	agBase := int64(ag) * int64(sb.agBlocks) * int64(sb.blockSize)
	be := binary.BigEndian

	// ── AGF (block 1 of AG) ────────────────────────────────────────────────
	firstFree := uint32(fmtMetaBlocksAGN)
	if ag == 0 {
		// AG 0 also reserves the internal log immediately after the metadata,
		// so its free space starts past the log (keeps a single free extent).
		firstFree = uint32(fmtMetaBlocksAG0) + fmtLogBlocks
	}
	freeBlks := sb.agBlocks - firstFree
	agfBuf := make([]byte, fmtBlockSize)
	be.PutUint32(agfBuf[agfOffMagic:], magicAGF)
	be.PutUint32(agfBuf[agfOffSeqNo:], ag)
	be.PutUint32(agfBuf[agfOffLength:], sb.agBlocks)
	be.PutUint32(agfBuf[agfOffBnoRoot:], fmtBnoRoot)
	be.PutUint32(agfBuf[agfOffCntRoot:], fmtCntRoot)
	be.PutUint32(agfBuf[agfOffBnoLevel:], 1)
	be.PutUint32(agfBuf[agfOffCntLevel:], 1)
	be.PutUint32(agfBuf[agfOffFreeBlks:], freeBlks)
	be.PutUint32(agfBuf[agfOffLongest:], freeBlks)
	updateCRC(agfBuf, agfOffCRC, agfStructSize)
	if _, err := f.WriteAt(agfBuf, agBase+int64(sb.blockSize)); err != nil {
		return fmt.Errorf("write AGF: %w", err)
	}

	// ── AGI (block 2 of AG) ────────────────────────────────────────────────
	agiBuf := make([]byte, fmtBlockSize)
	be.PutUint32(agiBuf[agiOffMagic:], magicAGI)
	be.PutUint32(agiBuf[agiOffSeqNo:], ag)
	be.PutUint32(agiBuf[agiOffLength:], sb.agBlocks)
	be.PutUint32(agiBuf[agiOffRoot:], fmtInobtRoot)
	be.PutUint32(agiBuf[agiOffLevel:], 1)
	if ag == 0 {
		// One 8-inode chunk pre-allocated; inode 48 (rootIno) in use.
		be.PutUint32(agiBuf[agiOffCount:], 8)
		be.PutUint32(agiBuf[agiOffFreeCount:], 7)
	}
	// else: count=0, freeCount=0 (zero-value already)
	updateCRC(agiBuf, agiOffCRC, agiStructSize)
	if _, err := f.WriteAt(agiBuf, agBase+2*int64(sb.blockSize)); err != nil {
		return fmt.Errorf("write AGI: %w", err)
	}

	hdr := btreeHdrSizeV5
	absBase := uint64(ag) * uint64(sb.agBlocks) // absolute first block of this AG

	// ── bno B-tree leaf (block 3 of AG) ───────────────────────────────────
	bnoLeaf := make([]byte, fmtBlockSize)
	be.PutUint32(bnoLeaf[0:], magicABTB)
	be.PutUint16(bnoLeaf[4:], 0)           // level = 0 (leaf)
	be.PutUint16(bnoLeaf[6:], 1)           // numrecs
	be.PutUint32(bnoLeaf[8:], 0xFFFFFFFF)  // left sibling: none
	be.PutUint32(bnoLeaf[12:], 0xFFFFFFFF) // right sibling: none
	be.PutUint64(bnoLeaf[16:], absBase+fmtBnoRoot)
	copy(bnoLeaf[32:], uuid[:])
	be.PutUint32(bnoLeaf[48:], ag) // owner = AG number
	// Record: (start, count)
	be.PutUint32(bnoLeaf[hdr:], firstFree)
	be.PutUint32(bnoLeaf[hdr+4:], freeBlks)
	updateCRC(bnoLeaf, btreeCRCOff, fmtBlockSize)
	if _, err := f.WriteAt(bnoLeaf, agBase+int64(fmtBnoRoot)*int64(sb.blockSize)); err != nil {
		return fmt.Errorf("write bno leaf: %w", err)
	}

	// ── cnt B-tree leaf (block 4 of AG) ───────────────────────────────────
	cntLeaf := make([]byte, fmtBlockSize)
	be.PutUint32(cntLeaf[0:], magicABTC)
	be.PutUint16(cntLeaf[4:], 0)
	be.PutUint16(cntLeaf[6:], 1)
	be.PutUint32(cntLeaf[8:], 0xFFFFFFFF)
	be.PutUint32(cntLeaf[12:], 0xFFFFFFFF)
	be.PutUint64(cntLeaf[16:], absBase+fmtCntRoot)
	copy(cntLeaf[32:], uuid[:])
	be.PutUint32(cntLeaf[48:], ag)
	be.PutUint32(cntLeaf[hdr:], firstFree)
	be.PutUint32(cntLeaf[hdr+4:], freeBlks)
	updateCRC(cntLeaf, btreeCRCOff, fmtBlockSize)
	if _, err := f.WriteAt(cntLeaf, agBase+int64(fmtCntRoot)*int64(sb.blockSize)); err != nil {
		return fmt.Errorf("write cnt leaf: %w", err)
	}

	// ── inobt leaf (block 5 of AG) ─────────────────────────────────────────
	inoLeaf := make([]byte, fmtBlockSize)
	be.PutUint32(inoLeaf[0:], magicIAB3)
	be.PutUint32(inoLeaf[8:], 0xFFFFFFFF)
	be.PutUint32(inoLeaf[12:], 0xFFFFFFFF)
	be.PutUint64(inoLeaf[16:], absBase+fmtInobtRoot)
	copy(inoLeaf[32:], uuid[:])
	be.PutUint32(inoLeaf[48:], ag)
	if ag == 0 {
		be.PutUint16(inoLeaf[6:], 1) // numrecs = 1
		// Record: startIno=48, freeCount=7, irFree=0xFE (bit0=used=root, bits1-7=free)
		be.PutUint32(inoLeaf[hdr:], fmtRootInodeAgRel)    // startIno (AG-relative)
		be.PutUint32(inoLeaf[hdr+4:], 7)                  // freeCount
		binary.BigEndian.PutUint64(inoLeaf[hdr+8:], 0xFE) // irFree (bit 0 cleared = in use)
	}
	// else: numrecs=0, no records
	updateCRC(inoLeaf, btreeCRCOff, fmtBlockSize)
	if _, err := f.WriteAt(inoLeaf, agBase+int64(fmtInobtRoot)*int64(sb.blockSize)); err != nil {
		return fmt.Errorf("write inobt leaf: %w", err)
	}

	return nil
}

// ──────────────────── Root inode writer ────────────────────────────────────

func fmtWriteRootInode(f fmtFile, sb *superblock) error {
	buf := make([]byte, fmtInodeSize)
	rootIno := uint64(fmtRootInodeAgRel) // = 48 (absolute, in AG 0)
	mode := uint16(0o40755)              // directory, rwxr-xr-x
	// Root dir starts with nlink=2 ("." inside itself plus the implicit ".."
	// — there is no parent dir entry for the root inode).
	initInodeV3(buf, rootIno, mode, uint16(fmtInodeSize), 2)

	in := &inode{
		num:    rootIno,
		mode:   mode,
		raw:    buf,
		format: inodeFmtLocal,
	}

	// Minimal short-form directory: count=0, i8count=0, parent=self (4 bytes).
	fork := in.dataFork()
	fork[0] = 0                                           // count
	fork[1] = 0                                           // i8count
	binary.BigEndian.PutUint32(fork[2:], uint32(rootIno)) // parent = self
	setInodeSize(in, 6)                                   // 2 + 4
	setInodeFormat(in, inodeFmtLocal)

	// Update CRC and write inode.
	if sb.hasCRC {
		updateCRC(in.raw, inoOffCRC, fmtInodeSize)
	}
	off := inodeByteOff(sb, 0, rootIno)
	if _, err := f.WriteAt(in.raw, off); err != nil {
		return fmt.Errorf("write root inode: %w", err)
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

	be.PutUint32(buf[sbOffMagic:], magicSB)
	be.PutUint32(buf[sbOffBlockSize:], sb.blockSize)
	be.PutUint64(buf[sbOffRootIno:], sb.rootIno)
	be.PutUint32(buf[sbOffAgBlocks:], sb.agBlocks)
	be.PutUint32(buf[sbOffAgCount:], agCount)
	be.PutUint16(buf[sbOffVersionNum:], uint16(xfsSBVersionV5)) // v5 + feature bits (low nibble 5 → hasCRC)
	be.PutUint16(buf[102:], 512) // sb_sectsize
	be.PutUint16(buf[sbOffInodeSize:], sb.inodeSize)
	be.PutUint16(buf[sbOffInopBlock:], sb.inopBlock)
	buf[121] = 9 // sb_sectlog (log2 512)
	buf[sbOffBlockLog] = fmtBlockLog
	buf[sbOffInodeLog] = fmtInodeLog
	buf[sbOffInopBLog] = sb.inopBLog
	buf[sbOffAgBlkLog] = sb.agBlkLog
	buf[sbOffDirBlkLog] = 0
	be.PutUint32(buf[sbOffFeatIncompat:], xfsSBFeatFType)
	// v5 feature words + alignment/geometry fields that xfs_repair cross-checks
	// against the version bits set above.
	be.PutUint32(buf[sbOffFeatures2:], xfsSBFeatures2)
	be.PutUint32(buf[sbOffBadFeatures2:], xfsSBFeatures2) // historical duplicate
	be.PutUint32(buf[sbOffInoAlignmt:], 2)                // ALIGNBIT in versionnum requires this
	buf[sbOffImaxPct] = 25

	// Filesystem-wide totals that xfs_repair cross-checks against AG metadata.
	// sb_dblocks = total data blocks; fdblocks = sum of per-AG AGF free counts
	// (AG0 reserves fmtMetaBlocksAG0 blocks, AG1+ reserve fmtMetaBlocksAGN);
	// icount/ifree match the single 8-inode chunk pre-allocated in AG0.
	dblocks := uint64(agCount) * uint64(sb.agBlocks)
	// AG 0's free space excludes both its metadata and the internal log.
	fdblocks := uint64(sb.agBlocks-fmtMetaBlocksAG0-fmtLogBlocks) +
		uint64(agCount-1)*uint64(sb.agBlocks-fmtMetaBlocksAGN)
	be.PutUint64(buf[sbOffDBlocks:], dblocks)
	be.PutUint64(buf[sbOffIcount:], 8)
	be.PutUint64(buf[sbOffIfree:], 7)
	be.PutUint64(buf[sbOffFdblocks:], fdblocks)

	// Internal log: contiguous run in AG 0 after the metadata (zeroed region;
	// the kernel formats it on first mount).
	be.PutUint64(buf[sbOffLogStart:], fmtLogStart)
	be.PutUint32(buf[sbOffLogBlocks:], fmtLogBlocks)

	// No realtime device: rblocks/rextents stay 0, but rextsize must be a
	// valid non-zero extent size (1 block) or xfs_repair flags "inconsistent
	// realtime geometry".
	be.PutUint32(buf[sbOffRExtSize:], 1)

	// UUID at offset 32 (sb_uuid, 16 bytes).
	copy(buf[32:], uuid[:])

	// Label at offset 108 (sb_fname, 12 bytes).
	copy(buf[108:], label)

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
