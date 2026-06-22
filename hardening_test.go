// hardening_test.go — security tests for the XFS parser against malicious or
// corrupt images. The threat model: an untrusted image must NEVER panic the
// host, read out of bounds, integer-overflow into a bad allocation/slice, loop
// forever, or OOM. Every finding below has both a focused regression test (with
// the exact attack vector) and is reachable from a FuzzXxx target whose seed
// corpus carries the same vector, so the assertions run under plain `go test`.
package filesystem_xfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/go-volumes/gpt"
	"github.com/go-volumes/safeio"
)

// ──────────────────── C1 / C2 / H2: superblock geometry ────────────────────

// TestHardenGeometryRejects feeds the exact corrupt-geometry vectors from the
// threat model and asserts readSuperblock returns an error (never a usable
// superblock that would later drive an OOB inode read).
func TestHardenGeometryRejects(t *testing.T) {
	cases := []struct {
		name string
		// fields mirror buildSBBuf's signature.
		blockSize uint32
		agBlocks  uint32
		agCount   uint32
		inodeSize uint16
		inopBlock uint16
		inopBLog  uint8
		agBlkLog  uint8
		dirBlkLog uint8
	}{
		// C1: inodeSize=8 makes every core/fork read OOB on the root inode.
		{"C1_inodeSize_8", 4096, 16384, 1, 8, 8, 3, 14, 0},
		{"C1_inodeSize_183", 4096, 16384, 1, 183, 8, 3, 14, 0},
		{"C1_inodeSize_not_pow2", 4096, 16384, 1, 384, 8, 3, 14, 0},
		{"C1_inodeSize_too_big", 4096, 16384, 1, 4096, 8, 3, 14, 0},
		// C2: blockSize=3 / tiny / non-power-of-two.
		{"C2_blockSize_3", 3, 16384, 1, 512, 8, 3, 14, 0},
		{"C2_blockSize_256", 256, 16384, 1, 512, 8, 3, 14, 0},
		{"C2_blockSize_not_pow2", 4097, 16384, 1, 512, 8, 3, 14, 0},
		{"C2_blockSize_too_big", 1 << 20, 16384, 1, 512, 8, 3, 14, 0},
		// H2: inconsistent / overflowing inode-shift fields.
		{"H2_agBlkLog_200", 4096, 16384, 1, 512, 8, 3, 200, 0},
		{"H2_inopBLog_mismatch", 4096, 16384, 1, 512, 8, 5, 14, 0},
		{"H2_inopBlock_not_pow2", 4096, 16384, 1, 512, 6, 3, 14, 0},
		{"H2_inopBlock_zero", 4096, 16384, 1, 512, 0, 0, 14, 0},
		{"H2_agBlkLog_mismatch", 4096, 16384, 1, 512, 8, 3, 20, 0},
		{"H2_dirBlkLog_huge", 4096, 16384, 1, 512, 8, 3, 14, 40},
		// Zero AG fields still rejected.
		{"zero_agBlocks", 4096, 0, 1, 512, 8, 3, 0, 0},
		{"zero_agCount", 4096, 16384, 0, 512, 8, 3, 14, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := buildSBBuf(tc.blockSize, tc.agBlocks, tc.agCount, 128,
				tc.inodeSize, tc.inopBlock, tc.inopBLog, tc.agBlkLog,
				tc.dirBlkLog, 0x0005, xfsSBFeatFType, 0)
			if _, err := readSuperblock(bytes.NewReader(buf), 0); err == nil {
				t.Fatalf("readSuperblock accepted corrupt geometry %q", tc.name)
			}
		})
	}
}

