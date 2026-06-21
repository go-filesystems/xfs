package filesystem_xfs

// leafdir_roundtrip_test.go — always-on (no-xfsprogs) coverage for leaf-form
// and node-form directories.
//
// The xfs_repair interop tests in test/xfsprogs_compat_test.go validate the
// on-disk layout against the canonical tools, but they are skip-gated: on a
// host without xfsprogs (the common CI / dev case) nothing exercises a
// promoted directory at all. These pure-Go tests fill that gap. They drive the
// public API through a close/re-open cycle, assert which on-disk form a
// directory lands in, and — without shelling out — re-read the written blocks
// to check the v5 header invariants and hash index the way xfs_repair would.

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"path/filepath"
	"testing"
)

// dirForm is the on-disk shape of a directory inode.
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

// inspectDirForm classifies directory inode `ino` by its extents and, for
// extent-form directories, the block that begins the leaf address space (the
// 32 GiB logical offset): a leaf1 block (0x3df1) means leaf form, a da3-node
// (0x3ebe) means node form, and no leaf-space extent at all means block form.
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
	leafLog := dirLeafByteOffset / uint64(fs.sb.blockSize)
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
	case magicDir3Leaf1:
		return formLeaf
	case magicDa3Node:
		return formNode
	default:
		t.Fatalf("leaf-space block has unexpected magic 0x%04X", magic)
		return formShort
	}
}

// makeDirFS formats a fresh image sized for `ags` allocation groups and returns
// the open filesystem plus its on-disk path. Closed on cleanup if still open.
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
// is set the files carry no data (inode-only), keeping the data-block allocator
// out of the picture for very large directories.
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

// TestLeafFormDirRoundTrip pushes the root past single-block capacity into leaf
// form (~300 entries: past the ~124 block-form cap, within a single leaf).
func TestLeafFormDirRoundTrip(t *testing.T) {
	runDirRoundTrip(t, 2, 300, formLeaf, false)
}

// TestNodeFormDirRoundTrip pushes the root past single-leaf capacity into node
// form (the hash index spills across leafN blocks under a da-btree node, with a
// free-index block for the bests). Files are empty to keep the run fast.
func TestNodeFormDirRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("node-form round-trip creates thousands of files; skipped in -short")
	}
	runDirRoundTrip(t, 4, 1500, formNode, true)
}

// TestDirFormProgression watches a directory climb block → leaf as entries are
// added, then collapse back to block form as they are removed.
func TestDirFormProgression(t *testing.T) {
	fs, _ := makeDirFS(t, 2)
	const n = 320
	for i := 0; i < n; i++ {
		if err := fs.WriteFile(fmt.Sprintf("/p-%05d", i), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %d: %v", i, err)
		}
	}
	if form := inspectDirForm(t, fs, fs.sb.rootIno); form != formLeaf {
		t.Fatalf("after %d adds form = %s, want leaf", n, form)
	}
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

// verifyV5CRC recomputes the CRC32C of a directory metadata block (with the CRC
// field zeroed) and checks it matches the stored value — the check the v5
// verifiers and xfs_repair apply. crcOff is the byte offset of the __le32 CRC.
func verifyV5CRC(t *testing.T, what string, blk []byte, crcOff int) {
	t.Helper()
	stored := binary.LittleEndian.Uint32(blk[crcOff:])
	tmp := make([]byte, len(blk))
	copy(tmp, blk)
	binary.LittleEndian.PutUint32(tmp[crcOff:], 0)
	if got := crc32.Checksum(tmp, crc32.MakeTable(crc32.Castagnoli)); got != stored {
		t.Fatalf("%s: CRC mismatch: stored %08x, recomputed %08x", what, stored, got)
	}
}

// TestLeafFormOnDiskHeaders is a local xfs_repair-lite: it builds a real
// leaf-form directory, reads the data and leaf blocks straight off the image,
// and validates the v5 header invariants xfs_repair enforces — magic, CRC,
// self-referential blkno and owning inode — plus that every leaf-index address
// resolves to a live entry whose name hash equals the index key. This catches
// v5-header and hash-index regressions on hosts without xfsprogs.
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
	leafLog := dirLeafByteOffset / uint64(fs.sb.blockSize)
	blkSize := int(fs.sb.blockSize)
	be := binary.BigEndian

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
				if m := be.Uint16(blk[8:]); m != magicDir3Leaf1 {
					t.Fatalf("leaf magic 0x%04X, want 0x%04X", m, magicDir3Leaf1)
				}
				verifyV5CRC(t, "leaf", blk, 12) // da3_blkinfo.crc @ 12
				if got := be.Uint64(blk[16:]); got != abs*fmtDaddrPerBlock {
					t.Fatalf("leaf blkno %d, want %d", got, abs*fmtDaddrPerBlock)
				}
				if got := be.Uint64(blk[48:]); got != fs.sb.rootIno {
					t.Fatalf("leaf owner %d, want %d", got, fs.sb.rootIno)
				}
				continue
			}
			if m := be.Uint32(blk[0:]); m != magicDir3Data {
				t.Fatalf("data block %d magic 0x%08X, want 0x%08X", e.startOff+uint64(b), m, magicDir3Data)
			}
			verifyV5CRC(t, "data", blk, 4) // dir3_blk_hdr.crc @ 4
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
	const dirLeafHdr = 64  // xfs_dir3_leaf_hdr size (v5)
	const dataAlignLog = 3 // XFS_DIR2_DATA_ALIGN_LOG
	count := int(be.Uint16(leafBlk[56:]))
	for i := 0; i < count; i++ {
		hash := be.Uint32(leafBlk[dirLeafHdr+i*8:])
		addr := be.Uint32(leafBlk[dirLeafHdr+i*8+4:])
		byteOff := uint64(addr) << dataAlignLog
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
