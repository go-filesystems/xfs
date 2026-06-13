package filesystem_xfs

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestXfsKernelCompat_Bigtime verifies that the driver decodes the bigtime
// timestamp encoding used by modern mkfs.xfs (XFS_DIFLAG2_BIGTIME): a 64-bit
// nanosecond count since the bigtime epoch, rather than the legacy
// seconds+nanoseconds pair. Skipped without mkfs.xfs + loop-mount privileges.
func TestXfsKernelCompat_Bigtime(t *testing.T) {
	mkfs := findSbinTool("mkfs.xfs")
	if mkfs == "" {
		t.Skip("mkfs.xfs not available — skipping bigtime test")
	}
	if !canLoopMount() {
		t.Skip("need root / passwordless sudo to loop-mount — skipping")
	}

	img := filepath.Join(t.TempDir(), "bt.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(350 << 20); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if out, err := exec.Command(mkfs, "-f", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.xfs: %v\n%s", err, out)
	}

	// A timestamp far enough in the future that a legacy misread (which packs
	// seconds into the high 32 bits) lands on an obviously different date.
	want := time.Date(2023, 6, 15, 12, 34, 56, 0, time.UTC)
	mnt := t.TempDir()
	script := "set -e" +
		"; mount -o loop " + img + " " + mnt +
		"; touch -d '2023-06-15 12:34:56 UTC' " + mnt + "/f" +
		"; sync; umount " + mnt
	if out, err := sudoSh(script); err != nil {
		_, _ = sudoSh("umount " + mnt + " 2>/dev/null || true")
		t.Fatalf("kernel populate: %v\n%s", err, out)
	}

	fs, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()

	st, err := fs.ExtendedStat("/f")
	if err != nil {
		t.Fatalf("ExtendedStat: %v", err)
	}
	if !st.MTime.UTC().Equal(want) {
		t.Errorf("MTime = %s, want %s", st.MTime.UTC(), want)
	}
}