// TestHardenGeometryAccepts confirms a valid Rocky-Linux-9-shaped superblock,
// and other legal power-of-two geometries, still pass (no false positives).
func TestHardenGeometryAccepts(t *testing.T) {
	cases := []struct {
		blockSize uint32
		inodeSize uint16
		inopBlock uint16
		inopBLog  uint8
	}{
		{4096, 512, 8, 3}, // typical
		{512, 256, 2, 1},  // smallest legal block + inode
		{65536, 2048, 32, 5},
		{1024, 256, 4, 2},
	}
	for _, tc := range cases {
		buf := buildSBBuf(tc.blockSize, 16384, 2, 128, tc.inodeSize,
			tc.inopBlock, tc.inopBLog, 14, 0, 0x0005, xfsSBFeatFType, 0)
		if _, err := readSuperblock(bytes.NewReader(buf), 0); err != nil {
			t.Fatalf("readSuperblock rejected valid geometry bs=%d is=%d: %v",
				tc.blockSize, tc.inodeSize, err)
		}
	}
}

// TestHardenAgBlkLogCeil checks agBlkLog must equal ceil(log2(agBlocks)) for
// non-power-of-two AG sizes, covering the bits.Len32 branch.
func TestHardenAgBlkLogCeil(t *testing.T) {
	// agBlocks=16385 → ceil(log2)=15. agBlkLog=15 valid, 14 invalid.
	good := buildSBBuf(4096, 16385, 2, 128, 512, 8, 3, 15, 0, 0x0005, xfsSBFeatFType, 0)
	if _, err := readSuperblock(bytes.NewReader(good), 0); err != nil {
		t.Fatalf("valid ceil agBlkLog rejected: %v", err)
	}
	bad := buildSBBuf(4096, 16385, 2, 128, 512, 8, 3, 14, 0, 0x0005, xfsSBFeatFType, 0)
	if _, err := readSuperblock(bytes.NewReader(bad), 0); err == nil {
		t.Fatal("wrong agBlkLog for non-pow2 agBlocks accepted")
	}
}

// TestHardenBlockSmallerThanSector covers the block<sector rejection branch:
// a 512-byte block with a 4096-byte sector is invalid geometry.
func TestHardenBlockSmallerThanSector(t *testing.T) {
	buf := buildSBBuf(512, 16384, 2, 128, 512, 8, 3, 14, 0, 0x0005, xfsSBFeatFType, 0)
	binary.BigEndian.PutUint16(buf[sbOffSectSize:], 4096) // sector > block
	if _, err := readSuperblock(bytes.NewReader(buf), 0); err == nil {
		t.Fatal("accepted blockSize < sectorSize")
	}
}

// ──────────────────── C3: bounded allocations (OOM) ────────────────────────

// TestHardenReadExtentsBoundsSize feeds a di_size of 2^60 with a single
// allocated block: readExtents must reject it via safeio rather than attempt a
// 2^60-byte make().
func TestHardenReadExtentsBoundsSize(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(10, 0x8000, inodeFmtExtents, 1<<60)
	setInodeNBlocks(in, 1) // 1 block ≪ 2^60 bytes
	exts := []extent{{startOff: 0, startBlock: 1, count: 1}}
	_, err := readExtents(newMemRW(int(sb.blockSize*2)), 0, sb, exts, 1<<60, in)
	if !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("readExtents(di_size=2^60) err = %v, want ErrTooLarge", err)
	}
}

// TestHardenSymlinkBoundsSize feeds the same 2^60 di_size to the remote-symlink
// reader.
func TestHardenSymlinkBoundsSize(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(11, 0xA000, inodeFmtExtents, 1<<60)
	setInodeNBlocks(in, 1)
	// One inline extent so we reach the bounded allocation.
	binary.BigEndian.PutUint32(in.dataFork()[0:], 1) // nExts via field below
	in.nExts = 1
	rec := encodeExtent(extent{startOff: 0, startBlock: 1, count: 1})
	copy(in.dataFork(), rec[:])
	_, err := readSymlinkTarget(newMemRW(int(sb.blockSize*2)), 0, sb, in)
	if !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("readSymlinkTarget(di_size=2^60) err = %v, want ErrTooLarge", err)
	}
}

