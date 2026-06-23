package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
)

// TestWriteNodeIndexErrorPaths drives writeNodeIndex with synthetic inputs that
// force both a multi-level da-node (K > maxNodeEnts leaves) and a multi-block
// free index (D > bestsPerFree data blocks), then injects a failure at each
// allocation and write step to cover the error-return branches without building
// a full multi-hundred-thousand-entry image.
func TestWriteNodeIndexErrorPaths(t *testing.T) {
	sb := allocTestSB() // blockSize 512 keeps the multi-level/multi-free thresholds tiny
	blkSize := int(sb.blockSize)
	perLeaf := (blkSize - dirLeafHdrSize) / 8
	maxNodeEnts := (blkSize - dirNodeHdrSize) / 8
	bestsPerFree := (blkSize - dirFreeHdrSize) / 2

	// Enough leaf entries to need > maxNodeEnts leaf blocks (forces 2 da-node
	// levels), and enough data blocks to need > 1 free-index block.
	nLents := (maxNodeEnts + 2) * perLeaf
	lents := make([]leafEnt, nLents)
	for i := range lents {
		lents[i] = leafEnt{hash: uint32(i), addr: uint32(i)}
	}
	D := bestsPerFree + 5
	blockEnd := make([]int, D)
	for i := range blockEnd {
		blockEnd[i] = dirDataHdrSize(sb.hasCRC) + 16
	}

	in := newTestInode(128, 0x4000, inodeFmtExtents, 0)

	// Save/restore the write hooks.
	oldAlloc := writeAllocBlocks
	oldData := writeWriteBlocksData
	oldList := writeWriteExtentList
	oldInode := writeWriteInode
	t.Cleanup(func() {
		writeAllocBlocks = oldAlloc
		writeWriteBlocksData = oldData
		writeWriteExtentList = oldList
		writeWriteInode = oldInode
	})

	call := func() error {
		return writeNodeIndex(newMemRW(0), 0, sb, in, 100, D, blockEnd, lents)
	}

	// 1. leaf/node region allocation fails.
	writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 0, errBoom }
	if err := call(); !errors.Is(err, errBoom) {
		t.Fatalf("leaf-region alloc fail: got %v", err)
	}

	// 2. free-index allocation fails (first alloc succeeds, second fails).
	var n int
	writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
		n++
		if n == 1 {
			return 1000, nil
		}
		return 0, errBoom
	}
	if err := call(); !errors.Is(err, errBoom) {
		t.Fatalf("free-index alloc fail: got %v", err)
	}

	writeAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) { return 1000, nil }

	// 3. leaf/node region write fails (first writeBlocksData call).
	n = 0
	writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error {
		n++
		if n == 1 {
			return errBoom
		}
		return nil
	}
	if err := call(); !errors.Is(err, errBoom) {
		t.Fatalf("leaf-region write fail: got %v", err)
	}

	// 4. free-index region write fails (second writeBlocksData call).
	n = 0
	writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error {
		n++
		if n == 2 {
			return errBoom
		}
		return nil
	}
	if err := call(); !errors.Is(err, errBoom) {
		t.Fatalf("free-index write fail: got %v", err)
	}

	writeWriteBlocksData = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error { return nil }

	// 5. extent-list write fails.
	writeWriteExtentList = func(*inode, []extent) error { return errBoom }
	if err := call(); !errors.Is(err, errBoom) {
		t.Fatalf("extent-list write fail: got %v", err)
	}
	writeWriteExtentList = func(*inode, []extent) error { return nil }

	// 6. inode write fails.
	writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return errBoom }
	if err := call(); !errors.Is(err, errBoom) {
		t.Fatalf("inode write fail: got %v", err)
	}

	// 7. success: a clean run returns nil and the inode reaches node geometry.
	writeWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return nil }
	if err := call(); err != nil {
		t.Fatalf("clean writeNodeIndex: %v", err)
	}
}

// leafdir_node_test.go — coverage for the multi-LEVEL da-node hash btree and
// the MULTI-BLOCK free index emitted by writeNodeIndex for very large
// directories.
//
// Forcing those paths through the public WriteFile API is impractical (it would
// need ~255k files and re-lays-out the whole directory on every add, O(n²)), so
// these tests build the directory in one shot via writeWholeDir with N entries
// that all hardlink a single backing inode. That keeps the image small (one
// real file inode, nlink=N) while still driving the real allocator, extent
// writer and on-disk index layout. The structure is then walked exactly the way
// xfs_repair walks it: descend the da-node btree from the root at the 32 GiB
// offset, follow the leaf sibling chain, and resolve every free-index window.

