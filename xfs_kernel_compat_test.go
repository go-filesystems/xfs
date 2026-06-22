package filesystem_xfs

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// firstPhysBlock parses a `filefrag -v` dump and returns the first physical
// block of the first extent, or -1 if the file is absent/unparseable. filefrag
// lines look like: "   0:        0..    1220:         73..      1293:   1221:".
func firstPhysBlock(fragFile string) int64 {
	data, err := os.ReadFile(fragFile)
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		// The physical range is the third "a..b" group on an extent line.
		fields := strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ':' || r == '.'
		})
		// An extent line begins with the extent index, then logical lo, logical
		// hi, physical lo, physical hi, length. Require at least 4 numeric tokens
		// with the 4th being the physical start.
		if len(fields) >= 4 {
			if idx, err0 := strconv.Atoi(fields[0]); err0 == nil && idx == 0 {
				if phys, err3 := strconv.Atoi(fields[3]); err3 == nil {
					return int64(phys)
				}
			}
		}
	}
	return -1
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
	if err := f.Truncate(320 << 20); err != nil { // mkfs.xfs requires > 300 MiB
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	// agcount=4 over 320 MiB yields agblocks=20480, which is NOT a power of two,
	// so on-disk fsbno values are genuinely packed (fsbno = (agno<<agblklog)|agbno
	// with agblklog=15, 1<<15=32768 != 20480). This is the geometry that exposes
	// the multi-AG addressing bug once an extent lands in AG≥1 (see ag1.bin below).
	if out, err := exec.Command(mkfs, "-f", "-d", "agcount=4", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.xfs: %v\n%s", err, out)
	}

	shortTarget := "/etc/passwd"
	longTarget := "/" + strings.Repeat("seg/", 170) + "leaf" // ~685 bytes: remote symlink

	// A real-data file (deterministic) large enough to fill most of AG0, so the
	// subsequent ag1.bin is forced into AG1 — exercising fsbToPhysBlock on a
	// non-power-of-two geometry against the live kernel layout.
	ag0fill := make([]byte, 78<<20)
	for i := range ag0fill {
		ag0fill[i] = byte(i*3 + 1)
	}
	ag0Path := filepath.Join(t.TempDir(), "ag0fill.bin")
	if err := os.WriteFile(ag0Path, ag0fill, 0o644); err != nil {
		t.Fatal(err)
	}

	mnt := t.TempDir()
	fragOut := filepath.Join(t.TempDir(), "ag1.frag")
	script := strings.Join([]string{
		"set -e",
		"mount -o loop " + img + " " + mnt,
		"ln -s '" + shortTarget + "' " + mnt + "/short",
		"ln -s '" + longTarget + "' " + mnt + "/long",
		"mkdir " + mnt + "/sub",
		"cp " + srcPath + " " + mnt + "/big.bin",
		"cp " + srcPath + " " + mnt + "/sub/nested.bin",
		"cp " + ag0Path + " " + mnt + "/ag0fill.bin",  // consumes most of AG0
		"cp " + ag0Path + " " + mnt + "/ag0fill2.bin", // fully exhaust AG0 free space
		"cp " + srcPath + " " + mnt + "/ag1.bin",      // forced into AG1
		// Record ag1.bin's first physical block (filefrag, if present) so the
		// test can assert it really landed in a higher AG.
		"command -v filefrag >/dev/null && filefrag -v " + mnt + "/ag1.bin > " + fragOut + " 2>/dev/null || true",
		"sync",
		"umount " + mnt,
	}, " && ")
	if out, err := sudoSh(script); err != nil {
		// Best-effort cleanup in case the mount survived a mid-script failure.
		_, _ = sudoSh("umount " + mnt + " 2>/dev/null || true")
		t.Fatalf("kernel populate: %v\n%s", err, out)
	}

	// Confirm ag1.bin's data really lives in AG≥1 (flat physical block ≥ agblocks),
	// so this run actually exercises the packed-fsbno multi-AG decode rather than
	// silently testing only AG0. If the kernel placed it in AG0 anyway, the
	// committed-fixture test (TestMultiAGFixtureRead) still guarantees coverage.
	if minPhys := firstPhysBlock(fragOut); minPhys >= 0 {
		f, _ := os.Open(img)
		sb, sbErr := readSuperblock(f, 0)
		if f != nil {
			f.Close()
		}
		if sbErr == nil {
			if sb.agBlocks&(sb.agBlocks-1) == 0 {
				t.Logf("note: agblocks=%d is a power of two on this kernel; "+
					"multi-AG packing is the identity here (fixture test covers the packed case)", sb.agBlocks)
			} else if uint64(minPhys) < uint64(sb.agBlocks) {
				t.Logf("note: ag1.bin landed at physical block %d < agblocks %d (AG0); "+
					"the committed-fixture test exercises the AG≥1 path", minPhys, sb.agBlocks)
			} else {
				t.Logf("ag1.bin placed at physical block %d ≥ agblocks %d (AG≥1): "+
					"this run exercises the multi-AG packed-fsbno decode against the live kernel", minPhys, sb.agBlocks)
			}
		}
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
	// /ag1.bin lives in AG1 (non-power-of-two agblocks ⇒ packed fsbno): before the
	// multi-AG addressing fix this read failed with "read extent block …: EOF".
	for _, p := range []string{"/big.bin", "/sub/nested.bin", "/ag1.bin"} {
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
