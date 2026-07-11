package filesystem_xfs

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGrowPartialLastAG(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grow.img")
	fs, err := Format(path, testOneAG, FormatConfig{Label: "grow"})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	if err := fs.WriteFile("/before.txt", []byte("pre\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Grow to 1.5 AGs: one full AG plus a partial AG of half an AG.
	newSize := testOneAG + testOneAG/2
	if err := fs.Grow(newSize); err != nil {
		t.Fatalf("Grow(partial): %v", err)
	}
	xfs := fs.(*xfsFS)
	if xfs.sb.agCount != 2 {
		t.Fatalf("agCount = %d, want 2", xfs.sb.agCount)
	}
	wantDBlocks := uint64(newSize) / uint64(xfs.sb.blockSize)
	if xfs.sb.dBlocks != wantDBlocks {
		t.Fatalf("dBlocks = %d, want %d", xfs.sb.dBlocks, wantDBlocks)
	}
	// AG 0 is full, AG 1 is partial.
	if xfs.sb.agLength(0) != xfs.sb.agBlocks {
		t.Fatalf("AG0 length = %d, want %d", xfs.sb.agLength(0), xfs.sb.agBlocks)
	}
	if xfs.sb.agLength(1) != xfs.sb.agBlocks/2 {
		t.Fatalf("AG1 length = %d, want %d", xfs.sb.agLength(1), xfs.sb.agBlocks/2)
	}

	// Write enough files to spill into the new AG and re-read them.
	for i := 0; i < 20; i++ {
		p := "/post" + string(rune('a'+i))
		if err := fs.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", p, err)
		}
	}
	if got, err := fs.ReadFile("/before.txt"); err != nil || string(got) != "pre\n" {
		t.Fatalf("read before.txt: %q %v", got, err)
	}

	// Re-open to confirm the on-disk geometry round-trips.
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	re, err := Open(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if re.(*xfsFS).sb.dBlocks != wantDBlocks {
		t.Fatalf("reopened dBlocks = %d, want %d", re.(*xfsFS).sb.dBlocks, wantDBlocks)
	}
}

func TestGrowRejectsPartialCurrentAG(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grow.img")
	fs, err := Format(path, testOneAG, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	// First grow into a partial AG.
	if err := fs.Grow(testOneAG + testOneAG/2); err != nil {
		t.Fatal(err)
	}
	// Growing again is unsupported because the current last AG is partial.
	err = fs.Grow(3 * testOneAG)
	if err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("second grow = %v, want partial-AG error", err)
	}
}

func TestGrowRejectsTinyPartialAG(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grow.img")
	fs, err := Format(path, testOneAG, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	// One AG plus a handful of blocks: the partial AG is below the minimum.
	tiny := testOneAG + 8*4096
	if err := fs.Grow(tiny); err == nil {
		t.Fatal("Grow(tiny partial AG): want error")
	}
}

func TestGrowMisalignedRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grow.img")
	fs, err := Format(path, testOneAG, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	if err := fs.Grow(testOneAG + 100); err == nil {
		t.Fatal("Grow(non-block-aligned): want error")
	}
	if err := fs.Grow(testOneAG); err != nil {
		t.Fatalf("Grow(same size) should be a no-op: %v", err)
	}
	if err := fs.Grow(testOneAG / 2); err == nil {
		t.Fatal("Grow(shrink): want error")
	}
}

func TestAGLengthUnset(t *testing.T) {
	// A superblock with dBlocks unset falls back to full AGs.
	sb := &superblock{agBlocks: 100, dBlocks: 0}
	if sb.agLength(5) != 100 {
		t.Fatalf("agLength unset = %d, want 100", sb.agLength(5))
	}
	sb.dBlocks = 250 // 2 full AGs + 50
	if sb.agLength(0) != 100 || sb.agLength(1) != 100 {
		t.Fatal("full AGs mis-sized")
	}
	if sb.agLength(2) != 50 {
		t.Fatalf("partial AG = %d, want 50", sb.agLength(2))
	}
}