// bulkLinkDir formats an image with room for `n` directory entries, creates one
// regular backing inode, and lays the root directory out with `n` entries (each
// named with namePrefix + a zero-padded index) all pointing at that inode (so a
// single inode satisfies xfs_repair's "every entry references a live inode"
// rule, with the backing inode's nlink set to n). A longer namePrefix yields
// fewer entries per data block — and thus more data blocks — which is how the
// multi-block free-index path is forced without needing millions of entries. It
// returns the open fs, the on-disk path, and the backing inode number.
func bulkLinkDir(t *testing.T, sizeBytes int64, n int, namePrefix string) (*xfsFS, string, uint64) {
	t.Helper()
	path := tempImage(t)
	fsIfc, err := Format(path, sizeBytes, FormatConfig{Label: "nodetest"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsIfc.(*xfsFS)
	t.Cleanup(func() { _ = fs.Close() })

	// One real backing inode (a small regular file).
	if err := fs.WriteFile("/target", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	targetIno, err := lookupInDir(fs.f, fs.partOffset, fs.sb,
		mustReadInode(t, fs, fs.sb.rootIno), "target")
	if err != nil {
		t.Fatalf("lookup target: %v", err)
	}

	// Build the root directory with n hardlink entries → target inode.
	entries := make([]dirEnt, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, dirEnt{fmt.Sprintf("%s%07d", namePrefix, i), targetIno, 1})
	}
	root := mustReadInode(t, fs, fs.sb.rootIno)
	if err := writeWholeDir(fs.f, fs.partOffset, fs.sb, root, fs.sb.rootIno, entries); err != nil {
		t.Fatalf("writeWholeDir(%d entries): %v", n, err)
	}

	// Set the backing inode's nlink to n (one per directory entry) so
	// xfs_repair's link-count check passes.
	tin := mustReadInode(t, fs, targetIno)
	binary.BigEndian.PutUint32(tin.raw[inoOffNLink:], uint32(n))
	if err := writeInode(fs.f, fs.partOffset, fs.sb, tin); err != nil {
		t.Fatalf("writeInode target nlink: %v", err)
	}
	if err := syncSuperblockCounts(fs.f, fs.partOffset, fs.sb); err != nil {
		t.Fatalf("syncSuperblockCounts: %v", err)
	}
	return fs, path, targetIno
}

func mustReadInode(t *testing.T, fs *xfsFS, ino uint64) *inode {
	t.Helper()
	in, err := readInode(fs.f, fs.partOffset, fs.sb, ino)
	if err != nil {
		t.Fatalf("readInode %d: %v", ino, err)
	}
	return in
}

func tempImage(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "node-*.img")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return f.Name()
}

// dirNodeStats walks the directory's on-disk da-node btree from its root at the
// 32 GiB logical offset and reports the tree height (number of node levels
// above the leaves), the leaf count, and the number of free-index blocks. It
// validates the v5 invariants xfs_repair checks along the way: every node and
// leaf block's magic, CRC, self-blkno and owner; the strictly-decreasing btree
// level on the way down; and that the leaf sibling chain is complete and
// hash-ordered.
type dirNodeStats struct {
	nodeLevels          int
	leafBlocks          int
	freeBlocks          int
	totalHash           int
	nodeSiblingsByLevel []int // node count reached via the sibling chain at each internal level
}

func walkDirNode(t *testing.T, fs *xfsFS, dirIno uint64) dirNodeStats {
	t.Helper()
	be := binary.BigEndian
	sb := fs.sb
	blkSize := int(sb.blockSize)
	in := mustReadInode(t, fs, dirIno)
	exts, err := dirExtents(fs.f, fs.partOffset, sb, in)
	if err != nil {
		t.Fatalf("dirExtents: %v", err)
	}
	leafLog := uint32(dirLeafByteOffset / uint64(blkSize))
	freeLog := uint32(dirFreeByteOffset / uint64(blkSize))

	// Map every logical leaf/free block → absolute block.
	logToAbs := map[uint32]uint64{}
	freeAbs := map[uint32]uint64{}
	for _, e := range exts {
		for b := uint32(0); b < e.count; b++ {
			lo := uint32(e.startOff) + b
			abs := e.startBlock + uint64(b)
			if uint64(lo) >= uint64(freeLog) {
				freeAbs[lo] = abs
			} else if lo >= leafLog {
				logToAbs[lo] = abs
			}
		}
	}

	readLog := func(lo uint32) []byte {
		abs, ok := logToAbs[lo]
		if !ok {
			t.Fatalf("logical leaf block %d (offset %d) not mapped", lo-leafLog, lo)
		}
		blk, err := readRawBlock(fs.f, fs.partOffset, sb, abs)
		if err != nil {
			t.Fatalf("read leaf block %d: %v", abs, err)
		}
		// Self-blkno + owner invariants.
		if got := be.Uint64(blk[16:]); got != sb.fsbToPhysBlock(abs)*fmtDaddrPerBlock {
			t.Fatalf("block %d blkno %d, want %d", abs, got, sb.fsbToPhysBlock(abs)*fmtDaddrPerBlock)
		}
		if got := be.Uint64(blk[48:]); got != dirIno {
			t.Fatalf("block %d owner %d, want %d", abs, got, dirIno)
		}
		verifyV5CRC(t, "danode", blk, 12)
		return blk
	}

	// Descend from the root (logical leafLog) to a leaf, counting node levels
	// and checking the level field strictly decreases.
	var stats dirNodeStats
	cur := leafLog
	prevLevel := 1 << 30
	for {
		blk := readLog(cur)
		magic := be.Uint16(blk[8:])
		if magic == magicDir3LeafN {
			break // reached the leaf level
		}
		if magic != magicDa3Node {
			t.Fatalf("block %d: unexpected magic 0x%04X in da-node descent", cur, magic)
		}
		lvl := int(be.Uint16(blk[58:]))
		if lvl >= prevLevel {
			t.Fatalf("da-node level %d not below parent %d", lvl, prevLevel)
		}
		prevLevel = lvl
		stats.nodeLevels++

		// Internal-node sibling chain check (the exact invariant xfs_repair's
		// verify_da_path enforces when it walks off the end of a node's
		// children): every node at this level must be reachable from the
		// leftmost one via forw links, with back links agreeing, and the chain
		// must terminate (forw==0) rather than dangle into block 0. Walk to the
		// leftmost node at this level, then forward across the whole level.
		leftmost := cur
		for {
			b := readLog(leftmost)
			if back := be.Uint32(b[4:]); back != 0 {
				leftmost = back
				continue
			}
			break
		}
		walk := leftmost
		var prevWalk uint32
		seen := 0
		for {
			b := readLog(walk)
			if be.Uint16(b[8:]) != magicDa3Node {
				t.Fatalf("level-%d sibling block %d magic 0x%04X != da-node", lvl, walk, be.Uint16(b[8:]))
			}
			if int(be.Uint16(b[58:])) != lvl {
				t.Fatalf("level-%d sibling block %d reports level %d", lvl, walk, be.Uint16(b[58:]))
			}
			if back := be.Uint32(b[4:]); back != prevWalk {
				t.Fatalf("level-%d block %d back=%d, want %d", lvl, walk, back, prevWalk)
			}
			// A 1-entry interior node trips xfs_repair's verify_da_path ("bad
			// forward block pointer, expected 0"); the writer must distribute
			// children so no interior node is left with a single entry.
			if c := int(be.Uint16(b[56:])); c < 2 {
				t.Fatalf("level-%d interior block %d has only %d entries (xfs_repair rejects 1-entry interior nodes)", lvl, walk, c)
			}
			seen++
			forw := be.Uint32(b[0:])
			if forw == 0 {
				break
			}
			prevWalk, walk = walk, forw
		}
		stats.nodeSiblingsByLevel = append(stats.nodeSiblingsByLevel, seen)

		// Follow the leftmost child to descend toward the leaves.
		cur = be.Uint32(blk[dirNodeHdrSize+4:])
	}

	// Walk the whole leaf sibling chain from the leftmost leaf. Find the
	// leftmost leaf by following back-siblings from `cur`.
	for {
		blk := readLog(cur)
		if back := be.Uint32(blk[4:]); back != 0 {
			cur = back
			continue
		}
		break
	}
	var lastHash uint32
	first := true
	for {
		blk := readLog(cur)
		if be.Uint16(blk[8:]) != magicDir3LeafN {
			t.Fatalf("leaf chain block %d magic 0x%04X != LEAFN", cur, be.Uint16(blk[8:]))
		}
		count := int(be.Uint16(blk[56:]))
		stats.leafBlocks++
		for i := 0; i < count; i++ {
			h := be.Uint32(blk[dirLeafHdrSize+i*8:])
			if !first && h < lastHash {
				t.Fatalf("leaf hash index not globally sorted: %08x after %08x", h, lastHash)
			}
			lastHash = h
			first = false
			stats.totalHash++
		}
		forw := be.Uint32(blk[0:])
		if forw == 0 {
			break
		}
		cur = forw
	}

	// Free-index blocks: walk freeLog upward, validating they tile [0, D).
	wantFirst := uint32(0)
	for lo := freeLog; ; lo++ {
		abs, ok := freeAbs[lo]
		if !ok {
			break
		}
		blk, err := readRawBlock(fs.f, fs.partOffset, sb, abs)
		if err != nil {
			t.Fatalf("read free block %d: %v", abs, err)
		}
		if m := be.Uint32(blk[0:]); m != magicDir3Free {
			t.Fatalf("free block %d magic 0x%08X != XDF3", abs, m)
		}
		verifyV5CRC(t, "free", blk, 4)
		if got := be.Uint32(blk[48:]); got != wantFirst {
			t.Fatalf("free block %d firstdb %d, want %d (windows must tile)", abs, got, wantFirst)
		}
		nvalid := be.Uint32(blk[52:])
		wantFirst += nvalid
		stats.freeBlocks++
	}
	return stats
}

// TestNodeDirMultiLevel forces a 2-level da-node btree (K > maxNodeEnts leafN
// blocks) and validates the on-disk structure the way xfs_repair would. At
// blockSize 4096, maxNodeEnts = perLeaf = 504, so K > 504 requires > 504*504 ≈
// 254k entries. Short names keep the entries-per-data-block high so the entry
// count (not the data-block count) is what drives the tree deep.
func TestNodeDirMultiLevel(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-level node dir builds ~255k entries; skipped in -short")
	}
	// 504*504 + 50: just past the 2-level threshold so the last interior node
	// would, under naive greedy packing, hold a single child — the exact shape
	// that made xfs_repair report "bad forward block pointer". The even-split
	// distribution must avoid it; the min-fill check in walkDirNode asserts so.
	const n = 504*504 + 50
	fs, _, _ := bulkLinkDir(t, 64<<20, n, "l-")

	if form := inspectDirForm(t, fs, fs.sb.rootIno); form != formNode {
		t.Fatalf("root form = %s, want node", form)
	}
	stats := walkDirNode(t, fs, fs.sb.rootIno)
	if stats.nodeLevels < 2 {
		t.Fatalf("da-node tree height = %d node levels, want >= 2 (multi-level)", stats.nodeLevels)
	}
	// The bottom internal level (level 1) must contain more than one node, all
	// linked by the forw/back sibling chain — the invariant whose absence made
	// xfs_repair report "bad magic number 0 in directory block 0".
	if len(stats.nodeSiblingsByLevel) == 0 ||
		stats.nodeSiblingsByLevel[len(stats.nodeSiblingsByLevel)-1] < 2 {
		t.Fatalf("level-1 internal-node sibling chain = %v, want last level >= 2 linked nodes",
			stats.nodeSiblingsByLevel)
	}
	if stats.totalHash != n+2 { // + "." and ".."
		t.Fatalf("hash index entries = %d, want %d (n + dot + dotdot)", stats.totalHash, n+2)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("ListDir returned %d entries, want %d", len(entries), n)
	}
}

