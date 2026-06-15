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
		allocAlignedAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32, uint32) (uint64, error) {
			return 0, errBoom
		}
		if err := growInobt(newMemRW(0), 0, sb, 0); !errors.Is(err, errBoom) {
			t.Fatalf("growInobt alloc error: got %v, want %v", err, errBoom)
		}
	})

	t.Run("agi re-read fails", func(t *testing.T) {
		restoreAllocHooks(t)
		// allocAllocBlocks succeeds, but the AGI re-read fails.
		allocAlignedAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32, uint32) (uint64, error) {
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
		allocAlignedAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32, uint32) (uint64, error) {
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

	t.Run("multi-level inobt rejected", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAlignedAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32, uint32) (uint64, error) {
			return 7, nil
		}
		agi := makeAGIBuffer(sb, 0, 5, 2 /* level=2 */, 0, 0)
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
			return agi, nil
		}
		err := growInobt(newMemRW(8*int(sb.blockSize)), 0, sb, 0)
		if err == nil || !contains(err.Error(), "level 2") {
			t.Fatalf("growInobt level>1: got %v, want error mentioning 'level 2'", err)
		}
	})

	t.Run("leaf full rejected", func(t *testing.T) {
		restoreAllocHooks(t)
		allocAlignedAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32, uint32) (uint64, error) {
			return 7, nil
		}
		agi := makeAGIBuffer(sb, 0, 5, 1, 0, 0)
		hdr := sb.agBTreeHdrSize()
		maxRecs := (int(sb.blockSize) - hdr) / inobtRecSize
		// Build a "full" leaf (numrecs == maxRecs).
		leaf := make([]byte, sb.blockSize)
		be.PutUint16(leaf[6:], uint16(maxRecs))
		allocAGIBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
			return agi, nil
		}
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return leaf, nil
		}
		err := growInobt(newMemRW(8*int(sb.blockSize)), 0, sb, 0)
		if err == nil || !contains(err.Error(), "inobt leaf full") {
			t.Fatalf("growInobt leaf full: got %v", err)
		}
	})

	t.Run("duplicate startIno rejected", func(t *testing.T) {
		restoreAllocHooks(t)
		// Allocator returns block 0 (agAbsBlock(0, 0)), so startIno = 0.
		allocAlignedAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32, uint32) (uint64, error) {
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
		allocAlignedAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32, uint32) (uint64, error) {
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
		allocAlignedAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32, uint32) (uint64, error) {
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
		allocAlignedAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32, uint32) (uint64, error) {
			return 7, nil
		}
		rw := newMemRW(8 * int(sb.blockSize))
		rw.writeHook = func(int64, []byte) error { return errBoom }
		if err := growInobt(rw, 0, sb, 0); !errors.Is(err, errBoom) {
			t.Fatalf("growInobt inode-slot write: got %v, want %v", err, errBoom)
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
		allocAlignedAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32, uint32) (uint64, error) {
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
		allocAlignedAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32, uint32) (uint64, error) {
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
		// Leaf: numrecs 0 → 1, first record startIno = 7*8 = 56.
		if nr := be.Uint16(savedLeaf[6:]); nr != 1 {
			t.Fatalf("leaf numrecs: got %d, want 1", nr)
		}
		hdr := sb.agBTreeHdrSize()
		if got := be.Uint32(savedLeaf[hdr:]); got != 56 {
			t.Fatalf("leaf startIno: got %d, want 56", got)
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

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
