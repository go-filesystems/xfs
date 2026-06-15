package filesystem_xfs

// dirleaf_test.go — tests for leaf-form and node-form directory writing.
//
// These exercise the writer end-to-end through the public API (Format +
// WriteFile/ListDir/ReadFile/DeleteFile, including a close/re-open cycle) and
// also check the on-disk shapes directly: which directory form a populated
// directory ends up in, and the internal consistency of the leaf-index, free
// and da-btree blocks the writer emits.

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"path/filepath"
	"sort"
	"testing"
)

// dirForm describes the on-disk form a directory inode is in.
type dirForm int

const (
	formShort dirForm = iota
	formBlock
	formLeaf
	formNode
)

func (f dirForm) String() string {
	switch f {
	case formShort:
		return "short"
	case formBlock:
		return "block"
	case formLeaf:
		return "leaf"
	case formNode:
		return "node"
	default:
		return "unknown"
	}
}

// inspectDirForm reads directory inode `ino` and classifies its on-disk form by
// examining its extents and (for extent-form dirs) the block that begins the
// leaf address space.
func inspectDirForm(t *testing.T, fs *xfsFS, ino uint64) dirForm {
	t.Helper()
	in, err := readInode(fs.f, fs.partOffset, fs.sb, ino)
	if err != nil {
		t.Fatalf("readInode %d: %v", ino, err)
	}
	if in.format == inodeFmtLocal {
		return formShort
	}
	exts, err := dirExtents(fs.f, fs.partOffset, fs.sb, in)
	if err != nil {
		t.Fatalf("dirExtents %d: %v", ino, err)
	}
	leafLog := dirLeafLogBlock(fs.sb)
	var leafStart uint64
	hasLeaf := false
	for _, e := range exts {
		if e.startOff >= leafLog && (!hasLeaf || e.startOff < leafStart) {
			leafStart, hasLeaf = e.startBlock, true
		}
	}
	if !hasLeaf {
		return formBlock
	}
	blk, err := readRawBlock(fs.f, fs.partOffset, fs.sb, leafStart)
	if err != nil {
		t.Fatalf("readRawBlock leaf %d: %v", leafStart, err)
	}
	switch magic := binary.BigEndian.Uint16(blk[8:]); magic {
	case magicDir3Leaf1, magicDir2Leaf1:
		return formLeaf
	case magicDa3Node, magicDaNode:
		return formNode
	default:
		t.Fatalf("leaf-space block has unexpected magic 0x%04X", magic)
		return formShort
	}
}

// makeDirFS formats a fresh image sized for `ags` allocation groups and returns
// the open filesystem plus its on-disk path (for re-open tests). Closed on
// cleanup if still open.
func makeDirFS(t *testing.T, ags int) (*xfsFS, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dir.img")
	fsIfc, err := Format(path, xfsTestSize*int64(ags), FormatConfig{Label: "dirtest"})
	if err != nil {
		t.Fatalf("Format(%d AGs): %v", ags, err)
	}
	t.Cleanup(func() { _ = fsIfc.Close() })
	return fsIfc.(*xfsFS), path
}

// assertDirContents lists "/" and reads every file back, checking that exactly
// the expected name→body set is present.
func assertDirContents(t *testing.T, fs *xfsFS, want map[string]string) {
	t.Helper()
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir /: %v", err)
	}
	if len(entries) != len(want) {
		t.Fatalf("ListDir /: got %d entries, want %d", len(entries), len(want))
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		body, ok := want[name]
		if !ok {
			t.Fatalf("unexpected entry %q", name)
		}
		if seen[name] {
			t.Fatalf("duplicate entry %q", name)
		}
		seen[name] = true
		got, err := fs.ReadFile("/" + name)
		if err != nil {
			t.Fatalf("ReadFile /%s: %v", name, err)
		}
		if string(got) != body {
			t.Fatalf("/%s: got %q want %q", name, got, body)
		}
	}
}

