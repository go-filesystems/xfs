package filesystem_xfs

// Reflink / shared-extent (copy-on-write) support.
//
// A reflink share lets two inodes reference the same physical filesystem
// blocks. XFS records how many inodes reference each shared block range in a
// per-AG reference-count B-tree (the refcountbt, magic "R3FC"); a block that is
// referenced by a single inode is *not* in the tree (its implicit refcount is
// 1), a free block has refcount 0, and only blocks shared by two or more inodes
// carry an explicit record with refcount >= 2. Deleting or overwriting a shared
// block decrements its refcount instead of freeing it; the block is returned to
// the free-space B-trees only when it is no longer shared.
//
// This implementation maintains a single-level refcountbt (one root block per
// AG). That holds up to refcMaxRecs records — ample for the images this package
// targets. A share that would overflow a single block returns errRefcountFull
// rather than silently corrupting the tree; growing the refcountbt to multiple
// levels is the one residual piece (see README).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
)

const (
	// refcRecSize is sizeof(struct xfs_refcount_rec): rc_startblock(be32) +
	// rc_blockcount(be32) + rc_refcount(be32).
	refcRecSize = 12
	// refcCOWFlag (XFS_REFC_COWFLAG) is the high bit of rc_startblock marking a
	// copy-on-write staging extent. A cleanly unmounted filesystem carries none;
	// we never write them but mask the bit off on read for robustness.
	refcCOWFlag = uint32(1) << 31
	// refcStartMask masks the COW flag off a stored rc_startblock.
	refcStartMask = refcCOWFlag - 1
)

// errRefcountFull is returned when a refcount operation would need more records
// than fit in the single-level refcountbt root block.
var errRefcountFull = errors.New("xfs: refcount B-tree root full (multi-level refcountbt unsupported)")

// refcMaxRecs is the number of refcount records that fit in one root block.
func refcMaxRecs(sb *superblock) int {
	return (int(sb.blockSize) - btreeHdrSize(sb.hasCRC)) / refcRecSize
}

// refcRec is a decoded reference-count record. start is AG-relative.
type refcRec struct {
	start    uint32
	count    uint32
	refcount uint32
}

// injectable seams for tests.
var (
	reflinkPathLookup   = pathLookup
	reflinkLookupInDir  = lookupInDir
	reflinkReadInode    = readInode
	reflinkWriteInode   = writeInode
	reflinkInlineExts   = inlineExtents
	reflinkAllocInode   = allocInode
	reflinkAddDirEntry  = addDirEntry
	reflinkWriteExtList = writeExtentList
	reflinkAGFBlock     = agfBlock
	reflinkReadAGBlock  = readAGBlock
	reflinkWriteAGBTree = writeAGBTree
	reflinkFreeBlocks   = freeBlocks
)

// refcountReadRecs reads the single-level refcount B-tree for AG ag and returns
// its records (AG-relative). Returns nil when the filesystem has no refcountbt.
func refcountReadRecs(rw readerWriterAt, partOff int64, sb *superblock, ag uint32) ([]refcRec, error) {
	agf, err := reflinkAGFBlock(rw, partOff, sb, ag)
	if err != nil {
		return nil, err
	}
	be := binary.BigEndian
	root := be.Uint32(agf[agfOffRefcntRoot:])
	level := be.Uint32(agf[agfOffRefcntLevel:])
	if root == 0 || level == 0 {
		return nil, nil
	}
	if level != 1 {
		return nil, errRefcountFull
	}
	blk, err := reflinkReadAGBlock(rw, partOff, sb, ag, root)
	if err != nil {
		return nil, err
	}
	if be.Uint32(blk[0:]) != magicRefcnt {
		return nil, fmt.Errorf("xfs: AG %d bad refcountbt magic 0x%08X", ag, be.Uint32(blk[0:]))
	}
	hdr := btreeHdrSize(sb.hasCRC)
	numrecs := int(be.Uint16(blk[6:]))
	if hdr+numrecs*refcRecSize > len(blk) {
		return nil, fmt.Errorf("xfs: AG %d refcountbt numrecs %d overflows block", ag, numrecs)
	}
	recs := make([]refcRec, numrecs)
	for i := 0; i < numrecs; i++ {
		off := hdr + i*refcRecSize
		recs[i] = refcRec{
			start:    be.Uint32(blk[off:]) & refcStartMask,
			count:    be.Uint32(blk[off+4:]),
			refcount: be.Uint32(blk[off+8:]),
		}
	}
	return recs, nil
}