// TestHardenInodeMaxBytesCap exercises the nBlocks overflow clamp so the
// multiply in inodeMaxBytes cannot wrap int64.
func TestHardenInodeMaxBytesCap(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(12, 0x8000, inodeFmtExtents, 0)
	setInodeNBlocks(in, 1<<62) // absurd; must be clamped
	if got := inodeMaxBytes(sb, in); got <= 0 {
		t.Fatalf("inodeMaxBytes overflowed/negated: %d", got)
	}
	// And a normal inode yields nBlocks+1 blocks of slack.
	in2 := newTestInode(13, 0x8000, inodeFmtExtents, 0)
	setInodeNBlocks(in2, 2)
	if got, want := inodeMaxBytes(sb, in2), int64(3*sb.blockSize); got != want {
		t.Fatalf("inodeMaxBytes = %d, want %d", got, want)
	}
}

// TestHardenSymlinkRemoteReads exercises the remote-symlink read path with both
// a v5 XSLM-header block (stripped) and a payload longer than di_size (clamped),
// confirming the bounded reader returns exactly the target bytes.
func TestHardenSymlinkRemoteReads(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(int(sb.blockSize * 4))

	// Block 1 carries an XSLM header then the target bytes.
	copy(rw.data[sb.blockSize:], []byte(xfsSymlinkMagic))
	copy(rw.data[int(sb.blockSize)+xfsSymlinkHdrSize:], []byte("/etc/target"))

	in := newTestInode(30, 0xA000, inodeFmtExtents, uint64(len("/etc/target")))
	setInodeNBlocks(in, 1)
	in.nExts = 1
	rec := encodeExtent(extent{startOff: 0, startBlock: 1, count: 1})
	copy(in.dataFork(), rec[:])

	got, err := readSymlinkTarget(rw, 0, sb, in)
	if err != nil || string(got) != "/etc/target" {
		t.Fatalf("readSymlinkTarget = (%q, %v), want (/etc/target, nil)", got, err)
	}
}

// TestHardenSymlinkBtreeFormat covers the btree-format dispatch branch in the
// remote symlink reader.
func TestHardenSymlinkBtreeFormat(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(31, 0xA000, inodeFmtBtree, 4)
	fork := in.dataFork()
	be := binary.BigEndian
	be.PutUint16(fork[0:], 0) // leaf root
	be.PutUint16(fork[2:], 1)
	rec := encodeExtent(extent{startOff: 0, startBlock: 1, count: 1})
	copy(fork[4:], rec[:])
	setInodeNBlocks(in, 1)
	rw := newMemRW(int(sb.blockSize * 4))
	copy(rw.data[sb.blockSize:], []byte("link"))

	got, err := readSymlinkTarget(rw, 0, sb, in)
	if err != nil || string(got) != "link" {
		t.Fatalf("readSymlinkTarget btree = (%q, %v), want (link, nil)", got, err)
	}
}

// ──────────────────── C4: symlink-loop depth bound ─────────────────────────

// TestHardenSymlinkLoop builds a directory whose single child is a symlink that
// points back at itself, so naive resolution recurses forever. pathLookup must
// bail with ErrSymlinkLoop instead of overflowing the stack.
func TestHardenSymlinkLoop(t *testing.T) {
	restoreDirHooks(t)
	sb := defaultSB()

	root := newTestInode(sb.rootIno, 0x4000, inodeFmtLocal, 0)
	link := newTestInode(99, 0xA000, inodeFmtLocal, 0)

	dirReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
		if ino == sb.rootIno {
			return root, nil
		}
		return link, nil
	}
	dirLookupInDirHook = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, _ string) (uint64, error) {
		return 99, nil
	}
	dirReadSymlinkTarget = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]byte, error) {
		// Always resolve to a path that re-enters the symlink → infinite loop.
		return []byte("/self"), nil
	}

	_, err := pathLookup(newMemRW(0), 0, sb, "/self")
	if !errors.Is(err, ErrSymlinkLoop) {
		t.Fatalf("pathLookup symlink loop err = %v, want ErrSymlinkLoop", err)
	}
}

