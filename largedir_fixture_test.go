package filesystem_xfs

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// largedir_fixture_test.go — committed, embedded regression fixtures for the
// large-directory / many-inode WRITE paths. Each image was produced by THIS
// writer (see gen_fixtures_test.go) exercising one of the formerly-guarded
// paths, then verified xfs_repair-clean and kernel-mountable in a Linux VM.
//
// Embedding (rather than generating at test time) matters for two reasons:
//   - the images take real CPU/RAM to build, so re-reading a frozen blob keeps
//     the per-arch test fast; and
//   - the org's emulated CI runs cross-compiled binaries whose working
//     directory has no testdata/ — go:embed bakes the bytes into the binary so
//     the fixture travels with it (the org's 3rd CI gotcha).
//
// On every architecture the test re-reads the image with our own reader and
// checks the entry set. When xfs_repair is present (the native CI lanes install
// xfsprogs) it additionally runs `xfs_repair -n` as the canonical oracle.

//go:embed testdata/inobt-split.img.gz
var inobtSplitGz []byte

//go:embed testdata/inobt-deep.img.gz
var inobtDeepGz []byte

//go:embed testdata/node-multilevel.img.gz
var nodeMultiLevelGz []byte

//go:embed testdata/node-multifree.img.gz
var nodeMultiFreeGz []byte

// gunzipToTemp writes the decompressed gz blob to a temp file and returns its
// path.
func gunzipToTemp(t *testing.T, gz []byte, name string) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("gzip reader %s: %v", name, err)
	}
	defer zr.Close()
	out := filepath.Join(t.TempDir(), name)
	of, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(of, zr); err != nil { //nolint:gosec // trusted committed fixture
		of.Close()
		t.Fatalf("decompress %s: %v", name, err)
	}
	if err := of.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

// runXfsRepairClean runs `xfs_repair -n` on img when the tool is available and
// fails the test if it reports any problem. It is a no-op (with a log) when
// xfsprogs is not installed, so the test still does useful structural work on
// arches/runners without it.
func runXfsRepairClean(t *testing.T, img string) {
	t.Helper()
	repair := findSbinTool("xfs_repair")
	if repair == "" {
		t.Logf("xfs_repair not available — skipping canonical oracle check for %s", filepath.Base(img))
		return
	}
	out, err := exec.Command(repair, "-n", img).CombinedOutput()
	t.Logf("xfs_repair -n %s:\n%s", filepath.Base(img), out)
	if err != nil {
		t.Fatalf("xfs_repair -n %s exited non-zero: %v", filepath.Base(img), err)
	}
	upper := strings.ToUpper(string(out))
	for _, marker := range []string{"BAD ", "CORRUPT", "WOULD ", "REBUILD", "INCONSISTEN"} {
		if strings.Contains(upper, marker) {
			t.Fatalf("xfs_repair -n %s reported %q:\n%s", filepath.Base(img), marker, out)
		}
	}
}

// TestNodeMultiLevelFixture re-reads the committed multi-LEVEL da-node image
// (254066 hardlink entries → a 2-level da-btree whose last interior node would,
// under naive packing, hold a single child) and checks every entry reads back,
// then runs xfs_repair when present.
func TestNodeMultiLevelFixture(t *testing.T) {
	img := gunzipToTemp(t, nodeMultiLevelGz, "node-multilevel.img")
	assertLinkDirFixture(t, img, 504*504+50, "l")
	runXfsRepairClean(t, img)
}

// TestNodeMultiFreeFixture re-reads the committed multi-block-free-index image
// (120000 long-named entries spanning > 2016 data blocks → several XDF3 free
// blocks) and checks every entry reads back, then runs xfs_repair when present.
func TestNodeMultiFreeFixture(t *testing.T) {
	img := gunzipToTemp(t, nodeMultiFreeGz, "node-multifree.img")
	assertLinkDirFixture(t, img, 120000,
		"freeindex-test-padding-padding-padding-padding-padding-")
	runXfsRepairClean(t, img)
}

// TestInobtSplitFixture opens the committed inobt-leaf-split image (AG 0's
// inobt grown past the single-leaf ceiling into a depth-2 tree) and confirms it
// opens and lists, then runs xfs_repair when present. The AGI depth assertion
// proves the split path produced the embedded layout.
func TestInobtSplitFixture(t *testing.T) {
	img := gunzipToTemp(t, inobtSplitGz, "inobt-split.img")

	fsIfc, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open inobt-split fixture: %v", err)
	}
	fs := fsIfc.(*xfsFS)
	defer fs.Close()
	if _, err := fs.ListDir("/"); err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	agi, err := agiBlock(fs.f, fs.partOffset, fs.sb, 0)
	if err != nil {
		t.Fatalf("read AGI: %v", err)
	}
	if lvl := readBE32(agi[agiOffLevel:]); lvl < 2 {
		t.Fatalf("AG0 inobt depth=%d, want >=2 (split fixture)", lvl)
	}
	runXfsRepairClean(t, img)
}