// refcountWriteRecs rewrites AG ag's refcount B-tree root with recs (which must
// be sorted by start and fit one block). The AGF root/level/blocks fields are
// unchanged (still a single-block, single-level tree).
func refcountWriteRecs(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, recs []refcRec) error {
	if len(recs) > refcMaxRecs(sb) {
		return errRefcountFull
	}
	agf, err := reflinkAGFBlock(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	be := binary.BigEndian
	root := be.Uint32(agf[agfOffRefcntRoot:])
	blk, err := reflinkReadAGBlock(rw, partOff, sb, ag, root)
	if err != nil {
		return err
	}
	hdr := btreeHdrSize(sb.hasCRC)
	be.PutUint16(blk[4:], 0) // level 0 (leaf)
	be.PutUint16(blk[6:], uint16(len(recs)))
	// Clear the record region, then lay records down in order.
	clear(blk[hdr:])
	for i, r := range recs {
		off := hdr + i*refcRecSize
		be.PutUint32(blk[off:], r.start&refcStartMask)
		be.PutUint32(blk[off+4:], r.count)
		be.PutUint32(blk[off+8:], r.refcount)
	}
	return reflinkWriteAGBTree(rw, partOff, sb, ag, root, blk)
}

// refcMerge sorts recs by start and coalesces adjacent records that are
// contiguous and carry the same refcount, dropping any record with refcount < 2
// (such blocks are singly owned and belong out of the tree).
func refcMerge(recs []refcRec) []refcRec {
	kept := recs[:0:0]
	for _, r := range recs {
		if r.count == 0 || r.refcount < 2 {
			continue
		}
		kept = append(kept, r)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].start < kept[j].start })
	out := kept[:0]
	for _, r := range kept {
		if n := len(out); n > 0 && out[n-1].start+out[n-1].count == r.start && out[n-1].refcount == r.refcount {
			out[n-1].count += r.count
			continue
		}
		out = append(out, r)
	}
	return out
}

// refcClipInside splits recs into the parts outside [s,e) (kept verbatim) and a
// gap-filled, sorted list of segments covering exactly [s,e) annotated with the
// current refcount (0 where no record covers the block — i.e. an implicit
// singly-owned block).
func refcClipInside(recs []refcRec, s, e uint32) (outside []refcRec, inside []refcRec) {
	for _, r := range recs {
		rEnd := r.start + r.count
		if rEnd <= s || r.start >= e {
			outside = append(outside, r)
			continue
		}
		if r.start < s {
			outside = append(outside, refcRec{r.start, s - r.start, r.refcount})
		}
		if rEnd > e {
			outside = append(outside, refcRec{e, rEnd - e, r.refcount})
		}
		cs, ce := max32(r.start, s), min32(rEnd, e)
		inside = append(inside, refcRec{cs, ce - cs, r.refcount})
	}
	sort.Slice(inside, func(i, j int) bool { return inside[i].start < inside[j].start })
	// Fill gaps between covered segments with implicit-refcount-0 records so the
	// caller sees a complete tiling of [s,e).
	var filled []refcRec
	pos := s
	for _, seg := range inside {
		if seg.start > pos {
			filled = append(filled, refcRec{pos, seg.start - pos, 0})
		}
		filled = append(filled, seg)
		pos = seg.start + seg.count
	}
	if pos < e {
		filled = append(filled, refcRec{pos, e - pos, 0})
	}
	return outside, filled
}

// refcIncrRange increments the reference count of [s, s+count) by one, treating
// uncovered (implicit refcount-1) blocks as becoming refcount 2. It returns the
// merged record set.
func refcIncrRange(recs []refcRec, s, count uint32) []refcRec {
	e := s + count
	outside, inside := refcClipInside(recs, s, e)
	for i := range inside {
		base := inside[i].refcount
		if base == 0 {
			base = 1 // implicit singly-owned block
		}
		inside[i].refcount = base + 1
	}
	return refcMerge(append(outside, inside...))
}

