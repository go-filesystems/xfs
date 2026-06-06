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
	const fourMiB = int64(4 * 1024 * 1024)
	path := filepath.Join(t.TempDir(), "img.xfs")
	fs, err := filesystem_xfs.Format(path, fourMiB, filesystem_xfs.FormatConfig{
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
	if strings.Contains(strings.ToUpper(string(out)), "ERROR") {
		t.Fatalf("xfs_repair -n reported ERROR in output:\n%s", out)
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

	const fourMiB = int64(4 * 1024 * 1024)
	path := filepath.Join(t.TempDir(), "grow.xfs")
	fs, err := filesystem_xfs.Format(path, fourMiB, filesystem_xfs.FormatConfig{
		Label: "growcompat",
	})
	if err != nil {
		t.Fatalf("filesystem_xfs.Format: %v", err)
	}
	if err := fs.WriteFile("/before-grow.txt", []byte("pre-grow content\n"), 0o644); err != nil {
		fs.Close()
		t.Fatalf("WriteFile pre-grow: %v", err)
	}

	// Grow from 1 AG (4 MiB) to 3 AGs (12 MiB).
	if err := fs.Grow(3 * fourMiB); err != nil {
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

// TestResizeShrinkErrSentinel is a tiny smoke test that the package's
// Resize() entry point returns filesystem.ErrShrinkUnsupported on an
// undersized request — same probe the diskimage CLI uses to decide
// whether to fall through to "recreate" semantics. Not gated on
// xfsprogs since it doesn't shell out.
func TestResizeShrinkErrSentinel(t *testing.T) {
	const fourMiB = int64(4 * 1024 * 1024)
	path := filepath.Join(t.TempDir(), "shrink.xfs")
	fs, err := filesystem_xfs.Format(path, 2*fourMiB, filesystem_xfs.FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.Resize(fourMiB); !errors.Is(err, filesystem.ErrShrinkUnsupported) {
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