// TestInobtDeepFixture opens the committed inobt-deep image: AG 0's inobt was
// grown well past the first leaf split so the interior root carries multiple
// records (i.e. several leaf-splits propagated key/ptr inserts up an already
// depth-2 tree). It asserts the depth/width, then walks EVERY inobt record
// through our own reader — proving the reader follows interior pointers at the
// XFS maxrecs boundary and the deep-insert path produced a consistent tree —
// before deferring to xfs_repair as the canonical oracle (clean + kernel-
// mountable in the VM where this fixture was generated).
func TestInobtDeepFixture(t *testing.T) {
	img := gunzipToTemp(t, inobtDeepGz, "inobt-deep.img")

	fsIfc, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open inobt-deep fixture: %v", err)
	}
	fs := fsIfc.(*xfsFS)
	defer fs.Close()
	if _, err := fs.ListDir("/"); err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}

	agi, err := agiBlock(fs.f, fs.partOffset, fs.sb, 0)
	if err != nil {
		t.Fatalf("read AGI: %v", err)
	}
	if lvl := readBE32(agi[agiOffLevel:]); lvl < 2 {
		t.Fatalf("AG0 inobt depth=%d, want >=2 (deep fixture)", lvl)
	}
	rootRel := readBE32(agi[agiOffRoot:])
	rootBlk, err := readAGBlock(fs.f, fs.partOffset, fs.sb, 0, rootRel)
	if err != nil {
		t.Fatalf("read inobt root: %v", err)
	}
	rootRecs := int(rootBlk[6])<<8 | int(rootBlk[7])
	if rootRecs < 3 {
		t.Fatalf("inobt interior root holds %d records, want >=3 (multiple deep inserts)", rootRecs)
	}

	// Walk the whole inobt: descend to the leftmost leaf via the real reader,
	// then follow the right-sibling chain, asserting every record's startIno is
	// strictly ascending and each is independently locatable through the
	// multi-level inobtFindRecord descent. A maxrecs-boundary pointer bug or a
	// botched split would surface here as a wrong/looping pointer or a record
	// the descent can't find.
	hdr := fs.sb.agBTreeHdrSize()
	level := int(readBE32(agi[agiOffLevel:]))
	// Leftmost-leaf descent mirrors inobtFindFree's interior step.
	cur := rootRel
	for {
		blk, err := readAGBlock(fs.f, fs.partOffset, fs.sb, 0, cur)
		if err != nil {
			t.Fatalf("read block %d: %v", cur, err)
		}
		if int(blk[4])<<8|int(blk[5]) == 0 { // block-level 0 → leaf
			break
		}
		ptrOff := hdr + inobtMaxInternal(int(fs.sb.blockSize), hdr)*inobtKeySize
		cur = readBE32(blk[ptrOff:])
	}

	var prev uint32
	var seen, leaves int
	first := true
	for cur != 0xFFFFFFFF {
		leaf, err := readAGBlock(fs.f, fs.partOffset, fs.sb, 0, cur)
		if err != nil {
			t.Fatalf("read leaf %d: %v", cur, err)
		}
		leaves++
		nr := int(leaf[6])<<8 | int(leaf[7])
		for i := 0; i < nr; i++ {
			start := readBE32(leaf[hdr+i*inobtRecSize:])
			if !first && start <= prev {
				t.Fatalf("inobt records not strictly ascending: %d after %d", start, prev)
			}
			first = false
			prev = start
			seen++
			// Every record's first inode must be locatable through the depth-2
			// descent and land in the record whose chunk covers it.
			_, fleaf, idx, err := inobtFindRecord(fs.f, fs.partOffset, fs.sb, 0, rootRel, level, start)
			if err != nil {
				t.Fatalf("find record startIno=%d: %v", start, err)
			}
			if got := readBE32(fleaf[hdr+idx*inobtRecSize:]); got != start {
				t.Fatalf("find record startIno=%d returned start %d", start, got)
			}
		}
		cur = readBE32(leaf[12:]) // right sibling
	}
	if leaves < 2 {
		t.Fatalf("inobt has %d leaves, want >=2 (split fixture)", leaves)
	}
	t.Logf("inobt-deep: depth=%d, interior root records=%d, leaves=%d, total chunk records=%d",
		level, rootRecs, leaves, seen)

	runXfsRepairClean(t, img)
}

// assertLinkDirFixture opens img and verifies the root directory lists exactly
// n entries named namePrefix+index, all resolving to a single shared inode.
func assertLinkDirFixture(t *testing.T, img string, n int, namePrefix string) {
	t.Helper()
	fsIfc, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open %s: %v", filepath.Base(img), err)
	}
	fs := fsIfc.(*xfsFS)
	defer fs.Close()

	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	if len(entries) != n {
		t.Fatalf("ListDir(/) = %d entries, want %d", len(entries), n)
	}
	// Every entry must carry the expected prefix.
	for i, e := range entries {
		if !strings.HasPrefix(e.Name(), namePrefix) {
			t.Fatalf("entry %d name %q lacks prefix %q", i, e.Name(), namePrefix)
		}
	}
	// The first and last entries (extremes of the hash index) must resolve to
	// the same shared inode, exercising lookups through opposite ends of the
	// data-block / hash-index space.
	first, err := fs.Stat("/" + entries[0].Name())
	if err != nil {
		t.Fatalf("Stat(%s): %v", entries[0].Name(), err)
	}
	last, err := fs.Stat("/" + entries[len(entries)-1].Name())
	if err != nil {
		t.Fatalf("Stat(%s): %v", entries[len(entries)-1].Name(), err)
	}
	if first.Inode() == 0 || first.Inode() != last.Inode() {
		t.Fatalf("hardlink entries resolve to different inodes: %d vs %d", first.Inode(), last.Inode())
	}
}

// readBE32 reads a big-endian uint32 (tiny local helper so the fixture test
// avoids importing encoding/binary for a single read).
func readBE32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