// runDirRoundTrip writes `n` files into the root, asserts the directory reaches
// `wantForm`, verifies listing+reads, re-opens the image and re-verifies, then
// deletes everything and checks the directory ends up empty. When `emptyFiles`
// is set the files carry no data (inode-only), which keeps the data-block
// allocator from churning its free-space B-trees for very large directories —
// isolating the directory machinery under test from that separate limit.
func runDirRoundTrip(t *testing.T, ags, n int, wantForm dirForm, emptyFiles bool) {
	t.Helper()
	fs, path := makeDirFS(t, ags)

	want := map[string]string{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/f-%06d.dat", i)
		body := ""
		if !emptyFiles {
			body = fmt.Sprintf("body-of-file-%d", i)
		}
		if err := fs.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
		want[name[1:]] = body
	}

	if form := inspectDirForm(t, fs, fs.sb.rootIno); form != wantForm {
		t.Fatalf("root directory form = %s, want %s", form, wantForm)
	}
	assertDirContents(t, fs, want)

	// Persist and re-open: the on-disk layout alone must round-trip.
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopenedIfc, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open(reopen): %v", err)
	}
	reopened := reopenedIfc.(*xfsFS)
	if form := inspectDirForm(t, reopened, reopened.sb.rootIno); form != wantForm {
		t.Fatalf("re-opened root form = %s, want %s", form, wantForm)
	}
	assertDirContents(t, reopened, want)

	// Delete every file; the directory must end up empty and walkable.
	for name := range want {
		if err := reopened.DeleteFile("/" + name); err != nil {
			t.Fatalf("DeleteFile /%s: %v", name, err)
		}
	}
	entries, err := reopened.ListDir("/")
	if err != nil {
		t.Fatalf("post-delete ListDir /: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("post-delete ListDir /: %d entries left, want 0", len(entries))
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close(reopen): %v", err)
	}
}

// TestLeafFormDirectory pushes the root past single-block capacity into leaf
// form (~300 entries: > ~124 block-form cap, < ~500 single-leaf cap).
func TestLeafFormDirectory(t *testing.T) {
	runDirRoundTrip(t, 2, 300, formLeaf, false)
}

// TestNodeFormDirectory pushes the root past single-leaf capacity into node
// form (the hash index no longer fits one leaf block, so it splits across
// leafn blocks indexed by a da-btree node, with free blocks for the bests).
func TestNodeFormDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("node-form directory test creates thousands of files; skipped in -short")
	}
	runDirRoundTrip(t, 4, 1500, formNode, true)
}

// TestDirFormProgression watches a single directory climb sf → block → leaf as
// entries are added one at a time, then descend back as they are removed.
func TestDirFormProgression(t *testing.T) {
	fs, _ := makeDirFS(t, 2)
	const n = 320
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/p-%05d", i)
		if err := fs.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	if form := inspectDirForm(t, fs, fs.sb.rootIno); form != formLeaf {
		t.Fatalf("after %d adds form = %s, want leaf", n, form)
	}
	// Remove most entries; the directory must collapse back to block form and
	// still list the survivors correctly.
	want := map[string]string{}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/p-%05d", i)
		if i%40 == 0 { // keep 8 entries
			want[name[1:]] = "x"
			continue
		}
		if err := fs.DeleteFile(name); err != nil {
			t.Fatalf("DeleteFile %s: %v", name, err)
		}
	}
	if form := inspectDirForm(t, fs, fs.sb.rootIno); form != formBlock {
		t.Fatalf("after deletions form = %s, want block", form)
	}
	assertDirContents(t, fs, want)
}

// ──────────────────── structural unit tests ────────────────────────────────

// testSB returns a v5 (CRC + ftype) superblock matching what Format() emits.
func testSBv5() *superblock {
	return &superblock{
		blockSize: 4096,
		hasCRC:    true,
		hasFType:  true,
		dirBlkLog: 0,
	}
}

