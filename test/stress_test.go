package filesystem_xfs_test

// stress_test.go — intensive concurrent stress tests for the XFS public API.

// Each image (Rocky, AlmaLinux, Amazon Linux) is tested with tens of thousands
// of simultaneous read / write / delete / listdir / stat operations spread
// across many goroutines.
//
// All tests are skipped when the target qcow2 is not present in ~/.mock/cache.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	disk_qcow2 "github.com/go-diskimages/qcow2"
)

// ──────────────────── image helpers ────────────────────────────────────────

// imageSpec describes one cloud image that may be present in the mock cache.
type imageSpec struct {
	distro     string // human label used in test names and log messages
	candidates []string
}

// allImages is the set of images exercised by the stress suite.
var allImages = []imageSpec{
	{
		distro: "rocky",
		candidates: []string{
			// Rocky 10
			"https____dl.rockylinux.org_pub_rocky_10_images_aarch64_Rocky-10-GenericCloud-Base.latest.aarch64.qcow2/Rocky-10-GenericCloud-Base.latest.aarch64.qcow2",
			"https____dl.rockylinux.org_pub_rocky_10_images_x86_64_Rocky-10-GenericCloud-Base.latest.x86_64.qcow2/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2",
			// Rocky 9
			"https____dl.rockylinux.org_pub_rocky_9_images_aarch64_Rocky-9-GenericCloud-Base.latest.aarch64.qcow2/Rocky-9-GenericCloud-Base.latest.aarch64.qcow2",
			"https____dl.rockylinux.org_pub_rocky_9_images_x86_64_Rocky-9-GenericCloud-Base.latest.x86_64.qcow2/Rocky-9-GenericCloud-Base.latest.x86_64.qcow2",
		},
	},
	{
		distro: "alma",
		candidates: []string{
			// AlmaLinux 10
			"https____repo.almalinux.org_almalinux_10_cloud_aarch64_images_AlmaLinux-10-GenericCloud-latest.aarch64.qcow2/AlmaLinux-10-GenericCloud-latest.aarch64.qcow2",
			"https____repo.almalinux.org_almalinux_10_cloud_x86_64_images_AlmaLinux-10-GenericCloud-latest.x86_64.qcow2/AlmaLinux-10-GenericCloud-latest.x86_64.qcow2",
			// AlmaLinux 9
			"https____repo.almalinux.org_almalinux_9_cloud_aarch64_images_AlmaLinux-9-GenericCloud-latest.aarch64.qcow2/AlmaLinux-9-GenericCloud-latest.aarch64.qcow2",
			"https____repo.almalinux.org_almalinux_9_cloud_x86_64_images_AlmaLinux-9-GenericCloud-latest.x86_64.qcow2/AlmaLinux-9-GenericCloud-latest.x86_64.qcow2",
		},
	},
	{
		distro: "amazon",
		candidates: []string{
			// Amazon Linux 2023 aarch64
			"https____cdn.amazonlinux.com_al2023_os-images_latest_kvm-arm64_al2023-kvm-2023.6.20250303.0-kernel-6.1-arm64.xfs.gpt.qcow2/al2023-kvm-2023.6.20250303.0-kernel-6.1-arm64.xfs.gpt.qcow2",
			// Amazon Linux 2023 x86_64
			"https____cdn.amazonlinux.com_al2023_os-images_latest_kvm_al2023-kvm-2023.6.20250303.0-kernel-6.1-x86_64.xfs.gpt.qcow2/al2023-kvm-2023.6.20250303.0-kernel-6.1-x86_64.xfs.gpt.qcow2",
		},
	},
}

// resolveImage tries each candidate path under ~/.mock/cache and returns the
// absolute path to the first qcow2 that exists on disk. Returns "" if none.
func resolveImage(spec imageSpec) string {
	home := os.Getenv("HOME")
	cacheDir := filepath.Join(home, ".mock", "cache")

	for _, rel := range spec.candidates {
		p := filepath.Join(cacheDir, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// Fallback: scan the cache for any directory whose name contains the
	// distro keyword and pick the first qcow2 inside it.
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !strings.Contains(strings.ToLower(e.Name()), spec.distro) {
			continue
		}
		subEntries, _ := os.ReadDir(filepath.Join(cacheDir, e.Name()))
		for _, f := range subEntries {
			if strings.HasSuffix(f.Name(), ".qcow2") {
				return filepath.Join(cacheDir, e.Name(), f.Name())
			}
		}
	}
	return ""
}

// toRaw converts a qcow2 to a raw image adjacent to it, reusing a cached
// conversion. Fails the test on any error.
func toRaw(t *testing.T, src string) string {
	t.Helper()
	raw := strings.TrimSuffix(src, ".qcow2") + "-xfsstress.raw"
	qi, _ := os.Stat(src)
	ri, rerr := os.Stat(raw)
	if rerr != nil || (qi != nil && ri.ModTime().Before(qi.ModTime())) {
		t.Logf("converting %s → raw", filepath.Base(src))
		if err := disk_qcow2.ConvertToRaw(src, raw, os.Stdout); err != nil {
			t.Fatalf("disk_qcow2.ConvertToRaw: %v", err)
		}
	}
	return raw
}

// copyForWrite copies the shared read-only raw image to a fresh writable
// temp file. Each test gets its own copy so concurrent writes don't collide.
func copyForWrite(t *testing.T, raw string) string {
	t.Helper()
	in, err := os.Open(raw)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer in.Close()
	out, err := os.CreateTemp(t.TempDir(), "xfs-*.raw")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatalf("copy raw: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	return out.Name()
}