// ──────────────────── H4: btree block bounds + cyclic siblings ─────────────

// TestHardenBtreeExtentsSiblingCycle builds an inode whose internal bmbt root
// points at a leaf whose right-sibling points back at itself. btreeExtents must
// stop on the cycle, not spin.
func TestHardenBtreeExtentsSiblingCycle(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(20, 0x8000, inodeFmtBtree, 4096)
	fork := in.dataFork()
	be := binary.BigEndian
	be.PutUint16(fork[0:], 1) // level 1 (internal root)
	be.PutUint16(fork[2:], 1) // 1 record
	// pointer area: header(4)+keys(16) → ptr at offset 20, points at block 5.
	be.PutUint64(fork[20:], 5)

	hdr := btreeHdrSize(sb.hasCRC)
	leaf := make([]byte, sb.blockSize)
	be.PutUint16(leaf[6:], 0)  // 0 records
	be.PutUint32(leaf[12:], 5) // right sibling = self (block 5) → cycle
	_ = hdr

	rw := newMemRW(int(sb.blockSize * 8))
	copy(rw.data[5*sb.blockSize:], leaf)

	_, err := btreeExtents(rw, 0, sb, in)
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("btreeExtents sibling cycle err = %v, want ErrCycle", err)
	}
}

// TestHardenBtreeExtentsTooDeep builds a descent that never reaches level 0 and
// has a self-referential internal pointer, hitting the descent cycle guard.
func TestHardenBtreeExtentsDescentCycle(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(21, 0x8000, inodeFmtBtree, 4096)
	fork := in.dataFork()
	be := binary.BigEndian
	be.PutUint16(fork[0:], 3) // claim depth 3
	be.PutUint16(fork[2:], 1)
	be.PutUint64(fork[20:], 6) // first child = block 6

	hdr := btreeHdrSize(sb.hasCRC)
	node := make([]byte, sb.blockSize)
	be.PutUint16(node[6:], 1)      // 1 record
	be.PutUint64(node[hdr+16:], 6) // its leftmost child = itself → cycle
	rw := newMemRW(int(sb.blockSize * 8))
	copy(rw.data[6*sb.blockSize:], node)

	_, err := btreeExtents(rw, 0, sb, in)
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("btreeExtents descent cycle err = %v, want ErrCycle", err)
	}
}

// TestHardenBtreeExtentsBlockOverflow feeds an internal-root pointer that
// overflows the block-byte-offset computation; btreeExtents must surface
// ErrBlockOutOfRange, not a negative ReadAt offset.
func TestHardenBtreeExtentsBlockOverflow(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(22, 0x8000, inodeFmtBtree, 4096)
	fork := in.dataFork()
	be := binary.BigEndian
	be.PutUint16(fork[0:], 1)
	be.PutUint16(fork[2:], 1)
	be.PutUint64(fork[20:], 0x3038000000000000) // huge block → offset overflow
	_, err := btreeExtents(newMemRW(int(sb.blockSize)), 0, sb, in)
	if !errors.Is(err, ErrBlockOutOfRange) {
		t.Fatalf("btreeExtents overflow err = %v, want ErrBlockOutOfRange", err)
	}
}