// mkEntries builds n synthetic directory entries with distinct names + inodes.
func mkEntries(n int) []dirEnt {
	out := make([]dirEnt, n)
	for i := 0; i < n; i++ {
		out[i] = dirEnt{name: fmt.Sprintf("entry-%05d", i), ino: uint64(1000 + i), ftype: 1}
	}
	return out
}

// TestPlaceDirEntries checks data-block packing: every entry lands within a
// block, free lengths are 0 or ≥ 8, and the block count is minimal.
func TestPlaceDirEntries(t *testing.T) {
	sb := testSBv5()
	all := append([]dirEnt{{".", 1, 2}, {"..", 1, 2}}, mkEntries(200)...)
	hdr := dirDataHdrSize(sb.hasCRC)
	placed, nData, freeLen := placeDirEntries(all, hdr, int(sb.blockSize), sb.hasFType)
	if len(placed) != len(all) {
		t.Fatalf("placed %d, want %d", len(placed), len(all))
	}
	if len(freeLen) != nData {
		t.Fatalf("freeLen len %d, want nData %d", len(freeLen), nData)
	}
	for i, p := range placed {
		if p.db < 0 || p.db >= nData {
			t.Fatalf("entry %d placed in block %d (nData %d)", i, p.db, nData)
		}
		end := p.off + dirEntrySize(len(all[i].name), sb.hasFType)
		if p.off < hdr || end > int(sb.blockSize) {
			t.Fatalf("entry %d at [%d,%d) escapes block bounds", i, p.off, end)
		}
	}
	for db, fl := range freeLen {
		if fl != 0 && fl < 8 {
			t.Fatalf("block %d free length %d is neither 0 nor ≥ 8", db, fl)
		}
	}
}

// TestBuildLeaf1Block verifies the leaf-index block: header magic/owner/count,
// a hash-sorted index, and a tail whose bestcount + bests array match the data
// blocks. It also round-trips every entry back through the data-block parser.
func TestBuildLeaf1Block(t *testing.T) {
	sb := testSBv5()
	const owner, dataAbs, leafAbs = 128, 64, 80
	all := append([]dirEnt{{".", owner, 2}, {"..", owner, 2}}, mkEntries(150)...)

	data, leaves, bests := buildDirDataBlocks(sb, owner, dataAbs, all)
	blkSize := int(sb.blockSize)
	nData := len(data) / blkSize
	leaf := buildLeaf1Block(sb, owner, leafAbs, leaves, bests)

	be := binary.BigEndian
	if got := be.Uint16(leaf[8:]); got != magicDir3Leaf1 {
		t.Fatalf("leaf magic 0x%04X, want 0x%04X", got, magicDir3Leaf1)
	}
	if got := be.Uint64(leaf[48:]); got != owner {
		t.Fatalf("leaf owner %d, want %d", got, owner)
	}
	if got := be.Uint16(leaf[56:]); int(got) != len(all) {
		t.Fatalf("leaf count %d, want %d", got, len(all))
	}
	// Tail bestcount must equal the data-block count.
	if got := be.Uint32(leaf[blkSize-4:]); int(got) != nData {
		t.Fatalf("tail bestcount %d, want nData %d", got, nData)
	}
	// bests[i] must match each data block's recomputed best-free length.
	bestsOff := blkSize - 4 - nData*2
	for db := 0; db < nData; db++ {
		blk := data[db*blkSize : (db+1)*blkSize]
		wantBest := recomputeBestFree(blk, sb)
		if got := be.Uint16(leaf[bestsOff+db*2:]); int(got) != wantBest {
			t.Fatalf("bests[%d] = %d, want %d", db, got, wantBest)
		}
	}
	// Leaf index must be sorted by (hash, addr).
	hdrSize := dirLeafHdrSize(sb.hasCRC)
	var prevHash, prevAddr uint32
	for i := 0; i < len(all); i++ {
		h := be.Uint32(leaf[hdrSize+i*8:])
		a := be.Uint32(leaf[hdrSize+i*8+4:])
		if i > 0 && (h < prevHash || (h == prevHash && a < prevAddr)) {
			t.Fatalf("leaf index not sorted at %d: (%08x,%08x) after (%08x,%08x)", i, h, a, prevHash, prevAddr)
		}
		prevHash, prevAddr = h, a
	}
	// Every entry must be recoverable by scanning the data blocks.
	gotNames := map[string]bool{}
	for db := 0; db < nData; db++ {
		for _, de := range parseDirBlock(data[db*blkSize:(db+1)*blkSize], sb.hasFType, sb.hasCRC) {
			gotNames[de.Name] = true
		}
	}
	for _, e := range all {
		if e.name == "." || e.name == ".." {
			continue
		}
		if !gotNames[e.name] {
			t.Fatalf("entry %q not found in data blocks", e.name)
		}
	}
}

