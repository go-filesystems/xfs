package filesystem_xfs

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// nonpow2WriteFixtureGz is a committed, real mkfs.xfs image with a
// NON-power-of-two sb_agblocks (agblocks=20480, agblklog=15, agcount=4),
// embedded so the write-path test runs on every CI arch — including the
// cross-compiled emulated binaries whose working directory does not contain
// testdata/.
//
// The image holds six 1.2 MiB regular files /d1/f.bin … /d6/f.bin (one per
// directory) plus a tiny /note.txt, all written by the Linux kernel. The
// per-directory files were laid out across all four AGs, so several of them
// (notably /d2/f.bin) have their single data extent in an AG ≥ 1 — exactly
// where a packed fsbno differs from a flat block index.
//
// It was produced in the org's validation VM with, roughly:
//
//	truncate -s 320M img && mkfs.xfs -f -d agcount=4 img    # agblocks=20480 (non-pow2)
//	mount img /mnt
//	for d in d1 d2 d3 d4 d5 d6; do mkdir /mnt/$d
//	  head -c 1200000 /dev/urandom > /mnt/$d/f.bin; done
//	echo -n "fixture-note-0\n" > /mnt/note.txt
//	umount /mnt && xfs_repair -n img   # clean
//
// The md5 sums below were captured in the VM at fixture-creation time.
const (
	nonpow2KeepNote = "cdd4ee4d8dca70eee7d9778bc8527f4c"
)

//go:embed testdata/xfs-write-nonpow2.img.gz
var nonpow2WriteFixtureGz []byte

// nonpow2KeepFiles are the files that must remain byte-identical after deleting
// /d2/f.bin. /d2/f.bin itself is intentionally absent.
var nonpow2KeepFiles = []struct {
	path string
	size int
	md5  string
}{
	{"/d1/f.bin", 1200000, "57347b803b6005cf0b17a9bc3b0ee9f3"},
	{"/d3/f.bin", 1200000, "080ee08a6ed47029e373f84aaec99470"},
	{"/d4/f.bin", 1200000, "42f50da4747707924ec6779fb96a5664"},
	{"/d5/f.bin", 1200000, "7d0bc37776a5c8937f10167fcf368bc2"},
	{"/d6/f.bin", 1200000, "9350149ccc75933436cbb123d8016efa"},
	{"/note.txt", 15, nonpow2KeepNote},
}

// TestNonPow2WriteDelete is the regression test for the latent non-power-of-two
// AG-geometry bug in the WRITE path. Deleting /d2/f.bin frees that file's data
// extent back to the free-space B-trees: freeBlocks decodes the extent's packed
// fsbno into (ag, agbno) to find the right AGF. The pre-fix freeBlocks used a
// flat /sb_agblocks division — correct only when sb_agblocks is a power of two
// (our own Format), but WRONG on this real mkfs.xfs geometry — so it credited
// the freed blocks to the wrong AG, corrupting per-AG free-space accounting
// (xfs_repair: "agf_freeblks N, counted M" / bad free-space btree). The fix
// pairs the packed encoder agAbsBlock with the packing inverse fsbToAgAgbno.
//
// This Go-level test proves the modify-write does not corrupt the other files;
// the full proof that the freed extent is accounted to the correct AG (clean
// xfs_repair -n) is exercised on a real kernel in the org's validation VM and
// recorded in the commit message.
func TestNonPow2WriteDelete(t *testing.T) {
	// Defensive: this test performs a REAL DeleteFile through every layer, so it
	// must run against the production hooks. Snapshot and restore the delete-path
	// inode-write hook in case an earlier test left a no-op override in place
	// (which would silently drop the inode write and mask the real behaviour).
	oldWriteInode := deleteWriteInode
	t.Cleanup(func() { deleteWriteInode = oldWriteInode })
	deleteWriteInode = writeInode

	img := decompressNonPow2WriteFixture(t)

	// Confirm the deleted file's extent really lives in AG ≥ 1 on this geometry,
	// so the delete exercises the multi-AG packed-fsbno free path (and not the
	// AG0 case the flat decode happens to get right).
	ag, nblocks := nonpow2ExtentAG(t, img, "/d2/f.bin")
	if ag < 1 {
		t.Fatalf("/d2/f.bin extent is in AG%d; need AG>=1 to exercise the non-pow2 free path", ag)
	}
	t.Logf("/d2/f.bin data extent is in AG%d, %d blocks (packed fsbno != flat index)", ag, nblocks)

	// Capture per-AG free-block accounting BEFORE the delete. This is what the
	// bug actually corrupts: the byte content of the surviving files is recomputed
	// from their (untouched) on-disk extent maps, so a wrong-AG free leaves those
	// reads byte-identical — the only Go-visible damage is that freeBlocks credits
	// the freed run to the wrong AGF. We therefore assert the AGF accounting moves
	// in the CORRECT AG (and only there), which is precisely the pre-fix failure.
	before := nonpow2AGFFreeBlks(t, img)

	fs, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open non-pow2 write fixture: %v", err)
	}

	if err := fs.DeleteFile("/d2/f.bin"); err != nil {
		fs.Close()
		t.Fatalf("DeleteFile(/d2/f.bin): %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close after delete: %v", err)
	}

	// The freed extent (nblocks blocks in AG `ag`) must land in AG `ag`'s AGF and
	// nowhere else. The pre-fix flat /sb_agblocks decode mis-derives the AG on this
	// non-power-of-two geometry, so it credits the wrong AGF — caught here.
	after := nonpow2AGFFreeBlks(t, img)
	for a := uint32(0); a < uint32(len(before)); a++ {
		delta := int64(after[a]) - int64(before[a])
		want := int64(0)
		if a == ag {
			want = int64(nblocks)
		}
		if delta != want {
			t.Errorf("AG%d agf_freeblks moved by %d, want %d "+
				"(freed extent mis-accounted to the wrong AG — the non-pow2 freeBlocks bug)",
				a, delta, want)
		}
	}

	// Reopen the mutated image and assert every other file is still byte-exact
	// and /d2/f.bin is gone. A wrong-AG free (the bug) would either leave the
	// trees inconsistent or stomp accounting that, on reopen and read, surfaces
	// as a corrupted/short read of a neighbouring file.
	fs2, err := Open(img, -1)
	if err != nil {
		t.Fatalf("reopen after delete: %v", err)
	}
	defer fs2.Close()

	if _, err := fs2.ReadFile("/d2/f.bin"); err == nil {
		t.Errorf("ReadFile(/d2/f.bin) succeeded after delete; want not-found error")
	}

	for _, c := range nonpow2KeepFiles {
		got, err := fs2.ReadFile(c.path)
		if err != nil {
			t.Errorf("ReadFile(%s) after delete: %v", c.path, err)
			continue
		}
		if len(got) != c.size {
			t.Errorf("ReadFile(%s): got %d bytes, want %d", c.path, len(got), c.size)
		}
		sum := md5.Sum(got)
		if h := hex.EncodeToString(sum[:]); h != c.md5 {
			t.Errorf("ReadFile(%s): md5 %s, want %s (file corrupted by wrong-AG free)", c.path, h, c.md5)
		}
	}

	// The deleted directory entry must be gone from /d2 too.
	entries, err := fs2.ListDir("/d2")
	if err != nil {
		t.Fatalf("ListDir(/d2): %v", err)
	}
	for _, e := range entries {
		if e.Name() == "f.bin" {
			t.Errorf("ListDir(/d2) still lists f.bin after delete")
		}
	}
}