// TestNodeDirMultiFreeIndex forces a multi-BLOCK free index (D > bestsPerFree
// data blocks) using long entry names: at blockSize 4096 the bests array holds
// bestsPerFree = (4096-64)/2 = 2016 entries, so the directory must span > 2016
// data blocks. Long names (~64 bytes/entry → ~56 entries/block) reach > 2016
// data blocks at ~120k entries while keeping the da-node tree shallow.
func TestNodeDirMultiFreeIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-block free-index dir builds ~120k long-named entries; skipped in -short")
	}
	const n = 120000
	// 56-char prefix → ~64-byte entries → ~56 per 4 KiB block → > 2016 blocks.
	prefix := "freeindex-test-padding-padding-padding-padding-padding-"
	fs, _, _ := bulkLinkDir(t, 64<<20, n, prefix)

	if form := inspectDirForm(t, fs, fs.sb.rootIno); form != formNode {
		t.Fatalf("root form = %s, want node", form)
	}
	stats := walkDirNode(t, fs, fs.sb.rootIno)
	if stats.freeBlocks < 2 {
		t.Fatalf("free-index blocks = %d, want >= 2 (multi-block free index)", stats.freeBlocks)
	}
	if stats.totalHash != n+2 { // + "." and ".."
		t.Fatalf("hash index entries = %d, want %d (n + dot + dotdot)", stats.totalHash, n+2)
	}
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != n {
		t.Fatalf("ListDir returned %d entries, want %d", len(entries), n)
	}
}