// recomputeBestFree returns the largest free-region length in a data block by
// scanning it independently of the bestfree header — the check xfs_repair does.
func recomputeBestFree(blk []byte, sb *superblock) int {
	be := binary.BigEndian
	off := dirDataHdrSize(sb.hasCRC)
	best := 0
	for off+4 <= len(blk) {
		if be.Uint16(blk[off:]) == dirFreeTag {
			length := int(be.Uint16(blk[off+2:]))
			if length < 8 {
				break
			}
			if length > best {
				best = length
			}
			off += length
			continue
		}
		ino := be.Uint64(blk[off:])
		if ino == 0 {
			break
		}
		namelen := int(blk[off+8])
		if namelen == 0 {
			break
		}
		off += dirEntrySize(namelen, sb.hasFType)
	}
	return best
}

// TestBuildDaNodeAndFreeBlocks checks the node-form metadata blocks: the
// da-btree node is level 1 with hash-sorted child pointers, the leafn children
// hold the full sorted index, and the free block's bests mirror the data blocks.
func TestBuildDaNodeAndFreeBlocks(t *testing.T) {
	sb := testSBv5()
	const owner, dataAbs = 128, 64
	all := append([]dirEnt{{".", owner, 2}, {"..", owner, 2}}, mkEntries(900)...)
	data, leaves, bests := buildDirDataBlocks(sb, owner, dataAbs, all)
	sortLeafIndex(leaves)

	blkSize := int(sb.blockSize)
	nData := len(data) / blkSize
	leafnCap := (blkSize - dirLeafHdrSize(sb.hasCRC)) / 8
	nLeaf := (len(leaves) + leafnCap - 1) / leafnCap
	if nLeaf < 2 {
		nLeaf = 2
	}
	per := (len(leaves) + nLeaf - 1) / nLeaf
	leafLog := dirLeafLogBlock(sb)

	be := binary.BigEndian
	nodeEnts := make([]nodeBtreeEnt, 0, nLeaf)
	var indexTotal int
	for i := 0; i < nLeaf; i++ {
		lo, hi := i*per, (i+1)*per
		if hi > len(leaves) {
			hi = len(leaves)
		}
		slice := leaves[lo:hi]
		blk := make([]byte, blkSize)
		buildLeafnBlock(sb, blk, uint64(100+i), owner, 0, 0, slice)
		if got := be.Uint16(blk[8:]); got != magicDir3Leafn {
			t.Fatalf("leafn[%d] magic 0x%04X, want 0x%04X", i, got, magicDir3Leafn)
		}
		if got := be.Uint16(blk[56:]); int(got) != len(slice) {
			t.Fatalf("leafn[%d] count %d, want %d", i, got, len(slice))
		}
		indexTotal += len(slice)
		nodeEnts = append(nodeEnts, nodeBtreeEnt{hash: slice[len(slice)-1].hash, before: uint32(leafLog + 1 + uint64(i))})
	}
	if indexTotal != len(leaves) {
		t.Fatalf("leafn blocks hold %d index entries, want %d", indexTotal, len(leaves))
	}

	node := buildDaNodeBlock(sb, 99, owner, nodeEnts)
	if got := be.Uint16(node[8:]); got != magicDa3Node {
		t.Fatalf("node magic 0x%04X, want 0x%04X", got, magicDa3Node)
	}
	if got := be.Uint16(node[56:]); int(got) != nLeaf {
		t.Fatalf("node count %d, want %d", got, nLeaf)
	}
	if got := be.Uint16(node[58:]); got != 1 {
		t.Fatalf("node level %d, want 1", got)
	}
	// Node child hashes must be ascending.
	nodeHdr := dirNodeHdrSize(sb.hasCRC)
	prev := uint32(0)
	for i := 0; i < nLeaf; i++ {
		h := be.Uint32(node[nodeHdr+i*8:])
		if i > 0 && h < prev {
			t.Fatalf("node child hashes not ascending at %d: %08x < %08x", i, h, prev)
		}
		prev = h
	}

	// Free block: header counts + bests array mirror the data blocks.
	freeCap := (blkSize - dirFreeHdrSize(sb.hasCRC)) / 2
	free := buildFreeBlocks(sb, 200, owner, bests, nData, 1, freeCap)
	if got := be.Uint32(free[0:]); got != magicDir3Free {
		t.Fatalf("free magic 0x%08X, want 0x%08X", got, magicDir3Free)
	}
	if got := be.Uint32(free[52:]); int(got) != nData {
		t.Fatalf("free nvalid %d, want %d", got, nData)
	}
	freeHdr := dirFreeHdrSize(sb.hasCRC)
	for db := 0; db < nData; db++ {
		if got := be.Uint16(free[freeHdr+db*2:]); int(got) != bests[db] {
			t.Fatalf("free bests[%d] = %d, want %d", db, got, bests[db])
		}
	}
}

