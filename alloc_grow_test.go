package filesystem_xfs

// alloc_grow_test.go — regression tests for growInobt + the allocInode
// growth path. These tests exercise the writer end-to-end via Format() so
// they catch overlapping bugs in inobt-record placement, AGI accounting,
// inode-slot initialisation, and the existing free/lookup paths.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"
)

// TestAllocInode_GrowsInobt creates 100 files in a single directory,
// reads each one back, deletes them, and asserts that:
//  1. every create succeeds (the inobt must grow past Format's seed chunk);
//  2. ListDir returns all 100 entries between create and delete;
//  3. after deleting every file, the AGI free-inode count returns to the
//     value it had immediately after the inobt had grown (no leaked
//     allocations).
func TestAllocInode_GrowsInobt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grow.img")
	// 2 AGs → 8 MiB. One AG would also work but we keep an extra so the
	// allocator has room for the new chunks' backing blocks plus the per-
	// file data blocks.
	fs, err := Format(path, 2*xfsTestSize, FormatConfig{Label: "growtest"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	const N = 100
	for i := 0; i < N; i++ {
		p := fmt.Sprintf("/grow-%03d.txt", i)
		body := fmt.Appendf(nil, "payload-%d", i)
		if err := fs.WriteFile(p, body, 0o644); err != nil {
			t.Fatalf("WriteFile %s (i=%d): %v", p, i, err)
		}
	}

	// Sanity: list the directory and ensure we see all entries.
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir /: %v", err)
	}
	if len(entries) != N {
		t.Fatalf("ListDir /: got %d entries, want %d", len(entries), N)
	}
	// Spot-check a few reads.
	for _, i := range []int{0, 1, 17, 42, N - 1} {
		p := fmt.Sprintf("/grow-%03d.txt", i)
		got, err := fs.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", p, err)
		}
		want := fmt.Appendf(nil, "payload-%d", i)
		if string(got) != string(want) {
			t.Fatalf("ReadFile %s: got %q, want %q", p, got, want)
		}
	}

	// Snapshot the AGI free counts before delete so we can verify the final
	// state recovers to a clean post-grow baseline.
	xfs := fs.(*xfsFS)
	preDel := snapshotAGIFreeCounts(t, xfs)

	for i := 0; i < N; i++ {
		p := fmt.Sprintf("/grow-%03d.txt", i)
		if err := fs.DeleteFile(p); err != nil {
			t.Fatalf("DeleteFile %s: %v", p, err)
		}
	}

	// After deleting all N files, every AG's free-inode count should be at
	// least preDel + N (the N inodes we just freed). Equality holds when
	// every delete completed without side-effects (no inobt records were
	// removed — freeInode only flips bits — so the freeCount strictly
	// climbs by N).
	postDel := snapshotAGIFreeCounts(t, xfs)
	totalPre := uint64(0)
	totalPost := uint64(0)
	for ag := range preDel {
		totalPre += uint64(preDel[ag])
		totalPost += uint64(postDel[ag])
	}
	if totalPost != totalPre+N {
		t.Fatalf("freeCount delta: got +%d, want +%d (pre=%v post=%v)",
			totalPost-totalPre, N, preDel, postDel)
	}

	// Final pass: list the directory; it should be empty again.
	if entries, err := fs.ListDir("/"); err != nil {
		t.Fatalf("post-delete ListDir: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("post-delete ListDir: %d entries left, want 0", len(entries))
	}
}

// snapshotAGIFreeCounts returns the freeCount field of every AGI block in
// the filesystem.
func snapshotAGIFreeCounts(t *testing.T, fs *xfsFS) []uint32 {
	t.Helper()
	out := make([]uint32, fs.sb.agCount)
	for ag := uint32(0); ag < fs.sb.agCount; ag++ {
		buf, err := agiBlock(fs.f, fs.partOffset, fs.sb, ag)
		if err != nil {
			t.Fatalf("agiBlock ag=%d: %v", ag, err)
		}
		out[ag] = binary.BigEndian.Uint32(buf[agiOffFreeCount:])
	}
	return out
}

// growTestSB returns a superblock close to a real Format'd image
// (blockSize=4096, inodeSize=512). growInobt calls initInodeV3 which
// writes to inode-offset 152 (di_ino), so the smaller 64-byte inodes of
// allocTestSB would panic; this helper sticks to the real geometry.
func growTestSB() *superblock {
	return &superblock{
		blockSize: 4096,
		agBlocks:  1024,
		agCount:   2,
		inodeSize: 512,
		inopBlock: 8,
		inopBLog:  3,
		agBlkLog:  10,
		hasCRC:    true,
		hasFType:  true,
	}
}