// refcDecrRange decrements the reference count of [s, s+count) by one. Blocks
// that were covered by a record drop toward refcount 1 (removed from the tree
// when they reach it); blocks that were *not* in the tree were singly owned by
// the caller's inode and are returned as ranges to free. It returns the merged
// record set plus the AG-relative ranges the caller must release to the
// free-space B-trees.
func refcDecrRange(recs []refcRec, s, count uint32) (out []refcRec, free []refcRec) {
	e := s + count
	outside, inside := refcClipInside(recs, s, e)
	for _, seg := range inside {
		if seg.refcount == 0 {
			// Singly owned by this inode: free it.
			free = append(free, refcRec{seg.start, seg.count, 0})
			continue
		}
		outside = append(outside, refcRec{seg.start, seg.count, seg.refcount - 1})
	}
	return refcMerge(outside), free
}

// max32/min32 are tiny helpers (Go's builtin min/max exist but keeping explicit
// uint32 helpers documents intent at the call sites).
func max32(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}

func min32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

// refcountShareExtent bumps the reference count of the fsbno extent [absBlock,
// absBlock+count) by one. The extent must lie within a single AG.
func refcountShareExtent(rw readerWriterAt, partOff int64, sb *superblock, absBlock uint64, count uint32) error {
	ag, agbno := sb.fsbToAgAgbno(absBlock)
	if uint64(agbno)+uint64(count) > uint64(sb.agBlocks) {
		return fmt.Errorf("xfs: reflink extent [%d,+%d) crosses AG %d boundary", agbno, count, ag)
	}
	recs, err := refcountReadRecs(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	return refcountWriteRecs(rw, partOff, sb, ag, refcIncrRange(recs, agbno, count))
}

// refcountFreeExtent releases the fsbno extent [absBlock, absBlock+count) with
// copy-on-write semantics: shared blocks are decremented in the refcountbt and
// only the no-longer-referenced (previously singly-owned) sub-ranges are freed
// to the free-space B-trees. Used when deleting or overwriting a reflinked
// inode. The extent must lie within a single AG.
func refcountFreeExtent(rw readerWriterAt, partOff int64, sb *superblock, absBlock uint64, count uint32) error {
	ag, agbno := sb.fsbToAgAgbno(absBlock)
	if uint64(agbno)+uint64(count) > uint64(sb.agBlocks) {
		return fmt.Errorf("xfs: reflink free extent [%d,+%d) crosses AG %d boundary", agbno, count, ag)
	}
	recs, err := refcountReadRecs(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	out, free := refcDecrRange(recs, agbno, count)
	if err := refcountWriteRecs(rw, partOff, sb, ag, out); err != nil {
		return err
	}
	for _, fr := range free {
		if err := reflinkFreeBlocks(rw, partOff, sb, sb.agAbsBlock(ag, fr.start), fr.count); err != nil {
			return err
		}
	}
	return nil
}

// inodeIsReflinked reports whether in carries the XFS_DIFLAG2_REFLINK flag.
func inodeIsReflinked(in *inode) bool {
	return in.raw[inoOffVersion] >= 3 &&
		binary.BigEndian.Uint64(in.raw[inoOffFlags2:])&xfsDiflag2Reflink != 0
}

// resetFreedInodeFork scrubs a to-be-freed reflinked inode back to an empty
// extents inode: it clears the data fork, zeroes di_nextents/di_nblocks/di_size
// and drops the reflink flag so the freed inode carries no dangling references
// to blocks that may still be shared with other inodes.
func resetFreedInodeFork(in *inode) {
	setInodeNExtents(in, 0)
	setInodeNBlocks(in, 0)
	setInodeSize(in, 0)
	setInodeFormat(in, inodeFmtExtents)
	// di_flags2: clear the reflink bit (leave any other flags intact).
	f := binary.BigEndian.Uint64(in.raw[inoOffFlags2:])
	binary.BigEndian.PutUint64(in.raw[inoOffFlags2:], f&^uint64(xfsDiflag2Reflink))
	// Zero the NREXT64 big-extent counter too, in case it was set.
	if binary.BigEndian.Uint64(in.raw[inoOffFlags2:])&xfsDiflag2Nrext64 != 0 {
		binary.BigEndian.PutUint64(in.raw[inoOffBigNExtents:], 0)
	}
	clear(in.dataFork())
}

// setInodeReflinkFlag sets XFS_DIFLAG2_REFLINK in the raw inode buffer.
func setInodeReflinkFlag(in *inode) {
	f := binary.BigEndian.Uint64(in.raw[inoOffFlags2:])
	binary.BigEndian.PutUint64(in.raw[inoOffFlags2:], f|xfsDiflag2Reflink)
}

// reflinkFile clones srcPath into a new file dstPath that shares srcPath's data
// extents copy-on-write. Both inodes are marked reflinked and the shared
// extents' reference counts are raised. srcPath must be a regular file and
// dstPath must not already exist.
func reflinkFile(rw readerWriterAt, partOff int64, sb *superblock, srcPath, dstPath string) error {
	if !sb.hasReflink {
		return fmt.Errorf("xfs: reflink: filesystem was not formatted with reflink support")
	}
	srcPath = path.Clean(srcPath)
	dstPath = path.Clean(dstPath)

	srcIn, err := reflinkPathLookup(rw, partOff, sb, srcPath)
	if err != nil {
		return fmt.Errorf("xfs: reflink source %q: %w", srcPath, err)
	}
	if !srcIn.isRegular() {
		return fmt.Errorf("xfs: reflink source %q is not a regular file", srcPath)
	}

	parentPath, name := path.Split(dstPath)
	parentPath = strings.TrimSuffix(parentPath, "/")
	if parentPath == "" {
		parentPath = "/"
	}
	if name == "" {
		return fmt.Errorf("xfs: reflink: invalid destination %q", dstPath)
	}
	dirIn, err := reflinkPathLookup(rw, partOff, sb, parentPath)
	if err != nil {
		return fmt.Errorf("xfs: reflink destination parent %q: %w", parentPath, err)
	}
	if !dirIn.isDir() {
		return fmt.Errorf("xfs: reflink destination parent %q is not a directory", parentPath)
	}
	if _, err := reflinkLookupInDir(rw, partOff, sb, dirIn, name); err == nil {
		return fmt.Errorf("xfs: reflink destination %q already exists", dstPath)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	// Gather the source's data extents. btree-fork sources are the residual
	// case; extents and (inline) local forks are handled.
	var exts []extent
	switch srcIn.format {
	case inodeFmtExtents:
		exts, err = reflinkInlineExts(srcIn)
		if err != nil {
			return err
		}
	case inodeFmtLocal:
		// No shared blocks: fall back to a plain copy of the inline bytes.
		return reflinkCopyLocal(rw, partOff, sb, srcIn, dirIn, name)
	default:
		return fmt.Errorf("xfs: reflink source %q uses unsupported fork format %d (btree-fork reflink is not implemented)", srcPath, srcIn.format)
	}

	// Allocate the destination inode in the parent's AG.
	dstAG := inoAG(sb, dirIn.num)
	dstIno, err := reflinkAllocInode(rw, partOff, sb, dstAG)
	if err != nil {
		for a := uint32(0); a < sb.agCount && err != nil; a++ {
			if a == dstAG {
				continue
			}
			dstIno, err = reflinkAllocInode(rw, partOff, sb, a)
		}
		if err != nil {
			return fmt.Errorf("xfs: reflink: no free inode: %w", err)
		}
	}

	dstBuf := make([]byte, sb.inodeSize)
	initInodeV3(dstBuf, dstIno, srcIn.mode, sb.inodeSize, 1, sb.uuid)
	dstIn := &inode{num: dstIno, mode: srcIn.mode, raw: dstBuf}
	setInodeFormat(dstIn, inodeFmtExtents)
	setInodeSize(dstIn, srcIn.size)
	setInodeNBlocks(dstIn, srcIn.nBlocks)
	setInodeNExtents(dstIn, uint32(len(exts)))
	if err := reflinkWriteExtList(dstIn, exts); err != nil {
		return err
	}
	setInodeReflinkFlag(dstIn)
	if err := reflinkWriteInode(rw, partOff, sb, dstIn); err != nil {
		return err
	}

	// Mark the source reflinked too and bump the shared extents' refcounts.
	setInodeReflinkFlag(srcIn)
	if err := reflinkWriteInode(rw, partOff, sb, srcIn); err != nil {
		return err
	}
	for _, e := range exts {
		if err := refcountShareExtent(rw, partOff, sb, e.startBlock, e.count); err != nil {
			return err
		}
	}

	return reflinkAddDirEntry(rw, partOff, sb, dirIn, dstIno, name, 1 /* DT_REG */)
}

// reflinkBreakAndWrite replaces the content of a reflinked file with data,
// breaking the share: the old (shared) extents are released copy-on-write
// (decrementing their reference counts, freeing only truly-private blocks),
// fresh private blocks are allocated for the new content, and the reflink flag
// is cleared. An empty payload leaves an empty extents-format file.
func reflinkBreakAndWrite(rw readerWriterAt, partOff int64, sb *superblock, in *inode, data []byte) error {
	// Release the current extents copy-on-write.
	var oldExts []extent
	var err error
	switch in.format {
	case inodeFmtExtents:
		oldExts, err = reflinkInlineExts(in)
	case inodeFmtBtree:
		oldExts, err = btreeExtents(rw, partOff, sb, in)
	}
	if err != nil {
		return err
	}
	for _, e := range oldExts {
		if err := refcountFreeExtent(rw, partOff, sb, e.startBlock, e.count); err != nil {
			return err
		}
	}

	// Drop the reflink flag and reset the fork before laying down new content.
	f := binary.BigEndian.Uint64(in.raw[inoOffFlags2:])
	binary.BigEndian.PutUint64(in.raw[inoOffFlags2:], f&^uint64(xfsDiflag2Reflink))
	clear(in.dataFork())
	setInodeFormat(in, inodeFmtExtents)

	if len(data) == 0 {
		setInodeSize(in, 0)
		setInodeNBlocks(in, 0)
		setInodeNExtents(in, 0)
		return reflinkWriteInode(rw, partOff, sb, in)
	}

	blockSize := uint64(sb.blockSize)
	nBlocks := uint32((uint64(len(data)) + blockSize - 1) / blockSize)
	ag := inoAG(sb, in.num)
	absBlock, err := allocBlocks(rw, partOff, sb, ag, nBlocks)
	if err != nil {
		for a := uint32(0); a < sb.agCount && err != nil; a++ {
			if a == ag {
				continue
			}
			absBlock, err = allocBlocks(rw, partOff, sb, a, nBlocks)
		}
		if err != nil {
			return fmt.Errorf("xfs: reflink break: no space for %d blocks: %w", nBlocks, err)
		}
	}
	if err := reflinkWriteExtList(in, []extent{{startOff: 0, startBlock: absBlock, count: nBlocks}}); err != nil {
		return err
	}
	setInodeSize(in, uint64(len(data)))
	setInodeNBlocks(in, uint64(nBlocks))
	setInodeNExtents(in, 1)
	if err := reflinkWriteInode(rw, partOff, sb, in); err != nil {
		return err
	}
	return writeBlocksData(rw, partOff, sb, absBlock, nBlocks, data)
}

// reflinkCopyLocal handles the degenerate reflink of an inline (local-format)
// source: there are no shared blocks, so the destination is an ordinary copy of
// the inline bytes.
func reflinkCopyLocal(rw readerWriterAt, partOff int64, sb *superblock, srcIn, dirIn *inode, name string) error {
	data := make([]byte, srcIn.size)
	copy(data, srcIn.dataFork()[:srcIn.size])
	return createFile(rw, partOff, sb, dirIn, name, data, os.FileMode(srcIn.mode&0o777))
}
