package filesystem_xfs

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// findSbinTool resolves a tool that may live in an sbin directory (mkfs.xfs and
// friends are typically /usr/sbin on Linux CI).
func findSbinTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, d := range []string{"/usr/local/sbin", "/usr/sbin", "/sbin"} {
		c := filepath.Join(d, name)
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// canLoopMount reports whether the test can create a loopback mount: it needs
// to be root, or have passwordless sudo.
func canLoopMount() bool {
	if os.Geteuid() == 0 {
		return true
	}
	return exec.Command("sudo", "-n", "true").Run() == nil
}

// sudoSh runs a /bin/sh script with root privileges (directly when already
// root, otherwise via sudo).
func sudoSh(script string) ([]byte, error) {
	if os.Geteuid() == 0 {
		return exec.Command("sh", "-c", script).CombinedOutput()
	}
	return exec.Command("sudo", "sh", "-c", script).CombinedOutput()
}

// TestXfsKernelCompat_Read formats a real XFS image with mkfs.xfs, populates it
// through the Linux kernel (symlinks, a multi-block regular file, a nested
// directory), then verifies this driver can read it all back.
//
// Modern mkfs.xfs enables the NREXT64 feature (64-bit di_big_nextents) and
// stores remote symlink blocks with an xfs_dsymlink_hdr — neither of which the
// driver handled before. This test is the regression guard for both: it is
// skipped when mkfs.xfs or loop-mount privileges are unavailable.
func TestXfsKernelCompat_Read(t *testing.T) {
	mkfs := findSbinTool("mkfs.xfs")
	if mkfs == "" {
		t.Skip("mkfs.xfs not available — skipping XFS kernel cross-compat test")
	}
	if !canLoopMount() {
		t.Skip("need root / passwordless sudo to loop-mount — skipping")
	}

	// Deterministic content for the regular files (256 KiB → spans many blocks).
	content := make([]byte, 256*1024)
	for i := range content {
		content[i] = byte(i*7 + 3)
	}
	srcPath := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	img := filepath.Join(t.TempDir(), "xfs.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(350 << 20); err != nil { // mkfs.xfs requires > 300 MiB
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if out, err := exec.Command(mkfs, "-f", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.xfs: %v\n%s", err, out)
	}

	shortTarget := "/etc/passwd"
	longTarget := "/" + strings.Repeat("seg/", 170) + "leaf" // ~685 bytes: remote symlink

	mnt := t.TempDir()
	script := strings.Join([]string{
		"set -e",
		"mount -o loop " + img + " " + mnt,
		"ln -s '" + shortTarget + "' " + mnt + "/short",
		"ln -s '" + longTarget + "' " + mnt + "/long",
		"mkdir " + mnt + "/sub",
		"cp " + srcPath + " " + mnt + "/big.bin",
		"cp " + srcPath + " " + mnt + "/sub/nested.bin",
		"sync",
		"umount " + mnt,
	}, " && ")
	if out, err := sudoSh(script); err != nil {
		// Best-effort cleanup in case the mount survived a mid-script failure.
		_, _ = sudoSh("umount " + mnt + " 2>/dev/null || true")
		t.Fatalf("kernel populate: %v\n%s", err, out)
	}

	fs, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open kernel-created XFS image: %v", err)
	}
	defer fs.Close()

	// Symlinks: short (inline/local) and long (remote/extent with XSLM header).
	if got, err := fs.ReadLink("/short"); err != nil || got != shortTarget {
		t.Errorf("ReadLink(/short) = %q, %v; want %q", got, err, shortTarget)
	}
	if got, err := fs.ReadLink("/long"); err != nil || got != longTarget {
		t.Errorf("ReadLink(/long) = %d bytes, %v; want %d bytes", len(got), err, len(longTarget))
	}

	// Regular file read exercises the NREXT64 extent-count path: before the fix
	// the extent count read as 0 and the file came back all-zero.
	for _, p := range []string{"/big.bin", "/sub/nested.bin"} {
		got, err := fs.ReadFile(p)
		if err != nil {
			t.Errorf("ReadFile(%s): %v", p, err)
			continue
		}
		if !bytes.Equal(got, content) {
			t.Errorf("ReadFile(%s): %d bytes, content mismatch (want %d)", p, len(got), len(content))
		}
	}

	// Directory listing through the kernel-written root.
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
	}
	for _, name := range []string{"short", "long", "sub", "big.bin"} {
		if !seen[name] {
			t.Errorf("ListDir(/) missing %q", name)
		}
	}
}
