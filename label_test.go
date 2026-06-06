package filesystem_xfs

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openFreshFS(t *testing.T) (FS, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	return fs, p
}

func TestXfsSetLabel_Roundtrip(t *testing.T) {
	fs, _ := openFreshFS(t)
	defer fs.Close()

	if err := fs.SetLabel("rootfs"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if got := fs.Label(); got != "rootfs" {
		t.Errorf("Label() = %q, want %q", got, "rootfs")
	}
}

func TestXfsSetLabel_PersistsAcrossReopen(t *testing.T) {
	fs, img := openFreshFS(t)
	if err := fs.SetLabel("data1"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	fs.Close()

	fs2, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs2.Close()
	if got := fs2.Label(); got != "data1" {
		t.Errorf("after reopen Label() = %q, want %q", got, "data1")
	}
}

func TestXfsSetLabel_RejectsTooLong(t *testing.T) {
	fs, _ := openFreshFS(t)
	defer fs.Close()

	// MaxLabelLen is 12 — anything past that must error and leave the
	// existing label untouched.
	before := fs.Label()
	if err := fs.SetLabel(strings.Repeat("x", MaxLabelLen+1)); err == nil {
		t.Error("SetLabel with oversize input unexpectedly succeeded")
	}
	if after := fs.Label(); after != before {
		t.Errorf("Label() changed after rejected SetLabel: %q -> %q", before, after)
	}
}

func TestXfsSetLabel_ShorterClearsTrailingBytes(t *testing.T) {
	fs, img := openFreshFS(t)
	if err := fs.SetLabel("longlabel"); err != nil { // 9 bytes
		t.Fatalf("first SetLabel: %v", err)
	}
	if err := fs.SetLabel("hi"); err != nil { // 2 bytes — shorter
		t.Fatalf("second SetLabel: %v", err)
	}
	fs.Close()

	// Verify the on-disk label slot is null-padded — no trailing
	// garbage from the previous longer label.
	f, err := os.Open(img)
	if err != nil {
		t.Fatalf("open img: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	want := append([]byte("hi"), make([]byte, MaxLabelLen-2)...)
	got := buf[labelFieldOffset : labelFieldOffset+MaxLabelLen]
	for i, b := range want {
		if got[i] != b {
			t.Errorf("on-disk label byte %d = 0x%02x, want 0x%02x (label slot = %q)",
				i, got[i], b, got)
			break
		}
	}
}

func TestXfsSetLabel_UpdatesCRC(t *testing.T) {
	fs, img := openFreshFS(t)
	if err := fs.SetLabel("crctest"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	fs.Close()

	// Reopen via the normal Open path — its readSuperblock has its own
	// magic check. A torn CRC wouldn't be caught here unless we
	// explicitly verify; so additionally validate by hand below.
	fs2, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open after SetLabel: %v", err)
	}
	fs2.Close()

	f, err := os.Open(img)
	if err != nil {
		t.Fatalf("open img: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	// Sanity-check magic on the raw bytes.
	if binary.BigEndian.Uint32(buf[sbOffMagic:]) != magicSB {
		t.Fatalf("magic on disk corrupted after SetLabel")
	}
	if !verifyCRC(buf, sbOffCRC, sbCRCLen) {
		t.Error("primary superblock CRC failed verification after SetLabel")
	}
}

func TestXfsSetLabel_PropagatesToAllAGSecondaries(t *testing.T) {
	// xfsTestSize formats with 1 AG, so this still exercises the loop
	// even though there's a single iteration. We additionally verify
	// each AG superblock carries the label.
	fs, img := openFreshFS(t)
	if err := fs.SetLabel("ag-sync"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	concrete := fs.(*xfsFS)
	agCount := concrete.sb.agCount
	blockSize := int64(concrete.sb.blockSize)
	agBlocks := int64(concrete.sb.agBlocks)
	fs.Close()

	f, err := os.Open(img)
	if err != nil {
		t.Fatalf("open img: %v", err)
	}
	defer f.Close()
	for ag := uint32(0); ag < agCount; ag++ {
		off := int64(ag) * agBlocks * blockSize
		buf := make([]byte, 512)
		if _, err := f.ReadAt(buf, off); err != nil {
			t.Fatalf("read AG %d: %v", ag, err)
		}
		if binary.BigEndian.Uint32(buf[sbOffMagic:]) != magicSB {
			t.Fatalf("AG %d superblock magic mismatch", ag)
		}
		lbl := strings.TrimRight(string(buf[labelFieldOffset:labelFieldOffset+MaxLabelLen]), "\x00")
		if lbl != "ag-sync" {
			t.Errorf("AG %d label = %q, want %q", ag, lbl, "ag-sync")
		}
		if !verifyCRC(buf, sbOffCRC, sbCRCLen) {
			t.Errorf("AG %d superblock CRC failed after SetLabel", ag)
		}
	}
}