// TestNonPow2WriteFixtureGeometry asserts the committed write fixture really has
// the bug-triggering non-power-of-two geometry, so TestNonPow2WriteDelete keeps
// exercising the multi-AG free path even if the fixture is ever regenerated.
func TestNonPow2WriteFixtureGeometry(t *testing.T) {
	img := decompressNonPow2WriteFixture(t)
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
		t.Fatalf("fixture agblocks=%d is a power of two — does not exercise the non-pow2 free bug", sb.agBlocks)
	}
	if sb.agCount < 2 {
		t.Fatalf("fixture agcount=%d — need >=2 AGs to exercise the multi-AG free path", sb.agCount)
	}
}

// nonpow2ExtentAG resolves path to its inode, reads its data extents, and
// returns the AG of the first extent's packed fsbno (via the packing inverse)
// together with the file's total block count. The fixture's files are a single
// contiguous extent, so this AG is where the whole file is freed.
func nonpow2ExtentAG(t *testing.T, img, path string) (ag, nblocks uint32) {
	t.Helper()
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sb, err := readSuperblock(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	in, err := fsPathLookup(f, 0, sb, path)
	if err != nil {
		t.Fatalf("fsPathLookup(%s): %v", path, err)
	}
	var exts []extent
	switch in.format {
	case inodeFmtExtents:
		exts, err = inlineExtents(in)
	case inodeFmtBtree:
		exts, err = btreeExtents(f, 0, sb, in)
	default:
		t.Fatalf("%s: unexpected fork format %d", path, in.format)
	}
	if err != nil {
		t.Fatalf("%s extents: %v", path, err)
	}
	if len(exts) != 1 {
		t.Fatalf("%s has %d extents; this test assumes a single contiguous extent", path, len(exts))
	}
	ag, _ = sb.fsbToAgAgbno(exts[0].startBlock)
	return ag, exts[0].count
}

// nonpow2AGFFreeBlks reads each AG's AGF free-block count (agf_freeblks) from
// the on-disk image, returning a slice indexed by AG number.
func nonpow2AGFFreeBlks(t *testing.T, img string) []uint32 {
	t.Helper()
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sb, err := readSuperblock(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]uint32, sb.agCount)
	for a := uint32(0); a < sb.agCount; a++ {
		agf, err := allocAGFBlock(f, 0, sb, a)
		if err != nil {
			t.Fatalf("read AGF ag=%d: %v", a, err)
		}
		out[a] = binary.BigEndian.Uint32(agf[agfOffFreeBlks:])
	}
	return out
}

// decompressNonPow2WriteFixture gunzips the embedded committed write fixture
// into a fresh temp file (each call gets its own copy so the destructive delete
// in one subtest never affects another) and returns its path.
func decompressNonPow2WriteFixture(t *testing.T) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(nonpow2WriteFixtureGz))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	out := filepath.Join(t.TempDir(), "xfs-write-nonpow2.img")
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
