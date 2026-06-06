package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// TestXfsGrowTo verifies that GrowTo extends the backing file, updates the
// AG count and can be re-opened successfully.
func TestXfsGrowTo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xfs-grow.img")
	ifs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer ifs.Close()
	fs := ifs.(*xfsFS)

	initialAgCount := fs.sb.agCount
	newSize := xfsTestSize * 2
	if err := fs.GrowTo(newSize); err != nil {
		t.Fatalf("GrowTo: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() != newSize {
		t.Fatalf("size = %d, want %d", st.Size(), newSize)
	}

	ifs2, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open after grow: %v", err)
	}
	defer ifs2.Close()
	fs2 := ifs2.(*xfsFS)
	if fs2.sb.agCount != initialAgCount*2 {
		t.Fatalf("ag count = %d, want %d", fs2.sb.agCount, initialAgCount*2)
	}
}

// TestXfsGrow_IsGrowToAlias asserts that Grow and GrowTo are
// interchangeable entry points (Grow is the canonical name; GrowTo is
// kept as a compatibility shim against filesystem.Grower).
func TestXfsGrow_IsGrowToAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xfs-grow.img")
	ifs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer ifs.Close()
	fs := ifs.(*xfsFS)

	// Calling Grow directly produces the same on-disk effect as GrowTo.
	newSize := xfsTestSize * 3
	if err := fs.Grow(newSize); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	if fs.sb.agCount != 3 {
		t.Fatalf("after Grow: agCount=%d, want 3", fs.sb.agCount)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() != newSize {
		t.Fatalf("size = %d, want %d", st.Size(), newSize)
	}
}

// TestXfsGrow_EqualSizeIsNoop ensures a Grow call targeting the current
// size returns nil and does not perturb on-disk geometry.
func TestXfsGrow_EqualSizeIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "noop.img")
	ifs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer ifs.Close()
	fs := ifs.(*xfsFS)

	before := fs.sb.agCount
	if err := fs.Grow(xfsTestSize); err != nil {
		t.Fatalf("Grow(equal): %v", err)
	}
	if fs.sb.agCount != before {
		t.Fatalf("Grow(equal) changed agCount: %d -> %d", before, fs.sb.agCount)
	}
}

// TestXfsGrow_PartialAGRejected asserts XFS's "whole-AG only" growth
// rule: a request that lands strictly between two AG boundaries is
// rejected with a clear error and the on-disk size is left untouched.
func TestXfsGrow_PartialAGRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.img")
	ifs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer ifs.Close()
	fs := ifs.(*xfsFS)

	// xfsTestSize is exactly 1 AG; xfsTestSize + 4096 is 1 AG + 1 block.
	bad := xfsTestSize + fmtBlockSize
	err = fs.Grow(bad)
	if err == nil {
		t.Fatal("Grow(partial-AG) unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "AG") {
		t.Fatalf("error %q should mention the AG-alignment requirement", err)
	}
	// agCount must NOT have changed.
	if fs.sb.agCount != 1 {
		t.Fatalf("agCount after rejected Grow = %d, want 1", fs.sb.agCount)
	}
}

// TestXfsGrow_BlockMisaligned rejects sizes that are not multiples of
// the filesystem block size.
func TestXfsGrow_BlockMisaligned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "misaligned.img")
	ifs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer ifs.Close()
	fs := ifs.(*xfsFS)
	if err := fs.Grow(xfsTestSize + 1); err == nil {
		t.Fatal("Grow(+1 byte) unexpectedly succeeded")
	}
}