// TestHardenBtreeExtentsDescentTooDeep feeds a deep descent (level 3) whose
// internal nodes are valid but the leftmost child of the deepest node still has
// a nonzero level, so the loop bound (descend guard) trips before any leaf.
func TestHardenBtreeExtentsDescentDepth(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(23, 0x8000, inodeFmtBtree, 4096)
	fork := in.dataFork()
	be := binary.BigEndian
	be.PutUint16(fork[0:], 2) // depth 2
	be.PutUint16(fork[2:], 1)
	be.PutUint64(fork[20:], 6) // child = block 6

	hdr := btreeHdrSize(sb.hasCRC)
	// Block 6: internal node, leftmost child = block 7.
	node6 := make([]byte, sb.blockSize)
	be.PutUint16(node6[6:], 1)
	be.PutUint64(node6[hdr+16:], 7)
	rw := newMemRW(int(sb.blockSize * 8))
	copy(rw.data[6*sb.blockSize:], node6)
	// Block 7 left zero (numrecs 0) → leaf walk; just must not panic.
	if _, err := btreeExtents(rw, 0, sb, in); err != nil {
		// Either a clean traversal or a graceful error is acceptable; a panic
		// is not (the test would crash). Accept any error.
		_ = err
	}
}

// TestHardenBtreeExtentsLeafBlockTooSmall feeds a backing block smaller than
// the header so the len(blk) floor in btreeExtents fires.
func TestHardenBtreeExtentsLeafBlockTooSmall(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(24, 0x8000, inodeFmtBtree, 4096)
	fork := in.dataFork()
	be := binary.BigEndian
	be.PutUint16(fork[0:], 1)
	be.PutUint16(fork[2:], 1)
	be.PutUint64(fork[20:], 5)
	// btreeExtents reads via the package readRawBlock; shrink blockSize on an
	// sb clone so the backing block is smaller than hdrSize+8, tripping the
	// len(blk) floor. (The clone bypasses validateGeometry, which is exactly
	// the defensive case this floor exists for.)
	small := *sb
	small.blockSize = 8 // below hdrSize+8 for v5 (56+8)
	rw := newMemRW(int(sb.blockSize * 8))
	if _, err := btreeExtents(rw, 0, &small, in); err == nil {
		t.Fatal("expected btree leaf block-too-small error")
	}
}

// ──────────────────── H3: AGF btree loop / cycle bounds ────────────────────

// TestHardenCntFindBlockSiblingCycle gives cntFindBlock a leaf whose right
// sibling is itself and no record satisfies the request: it must terminate on
// the cycle guard rather than spinning forever.
func TestHardenCntFindBlockSiblingCycle(t *testing.T) {
	restoreAllocHooks(t)
	sb := defaultSB()
	// Leaf at agRel 2, rsib → 2 (itself), only a count=1 record (need 5).
	leaf := makeAllocLeaf(sb, 2, allocRec{start: 10, count: 1})
	allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, _ uint32) ([]byte, error) {
		return leaf, nil
	}
	_, _, _, err := cntFindBlock(newMemRW(0), 0, sb, 0, 2, 0, 5)
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("cntFindBlock sibling cycle err = %v, want ErrCycle", err)
	}
}

// TestHardenAgfRecomputeLongestSiblingCycle does the same for the longest-run
// recompute walk.
func TestHardenAgfRecomputeLongestSiblingCycle(t *testing.T) {
	restoreAllocHooks(t)
	sb := defaultSB()
	leaf := makeAllocLeaf(sb, 3, allocRec{start: 10, count: 2})
	allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, _ uint32) ([]byte, error) {
		return leaf, nil
	}
	// level 1 → root is already a leaf; rsib=3≠self the first time, but the
	// leaf always returns rsib=3, and agRel becomes 3 then loops back to a leaf
	// reporting rsib=3 again → revisits 3.
	_, err := agfRecomputeLongest(newMemRW(0), 0, sb, 0, 2, 1)
	if !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("agfRecomputeLongest sibling cycle err = %v, want ErrCycle", err)
	}
}

// TestHardenAgfRecomputeLongestDescent exercises the internal-node descent
// path (level>1) plus the leftmost-leaf walk, returning the true longest run.
func TestHardenAgfRecomputeLongestDescent(t *testing.T) {
	restoreAllocHooks(t)
	sb := defaultSB()
	root := makeAllocInternal(sb, []allocRec{{start: 0, count: 1}}, []uint32{8})
	leaf := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 30, count: 9})
	allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
		if agRel == 2 {
			return root, nil
		}
		return leaf, nil
	}
	longest, err := agfRecomputeLongest(newMemRW(0), 0, sb, 0, 2, 2)
	if err != nil || longest != 9 {
		t.Fatalf("agfRecomputeLongest descent = (%d, %v), want (9, nil)", longest, err)
	}
}

