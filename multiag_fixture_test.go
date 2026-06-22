package filesystem_xfs

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	_ "embed"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// multiagFixtureGz is the committed mkfs.xfs fixture, embedded so the test runs
// on every CI arch — including the cross-compiled emulated binaries whose
// working directory does not contain testdata/.
//
//go:embed testdata/xfs-multiag.img.gz
var multiagFixtureGz []byte

// TestMultiAGFixtureRead reads a committed, real mkfs.xfs image whose default
// geometry has a NON-power-of-two sb_agblocks (agblocks=20480, agblklog=15,
// agcount=4). XFS packs an on-disk filesystem block number as
// fsbno = (agno << agblklog) | agbno, so for any extent in AG≥1 the packed
// fsbno differs from the flat block index (e.g. AG1 block 24 → flat 20504,
// packed 32792). The driver previously translated fsbno→byte-offset as if it
// were a flat block number (fsbno*blockSize), which is only correct when
// agblocks is a power of two; reading an extent in a higher AG failed with
// "read extent block …: EOF".
//
// The fixture's /highag.bin file was deterministically written by the Linux
// kernel into AG1 (its single extent starts at flat block 20504, packed fsbno
// 32792), so reading it byte-identically exercises the multi-AG fsbToPhysBlock
// decode on every architecture — including big-endian s390x — without needing a
// live kernel. The fixture was produced in the org's debian validation VM with:
//
//	truncate -s 320M img && mkfs.xfs -f -d agcount=4 img
//	# fill AG0 with a real 78 MiB file, then write the deterministic files below
//	xfs_repair -n img   # clean
//
// It is stored gzip-compressed (the AG0 filler is highly compressible) to keep
// the committed blob small.
func TestMultiAGFixtureRead(t *testing.T) {
	img := decompressFixture(t)

	fs, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open multi-AG fixture: %v", err)
	}
	defer fs.Close()

	// Files written through the real kernel with deterministic content. The
	// md5 sums were captured in the VM at fixture-creation time.
	cases := []struct {
		path string
		size int
		md5  string
	}{
		{"/small.txt", 18, "a99ee5708c0e87a4904d08c785703665"},
		{"/sub/deep/nested.txt", 25, "eb99f842863ff4daf247731608b11e20"},
		{"/medium.bin", 200000, "d7f02347e9fd18d5b486e864a9d37c08"},
		// highag.bin lives in AG1 (flat block 20504, packed fsbno 32792); this
		// is the case that failed before the multi-AG addressing fix.
		{"/highag.bin", 8 * 1024 * 1024, "263fa8e7d8ddcab4352cd9b242ddfe83"},
	}
	for _, c := range cases {
		got, err := fs.ReadFile(c.path)
		if err != nil {
			t.Errorf("ReadFile(%s): %v", c.path, err)
			continue
		}
		if len(got) != c.size {
			t.Errorf("ReadFile(%s): got %d bytes, want %d", c.path, len(got), c.size)
		}
		sum := md5.Sum(got)
		if h := hex.EncodeToString(sum[:]); h != c.md5 {
			t.Errorf("ReadFile(%s): md5 %s, want %s", c.path, h, c.md5)
		}
	}

	// Verify the small-file contents directly too (cheap and explicit).
	if got, err := fs.ReadFile("/small.txt"); err != nil || !bytes.Equal(got, []byte("hello-from-ag-test")) {
		t.Errorf("ReadFile(/small.txt) = %q, %v", got, err)
	}

	// Symlink target (the kernel may store it inline or remote).
	if got, err := fs.ReadLink("/link-to-nested"); err != nil || got != "sub/deep/nested.txt" {
		t.Errorf("ReadLink(/link-to-nested) = %q, %v; want %q", got, err, "sub/deep/nested.txt")
	}

	// Directory listing through the kernel-written root must surface every file.
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
	}
	for _, name := range []string{"small.txt", "medium.bin", "highag.bin", "sub", "link-to-nested"} {
		if !seen[name] {
			t.Errorf("ListDir(/) missing %q", name)
		}
	}
}

// TestMultiAGFixtureGeometry asserts the committed fixture really has the
// bug-triggering non-power-of-two geometry, so the test above keeps exercising
// the multi-AG path even if the fixture is ever regenerated.
func TestMultiAGFixtureGeometry(t *testing.T) {
	img := decompressFixture(t)
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sb, err := readSuperblock(f, 0)
	if err != nil {
		t.Fatalf("readSuperblock: %v", err)
	}
	if sb.agBlocks&(sb.agBlocks-1) == 0 {
		t.Fatalf("fixture agblocks=%d is a power of two — does not exercise the multi-AG bug", sb.agBlocks)
	}
	if sb.agCount < 2 {
		t.Fatalf("fixture agcount=%d — need ≥2 AGs to exercise multi-AG addressing", sb.agCount)
	}
	// fsbToPhysBlock must decode AG1 block 0 to flat block agBlocks (not 1<<agblklog).
	ag1block0 := uint64(1) << uint64(sb.agBlkLog) // packed fsbno of (AG1, block 0)
	if got := sb.fsbToPhysBlock(ag1block0); got != uint64(sb.agBlocks) {
		t.Errorf("fsbToPhysBlock(AG1.0)=%d, want %d (agBlocks)", got, sb.agBlocks)
	}
}

// decompressFixture gunzips the embedded committed fixture into a temp file and
// returns its path.
func decompressFixture(t *testing.T) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(multiagFixtureGz))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	out := filepath.Join(t.TempDir(), "xfs-multiag.img")
	of, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(of, zr); err != nil { //nolint:gosec // trusted committed fixture
		of.Close()
		t.Fatalf("decompress: %v", err)
	}
	if err := of.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}