// TestXfsGrow_ShrinkErrorViaGrow asserts the direct Grow() entry point
// returns a descriptive error (not the sentinel) when called with a
// shrink target. The sentinel is reserved for the Resize() path.
func TestXfsGrow_ShrinkErrorViaGrow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shrink-grow.img")
	ifs, err := Format(path, 2*xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer ifs.Close()
	fs := ifs.(*xfsFS)

	err = fs.Grow(xfsTestSize)
	if err == nil {
		t.Fatal("Grow(shrink) unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "shrink") {
		t.Fatalf("Grow(shrink) error = %q, expected to mention 'shrink'", err)
	}
}

// TestXfsResize_ShrinkReturnsSentinel asserts that Resize() returns the
// uniform filesystem.ErrShrinkUnsupported sentinel for any newSize
// strictly less than the current size. Callers that pivot on this
// error (e.g. the diskimage CLI) must be able to use errors.Is().
func TestXfsResize_ShrinkReturnsSentinel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shrink.img")
	// Format 2 AGs so we have room to "shrink" back to 1.
	ifs, err := Format(path, 2*xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer ifs.Close()
	fs := ifs.(*xfsFS)

	err = fs.Resize(xfsTestSize) // shrink from 2 AGs to 1 AG
	if !errors.Is(err, filesystem.ErrShrinkUnsupported) {
		t.Fatalf("Resize(shrink) = %v, want ErrShrinkUnsupported", err)
	}

	// State must be untouched: still 2 AGs.
	if fs.sb.agCount != 2 {
		t.Fatalf("Resize(shrink) left agCount=%d, want 2 (unchanged)", fs.sb.agCount)
	}
}

// TestXfsResize_GrowDispatch covers the growth side of Resize: it should
// behave identically to Grow when newSize >= current.
func TestXfsResize_GrowDispatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resize.img")
	ifs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer ifs.Close()
	fs := ifs.(*xfsFS)

	// Equal-size resize is a no-op.
	if err := fs.Resize(xfsTestSize); err != nil {
		t.Fatalf("Resize(equal): %v", err)
	}
	if fs.sb.agCount != 1 {
		t.Fatalf("after Resize(equal): agCount=%d, want 1", fs.sb.agCount)
	}

	// Resize bigger routes to Grow.
	if err := fs.Resize(4 * xfsTestSize); err != nil {
		t.Fatalf("Resize(4x): %v", err)
	}
	if fs.sb.agCount != 4 {
		t.Fatalf("after Resize(4x): agCount=%d, want 4", fs.sb.agCount)
	}
}

// TestXfsResize_SatisfiesResizerInterface asserts the FS exposed by
// Open also satisfies the package-level filesystem.Resizer probe — the
// uniform capability check used by the generic CLI dispatcher.
func TestXfsResize_SatisfiesResizerInterface(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iface.img")
	ifs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer ifs.Close()

	r, ok := ifs.(filesystem.Resizer)
	if !ok {
		t.Fatal("xfs FS does not satisfy filesystem.Resizer")
	}
	// Sentinel must round-trip through the interface.
	if err := r.Resize(0); !errors.Is(err, filesystem.ErrShrinkUnsupported) {
		t.Fatalf("Resizer.Resize(0) = %v, want ErrShrinkUnsupported", err)
	}
}