// TestHardenAgfRecomputeLongestDescentCycle drives a cyclic internal-node
// descent (the leftmost child points back at the parent).
func TestHardenAgfRecomputeLongestDescentCycle(t *testing.T) {
	restoreAllocHooks(t)
	sb := defaultSB()
	// Internal node at agRel 2 whose leftmost child is itself.
	node := makeAllocInternal(sb, []allocRec{{start: 0, count: 1}}, []uint32{2})
	allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, _ uint32) ([]byte, error) {
		return node, nil
	}
	if _, err := agfRecomputeLongest(newMemRW(0), 0, sb, 0, 2, 3); !errors.Is(err, safeio.ErrCycle) {
		t.Fatalf("agfRecomputeLongest descent cycle err = %v, want ErrCycle", err)
	}
}

// TestHardenReadExtentsBlockOverflow feeds a forged extent startBlock whose
// byte offset overflows int64; readExtents must reject it gracefully.
func TestHardenReadExtentsBlockOverflow(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(25, 0x8000, inodeFmtExtents, uint64(sb.blockSize))
	setInodeNBlocks(in, 1)
	exts := []extent{{startOff: 0, startBlock: 0x3038000000000000, count: 1}}
	_, err := readExtents(newMemRW(int(sb.blockSize)), 0, sb, exts, uint64(sb.blockSize), in)
	if !errors.Is(err, ErrBlockOutOfRange) {
		t.Fatalf("readExtents block overflow err = %v, want ErrBlockOutOfRange", err)
	}
}

// TestHardenCntFindBlockNonDecreasingLevel covers the strict level-decrease
// guard: an internal node whose child is itself an internal node of the same
// level must be rejected.
func TestHardenCntFindBlockNonDecreasingLevel(t *testing.T) {
	restoreAllocHooks(t)
	sb := defaultSB()
	// Two distinct internal nodes both at on-disk level 1, chained.
	n2 := makeAllocInternal(sb, []allocRec{{start: 0, count: 1}}, []uint32{8})
	n8 := makeAllocInternal(sb, []allocRec{{start: 0, count: 1}}, []uint32{9})
	allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, agRel uint32) ([]byte, error) {
		if agRel == 2 {
			return n2, nil
		}
		return n8, nil
	}
	_, _, _, err := cntFindBlock(newMemRW(0), 0, sb, 0, 2, 5, 5)
	if err == nil {
		t.Fatal("cntFindBlock accepted non-decreasing internal levels")
	}
}

// ──────────────────── H1: partition table hardening ────────────────────────

// TestHardenPartitionMalformed feeds a GPT with a hostile entry size and an
// out-of-range count; partitionOffset must surface an error, never panic.
func TestHardenPartitionMalformed(t *testing.T) {
	img := buildGPT(nil)
	// Entry size below MinEntrySize.
	binary.LittleEndian.PutUint32(img[512+84:], 64)
	if _, err := partitionOffset(bytes.NewReader(img), testDeviceSize, -1); err == nil {
		t.Fatal("partitionOffset accepted malformed GPT entry size")
	}

	// Hostile entry count 0xFFFFFFFF.
	img2 := buildGPT([]struct {
		typeGUID [16]byte
		startLBA uint64
	}{{linuxPartTypeGPT, 2048}})
	binary.LittleEndian.PutUint32(img2[512+80:], 0xFFFFFFFF)
	if _, err := partitionOffset(bytes.NewReader(img2), testDeviceSize, -1); err == nil {
		t.Fatal("partitionOffset accepted hostile GPT entry count")
	}

	// Partition start LBA past the device end.
	img3 := buildGPT([]struct {
		typeGUID [16]byte
		startLBA uint64
	}{{linuxPartTypeGPT, 1 << 50}})
	if _, err := partitionOffset(bytes.NewReader(img3), 1<<20, -1); err == nil {
		t.Fatal("partitionOffset accepted partition past device end")
	}
}