// TestGrowInobt_Mocked exercises growInobt's error branches in isolation
// (alloc failure, AGI re-read failure, inobt-leaf full, leaf write
// failure, AGI write failure, multi-level inobt rejection). These are the
// paths that a happy-path Format-driven test cannot reach because Format
// always produces a single-level inobt with abundant free space.
func TestGrowInobt_Mocked(t *testing.T) {
	sb := growTestSB()
	be := binary.BigEndian

	t.Run("alloc blocks fails", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 0, errBoom
		}
		if err := growInobt(newMemRW(0), 0, sb, 0); !errors.Is(err, errBoom) {
			t.Fatalf("growInobt alloc error: got %v, want %v", err, errBoom)
		}
	})

	t.Run("agi re-read fails", func(t *testing.T) {
		restoreAllocHooks(t)
		// allocAllocBlocks succeeds, but the AGI re-read fails.
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			// Return an absolute block within AG 0 (block 7).
			return 7, nil
		}
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
			return nil, errBoom
		}
		// growInobt writes 64 inode slots before re-reading AGI; the memRW
		// is happy to grow on write. We expect the AGI error to surface.
		if err := growInobt(newMemRW(8*int(sb.blockSize)), 0, sb, 0); !errors.Is(err, errBoom) {
			t.Fatalf("growInobt AGI re-read error: got %v, want %v", err, errBoom)
		}
	})

	t.Run("leaf read fails", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 7, nil
		}
		agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
			return agi, nil
		}
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return nil, errBoom
		}
		if err := growInobt(newMemRW(8*int(sb.blockSize)), 0, sb, 0); !errors.Is(err, errBoom) {
			t.Fatalf("growInobt leaf read error: got %v, want %v", err, errBoom)
		}
	})

	t.Run("leaf full triggers split to depth 2", func(t *testing.T) {
		restoreAllocHooks(t)
		hdr := sb.agBTreeHdrSize()
		maxRecs := (int(sb.blockSize) - hdr) / inobtRecSize

		// The chunk alloc returns block 2096 (2096 % 1024 = 48, a multiple of
		// blocksPerChunk=8 → growInobt's aligned fast path, one alloc, no slack)
		// → startIno = 2096*8 = 16768, larger than every existing record so the
		// new record lands at the end. The split alloc then hands out blocks 201
		// (right leaf) and 202 (internal root).
		var allocCalls int
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			allocCalls++
			switch allocCalls {
			case 1:
				return 2096, nil // inode chunk (aligned)
			case 2:
				return 201, nil // right leaf
			default:
				return 202, nil // internal root
			}
		}
		agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		// Build a genuinely full leaf with maxRecs sorted records starting at a
		// high base (so the chunk's smaller startIno lands at the front and
		// never collides with an existing record regardless of AG geometry).
		const recBase = 1 << 20
		leaf := make([]byte, sb.blockSize)
		be.PutUint16(leaf[6:], uint16(maxRecs))
		for i := 0; i < maxRecs; i++ {
			off := hdr + i*inobtRecSize
			be.PutUint32(leaf[off:], uint32(recBase+i*inobtChunkInodes))
			be.PutUint32(leaf[off+4:], 0) // freecount=0 → this chunk is "full"
			be.PutUint64(leaf[off+8:], 0)
		}
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
			return agi, nil
		}
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return leaf, nil
		}
		written := map[uint32][]byte{}
		allocWriteAGBTree = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, agRel uint32, blk []byte) error {
			cp := make([]byte, len(blk))
			copy(cp, blk)
			written[agRel] = cp
			return nil
		}
		var finalAGI []byte
		allocWriteAGI = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, buf []byte) error {
			finalAGI = buf
			return nil
		}
		if err := growInobt(newMemRW(8*int(sb.blockSize)), 0, sb, 0); err != nil {
			t.Fatalf("growInobt split: unexpected error %v", err)
		}

		// AGI must now point at the new internal root (block 202) at depth 2.
		if got := be.Uint32(finalAGI[agiOffRoot:]); got != 202 {
			t.Fatalf("AGI root after split: got %d, want 202", got)
		}
		if got := be.Uint32(finalAGI[agiOffLevel:]); got != 2 {
			t.Fatalf("AGI level after split: got %d, want 2", got)
		}

		// Three blocks were rewritten: left leaf (orig root block 5), right
		// leaf (201), internal root (202).
		root := written[202]
		if root == nil {
			t.Fatalf("internal root block 202 was not written")
		}
		if lvl := be.Uint16(root[4:]); lvl != 1 {
			t.Fatalf("internal root level: got %d, want 1", lvl)
		}
		if nr := be.Uint16(root[6:]); nr != 2 {
			t.Fatalf("internal root numrecs: got %d, want 2", nr)
		}
		maxInternal := (int(sb.blockSize) - hdr) / (inobtKeySize + inobtPtrSize)
		ptrBase := hdr + maxInternal*inobtKeySize
		if p0 := be.Uint32(root[ptrBase:]); p0 != 5 {
			t.Fatalf("internal root ptr[0]: got %d, want 5 (left leaf)", p0)
		}
		if p1 := be.Uint32(root[ptrBase+inobtPtrSize:]); p1 != 201 {
			t.Fatalf("internal root ptr[1]: got %d, want 201 (right leaf)", p1)
		}

		// The total record count must be preserved across the two leaves and
		// the sibling chain must link left→right.
		left := written[5]
		right := written[201]
		total := int(be.Uint16(left[6:])) + int(be.Uint16(right[6:]))
		if total != maxRecs+1 {
			t.Fatalf("split record total: got %d, want %d", total, maxRecs+1)
		}
		if rsib := be.Uint32(left[12:]); rsib != 201 {
			t.Fatalf("left leaf rsib: got %d, want 201", rsib)
		}
		if lsib := be.Uint32(right[8:]); lsib != 5 {
			t.Fatalf("right leaf lsib: got %d, want 5", lsib)
		}
		// Records stay globally sorted: left's last < right's first.
		leftLast := be.Uint32(left[hdr+(int(be.Uint16(left[6:]))-1)*inobtRecSize:])
		rightFirst := be.Uint32(right[hdr:])
		if leftLast >= rightFirst {
			t.Fatalf("split not sorted: left last %d >= right first %d", leftLast, rightFirst)
		}
	})

	t.Run("split allocation and write failures", func(t *testing.T) {
		hdr := sb.agBTreeHdrSize()
		maxRecs := (int(sb.blockSize) - hdr) / inobtRecSize
		// A genuinely full leaf (level 0) with sorted records so the leaf split
		// partitions it. selfRel=5, rightsib=null so no old-right fixup.
		fullLeaf := func() []byte {
			leaf := make([]byte, sb.blockSize)
			be.PutUint16(leaf[4:], 0) // level 0
			be.PutUint16(leaf[6:], uint16(maxRecs))
			be.PutUint32(leaf[8:], 0xFFFFFFFF)
			be.PutUint32(leaf[12:], 0xFFFFFFFF)
			for i := 0; i < maxRecs; i++ {
				be.PutUint32(leaf[hdr+i*inobtRecSize:], uint32(1+i))
			}
			return leaf
		}
		var rec [inobtRecSize]byte
		be.PutUint32(rec[0:], 1<<20) // larger than every existing record

		// inobtSplitLeaf: right-leaf allocation fails.
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 0, errBoom
		}
		if _, err := inobtSplitLeaf(newMemRW(0), 0, sb, 0, 5, fullLeaf(), maxRecs, rec[:]); !errors.Is(err, errBoom) {
			t.Fatalf("leaf-split alloc fail: got %v", err)
		}

		// inobtSplitLeaf: each of the two btree writes (left, right) fails.
		for failOn := 1; failOn <= 2; failOn++ {
			restoreAllocHooks(t)
			allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
				return 200, nil
			}
			var w int
			allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error {
				w++
				if w == failOn {
					return errBoom
				}
				return nil
			}
			if _, err := inobtSplitLeaf(newMemRW(0), 0, sb, 0, 5, fullLeaf(), maxRecs, rec[:]); !errors.Is(err, errBoom) {
				t.Fatalf("leaf-split write fail #%d: got %v", failOn, err)
			}
		}

		// Clean leaf split returns a sorted separator and the right block number.
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 201, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		res, err := inobtSplitLeaf(newMemRW(0), 0, sb, 0, 5, fullLeaf(), maxRecs, rec[:])
		if err != nil || !res.split || res.rightRel != 201 {
			t.Fatalf("clean leaf split: res=%+v err=%v", res, err)
		}

		// inobtInsertChunkRecord: the new-root allocation (after a root leaf
		// split) fails. Mock a full level-0 leaf for any block read.
		restoreAllocHooks(t)
		full := fullLeaf()
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return full, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		var n int
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			n++
			if n == 1 {
				return 201, nil // right leaf for the split
			}
			return 0, errBoom // new root alloc fails
		}
		agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		if err := inobtInsertChunkRecord(newMemRW(0), 0, sb, 0, agi, 5, 1, 1<<20); !errors.Is(err, errBoom) {
			t.Fatalf("new-root alloc fail: got %v", err)
		}

		// Clean root split via inobtInsertChunkRecord: AGI promoted to level 2,
		// root repointed at the freshly-allocated internal node (202).
		restoreAllocHooks(t)
		full = fullLeaf()
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return full, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		var m int
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			m++
			if m == 1 {
				return 201, nil // right leaf
			}
			return 202, nil // new root
		}
		agi = makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		if err := inobtInsertChunkRecord(newMemRW(0), 0, sb, 0, agi, 5, 1, 1<<20); err != nil {
			t.Fatalf("clean root split: %v", err)
		}
		if be.Uint32(agi[agiOffLevel:]) != 2 {
			t.Fatalf("clean root split AGI level = %d, want 2", be.Uint32(agi[agiOffLevel:]))
		}
		if be.Uint32(agi[agiOffRoot:]) != 202 {
			t.Fatalf("clean root split AGI root = %d, want 202", be.Uint32(agi[agiOffRoot:]))
		}
	})

	t.Run("duplicate startIno rejected", func(t *testing.T) {
		restoreAllocHooks(t)
		// Allocator returns block 0 (agAbsBlock(0, 0)), so startIno = 0.
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 0, nil
		}
		agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		hdr := sb.agBTreeHdrSize()
		leaf := make([]byte, sb.blockSize)
		be.PutUint16(leaf[6:], 1)
		// Existing record with startIno = 0 (duplicate of what growInobt is
		// about to insert).
		be.PutUint32(leaf[hdr:], 0)
		be.PutUint32(leaf[hdr+4:], inobtChunkInodes)
		be.PutUint64(leaf[hdr+8:], 0xFFFFFFFFFFFFFFFF)
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
			return agi, nil
		}
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return leaf, nil
		}
		err := growInobt(newMemRW(8*int(sb.blockSize)), 0, sb, 0)
		if err == nil || !contains(err.Error(), "already has a record") {
			t.Fatalf("growInobt duplicate: got %v", err)
		}
	})

	t.Run("leaf write fails", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 7, nil
		}
		agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		hdr := sb.agBTreeHdrSize()
		_ = hdr
		leaf := make([]byte, sb.blockSize)
		be.PutUint16(leaf[6:], 0)
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
			return agi, nil
		}
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return leaf, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error {
			return errBoom
		}
		if err := growInobt(newMemRW(8*int(sb.blockSize)), 0, sb, 0); !errors.Is(err, errBoom) {
			t.Fatalf("growInobt leaf write: got %v, want %v", err, errBoom)
		}
	})

	t.Run("agi write fails", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 7, nil
		}
		agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		leaf := make([]byte, sb.blockSize)
		be.PutUint16(leaf[6:], 0)
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
			return agi, nil
		}
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return leaf, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error {
			return nil
		}
		allocWriteAGI = func(io.WriterAt, int64, *superblock, uint32, []byte) error {
			return errBoom
		}
		if err := growInobt(newMemRW(8*int(sb.blockSize)), 0, sb, 0); !errors.Is(err, errBoom) {
			t.Fatalf("growInobt AGI write: got %v, want %v", err, errBoom)
		}
	})

	t.Run("inode slot write fails", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 7, nil
		}
		rw := newMemRW(8 * int(sb.blockSize))
		rw.writeHook = func(int64, []byte) error { return errBoom }
		if err := growInobt(rw, 0, sb, 0); !errors.Is(err, errBoom) {
			t.Fatalf("growInobt inode-slot write: got %v, want %v", err, errBoom)
		}
	})

	t.Run("misaligned probe takes over-allocate fallback", func(t *testing.T) {
		restoreAllocHooks(t)
		// First alloc (the exact-size probe) returns a misaligned block (3 % 8 !=
		// 0), forcing growInobt to free the probe and over-allocate 2*8-1 blocks,
		// then trim the head/tail slack. Subsequent allocs serve the chunk's
		// aligned run; the inobt insert lands in a single-leaf root with room.
		var calls int
		var freed int
		allocAllocBlocks = func(_ readerWriterAt, _ int64, _ *superblock, _ uint32, n uint32) (uint64, error) {
			calls++
			if calls == 1 {
				return 3, nil // misaligned probe
			}
			return 16, nil // aligned-enough over-alloc run (16 % 8 == 0)
		}
		allocFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error {
			freed++
			return nil
		}
		agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		leaf := make([]byte, sb.blockSize)
		be.PutUint16(leaf[6:], 0) // empty leaf root, has room
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
			return agi, nil
		}
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return leaf, nil
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		allocWriteAGI = func(io.WriterAt, int64, *superblock, uint32, []byte) error { return nil }
		if err := growInobt(newMemRW(8*int(sb.blockSize)), 0, sb, 0); err != nil {
			t.Fatalf("growInobt misaligned-probe fallback: %v", err)
		}
		if freed == 0 {
			t.Fatalf("expected the fallback to free the probe + slack, but no free happened")
		}
	})

	t.Run("misaligned probe free-back fails", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 3, nil // misaligned → fallback frees the probe, which fails
		}
		allocFreeBlocks = func(readerWriterAt, int64, *superblock, uint64, uint32) error { return errBoom }
		if err := growInobt(newMemRW(8*int(sb.blockSize)), 0, sb, 0); !errors.Is(err, errBoom) {
			t.Fatalf("growInobt probe free-back fail: got %v", err)
		}
	})

	t.Run("invalid inopBlock rejected", func(t *testing.T) {
		restoreAllocHooks(t)
		bad := *sb
		bad.inopBlock = 0 // forces blocksPerChunk = 0
		err := growInobt(newMemRW(8*int(sb.blockSize)), 0, &bad, 0)
		if err == nil || !contains(err.Error(), "invalid inopBlock") {
			t.Fatalf("growInobt invalid inopBlock: got %v", err)
		}
	})

	t.Run("sorted insert preserves order", func(t *testing.T) {
		restoreAllocHooks(t)
		// allocAllocBlocks returns block 16 → startIno = 16*8 = 128.
		// Existing records: [56, 256]. The new record (128) must land in
		// the middle.
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 16, nil
		}
		agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		hdr := sb.agBTreeHdrSize()
		leaf := make([]byte, sb.blockSize)
		be.PutUint16(leaf[6:], 2)
		// Record 0: startIno=56.
		be.PutUint32(leaf[hdr:], 56)
		be.PutUint32(leaf[hdr+4:], 8)
		be.PutUint64(leaf[hdr+8:], 0xFE)
		// Record 1: startIno=256.
		be.PutUint32(leaf[hdr+inobtRecSize:], 256)
		be.PutUint32(leaf[hdr+inobtRecSize+4:], inobtChunkInodes)
		be.PutUint64(leaf[hdr+inobtRecSize+8:], 0xFFFFFFFFFFFFFFFF)
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
			return agi, nil
		}
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return leaf, nil
		}
		var savedLeaf []byte
		allocWriteAGBTree = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, _ uint32, blk []byte) error {
			savedLeaf = append([]byte(nil), blk...)
			return nil
		}
		allocWriteAGI = func(io.WriterAt, int64, *superblock, uint32, []byte) error { return nil }
		if err := growInobt(newMemRW(8*int(sb.blockSize)), 0, sb, 0); err != nil {
			t.Fatalf("growInobt sorted: %v", err)
		}
		nr := be.Uint16(savedLeaf[6:])
		if nr != 3 {
			t.Fatalf("numrecs after insert: got %d, want 3", nr)
		}
		got := []uint32{
			be.Uint32(savedLeaf[hdr+0*inobtRecSize:]),
			be.Uint32(savedLeaf[hdr+1*inobtRecSize:]),
			be.Uint32(savedLeaf[hdr+2*inobtRecSize:]),
		}
		want := []uint32{56, 128, 256}
		for i, w := range want {
			if got[i] != w {
				t.Fatalf("sorted insert record %d: got startIno=%d, want %d (full %v)", i, got[i], w, got)
			}
		}
	})

	t.Run("happy path", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 7, nil
		}
		agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		// Initialise AGI count/freeCount so we can verify the increments.
		be.PutUint32(agi[agiOffCount:], 8)
		be.PutUint32(agi[agiOffFreeCount:], 0)
		leaf := make([]byte, sb.blockSize)
		be.PutUint16(leaf[6:], 0)
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
			return agi, nil
		}
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return leaf, nil
		}
		var savedAGI []byte
		var savedLeaf []byte
		allocWriteAGBTree = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, _ uint32, blk []byte) error {
			savedLeaf = append([]byte(nil), blk...)
			return nil
		}
		allocWriteAGI = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, buf []byte) error {
			savedAGI = append([]byte(nil), buf...)
			return nil
		}
		if err := growInobt(newMemRW(8*int(sb.blockSize)), 0, sb, 0); err != nil {
			t.Fatalf("growInobt happy: %v", err)
		}
		// AGI: count went 8 → 8+64, freeCount 0 → 64.
		if got := be.Uint32(savedAGI[agiOffCount:]); got != 8+inobtChunkInodes {
			t.Fatalf("AGI count: got %d, want %d", got, 8+inobtChunkInodes)
		}
		if got := be.Uint32(savedAGI[agiOffFreeCount:]); got != inobtChunkInodes {
			t.Fatalf("AGI freeCount: got %d, want %d", got, inobtChunkInodes)
		}
		// Leaf: numrecs 0 → 1. The allocator returned block 7, which growInobt
		// rounds up to the next blocksPerChunk (8) boundary → block 8, so
		// startIno = 8*8 = 64 (a 64-inode-aligned chunk).
		if nr := be.Uint16(savedLeaf[6:]); nr != 1 {
			t.Fatalf("leaf numrecs: got %d, want 1", nr)
		}
		hdr := sb.agBTreeHdrSize()
		if got := be.Uint32(savedLeaf[hdr:]); got != 64 {
			t.Fatalf("leaf startIno: got %d, want 64", got)
		}
		if got := be.Uint32(savedLeaf[hdr+4:]); got != inobtChunkInodes {
			t.Fatalf("leaf freeCount: got %d, want %d", got, inobtChunkInodes)
		}
		if got := be.Uint64(savedLeaf[hdr+8:]); got != 0xFFFFFFFFFFFFFFFF {
			t.Fatalf("leaf irFree: got 0x%X, want all-ones", got)
		}
	})
}