// TestXfsGrow_SecondarySBPresent verifies that, after growing the
// filesystem, every newly-appended AG has a valid secondary superblock
// at block 0 — that's what xfs_repair and SetLabel both look for, and
// it's the part the pre-Grow writer did not lay down.
func TestXfsGrow_SecondarySBPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secondary.img")
	ifs, err := Format(path, xfsTestSize, FormatConfig{Label: "growsec"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := ifs.(*xfsFS)
	if err := fs.Grow(4 * xfsTestSize); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	ifs.Close()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	// AG 0 secondary == primary; we test AGs 1..3.
	agSize := int64(fmtAgBlocks) * fmtBlockSize
	for ag := int64(1); ag < 4; ag++ {
		off := ag * agSize
		buf := make([]byte, 512)
		if _, err := f.ReadAt(buf, off); err != nil {
			t.Fatalf("read AG %d secondary SB: %v", ag, err)
		}
		if binary.BigEndian.Uint32(buf[sbOffMagic:]) != magicSB {
			t.Fatalf("AG %d: missing/wrong superblock magic", ag)
		}
		if got := binary.BigEndian.Uint32(buf[sbOffAgCount:]); got != 4 {
			t.Fatalf("AG %d: agCount = %d, want 4", ag, got)
		}
		if !verifyCRC(buf, sbOffCRC, sbCRCLen) {
			t.Fatalf("AG %d: secondary SB failed CRC verification", ag)
		}
	}
}

// TestXfsGrow_SetLabelTouchesGrownAGs is a "did the Grow path lay down
// the secondary SBs SetLabel will look for" regression: write a label
// AFTER a Grow and check every AG's on-disk SB carries the new label.
// Pre-Grow this would have failed in AGs 1..N because their block 0
// was zeroed.
func TestXfsGrow_SetLabelTouchesGrownAGs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grow-then-label.img")
	ifs, err := Format(path, xfsTestSize, FormatConfig{Label: "before"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs := ifs.(*xfsFS)
	if err := fs.Grow(3 * xfsTestSize); err != nil {
		t.Fatalf("Grow: %v", err)
	}
	if err := fs.SetLabel("after"); err != nil {
		t.Fatalf("SetLabel after Grow: %v", err)
	}
	ifs.Close()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	agSize := int64(fmtAgBlocks) * fmtBlockSize
	for ag := int64(0); ag < 3; ag++ {
		off := ag * agSize
		buf := make([]byte, 512)
		if _, err := f.ReadAt(buf, off); err != nil {
			t.Fatalf("read AG %d SB: %v", ag, err)
		}
		got := strings.TrimRight(string(buf[labelFieldOffset:labelFieldOffset+MaxLabelLen]), "\x00")
		if got != "after" {
			t.Errorf("AG %d label = %q, want \"after\"", ag, got)
		}
	}
}

// fakeWriterAt is a tiny writerAtOnly that always fails, used to drive
// the fmtWriteSecondarySB error path in unit tests.
type fakeWriterAt struct{ err error }

func (f *fakeWriterAt) WriteAt(p []byte, off int64) (int, error) {
	return 0, f.err
}

// TestFmtWriteSecondarySB_AG0Noop and the WriteAt-failure case round
// out coverage of the secondary-SB writer's two non-happy branches.
func TestFmtWriteSecondarySB_AG0Noop(t *testing.T) {
	sb := &superblock{
		blockSize: fmtBlockSize, agBlocks: fmtAgBlocks, agCount: 1,
		inodeSize: fmtInodeSize, inopBlock: fmtInopBlock,
		inopBLog: fmtInopBLog, agBlkLog: fmtAgBlkLog, hasCRC: true,
	}
	// ag = 0 must return nil without touching the writer (the primary SB
	// owns block 0 of AG 0; rewriting from here would double-stamp).
	w := &fakeWriterAt{err: errors.New("must not be called")}
	if err := fmtWriteSecondarySB(w, 0, sb, 0, 1, [16]byte{}, ""); err != nil {
		t.Fatalf("fmtWriteSecondarySB(ag=0) = %v, want nil", err)
	}
}

func TestFmtWriteSecondarySB_WriteFails(t *testing.T) {
	sb := &superblock{
		blockSize: fmtBlockSize, agBlocks: fmtAgBlocks, agCount: 2,
		inodeSize: fmtInodeSize, inopBlock: fmtInopBlock,
		inopBLog: fmtInopBLog, agBlkLog: fmtAgBlkLog, hasCRC: true,
	}
	boom := errors.New("write boom")
	w := &fakeWriterAt{err: boom}
	err := fmtWriteSecondarySB(w, 0, sb, 1, 2, [16]byte{}, "")
	if err == nil {
		t.Fatal("fmtWriteSecondarySB unexpectedly succeeded with failing writer")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error chain missing boom sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "AG 1") {
		t.Fatalf("error %q should identify the failing AG", err)
	}
}

// TestXfsGrow_WriteAfterGrow exercises the inode/extent path on the
// grown AGs: keep writing files until enough land past the initial AG.
// allocInode now grows the inobt and Format only seeds AG 0, so the
// extra AGs added by Grow must be usable end-to-end (allocator chooses
// AG 0 first, then spills as it fills).
func TestXfsGrow_WriteAfterGrow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grow-write.img")
	ifs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer ifs.Close()
	fs := ifs.(*xfsFS)

	if err := fs.Grow(3 * xfsTestSize); err != nil {
		t.Fatalf("Grow: %v", err)
	}

	// Write a modest fan of small files; with the inobt grow path in
	// place this comfortably exceeds AG 0's seed budget.
	for i := 0; i < 30; i++ {
		p := fmt.Sprintf("/post-grow-%02d.txt", i)
		body := []byte(fmt.Sprintf("file %d after grow", i))
		if err := fs.WriteFile(p, body, 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", p, err)
		}
	}
	for i := 0; i < 30; i++ {
		p := fmt.Sprintf("/post-grow-%02d.txt", i)
		want := []byte(fmt.Sprintf("file %d after grow", i))
		got, err := fs.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", p, err)
		}
		if string(got) != string(want) {
			t.Fatalf("ReadFile %s: got %q want %q", p, got, want)
		}
	}
}
