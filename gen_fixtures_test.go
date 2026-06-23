package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// gen_fixtures_test.go — opt-in generators that emit XFS images exercising the
// large-directory / many-inode write paths, for offline validation with the
// real xfsprogs oracle (xfs_repair / kernel loop-mount) in a Linux VM. Set
// XFS_GEN_FIXTURES=<outdir> to run; otherwise these are skipped. They are NOT
// CI gates (the committed pure-Go structural tests are); they are the bridge
// used to prove the writer's output is xfs_repair-clean.

func genOutDir(t *testing.T) string {
	dir := os.Getenv("XFS_GEN_FIXTURES")
	if dir == "" {
		t.Skip("set XFS_GEN_FIXTURES=<outdir> to generate validation images")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestGenMultiLevelNodeDir writes node-multilevel.img: a root directory with a
// 2-level da-node hash btree (and, given the entry count, a multi-block free
// index too if D crosses 2016). Entries hardlink a single backing inode.
func TestGenMultiLevelNodeDir(t *testing.T) {
	dir := genOutDir(t)
	out := filepath.Join(dir, "node-multilevel.img")
	// 504*504 + 50 produces the awkward "last interior node would hold one
	// child" shape that the even-split distribution must smooth out.
	writeBulkLinkImage(t, out, 504*504+50, "l")
}

// TestGenMultiFreeIndexDir writes node-multifree.img: a root directory whose
// long-named entries span > 2016 data blocks, forcing a multi-block free index.
func TestGenMultiFreeIndexDir(t *testing.T) {
	dir := genOutDir(t)
	out := filepath.Join(dir, "node-multifree.img")
	writeBulkLinkImage(t, out, 120000,
		"freeindex-test-padding-padding-padding-padding-padding-")
}

// TestGenInobtSplitImage writes inobt-split.img: enough inode chunks in AG 0 to
// fill the single inobt leaf root and force a leaf-split to a depth-2 inobt. It
// drives growInobt directly to add fresh 64-inode chunks (all inodes left FREE,
// which xfs_repair accepts) until the AGI depth grows past 1 — far cheaper than
// creating tens of thousands of real files, while exercising exactly the split
// path. The root directory and its inode chunk stay untouched.
func TestGenInobtSplitImage(t *testing.T) {
	dir := genOutDir(t)
	out := filepath.Join(dir, "inobt-split.img")

	fsIfc, err := Format(out, 512<<20, FormatConfig{Label: "inobtsplit"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsIfc.(*xfsFS)

	// The single leaf root holds (blockSize-hdr)/inobtRecSize records (= chunks).
	// Add a few past that ceiling to guarantee a split, then a margin more so
	// the depth-2 tree carries records in both leaves.
	hdr := fs.sb.agBTreeHdrSize()
	maxChunks := (int(fs.sb.blockSize) - hdr) / inobtRecSize
	target := maxChunks + 8 // a handful past the single-leaf ceiling forces a split
	level := uint32(1)
	added := 0
	for i := 0; i < target && level < 2; i++ {
		if err := growInobt(fs.f, fs.partOffset, fs.sb, 0); err != nil {
			t.Fatalf("growInobt #%d: %v", i, err)
		}
		added++
		agi, err := agiBlock(fs.f, fs.partOffset, fs.sb, 0)
		if err != nil {
			t.Fatalf("read AGI: %v", err)
		}
		level = binary.BigEndian.Uint32(agi[agiOffLevel:])
	}
	target = added
	if err := syncSuperblockCounts(fs.f, fs.partOffset, fs.sb); err != nil {
		t.Fatalf("sync counts: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	t.Logf("inobt-split.img: AG0 inobt depth=%d after %d chunks (want >=2)", level, target)
	if level < 2 {
		t.Fatalf("inobt did not split: AG0 depth=%d", level)
	}
}

// TestGenInobtDeepImage writes inobt-deep.img: it drives growInobt well past
// the first leaf split so the AG-0 inobt is already depth 2 *with multiple
// interior records*, exercising the general record-insert path that splits a
// leaf under an existing interior node and propagates the new key/ptr up the
// existing tree. Every added inode is left FREE (xfs_repair accepts free
// chunks), so this is far cheaper than creating millions of real files while
// exercising exactly the deep-insert path on the real oracle.
//
// Bounded sub-case: this fixture stops at a wide depth-2 tree rather than a
// depth-3 root. Reaching depth 3 here would need ~maxNode×maxChunks ≈ 127k free
// chunks, but the writer's deliberately-simple bno/cnt free-space allocator is
// single-leaf (no free-space-tree split) and overflows around ~540 chunks once
// the inobt's own block allocations fragment the free space — a *separate*
// not-implemented boundary (free-space-btree split). The depth-2→3 growth code
// itself is fully exercised by the unit test TestInobtDeepInsert
// ("interior root split grows tree to depth 3").
func TestGenInobtDeepImage(t *testing.T) {
	dir := genOutDir(t)
	out := filepath.Join(dir, "inobt-deep.img")

	// 2 GiB gives a big AG 0 so the free-space trees have room for the many
	// 8-block inode chunks plus the inobt's own leaf/interior blocks.
	fsIfc, err := Format(out, 2<<30, FormatConfig{Label: "inobtdeep"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsIfc.(*xfsFS)

	hdr := fs.sb.agBTreeHdrSize()
	maxChunks := (int(fs.sb.blockSize) - hdr) / inobtRecSize                // records per leaf
	maxNode := (int(fs.sb.blockSize) - hdr) / (inobtKeySize + inobtPtrSize) // ptrs per interior node

	// Target: enough chunks to put several records in the interior root (so the
	// split-leaf-then-insert-into-interior path runs many times). Each ~maxChunks
	// further inserts past the first split adds one interior record; 2 maxChunks
	// past the first split (≈3×maxChunks total) yields a depth-2 root with 3-4
	// children, well into the new deep-insert path. We cap below the point where
	// the simplistic bno/cnt free-space allocator (single-leaf, no split) would
	// itself overflow — extending that allocator is a separate boundary.
	target := maxChunks * 2
	level := uint32(1)
	added := 0
	for i := 0; i < target; i++ {
		if err := growInobt(fs.f, fs.partOffset, fs.sb, 0); err != nil {
			t.Fatalf("growInobt #%d: %v", i, err)
		}
		added++
		agi, err := agiBlock(fs.f, fs.partOffset, fs.sb, 0)
		if err != nil {
			t.Fatalf("read AGI: %v", err)
		}
		level = binary.BigEndian.Uint32(agi[agiOffLevel:])
	}

	// Inspect the interior root so the log records how deep/wide we got.
	agi, err := agiBlock(fs.f, fs.partOffset, fs.sb, 0)
	if err != nil {
		t.Fatalf("read AGI: %v", err)
	}
	rootRel := binary.BigEndian.Uint32(agi[agiOffRoot:])
	rootBlk, err := readAGBlock(fs.f, fs.partOffset, fs.sb, 0, rootRel)
	if err != nil {
		t.Fatalf("read inobt root: %v", err)
	}
	rootRecs := binary.BigEndian.Uint16(rootBlk[6:])

	if err := syncSuperblockCounts(fs.f, fs.partOffset, fs.sb); err != nil {
		t.Fatalf("sync counts: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	t.Logf("inobt-deep.img: AG0 inobt depth=%d, root holds %d records (maxNode=%d) after %d chunks",
		level, rootRecs, maxNode, added)
	if level < 2 || rootRecs < 3 {
		t.Fatalf("inobt not deep enough: depth=%d rootRecs=%d (want depth>=2 with >=3 interior records)", level, rootRecs)
	}
}

// writeBulkLinkImage produces an image whose root directory holds n entries
// (named namePrefix+index) all hardlinking one backing inode, then closes it.
func writeBulkLinkImage(t *testing.T, out string, n int, namePrefix string) {
	t.Helper()
	// 160 MiB yields multiple allocation groups, so xfs_repair validates the
	// geometry without needing -o force_geometry (it refuses single-AG images).
	fsIfc, err := Format(out, 160<<20, FormatConfig{Label: "nodegen"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := fsIfc.(*xfsFS)
	if err := fs.WriteFile("/target", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	targetIno, err := lookupInDir(fs.f, fs.partOffset, fs.sb,
		mustReadInode(t, fs, fs.sb.rootIno), "target")
	if err != nil {
		t.Fatalf("lookup target: %v", err)
	}
	entries := make([]dirEnt, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, dirEnt{fmt.Sprintf("%s%07d", namePrefix, i), targetIno, 1})
	}
	root := mustReadInode(t, fs, fs.sb.rootIno)
	if err := writeWholeDir(fs.f, fs.partOffset, fs.sb, root, fs.sb.rootIno, entries); err != nil {
		t.Fatalf("writeWholeDir: %v", err)
	}
	tin := mustReadInode(t, fs, targetIno)
	binary.BigEndian.PutUint32(tin.raw[inoOffNLink:], uint32(n))
	if err := writeInode(fs.f, fs.partOffset, fs.sb, tin); err != nil {
		t.Fatalf("writeInode nlink: %v", err)
	}
	if err := syncSuperblockCounts(fs.f, fs.partOffset, fs.sb); err != nil {
		t.Fatalf("sync counts: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	t.Logf("wrote %s (%d entries)", out, n)
}