// TestAllocInode_GrowFailurePropagates checks that when both find-free
// AND grow-inobt fail (e.g. no free blocks left), allocInode returns the
// grow error rather than masking it.
func TestAllocInode_GrowFailurePropagates(t *testing.T) {
	sb := allocTestSB()

	restoreAllocHooks(t)
	agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
	allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
		return agi, nil
	}
	allocInobtFindFree = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, []byte, int, error) {
		return 0, nil, 0, errInobtFull
	}
	allocGrowInobt = func(readerWriterAt, int64, *superblock, uint32) error {
		return errBoom
	}
	if _, err := allocInode(newMemRW(0), 0, sb, 0); !errors.Is(err, errBoom) {
		t.Fatalf("allocInode grow failure: got %v, want %v", err, errBoom)
	}
}

// TestAllocInode_NonFullErrorNotGrown checks that a non-sentinel error
// from inobtFindFree (e.g. I/O failure) propagates without triggering
// growInobt.
func TestAllocInode_NonFullErrorNotGrown(t *testing.T) {
	sb := allocTestSB()

	restoreAllocHooks(t)
	agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
	allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
		return agi, nil
	}
	allocInobtFindFree = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, []byte, int, error) {
		return 0, nil, 0, errBoom // not errInobtFull
	}
	growCalled := false
	allocGrowInobt = func(readerWriterAt, int64, *superblock, uint32) error {
		growCalled = true
		return nil
	}
	if _, err := allocInode(newMemRW(0), 0, sb, 0); !errors.Is(err, errBoom) {
		t.Fatalf("allocInode non-full error: got %v, want %v", err, errBoom)
	}
	if growCalled {
		t.Fatalf("growInobt called for non-full error; should propagate I/O errors as-is")
	}
}