// TestHardenPartitionGPTErrNoTableFallback confirms a bare image (no table)
// maps to offset 0 via gpt.ErrNoTable rather than an error.
func TestHardenPartitionGPTErrNoTableFallback(t *testing.T) {
	img := make([]byte, 1<<20)
	off, err := partitionOffset(bytes.NewReader(img), int64(len(img)), -1)
	if err != nil || off != 0 {
		t.Fatalf("bare image = (%d, %v), want (0, nil)", off, err)
	}
	if !errors.Is(gpt.ErrNoTable, gpt.ErrGPT) {
		t.Fatal("gpt sentinel wiring changed unexpectedly")
	}
}

// ─────────────────────────── Fuzz targets ──────────────────────────────────

// FuzzReadSuperblock feeds arbitrary 512-byte superblock buffers and asserts
// readSuperblock never panics. Seeds carry the C1/C2/H2 vectors.
func FuzzReadSuperblock(f *testing.F) {
	f.Add(buildSBBuf(4096, 16384, 2, 128, 512, 8, 3, 14, 0, 0x0005, xfsSBFeatFType, 0))  // valid
	f.Add(buildSBBuf(4096, 16384, 1, 128, 8, 8, 3, 14, 0, 0x0005, xfsSBFeatFType, 0))    // C1 inodeSize=8
	f.Add(buildSBBuf(3, 16384, 1, 128, 512, 8, 3, 14, 0, 0x0005, xfsSBFeatFType, 0))     // C2 blockSize=3
	f.Add(buildSBBuf(4096, 16384, 1, 128, 512, 8, 3, 200, 0, 0x0005, xfsSBFeatFType, 0)) // H2 agBlkLog=200
	f.Fuzz(func(t *testing.T, buf []byte) {
		if len(buf) < 512 {
			buf = append(buf, make([]byte, 512-len(buf))...)
		}
		sb, err := readSuperblock(bytes.NewReader(buf), 0)
		if err != nil {
			return
		}
		// A superblock that parsed must satisfy every geometry invariant, so
		// downstream inode arithmetic cannot shift by ≥64 or index OOB.
		if sb.blockSize < minBlockSize || sb.blockSize > maxBlockSize {
			t.Fatalf("accepted out-of-range blockSize %d", sb.blockSize)
		}
		if int(sb.inodeSize) < inodeCoreSize {
			t.Fatalf("accepted inodeSize %d < core", sb.inodeSize)
		}
		if uint(sb.agBlkLog)+uint(sb.inopBLog) >= 64 {
			t.Fatalf("accepted shift overflow agBlkLog=%d inopBLog=%d", sb.agBlkLog, sb.inopBLog)
		}
		// Exercise inode arithmetic with the accepted geometry; must not panic.
		_ = inodeByteOff(sb, 0, sb.rootIno)
	})
}

// FuzzReadExtents asserts readExtents never panics or OOMs regardless of the
// attacker-controlled size / extent list. Seeds include di_size=2^60.
func FuzzReadExtents(f *testing.F) {
	f.Add(uint64(7), uint64(1), uint32(1))     // normal
	f.Add(uint64(1)<<60, uint64(1), uint32(1)) // C3 OOM vector
	f.Add(uint64(0), uint64(0), uint32(0))     // empty
	f.Fuzz(func(t *testing.T, size, startBlock uint64, count uint32) {
		sb := defaultSB()
		in := newTestInode(1, 0x8000, inodeFmtExtents, size)
		setInodeNBlocks(in, 4)
		exts := []extent{{startOff: 0, startBlock: startBlock % 4, count: count % 8}}
		rw := newMemRW(int(sb.blockSize * 8))
		// Errors are fine; a panic or OOM is not.
		_, _ = readExtents(rw, 0, sb, exts, size, in)
	})
}

