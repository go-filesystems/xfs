package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
)

// quotacheck — recompute the on-disk dquots from a full inode scan.
//
// XFS charges every inode (and the blocks it maps) to the user, group and
// project identity that owns it, excluding the internal quota inodes
// themselves. xfs_repair independently recomputes these totals and compares
// them to the dquots, so any operation that changes inode ownership or block
// usage must leave the dquots consistent. Rather than track deltas through
// every write path, we recompute the whole set (a "quotacheck") after quota
// setup and after each mutation — correct by construction and cheap for the
// image sizes this package targets.

// inoOffProjidLo / inoOffProjidHi are the split 16-bit project-id halves in
// the v3 inode core (di_projid_lo at 20, di_projid_hi at 24).
const (
	inoOffProjidLo = 20
	inoOffProjidHi = 24
)

// quotaAcct accumulates per-identity usage during a quotacheck.
type quotaAcct struct {
	bcount uint64
	icount uint64
}

var quotaReadInode = readInode

// inobtEnumerate walks the inode B-tree of AG ag, invoking fn for every leaf
// record (its AG-relative start inode and 64-bit free bitmap). It descends to
// the leftmost leaf then follows right-sibling pointers, so it handles
// multi-level inode B-trees as well as the single-root common case.
func inobtEnumerate(rw readerWriterAt, partOff int64, sb *superblock, ag uint32, fn func(startIno uint32, irFree uint64) error) error {
	agi, err := agiBlock(rw, partOff, sb, ag)
	if err != nil {
		return err
	}
	be := binary.BigEndian
	hdrSize := sb.agBTreeHdrSize()
	agRel := be.Uint32(agi[agiOffRoot:])

	// Descend to the leftmost leaf.
	for steps := 0; ; steps++ {
		if steps > allocBTreeMaxSteps {
			return fmt.Errorf("xfs: inobt AG %d descent too deep (corrupt)", ag)
		}
		blk, err := readAGBlock(rw, partOff, sb, ag, agRel)
		if err != nil {
			return err
		}
		if be.Uint16(blk[4:]) == 0 { // level 0 = leaf
			break
		}
		ptrOff := hdrSize + inobtMaxInternal(len(blk), hdrSize)*inobtKeySize
		agRel = be.Uint32(blk[ptrOff:])
	}

	// Walk the leaf chain via the right-sibling pointer (offset 12).
	for steps := 0; agRel != 0xFFFFFFFF; steps++ {
		if steps > allocBTreeMaxSteps {
			return fmt.Errorf("xfs: inobt AG %d leaf chain too long (corrupt)", ag)
		}
		blk, err := readAGBlock(rw, partOff, sb, ag, agRel)
		if err != nil {
			return err
		}
		numrecs := int(be.Uint16(blk[6:]))
		if hdrSize+numrecs*inobtRecSize > len(blk) {
			return fmt.Errorf("xfs: inobt AG %d numrecs %d overflows block", ag, numrecs)
		}
		for i := 0; i < numrecs; i++ {
			off := hdrSize + i*inobtRecSize
			startIno := be.Uint32(blk[off:])
			irFree := be.Uint64(blk[off+8:])
			if err := fn(startIno, irFree); err != nil {
				return err
			}
		}
		agRel = be.Uint32(blk[12:])
	}
	return nil
}

// inodeProjid returns the 32-bit project id from a raw inode buffer.
func inodeProjid(raw []byte) uint32 {
	be := binary.BigEndian
	return uint32(be.Uint16(raw[inoOffProjidHi:]))<<16 | uint32(be.Uint16(raw[inoOffProjidLo:]))
}

// quotaRecompute scans every allocated inode and rewrites the enabled quota
// inodes' dquots with the resulting per-identity block/inode usage. The three
// quota inodes are themselves exempt from accounting (as in the kernel).
func quotaRecompute(rw readerWriterAt, partOff int64, sb *superblock) error {
	if sb.quotaFlags == 0 {
		return nil
	}
	user := map[uint32]*quotaAcct{}
	group := map[uint32]*quotaAcct{}
	proj := map[uint32]*quotaAcct{}
	exempt := map[uint64]bool{sb.uQuotino: true, sb.gQuotino: true, sb.pQuotino: true}

	add := func(m map[uint32]*quotaAcct, id uint32, blocks uint64) {
		a := m[id]
		if a == nil {
			a = &quotaAcct{}
			m[id] = a
		}
		a.icount++
		a.bcount += blocks
	}

	for ag := uint32(0); ag < sb.agCount; ag++ {
		err := inobtEnumerate(rw, partOff, sb, ag, func(startIno uint32, irFree uint64) error {
			for bit := 0; bit < inobtChunkInodes; bit++ {
				if (irFree>>uint(bit))&1 == 1 {
					continue // free slot
				}
				ino := inoFromAGRel(sb, ag, startIno+uint32(bit))
				if exempt[ino] {
					continue
				}
				in, err := quotaReadInode(rw, partOff, sb, ino)
				if err != nil {
					return err
				}
				if in.mode == 0 {
					continue // unlinked/free inode
				}
				add(user, binary.BigEndian.Uint32(in.raw[inoOffUID:]), in.nBlocks)
				add(group, binary.BigEndian.Uint32(in.raw[inoOffGID:]), in.nBlocks)
				add(proj, inodeProjid(in.raw), in.nBlocks)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	if sb.uQuotino != 0 {
		if err := quotaWriteDquots(rw, partOff, sb, sb.uQuotino, dqTypeUser, user); err != nil {
			return err
		}
	}
	if sb.gQuotino != 0 {
		if err := quotaWriteDquots(rw, partOff, sb, sb.gQuotino, dqTypeGroup, group); err != nil {
			return err
		}
	}
	if sb.pQuotino != 0 {
		if err := quotaWriteDquots(rw, partOff, sb, sb.pQuotino, dqTypeProj, proj); err != nil {
			return err
		}
	}
	return nil
}

// errQuotaIDRange is returned when a quota id is larger than the single
// pre-allocated dquot block can address (multi-block quota files unsupported).
var errQuotaIDRange = fmt.Errorf("xfs: quota id exceeds single dquot block (multi-block quota files unsupported)")

// quotaWriteDquots rewrites the (single-block) quota inode qino's dquot cluster
// with the accumulated usage. Ids beyond the block's capacity are rejected.
func quotaWriteDquots(rw readerWriterAt, partOff int64, sb *superblock, qino uint64, dqType uint8, acct map[uint32]*quotaAcct) error {
	perBlock := dquotsPerBlock(sb)
	for id := range acct {
		if int(id) >= perBlock {
			return errQuotaIDRange
		}
	}
	in, err := quotaReadInode(rw, partOff, sb, qino)
	if err != nil {
		return err
	}
	exts, err := inlineExtents(in)
	if err != nil {
		return err
	}
	if len(exts) == 0 {
		return fmt.Errorf("xfs: quota inode %d has no data block", qino)
	}
	// Rebuild the whole cluster with valid dquots, then charge accumulated usage.
	block := make([]byte, sb.blockSize)
	for i := 0; i < perBlock; i++ {
		bcount, icount := uint64(0), uint64(0)
		if a := acct[uint32(i)]; a != nil {
			bcount, icount = a.bcount, a.icount
		}
		buildDquot(block[i*dqBlkSize:], sb, dqType, uint32(i), bcount, icount)
	}
	return writeBlocksData(rw, partOff, sb, exts[0].startBlock, 1, block)
}