// TestAllocInode_AGIReReadFailsAfterGrow covers the second AGI read in
// allocInode that happens after growInobt rewrites the on-disk AGI.
func TestAllocInode_AGIReReadFailsAfterGrow(t *testing.T) {
	sb := allocTestSB()

	restoreAllocHooks(t)
	calls := 0
	agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
	allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
		calls++
		if calls == 1 {
			return agi, nil
		}
		return nil, errBoom
	}
	allocInobtFindFree = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, []byte, int, error) {
		return 0, nil, 0, errInobtFull
	}
	allocGrowInobt = func(readerWriterAt, int64, *superblock, uint32) error {
		return nil
	}
	if _, err := allocInode(newMemRW(0), 0, sb, 0); !errors.Is(err, errBoom) {
		t.Fatalf("allocInode AGI re-read: got %v, want %v", err, errBoom)
	}
}

// TestAllocInode_FindFreeFailsAfterGrow covers the case where growInobt
// claims success but the post-grow find-free still cannot locate a free
// inode (defensive — shouldn't normally happen).
func TestAllocInode_FindFreeFailsAfterGrow(t *testing.T) {
	sb := allocTestSB()

	restoreAllocHooks(t)
	agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
	allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
		return agi, nil
	}
	calls := 0
	allocInobtFindFree = func(readerWriterAt, int64, *superblock, uint32, uint32, int) (uint32, []byte, int, error) {
		calls++
		return 0, nil, 0, errInobtFull
	}
	allocGrowInobt = func(readerWriterAt, int64, *superblock, uint32) error {
		return nil
	}
	if _, err := allocInode(newMemRW(0), 0, sb, 0); err == nil {
		t.Fatal("expected allocInode to fail when find-free still reports full after grow")
	}
	if calls < 2 {
		t.Fatalf("expected find-free to be retried; got %d calls", calls)
	}
}