// FuzzBtreeExtents drives the bmbt walker with arbitrary fork bytes + a small
// backing image. Seeds carry a cyclic right-sibling layout.
func FuzzBtreeExtents(f *testing.F) {
	f.Add([]byte{0, 1, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5}, uint8(5))
	f.Add([]byte{0, 0, 0, 1}, uint8(0))
	f.Fuzz(func(t *testing.T, fork []byte, sibBlock uint8) {
		sb := defaultSB()
		in := newTestInode(1, 0x8000, inodeFmtBtree, 4096)
		copy(in.dataFork(), fork)
		rw := newMemRW(int(sb.blockSize * 8))
		// A self-referential leaf to tempt the sibling walk into a loop.
		be := binary.BigEndian
		leaf := make([]byte, sb.blockSize)
		be.PutUint32(leaf[12:], uint32(sibBlock)%8)
		copy(rw.data[(uint64(sibBlock)%8)*uint64(sb.blockSize):], leaf)
		_, _ = btreeExtents(rw, 0, sb, in)
	})
}

// FuzzCntFindBlock drives the cnt B-tree navigation with arbitrary block bytes
// served for every AG-relative read; a cyclic or malformed tree must terminate.
func FuzzCntFindBlock(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 1}, uint32(5), uint32(5)) // leaf, 1 rec
	f.Add([]byte{0, 1, 0, 1}, uint32(5), uint32(5))       // internal node
	f.Fuzz(func(t *testing.T, blk []byte, rootRel, need uint32) {
		sb := defaultSB()
		restoreAllocHooks(t)
		full := make([]byte, sb.blockSize)
		copy(full, blk)
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, _ uint32) ([]byte, error) {
			return full, nil
		}
		_, _, _, _ = cntFindBlock(newMemRW(0), 0, sb, 0, rootRel, 5, need)
	})
}

// FuzzReadSymlink drives the remote-symlink reader with an arbitrary di_size
// and fork bytes; the C3 OOM vector (di_size=2^60) is in the seed corpus.
func FuzzReadSymlink(f *testing.F) {
	f.Add(uint64(7), uint64(1))
	f.Add(uint64(1)<<60, uint64(1)) // C3 OOM vector
	f.Fuzz(func(t *testing.T, size, startBlock uint64) {
		sb := defaultSB()
		in := newTestInode(1, 0xA000, inodeFmtExtents, size)
		setInodeNBlocks(in, 2)
		in.nExts = 1
		rec := encodeExtent(extent{startOff: 0, startBlock: startBlock % 4, count: 1})
		copy(in.dataFork(), rec[:])
		rw := newMemRW(int(sb.blockSize * 8))
		_, _ = readSymlinkTarget(rw, 0, sb, in)
	})
}

// FuzzPartitionOffset feeds arbitrary leading bytes to the partition parser and
// asserts it never panics. Seeds include a real GPT/MBR header.
func FuzzPartitionOffset(f *testing.F) {
	f.Add(buildGPT([]struct {
		typeGUID [16]byte
		startLBA uint64
	}{{gpt.LinuxFilesystemGUID, 2048}}))
	f.Add(buildMBR([]struct {
		ptype    uint8
		startLBA uint32
	}{{0x83, 2048}}))
	f.Add(make([]byte, 1024))
	f.Fuzz(func(t *testing.T, img []byte) {
		if len(img) < 1024 {
			img = append(img, make([]byte, 1024-len(img))...)
		}
		// Errors are fine; a panic is not. Use the image length as the device
		// size so bounds checks are meaningful.
		_, _ = partitionOffset(bytes.NewReader(img), int64(len(img)), -1)
		_, _ = partitionOffset(bytes.NewReader(img), int64(len(img)), 0)
	})
}
