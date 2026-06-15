package filesystem_xfs_test

// xfsprogs_compat_test.go — cross-implementation write-side compatibility tests.
//
// These tests format an XFS image with our writer, write a file, and then
// invoke the canonical `xfsprogs` userspace tools (`xfs_repair -n`, `xfs_db`)
// to validate the on-disk layout. Both tests are skip-gated when `xfsprogs`
// is not available on PATH — they document a cross-compat audit, not a
// hard build dependency.

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
	filesystem_xfs "github.com/go-filesystems/xfs"
)

// xfsprogsImage formats a fresh 4 MiB XFS image at tmpDir/img.xfs, writes
// /hello.txt to it, then returns the image path. Use this from any test
// that wants to validate our writer's output against xfsprogs tools.
func xfsprogsImage(t *testing.T) string {
	t.Helper()
	const imgSize = int64(128 * 1024 * 1024) // two 64 MiB AGs (xfs_repair needs >1 AG)
	path := filepath.Join(t.TempDir(), "img.xfs")
	fs, err := filesystem_xfs.Format(path, imgSize, filesystem_xfs.FormatConfig{
		Label: "xfsprogs",
	})
	if err != nil {
		t.Fatalf("filesystem_xfs.Format: %v", err)
	}
	if err := fs.WriteFile("/hello.txt", []byte("hello from go-filesystems/xfs\n"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// TestWriteThenXfsRepair runs `xfs_repair -n` (read-only check) on an image
// produced by our writer and asserts the canonical fsck exits 0 with no
// "ERROR" lines in stdout/stderr.
func TestWriteThenXfsRepair(t *testing.T) {
	xfsRepair, err := exec.LookPath("xfs_repair")
	if err != nil {
		t.Skip("xfs_repair not found on PATH; install xfsprogs to run this test")
	}

	img := xfsprogsImage(t)

	// -n = no modify (read-only check). Exit code 0 = filesystem is clean.
	cmd := exec.Command(xfsRepair, "-n", img)
	out, runErr := cmd.CombinedOutput()
	t.Logf("xfs_repair -n output:\n%s", out)
	if runErr != nil {
		t.Fatalf("xfs_repair -n exited non-zero: %v", runErr)
	}
	upper := strings.ToUpper(string(out))
	for _, marker := range []string{"ERROR", "CORRUPT", "WOULD ", "BAD "} {
		if strings.Contains(upper, marker) {
			t.Fatalf("xfs_repair -n reported %q in output:\n%s", marker, out)
		}
	}
}

// TestGrowThenXfsRepair formats a 1-AG image, writes a file, grows it
// to 3 AGs, writes additional files into the freshly-extended part of
// the filesystem, then runs `xfs_repair -n` (read-only) on the result.
// Skip-gated when `xfs_repair` is not on PATH so contributors without
// xfsprogs installed still get a clean local test run.
//
// This is the end-to-end check that our Grow() implementation lays down
// a valid on-disk layout — secondary SBs in every grown AG, well-formed
// AGF/AGI/B-tree leaves, and a primary SB CRC that matches the new
// agcount.
func TestGrowThenXfsRepair(t *testing.T) {
	xfsRepair, err := exec.LookPath("xfs_repair")
	if err != nil {
		t.Skip("xfs_repair not found on PATH; install xfsprogs to run this test")
	}

	const oneAG = int64(16384 * 4096) // 64 MiB
	path := filepath.Join(t.TempDir(), "grow.xfs")
	fs, err := filesystem_xfs.Format(path, oneAG, filesystem_xfs.FormatConfig{
		Label: "growcompat",
	})
	if err != nil {
		t.Fatalf("filesystem_xfs.Format: %v", err)
	}
	if err := fs.WriteFile("/before-grow.txt", []byte("pre-grow content\n"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile pre-grow: %v", err)
	}

	// Grow from 1 AG (64 MiB) to 3 AGs (192 MiB).
	if err := fs.Grow(3 * oneAG); err != nil {
		fs.Close()
		t.Fatalf("Grow: %v", err)
	}

	// Write more files; with the inobt-growth path in place these
	// comfortably exceed the AG-0 seed budget and stress the per-AG
	// metadata laid down by Grow.
	for i := 0; i < 10; i++ {
		p := fmt.Sprintf("/post-grow-%02d.txt", i)
		if err := fs.WriteFile(p, []byte(fmt.Sprintf("file %d\n", i)), 0o644); err != nil {
			fs.Close()
			t.Fatalf("WriteFile %s: %v", p, err)
		}
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cmd := exec.Command(xfsRepair, "-n", path)
	out, runErr := cmd.CombinedOutput()
	t.Logf("xfs_repair -n (post-grow) output:\n%s", out)
	if runErr != nil {
		t.Fatalf("xfs_repair -n exited non-zero after grow: %v", runErr)
	}
	if strings.Contains(strings.ToUpper(string(out)), "ERROR") {
		t.Fatalf("xfs_repair -n reported ERROR after grow:\n%s", out)
	}
}

// TestBlockFormDirXfsRepair exercises the directory write paths that go
// beyond short form: a directory large enough to be promoted to block form, a
// subdirectory created inside it, short-form entries in the root, and
// deletions from both a block-form and a short-form directory. The result must
// be xfs_repair -n clean. Skip-gated on xfs_repair like the other interop tests.
func TestBlockFormDirXfsRepair(t *testing.T) {
	xfsRepair, err := exec.LookPath("xfs_repair")
	if err != nil {
		t.Skip("xfs_repair not found on PATH; install xfsprogs to run this test")
	}

	const oneAG = int64(16384 * 4096)
	path := filepath.Join(t.TempDir(), "blockdir.xfs")
	fs, err := filesystem_xfs.Format(path, 4*oneAG, filesystem_xfs.FormatConfig{Label: "blkdir"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	mustf := func(e error) {
		if e != nil {
			fs.Close()
			t.Fatal(e)
		}
	}
	mustf(fs.MkDir("/big", 0o755))
	for i := 0; i < 40; i++ { // promotes /big to block form
		mustf(fs.WriteFile(fmt.Sprintf("/big/f%03d.dat", i), []byte("data\n"), 0o644))
	}
	mustf(fs.MkDir("/big/sub", 0o755)) // subdir inside a block-form dir
	mustf(fs.WriteFile("/big/sub/x.txt", []byte("x\n"), 0o644))
	for i := 0; i < 5; i++ { // short-form root entries
		mustf(fs.WriteFile(fmt.Sprintf("/top%d", i), []byte("t\n"), 0o644))
	}
	mustf(fs.DeleteFile("/big/f000.dat")) // delete from block form
	mustf(fs.DeleteFile("/top0"))         // delete from short form
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cmd := exec.Command(xfsRepair, "-n", path)
	out, runErr := cmd.CombinedOutput()
	t.Logf("xfs_repair -n output:\n%s", out)
	if runErr != nil {
		t.Fatalf("xfs_repair -n reported problems: %v", runErr)
	}
	upper := strings.ToUpper(string(out))
	for _, marker := range []string{"ERROR", "CORRUPT", "WOULD ", "BAD "} {
		if strings.Contains(upper, marker) {
			t.Fatalf("xfs_repair -n reported %q in output:\n%s", marker, out)
		}
	}
}

// TestLeafFormDirXfsRepair builds a directory large enough to (a) outgrow a
// single directory block into leaf form (>~165 short-name entries, an XDD3
// data block plus a 0x3df1 leaf/index block) and (b) consume enough inodes
// that the inobt must grow several new 64-inode chunks. Both must be
// xfs_repair -n clean.
//
// This is the regression for two bugs: the leaf-form writer itself, and an
// inode-chunk allocation that returned only inopBlock-aligned blocks — a
// non-64-aligned chunk start makes xfs_repair map inodes onto file-data
// blocks. Earlier interop tests created too few files (≤ the root chunk's
// free-inode budget) to ever grow the inobt, so neither bug surfaced.
func TestLeafFormDirXfsRepair(t *testing.T) {
	xfsRepair, err := exec.LookPath("xfs_repair")
	if err != nil {
		t.Skip("xfs_repair not found on PATH; install xfsprogs to run this test")
	}

	const oneAG = int64(16384 * 4096)
	path := filepath.Join(t.TempDir(), "leafdir.xfs")
	fs, err := filesystem_xfs.Format(path, 4*oneAG, filesystem_xfs.FormatConfig{Label: "leafdir"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	mustf := func(e error) {
		if e != nil {
			fs.Close()
			t.Fatal(e)
		}
	}
	mustf(fs.MkDir("/d", 0o755))
	const nEntries = 300 // forces leaf form and several inode chunks
	for i := 0; i < nEntries; i++ {
		mustf(fs.WriteFile(fmt.Sprintf("/d/file%04d.txt", i), []byte(fmt.Sprintf("content %d\n", i)), 0o644))
	}
	mustf(fs.WriteFile("/top.txt", []byte("top\n"), 0o644))
	mustf(fs.DeleteFile("/d/file0000.txt")) // delete from a leaf-form dir
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cmd := exec.Command(xfsRepair, "-n", path)
	out, runErr := cmd.CombinedOutput()
	t.Logf("xfs_repair -n output:\n%s", out)
	if runErr != nil {
		t.Fatalf("xfs_repair -n reported problems: %v", runErr)
	}
	upper := strings.ToUpper(string(out))
	for _, marker := range []string{"ERROR", "CORRUPT", "WOULD ", "BAD ", "AGF_LONGEST"} {
		if strings.Contains(upper, marker) {
			t.Fatalf("xfs_repair -n reported %q in output:\n%s", marker, out)
		}
	}

	// Reopen and confirm every surviving entry reads back correctly.
	ro, err := filesystem_xfs.Open(path, -1)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer ro.Close()
	ents, err := ro.ListDir("/d")
	if err != nil {
		t.Fatalf("ListDir /d: %v", err)
	}
	if len(ents) != nEntries-1 {
		t.Fatalf("/d has %d entries, want %d", len(ents), nEntries-1)
	}
	got, err := ro.ReadFile("/d/file0150.txt")
	if err != nil {
		t.Fatalf("ReadFile /d/file0150.txt: %v", err)
	}
	if string(got) != "content 150\n" {
		t.Fatalf("file0150.txt = %q, want %q", got, "content 150\n")
	}
}

// TestNodeFormDirXfsRepair builds a directory large enough to outgrow leaf
// form into node form: the single leaf1 index block is replaced by a da3-node
// btree (0x3ebe) over multiple leafN blocks (0x3dff) plus a free-index block
// (XDF3) at the 64 GiB offset. It must be xfs_repair -n clean and read back.
func TestNodeFormDirXfsRepair(t *testing.T) {
	xfsRepair, err := exec.LookPath("xfs_repair")
	if err != nil {
		t.Skip("xfs_repair not found on PATH; install xfsprogs to run this test")
	}

	const oneAG = int64(16384 * 4096)
	path := filepath.Join(t.TempDir(), "nodedir.xfs")
	fs, err := filesystem_xfs.Format(path, 8*oneAG, filesystem_xfs.FormatConfig{Label: "nodedir"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	mustf := func(e error) {
		if e != nil {
			fs.Close()
			t.Fatal(e)
		}
	}
	mustf(fs.MkDir("/nd", 0o755))
	const nEntries = 2000 // forces node form (multiple leafN under a da3 node)
	for i := 0; i < nEntries; i++ {
		mustf(fs.WriteFile(fmt.Sprintf("/nd/file_%05d.txt", i), []byte(fmt.Sprintf("c%d\n", i)), 0o644))
	}
	mustf(fs.DeleteFile("/nd/file_00000.txt"))
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cmd := exec.Command(xfsRepair, "-n", path)
	out, runErr := cmd.CombinedOutput()
	t.Logf("xfs_repair -n output:\n%s", out)
	if runErr != nil {
		t.Fatalf("xfs_repair -n reported problems: %v", runErr)
	}
	upper := strings.ToUpper(string(out))
	for _, marker := range []string{"ERROR", "CORRUPT", "WOULD ", "BAD ", "AGF_LONGEST"} {
		if strings.Contains(upper, marker) {
			t.Fatalf("xfs_repair -n reported %q in output:\n%s", marker, out)
		}
	}

	ro, err := filesystem_xfs.Open(path, -1)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer ro.Close()
	ents, err := ro.ListDir("/nd")
	if err != nil {
		t.Fatalf("ListDir /nd: %v", err)
	}
	if len(ents) != nEntries-1 {
		t.Fatalf("/nd has %d entries, want %d", len(ents), nEntries-1)
	}
	got, err := ro.ReadFile("/nd/file_01000.txt")
	if err != nil {
		t.Fatalf("ReadFile /nd/file_01000.txt: %v", err)
	}
	if string(got) != "c1000\n" {
		t.Fatalf("file_01000.txt = %q, want %q", got, "c1000\n")
	}
}

// TestDirChurnXfsRepair grows a directory into node form one entry at a time,
// then deletes most of its entries — the workload that fragments the free
// space the most, since each add/remove frees and reallocates the whole
// directory. It must stay xfs_repair -n clean, which exercises free-extent
// coalescing in freeBlocks (without it the bno/cnt B-tree leaf fills and
// further frees fail with "cannot insert without a tree split").
func TestDirChurnXfsRepair(t *testing.T) {
	xfsRepair, err := exec.LookPath("xfs_repair")
	if err != nil {
		t.Skip("xfs_repair not found on PATH; install xfsprogs to run this test")
	}

	const oneAG = int64(16384 * 4096)
	path := filepath.Join(t.TempDir(), "churn.xfs")
	fs, err := filesystem_xfs.Format(path, 8*oneAG, filesystem_xfs.FormatConfig{Label: "churn"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	mustf := func(e error) {
		if e != nil {
			fs.Close()
			t.Fatal(e)
		}
	}
	mustf(fs.MkDir("/d", 0o755))
	for i := 0; i < 700; i++ { // grow well into node form
		mustf(fs.WriteFile(fmt.Sprintf("/d/f%04d", i), []byte("x\n"), 0o644))
	}
	for i := 0; i < 650; i++ { // shrink back toward block form
		mustf(fs.DeleteFile(fmt.Sprintf("/d/f%04d", i)))
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cmd := exec.Command(xfsRepair, "-n", path)
	out, runErr := cmd.CombinedOutput()
	t.Logf("xfs_repair -n output:\n%s", out)
	if runErr != nil {
		t.Fatalf("xfs_repair -n reported problems: %v", runErr)
	}
	upper := strings.ToUpper(string(out))
	for _, marker := range []string{"ERROR", "CORRUPT", "WOULD ", "BAD ", "AGF_LONGEST"} {
		if strings.Contains(upper, marker) {
			t.Fatalf("xfs_repair -n reported %q in output:\n%s", marker, out)
		}
	}

	ro, err := filesystem_xfs.Open(path, -1)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer ro.Close()
	ents, err := ro.ListDir("/d")
	if err != nil {
		t.Fatalf("ListDir /d: %v", err)
	}
	if len(ents) != 50 {
		t.Fatalf("/d has %d entries, want 50", len(ents))
	}
}

// TestResizeShrinkErrSentinel is a tiny smoke test that the package's
// Resize() entry point returns filesystem.ErrShrinkUnsupported on an
// undersized request — same probe the diskimage CLI uses to decide
// whether to fall through to "recreate" semantics. Not gated on
// xfsprogs since it doesn't shell out.
func TestResizeShrinkErrSentinel(t *testing.T) {
	const oneAG = int64(16384 * 4096) // 64 MiB, the minimum image size
	path := filepath.Join(t.TempDir(), "shrink.xfs")
	fs, err := filesystem_xfs.Format(path, 2*oneAG, filesystem_xfs.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.Resize(oneAG); !errors.Is(err, filesystem.ErrShrinkUnsupported) {
		t.Fatalf("Resize(shrink) = %v, want ErrShrinkUnsupported", err)
	}
}

// TestWriteThenXfsDb runs `xfs_db -r -c 'sb 0' -c 'p' <img>` on an image
// produced by our writer and asserts the canonical xfsprogs debugger can
// parse our superblock (exit 0, no error indicators in output).
func TestWriteThenXfsDb(t *testing.T) {
	xfsDb, err := exec.LookPath("xfs_db")
	if err != nil {
		t.Skip("xfs_db not found on PATH; install xfsprogs to run this test")
	}

	img := xfsprogsImage(t)

	// -r = read-only; 'sb 0' selects superblock 0; 'p' prints it.
	cmd := exec.Command(xfsDb, "-r", "-c", "sb 0", "-c", "p", img)
	out, runErr := cmd.CombinedOutput()
	t.Logf("xfs_db output:\n%s", out)
	if runErr != nil {
		t.Fatalf("xfs_db exited non-zero: %v", runErr)
	}
	upper := strings.ToUpper(string(out))
	for _, marker := range []string{"ERROR", "BAD MAGIC", "FATAL"} {
		if strings.Contains(upper, marker) {
			t.Fatalf("xfs_db reported %q in output:\n%s", marker, out)
		}
	}
	// Sanity: a parsed superblock dump always contains the magicnum field.
	if !strings.Contains(string(out), "magicnum") {
		t.Fatalf("xfs_db output missing 'magicnum' field (parse likely failed):\n%s", out)
	}
}