// TestInobtDeepInsertErrors covers the error branches of the deep-insert
// helpers: in-place interior insert (child did not split), interior-node split
// allocation/write/old-right-sibling failures, leaf-split old-right-sibling
// failures, and the new-root re-read / write failures during a depth grow.
func TestInobtDeepInsertErrors(t *testing.T) {
	sb := deepInobtSB()
	be := binary.BigEndian
	hdr := sb.agBTreeHdrSize()
	maxLeaf := (int(sb.blockSize) - hdr) / inobtRecSize
	maxNode := (int(sb.blockSize) - hdr) / (inobtKeySize + inobtPtrSize)

	fullLeaf := func(self, lsib, rsib, base uint32) []byte {
		blk := make([]byte, sb.blockSize)
		be.PutUint16(blk[4:], 0)
		be.PutUint16(blk[6:], uint16(maxLeaf))
		be.PutUint32(blk[8:], lsib)
		be.PutUint32(blk[12:], rsib)
		for i := 0; i < maxLeaf; i++ {
			be.PutUint32(blk[hdr+i*inobtRecSize:], base+uint32(i)*uint32(inobtChunkInodes))
		}
		return blk
	}

	t.Run("in-place insert under interior (child no split)", func(t *testing.T) {
		restoreAllocHooks(t)
		rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))
		// Root(block 1) → leaf 10 (has room: 1 record) , leaf 11.
		leaf10 := make([]byte, sb.blockSize)
		be.PutUint16(leaf10[6:], 1)
		be.PutUint32(leaf10[12:], 11)
		be.PutUint32(leaf10[hdr:], 64) // single record startIno=64
		leaf11 := fullLeaf(11, 10, 0xFFFFFFFF, 1<<20)
		root := makeInobtInternal(sb, []uint32{64, 1 << 20}, []uint32{10, 11})
		putFSBlock(rw, 0, sb, 0, 10, leaf10)
		putFSBlock(rw, 0, sb, 0, 11, leaf11)
		putFSBlock(rw, 0, sb, 0, 1, root)

		agi := makeAGIBuffer(sb, 0, 1, 2, 0, 0)
		// New record 128: descends into leaf 10 (which has room) → no split, no
		// alloc. Exercises the interior !res.split early return.
		if err := inobtInsertChunkRecord(rw, 0, sb, 0, agi, 1, 2, 128); err != nil {
			t.Fatalf("in-place interior insert: %v", err)
		}
		l10, _ := readAGBlock(rw, 0, sb, 0, 10)
		if nr := be.Uint16(l10[6:]); nr != 2 {
			t.Fatalf("leaf 10 numrecs: got %d, want 2", nr)
		}
		// Root unchanged (still depth 2, same root, 2 records).
		if be.Uint32(agi[agiOffLevel:]) != 2 || be.Uint16(root[6:]) != 2 {
			t.Fatalf("root changed unexpectedly")
		}
	})

	t.Run("interior child read fail propagates", func(t *testing.T) {
		restoreAllocHooks(t)
		root := makeInobtInternal(sb, []uint32{64, 1 << 20}, []uint32{10, 11})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 1 {
				return root, nil
			}
			return nil, errBoom // child read fails
		}
		agi := makeAGIBuffer(sb, 0, 1, 2, 0, 0)
		if err := inobtInsertChunkRecord(newMemRW(0), 0, sb, 0, agi, 1, 2, 128); !errors.Is(err, errBoom) {
			t.Fatalf("child read fail: got %v", err)
		}
	})

	t.Run("interior node split error branches", func(t *testing.T) {
		// A full interior node with sorted keys/ptrs; insert at the end.
		fullNode := func() []byte {
			keys := make([]uint32, maxNode)
			ptrs := make([]uint32, maxNode)
			for i := 0; i < maxNode; i++ {
				keys[i] = uint32(64 + i*1000)
				ptrs[i] = uint32(1000 + i)
			}
			n := makeInobtInternal(sb, keys, ptrs)
			be.PutUint32(n[12:], 0xFFFFFFFF) // no old-right sibling
			return n
		}

		// alloc fails.
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 0, errBoom
		}
		if _, err := inobtSplitNode(newMemRW(0), 0, sb, 0, 5, fullNode(), maxNode, 1<<30, 9999); !errors.Is(err, errBoom) {
			t.Fatalf("node-split alloc fail: got %v", err)
		}

		// left/right writes fail.
		for failOn := 1; failOn <= 2; failOn++ {
			restoreAllocHooks(t)
			allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
				return 300, nil
			}
			var w int
			allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error {
				w++
				if w == failOn {
					return errBoom
				}
				return nil
			}
			if _, err := inobtSplitNode(newMemRW(0), 0, sb, 0, 5, fullNode(), maxNode, 1<<30, 9999); !errors.Is(err, errBoom) {
				t.Fatalf("node-split write fail #%d: got %v", failOn, err)
			}
		}

		// old-right-sibling read then write fail.
		nodeWithRight := func() []byte {
			n := fullNode()
			be.PutUint32(n[12:], 77) // old right sibling = block 77
			return n
		}
		// sibling read fails.
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 300, nil }
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 77 {
				return nil, errBoom
			}
			return make([]byte, sb.blockSize), nil
		}
		if _, err := inobtSplitNode(newMemRW(0), 0, sb, 0, 5, nodeWithRight(), maxNode, 1<<30, 9999); !errors.Is(err, errBoom) {
			t.Fatalf("node-split old-right read fail: got %v", err)
		}
		// sibling write fails.
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 300, nil }
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return make([]byte, sb.blockSize), nil
		}
		var w int
		allocWriteAGBTree = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, agRel uint32, _ []byte) error {
			w++
			if agRel == 77 {
				return errBoom // the sibling fixup write
			}
			return nil
		}
		if _, err := inobtSplitNode(newMemRW(0), 0, sb, 0, 5, nodeWithRight(), maxNode, 1<<30, 9999); !errors.Is(err, errBoom) {
			t.Fatalf("node-split old-right write fail: got %v", err)
		}
	})

	t.Run("leaf split old-right-sibling fixup", func(t *testing.T) {
		// A full leaf whose right sibling is a real block (so the fixup runs).
		mkLeaf := func() []byte {
			l := fullLeaf(5, 0xFFFFFFFF, 88, 64) // rsib = 88
			return l
		}
		// sibling read fails.
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 300, nil }
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			if agRel == 88 {
				return nil, errBoom
			}
			return make([]byte, sb.blockSize), nil
		}
		var rec [inobtRecSize]byte
		be.PutUint32(rec[0:], 1<<20)
		if _, err := inobtSplitLeaf(newMemRW(0), 0, sb, 0, 5, mkLeaf(), maxLeaf, rec[:]); !errors.Is(err, errBoom) {
			t.Fatalf("leaf-split old-right read fail: got %v", err)
		}
		// sibling write fails.
		restoreAllocHooks(t)
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 300, nil }
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return make([]byte, sb.blockSize), nil
		}
		allocWriteAGBTree = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, agRel uint32, _ []byte) error {
			if agRel == 88 {
				return errBoom
			}
			return nil
		}
		if _, err := inobtSplitLeaf(newMemRW(0), 0, sb, 0, 5, mkLeaf(), maxLeaf, rec[:]); !errors.Is(err, errBoom) {
			t.Fatalf("leaf-split old-right write fail: got %v", err)
		}
	})

	t.Run("grow new-root read and write failures", func(t *testing.T) {
		// Full single-leaf root: split promotes to depth 2 via the new-root path.
		fullRootLeaf := func() []byte { return fullLeaf(5, 0xFFFFFFFF, 0xFFFFFFFF, 64) }

		// Old-root re-read (for leftKey/level) fails after the leaf split.
		restoreAllocHooks(t)
		var rcount int
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
			rcount++
			if rcount == 1 {
				return fullRootLeaf(), nil // initial descend read
			}
			return nil, errBoom // the post-split old-root re-read
		}
		allocWriteAGBTree = func(io.WriterAt, int64, *superblock, uint32, uint32, []byte) error { return nil }
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 201, nil }
		agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		if err := inobtInsertChunkRecord(newMemRW(0), 0, sb, 0, agi, 5, 1, 1<<20); !errors.Is(err, errBoom) {
			t.Fatalf("grow old-root re-read fail: got %v", err)
		}

		// New-root write fails.
		restoreAllocHooks(t)
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return fullRootLeaf(), nil
		}
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 201, nil }
		var w int
		allocWriteAGBTree = func(_ io.WriterAt, _ int64, _ *superblock, _ uint32, agRel uint32, _ []byte) error {
			w++
			// Let the two leaf-split writes pass; fail the new-root write (the
			// 3rd write, to a freshly-allocated block).
			if w == 3 {
				return errBoom
			}
			return nil
		}
		agi = makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		if err := inobtInsertChunkRecord(newMemRW(0), 0, sb, 0, agi, 5, 1, 1<<20); !errors.Is(err, errBoom) {
			t.Fatalf("grow new-root write fail: got %v", err)
		}
	})
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// deepInobtSB is a small-block geometry (512-byte blocks) chosen so a single
// interior node fills quickly: leaf maxRecs = (512-56)/16 = 28, interior
// maxRecs = (512-56)/8 = 57. agBlkLog=12 keeps every AG-0 block number equal to
// its absolute block (ag<<12 | rel = rel for ag 0), so a memRW indexed by block
// number backs the whole tree.
func deepInobtSB() *superblock {
	return &superblock{
		blockSize: 512,
		agBlocks:  4096,
		agCount:   2,
		inodeSize: 256,
		inopBlock: 8,
		inopBLog:  3,
		agBlkLog:  12,
		hasCRC:    true,
		hasFType:  true,
	}
}

