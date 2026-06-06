package filesystem_xfs_test

// xfsprogs_compat_test.go — cross-implementation write-side compatibility tests.
//
// These tests format an XFS image with our writer, write a file, and then
// invoke the canonical `xfsprogs` userspace tools (`xfs_repair -n`, `xfs_db`)
// to validate the on-disk layout. Both tests are skip-gated when `xfsprogs`
// is not available on PATH — they document a cross-compat audit, not a
// hard build dependency.

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