// TestDirHashKnownValues pins the directory name hash against values verified
// against mkfs (so the leaf index keys stay correct).
func TestDirHashKnownValues(t *testing.T) {
	cases := map[string]uint32{".": 0x2e, "..": 0x172e}
	for name, want := range cases {
		if got := xfsDirHash([]byte(name)); got != want {
			t.Fatalf("xfsDirHash(%q) = 0x%x, want 0x%x", name, got, want)
		}
	}
	// Sanity: sorting many real hashes is a total order (no panics, stable).
	hs := make([]uint32, 0, 500)
	for i := 0; i < 500; i++ {
		hs = append(hs, xfsDirHash([]byte(fmt.Sprintf("file-%d", i))))
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i] < hs[j] })
}

// verifyV5CRC recomputes the CRC32C of a directory metadata block (with the CRC
// field zeroed) and checks it matches the stored value — the same check the v5
// verifiers and xfs_repair apply. crcOff is the byte offset of the __le32 CRC.
func verifyV5CRC(t *testing.T, what string, blk []byte, crcOff int) {
	t.Helper()
	stored := binary.LittleEndian.Uint32(blk[crcOff:])
	tmp := make([]byte, len(blk))
	copy(tmp, blk)
	binary.LittleEndian.PutUint32(tmp[crcOff:], 0)
	got := crc32.Checksum(tmp, crc32.MakeTable(crc32.Castagnoli))
	if got != stored {
		t.Fatalf("%s: CRC mismatch: stored %08x, recomputed %08x", what, stored, got)
	}
}

