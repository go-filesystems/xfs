package filesystem_xfs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	disk_qcow2 "github.com/go-diskimages/qcow2"
	filesystem_xfs "github.com/go-filesystems/xfs"
)

// rockyRawPath locates a Rocky Linux qcow2 in the mock cache and converts
// it to a raw image using our qcow2 package (cached alongside the qcow2).
// Returns the raw path or calls t.Skip if prerequisites are missing.
func rockyRawPath(t *testing.T) string {
	t.Helper()

	home := os.Getenv("HOME")
	candidates := []string{
		filepath.Join(home, ".mock", "cache",
			"https____dl.rockylinux.org_pub_rocky_10_images_aarch64_Rocky-10-GenericCloud-Base.latest.aarch64.qcow2",
			"Rocky-10-GenericCloud-Base.latest.aarch64.qcow2"),
		filepath.Join(home, ".mock", "cache",
			"https____dl.rockylinux.org_pub_rocky_10_images_aarch64_Rocky-10-GenericCloud.latest.aarch64.qcow2",
			"Rocky-10-GenericCloud.latest.aarch64.qcow2"),
		filepath.Join(home, ".mock", "cache",
			"https____dl.rockylinux.org_pub_rocky_9_images_aarch64_Rocky-9-GenericCloud-Base.latest.aarch64.qcow2",
			"Rocky-9-GenericCloud-Base.latest.aarch64.qcow2"),
	}
	var qcow2Path string
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			qcow2Path = p
			break
		}
	}
	if qcow2Path == "" {
		t.Skip("no Rocky qcow2 found in mock cache — run 'mock pull' first")
		return ""
	}

	raw := strings.TrimSuffix(qcow2Path, ".qcow2") + "-xfstest.raw"
	qi, _ := os.Stat(qcow2Path)
	ri, rerr := os.Stat(raw)
	if rerr != nil || (qi != nil && ri.ModTime().Before(qi.ModTime())) {
		t.Logf("converting %s to raw (first run or stale)", filepath.Base(qcow2Path))
		if err := disk_qcow2.ConvertToRaw(qcow2Path, raw, os.Stdout); err != nil {
			t.Fatalf("disk_qcow2.ConvertToRaw: %v", err)
		}
	}
	return raw
}

// openRocky returns the FS handle for the Rocky test image. FS is an
// interface (since the move to a named public interface in fs.go) — the
// return type must not be pointer-to-interface.
func openRocky(t *testing.T) filesystem_xfs.FS {
	t.Helper()
	raw := rockyRawPath(t)
	fs, err := filesystem_xfs.Open(raw, -1)
	if err != nil {
		t.Fatalf("filesystem_xfs.Open: %v", err)
	}
	t.Cleanup(func() { fs.Close() })
	return fs
}
