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

// repairClean runs `xfs_repair -n` on img and fails the test if it exits
// non-zero or prints any of the corruption markers. Shared by the advanced
// feature interop tests below.
func repairClean(t *testing.T, xfsRepair, img string) {
	t.Helper()
	cmd := exec.Command(xfsRepair, "-n", img)
	out, runErr := cmd.CombinedOutput()
	t.Logf("xfs_repair -n output:\n%s", out)
	if runErr != nil {
		t.Fatalf("xfs_repair -n exited non-zero: %v", runErr)
	}
	upper := strings.ToUpper(string(out))
	// Note: xfs_repair always prints the benign phase header "moving disconnected
	// inodes to lost+found ..."; a *real* disconnection additionally prints
	// "... would move to lost+found", which the "WOULD " marker catches — so we
	// do not match on "disconnected" itself.
	for _, marker := range []string{"ERROR", "CORRUPT", "WOULD ", "BAD "} {
		if strings.Contains(upper, marker) {
			t.Fatalf("xfs_repair -n reported %q in output:\n%s", marker, out)
		}
	}
}

// TestReflinkXfsRepair formats a reflink-enabled image, clones a multi-block
// file (sharing extents), makes a second clone, deletes one clone (COW
// decrement) and overwrites another (COW break), then asserts xfs_repair -n is
// clean — validating the refcount B-tree and reflink feature bit against the
// canonical tools.
func TestReflinkXfsRepair(t *testing.T) {
	xfsRepair, err := exec.LookPath("xfs_repair")
	if err != nil {
		t.Skip("xfs_repair not found on PATH; install xfsprogs to run this test")
	}
	const oneAG = int64(16384 * 4096)
	path := filepath.Join(t.TempDir(), "reflink.xfs")
	fs, err := filesystem_xfs.Format(path, 3*oneAG, filesystem_xfs.FormatConfig{Label: "reflink", Reflink: true})
	if err != nil {
		t.Fatalf("Format(reflink): %v", err)
	}
	orig := make([]byte, 60000)
	for i := range orig {
		orig[i] = byte(i)
	}
	must := func(e error) {
		if e != nil {
			fs.Close()
			t.Fatal(e)
		}
	}
	must(fs.WriteFile("/orig.dat", orig, 0o644))
	must(fs.Reflink("/orig.dat", "/clone.dat"))
	must(fs.Reflink("/orig.dat", "/clone2.dat"))
	must(fs.DeleteFile("/clone2.dat"))                           // COW decrement
	must(fs.WriteFile("/clone.dat", make([]byte, 40000), 0o644)) // COW break
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	repairClean(t, xfsRepair, path)
}

// TestQuotaXfsRepair formats an image with user/group/project quotas, writes a
// file (which the quotacheck accounts for), and asserts xfs_repair -n accepts
// the classic quota inodes + dquots.
func TestQuotaXfsRepair(t *testing.T) {
	xfsRepair, err := exec.LookPath("xfs_repair")
	if err != nil {
		t.Skip("xfs_repair not found on PATH; install xfsprogs to run this test")
	}
	const oneAG = int64(16384 * 4096)
	path := filepath.Join(t.TempDir(), "quota.xfs")
	fs, err := filesystem_xfs.Format(path, 3*oneAG, filesystem_xfs.FormatConfig{
		Label: "quota",
		Quota: filesystem_xfs.QuotaConfig{User: true, Group: true, Project: true, Enforce: true},
	})
	if err != nil {
		t.Fatalf("Format(quota): %v", err)
	}
	if err := fs.WriteFile("/hello.txt", []byte("quota\n"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	repairClean(t, xfsRepair, path)
}

// TestGrowPartialAGXfsRepair grows a 1-AG filesystem into a partial final AG
// (1.5 AGs total), writes files into the extended region, and checks the result
// is xfs_repair -n clean.
func TestGrowPartialAGXfsRepair(t *testing.T) {
	xfsRepair, err := exec.LookPath("xfs_repair")
	if err != nil {
		t.Skip("xfs_repair not found on PATH; install xfsprogs to run this test")
	}
	const oneAG = int64(16384 * 4096)
	path := filepath.Join(t.TempDir(), "grow.xfs")
	fs, err := filesystem_xfs.Format(path, oneAG, filesystem_xfs.FormatConfig{Label: "grow"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	must := func(e error) {
		if e != nil {
			fs.Close()
			t.Fatal(e)
		}
	}
	must(fs.WriteFile("/before.txt", []byte("pre\n"), 0o644))
	must(fs.Grow(oneAG + oneAG/2)) // partial last AG
	for i := 0; i < 20; i++ {
		must(fs.WriteFile(fmt.Sprintf("/post-%02d.txt", i), []byte("x\n"), 0o644))
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	repairClean(t, xfsRepair, path)
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