// TestInobtDeepInsert drives the *real* recursive inobt insert against a
// hand-built multi-level tree in a memRW (real readAGBlock/writeAGBTree, only
// block allocation mocked). It covers the three deep-tree cases the depth-1
// split never reaches:
//
//  1. insert into a full leaf under an already-depth-2 root that still has room
//     → leaf split + interior key/ptr insert in place;
//  2. insert into a full leaf under a *full* interior root → leaf split that
//     overflows the root → interior-node split → tree grows to depth 3;
//  3. the resulting depth-3 tree is internally consistent: every record (old +
//     new) is reachable through inobtFindRecord with the right startIno.
func TestInobtDeepInsert(t *testing.T) {
	sb := deepInobtSB()
	be := binary.BigEndian
	hdr := sb.agBTreeHdrSize()
	maxLeaf := (int(sb.blockSize) - hdr) / inobtRecSize                  // 28
	maxNode := (int(sb.blockSize) - hdr) / (inobtKeySize + inobtPtrSize) // 57

	// chunkStride keeps every record's startIno a clean multiple of the 64-inode
	// chunk so the tree mirrors a real inobt.
	const chunkStride = uint32(inobtChunkInodes)

	// fullLeaf builds a level-0 leaf of maxLeaf records starting at base,
	// stepping by chunkStride, with the given left/right siblings.
	fullLeaf := func(self, lsib, rsib, base uint32) []byte {
		blk := make([]byte, sb.blockSize)
		be.PutUint16(blk[4:], 0)
		be.PutUint16(blk[6:], uint16(maxLeaf))
		be.PutUint32(blk[8:], lsib)
		be.PutUint32(blk[12:], rsib)
		be.PutUint64(blk[16:], sb.fsbToPhysBlock(sb.agAbsBlock(0, self))*fmtDaddrPerBlock)
		for i := 0; i < maxLeaf; i++ {
			off := hdr + i*inobtRecSize
			be.PutUint32(blk[off:], base+uint32(i)*chunkStride)
			be.PutUint32(blk[off+4:], 0) // full chunk
			be.PutUint64(blk[off+8:], 0)
		}
		return blk
	}

	t.Run("leaf split propagates into interior root with room", func(t *testing.T) {
		restoreAllocHooks(t)
		rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))

		// Tree: root(block 1, interior, 2 ptrs) → leaf 10 (full) , leaf 11 (full).
		// Insert lands in leaf 10 (lowest range) and splits it; the root absorbs
		// the new separator (numrecs 2 → 3) without splitting.
		base0 := uint32(64)                            // leaf 10 records
		base1 := base0 + uint32(maxLeaf)*chunkStride*2 // leaf 11 records (well above)
		leaf10 := fullLeaf(10, 0xFFFFFFFF, 11, base0)
		leaf11 := fullLeaf(11, 10, 0xFFFFFFFF, base1)
		root := makeInobtInternal(sb, []uint32{base0, base1}, []uint32{10, 11})
		putFSBlock(rw, 0, sb, 0, 10, leaf10)
		putFSBlock(rw, 0, sb, 0, 11, leaf11)
		putFSBlock(rw, 0, sb, 0, 1, root)

		var next uint32 = 100
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			b := next
			next++
			return uint64(b), nil
		}

		agi := makeAGIBuffer(sb, 0, 1, 2, 0, 0) // depth 2, root=block 1
		// New record between base0 and base0+stride: lands inside full leaf 10.
		newStart := base0 + chunkStride/2*0 + chunkStride*uint32(maxLeaf) - chunkStride // just below leaf 11's range edge but in leaf 10
		newStart = base0 + uint32(maxLeaf)*chunkStride - chunkStride/2                  // mid-range, > all of leaf 10's keys but < base1
		if err := inobtInsertChunkRecord(rw, 0, sb, 0, agi, 1, 2, newStart); err != nil {
			t.Fatalf("deep insert (root has room): %v", err)
		}
		// Root must stay at the same block and depth 2, now with 3 children.
		if be.Uint32(agi[agiOffRoot:]) != 1 || be.Uint32(agi[agiOffLevel:]) != 2 {
			t.Fatalf("root moved/grew unexpectedly: root=%d level=%d", be.Uint32(agi[agiOffRoot:]), be.Uint32(agi[agiOffLevel:]))
		}
		rootBlk, _ := readAGBlock(rw, 0, sb, 0, 1)
		if nr := be.Uint16(rootBlk[6:]); nr != 3 {
			t.Fatalf("root numrecs after leaf split: got %d, want 3", nr)
		}
		// The new record is reachable.
		agRel := newStart // record startIno == AG-relative inode of slot 0
		_, leaf, _, err := inobtFindRecord(rw, 0, sb, 0, 1, 2, agRel)
		if err != nil {
			t.Fatalf("find new record: %v", err)
		}
		_ = leaf
	})

	t.Run("interior root split grows tree to depth 3", func(t *testing.T) {
		restoreAllocHooks(t)
		rw := newMemRW(int(sb.agBlocks) * int(sb.blockSize))

		// Build a depth-2 tree with a FULL interior root: maxNode leaves, the
		// first one full and targeted by the insert. Leaf blocks live at 10..10+
		// maxNode-1, each holding maxLeaf records in disjoint ascending ranges.
		// rangeStride is the key span of one full leaf.
		rangeStride := uint32(maxLeaf) * chunkStride
		keys := make([]uint32, maxNode)
		ptrs := make([]uint32, maxNode)
		for i := 0; i < maxNode; i++ {
			self := uint32(10 + i)
			base := uint32(64) + uint32(i)*rangeStride*2 // *2 leaves a gap between leaves
			var lsib, rsib uint32 = 0xFFFFFFFF, 0xFFFFFFFF
			if i > 0 {
				lsib = uint32(10 + i - 1)
			}
			if i < maxNode-1 {
				rsib = uint32(10 + i + 1)
			}
			putFSBlock(rw, 0, sb, 0, self, fullLeaf(self, lsib, rsib, base))
			keys[i] = base
			ptrs[i] = self
		}
		root := makeInobtInternal(sb, keys, ptrs)
		const rootBlk = uint32(1)
		putFSBlock(rw, 0, sb, 0, rootBlk, root)

		var next uint32 = 200
		allocAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			b := next
			next++
			return uint64(b), nil
		}

		agi := makeAGIBuffer(sb, 0, rootBlk, 2, 0, 0)
		// Target the FIRST leaf (keys[0]..) mid-range so it splits; the new
		// separator must be inserted into the already-full root, splitting it
		// and growing the tree to depth 3.
		newStart := keys[0] + rangeStride - chunkStride/2 // inside leaf 0's range, distinct from its keys
		// Ensure it doesn't collide with an existing record: pick a value not on
		// the chunkStride grid of leaf 0 by nudging to an odd multiple offset.
		if newStart%chunkStride == 0 {
			newStart += chunkStride / 2
		}
		if err := inobtInsertChunkRecord(rw, 0, sb, 0, agi, rootBlk, 2, newStart); err != nil {
			t.Fatalf("deep insert (root full → grow): %v", err)
		}
		// Tree must have grown to depth 3 with a brand-new root.
		newRoot := be.Uint32(agi[agiOffRoot:])
		if be.Uint32(agi[agiOffLevel:]) != 3 {
			t.Fatalf("expected depth 3 after interior split, got level %d", be.Uint32(agi[agiOffLevel:]))
		}
		if newRoot == rootBlk {
			t.Fatalf("root block unchanged after grow: %d", newRoot)
		}
		nrBlk, _ := readAGBlock(rw, 0, sb, 0, newRoot)
		if lvl := be.Uint16(nrBlk[4:]); lvl != 2 {
			t.Fatalf("new root block-level: got %d, want 2", lvl)
		}
		if nr := be.Uint16(nrBlk[6:]); nr != 2 {
			t.Fatalf("new root numrecs: got %d, want 2", nr)
		}

		// Every original record plus the new one is reachable through the depth-3
		// tree, landing in a record whose chunk covers it.
		check := func(agRel uint32) {
			_, leaf, idx, err := inobtFindRecord(rw, 0, sb, 0, newRoot, 3, agRel)
			if err != nil {
				t.Fatalf("find %d in depth-3 tree: %v", agRel, err)
			}
			recOff := hdr + idx*inobtRecSize
			start := be.Uint32(leaf[recOff:])
			if agRel < start || agRel >= start+inobtChunkInodes {
				t.Fatalf("find %d returned wrong record (start %d)", agRel, start)
			}
		}
		// First record of the first leaf, a middle leaf, the last leaf, and the
		// freshly-inserted record.
		check(64)
		check(keys[maxNode/2])
		check(keys[maxNode-1])
		check(newStart)
	})
}