// TestLeafFormOnDiskHeaders is a local xfs_repair-lite: it builds a real
// leaf-form directory, reads the data and leaf blocks straight off the image,
// and validates the v5 header invariants that xfs_repair enforces — magic,
// CRC, self-referential blkno, and owning inode — plus that every leaf-index
// address resolves to a live directory entry whose name hash equals the index
// key. This catches v5-header and hash-index mistakes without needing xfsprogs.
func TestLeafFormOnDiskHeaders(t *testing.T) {
	const n = 300
	fs, _ := makeDirFS(t, 2)
	for i := 0; i < n; i++ {
		if err := fs.WriteFile(fmt.Sprintf("/h-%05d", i), []byte("y"), 0o644); err != nil {
			t.Fatalf("WriteFile %d: %v", i, err)
		}
	}

	in, err := readInode(fs.f, fs.partOffset, fs.sb, fs.sb.rootIno)
	if err != nil {
		t.Fatalf("readInode root: %v", err)
	}
	exts, err := dirExtents(fs.f, fs.partOffset, fs.sb, in)
	if err != nil {
		t.Fatalf("dirExtents: %v", err)
	}
	leafLog := dirLeafLogBlock(fs.sb)
	blkSize := int(fs.sb.blockSize)
	be := binary.BigEndian

	// Map logical data-block index -> on-disk block bytes, and find the leaf.
	dataBlocks := map[int][]byte{}
	var leafBlk []byte
	nData := 0
	for _, e := range exts {
		for b := uint32(0); b < e.count; b++ {
			abs := e.startBlock + uint64(b)
			blk, err := readRawBlock(fs.f, fs.partOffset, fs.sb, abs)
			if err != nil {
				t.Fatalf("readRawBlock %d: %v", abs, err)
			}
			if e.startOff >= leafLog {
				leafBlk = blk
				// Leaf header: magic@8, crc@12, blkno@16, owner@48.
				if m := be.Uint16(blk[8:]); m != magicDir3Leaf1 {
					t.Fatalf("leaf magic 0x%04X, want 0x%04X", m, magicDir3Leaf1)
				}
				verifyV5CRC(t, "leaf", blk, 12)
				if got := be.Uint64(blk[16:]); got != abs*fmtDaddrPerBlock {
					t.Fatalf("leaf blkno %d, want %d", got, abs*fmtDaddrPerBlock)
				}
				if got := be.Uint64(blk[48:]); got != fs.sb.rootIno {
					t.Fatalf("leaf owner %d, want %d", got, fs.sb.rootIno)
				}
				continue
			}
			// Data block: magic@0, crc@4, blkno@8, owner@40.
			if m := be.Uint32(blk[0:]); m != magicDir3Data {
				t.Fatalf("data block %d magic 0x%08X, want 0x%08X", e.startOff+uint64(b), m, magicDir3Data)
			}
			verifyV5CRC(t, "data", blk, 4)
			if got := be.Uint64(blk[8:]); got != abs*fmtDaddrPerBlock {
				t.Fatalf("data blkno %d, want %d", got, abs*fmtDaddrPerBlock)
			}
			if got := be.Uint64(blk[40:]); got != fs.sb.rootIno {
				t.Fatalf("data owner %d, want %d", got, fs.sb.rootIno)
			}
			dataBlocks[int(e.startOff+uint64(b))] = blk
			nData++
		}
	}
	if leafBlk == nil {
		t.Fatal("no leaf block found; directory is not in leaf form")
	}

	// Tail bestcount must equal the data-block count.
	if got := be.Uint32(leafBlk[blkSize-4:]); int(got) != nData {
		t.Fatalf("leaf tail bestcount %d, want %d data blocks", got, nData)
	}

	// Every leaf-index entry must resolve to a live entry with a matching hash.
	count := int(be.Uint16(leafBlk[56:]))
	hdrSize := dirLeafHdrSize(fs.sb.hasCRC)
	for i := 0; i < count; i++ {
		hash := be.Uint32(leafBlk[hdrSize+i*8:])
		addr := be.Uint32(leafBlk[hdrSize+i*8+4:])
		byteOff := uint64(addr) << dirDataAlignLog
		db := int(byteOff / uint64(blkSize))
		off := int(byteOff % uint64(blkSize))
		blk, ok := dataBlocks[db]
		if !ok {
			t.Fatalf("leaf entry %d addr points at missing data block %d", i, db)
		}
		if off+9 > len(blk) {
			t.Fatalf("leaf entry %d addr offset %d out of range", i, off)
		}
		namelen := int(blk[off+8])
		name := string(blk[off+9 : off+9+namelen])
		if got := xfsDirHash([]byte(name)); got != hash {
			t.Fatalf("leaf entry %d: name %q hash %08x != index key %08x", i, name, got, hash)
		}
	}
}
