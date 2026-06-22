// partition_test.go — whitebox unit tests for MBR/GPT partition detection.
//
// Partition parsing is delegated to the shared, hardened go-volumes/gpt
// package; these tests exercise the thin xfs adapter (partitionOffset /
// gptFirstLinux) over it, including the bare-image, auto-select, by-index, and
// not-found paths plus the device-size bounds that reject a hostile table.
package filesystem_xfs

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/go-volumes/gpt"
)

// testDeviceSize is a generous device size for the synthetic images below: the
// parser only reads the table header + entries (never partition payload), so a
// large ceiling lets the validated start offsets pass without materialising
// hundreds of MiB of zeros on disk.
const testDeviceSize = int64(1) << 40

// linuxPartTypeGPT is the Linux-filesystem GPT type GUID in on-disk byte order,
// re-exported from the shared parser for the table builders below.
var linuxPartTypeGPT = gpt.LinuxFilesystemGUID

// buildMBR returns a 1 MiB image (2048 sectors) with an MBR partition table.
// Each entry: {ptype, startLBA}; zero startLBA entries are skipped by the parser.
func buildMBR(entries []struct {
	ptype    uint8
	startLBA uint32
}) []byte {
	img := make([]byte, 1024*1024)
	img[510] = 0x55
	img[511] = 0xAA
	for i, e := range entries {
		if i >= 4 {
			break
		}
		off := 446 + i*16
		img[off+4] = e.ptype
		binary.LittleEndian.PutUint32(img[off+8:], e.startLBA)
		// One-sector length so the hardened parser sees a populated entry.
		binary.LittleEndian.PutUint32(img[off+12:], 1)
	}
	return img
}

// buildGPT returns a byte slice large enough to hold the GPT header at LBA 1
// and the partition table at partEntryLBA=2. Each entry occupies entrySize
// bytes. Partition extents are validated against testDeviceSize, not the
// returned slice length.
func buildGPT(entries []struct {
	typeGUID [16]byte
	startLBA uint64
}) []byte {
	const entrySize = 128
	numParts := uint32(128)
	partEntryLBA := uint64(2)

	size := int(partEntryLBA+1)*sectorSize + len(entries)*entrySize
	if size < 3*sectorSize {
		size = 3 * sectorSize
	}
	img := make([]byte, size)

	// GPT header at LBA 1.
	copy(img[512:], "EFI PART")
	binary.LittleEndian.PutUint64(img[512+72:], partEntryLBA)
	binary.LittleEndian.PutUint32(img[512+80:], numParts)
	binary.LittleEndian.PutUint32(img[512+84:], entrySize)

	// Partition entries at LBA 2.
	tableOff := int(partEntryLBA) * sectorSize
	for i, e := range entries {
		off := tableOff + i*entrySize
		copy(img[off:], e.typeGUID[:])
		binary.LittleEndian.PutUint64(img[off+32:], e.startLBA)
	}
	return img
}

// ──────────────────── bare image ──────────────────────────────────────────

func TestPartitionOffset_BareImage(t *testing.T) {
	// Neither MBR signature nor GPT magic — returns 0.
	img := make([]byte, 1024*1024)
	off, err := partitionOffset(bytes.NewReader(img), int64(len(img)), -1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	if off != 0 {
		t.Errorf("got %d, want 0 for bare image", off)
	}
}

func TestPartitionOffset_ZeroDeviceSize(t *testing.T) {
	// A backend that cannot report its size is treated as a bare image.
	img := buildMBR([]struct {
		ptype    uint8
		startLBA uint32
	}{{0x83, 2048}})
	off, err := partitionOffset(bytes.NewReader(img), 0, -1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	if off != 0 {
		t.Errorf("got %d, want 0 for zero device size", off)
	}
}

// ──────────────────── MBR ─────────────────────────────────────────────────

func TestPartitionOffset_MBR_AutoSelect(t *testing.T) {
	img := buildMBR([]struct {
		ptype    uint8
		startLBA uint32
	}{
		{0x83, 2048},
	})
	off, err := partitionOffset(bytes.NewReader(img), testDeviceSize, -1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	want := int64(2048) * sectorSize
	if off != want {
		t.Errorf("got %d, want %d", off, want)
	}
}

func TestPartitionOffset_MBR_SpecificIndex(t *testing.T) {
	img := buildMBR([]struct {
		ptype    uint8
		startLBA uint32
	}{
		{0x82, 2048}, // swap — should not be auto-selected
		{0x83, 4096}, // Linux
	})
	off, err := partitionOffset(bytes.NewReader(img), testDeviceSize, 0)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	// Index 0 = first entry regardless of type.
	want := int64(2048) * sectorSize
	if off != want {
		t.Errorf("got %d, want %d", off, want)
	}
}

func TestPartitionOffset_MBR_AutoSelectSkipsNonLinux(t *testing.T) {
	img := buildMBR([]struct {
		ptype    uint8
		startLBA uint32
	}{
		{0x82, 2048}, // swap
		{0x83, 4096}, // Linux
	})
	off, err := partitionOffset(bytes.NewReader(img), testDeviceSize, -1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	want := int64(4096) * sectorSize
	if off != want {
		t.Errorf("got %d, want %d", off, want)
	}
}

func TestPartitionOffset_MBR_IndexNotFound(t *testing.T) {
	img := buildMBR([]struct {
		ptype    uint8
		startLBA uint32
	}{
		{0x83, 2048},
	})
	_, err := partitionOffset(bytes.NewReader(img), testDeviceSize, 3)
	if err == nil {
		t.Error("expected error for out-of-range index, got nil")
	}
}

func TestPartitionOffset_MBR_NoLinuxPartition(t *testing.T) {
	// A swap-only MBR has no Linux partition; the auto-select fallback returns
	// the first populated entry instead of failing (still a bounds-checked
	// offset), matching the "give me the data partition" convenience.
	img := buildMBR([]struct {
		ptype    uint8
		startLBA uint32
	}{
		{0x82, 2048}, // swap only
	})
	off, err := partitionOffset(bytes.NewReader(img), testDeviceSize, -1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	want := int64(2048) * sectorSize
	if off != want {
		t.Errorf("got %d, want %d", off, want)
	}
}

// ──────────────────── GPT ─────────────────────────────────────────────────

func TestPartitionOffset_GPT_AutoSelect(t *testing.T) {
	entries := []struct {
		typeGUID [16]byte
		startLBA uint64
	}{
		{linuxPartTypeGPT, 2048},
	}
	img := buildGPT(entries)
	off, err := partitionOffset(bytes.NewReader(img), testDeviceSize, -1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	want := int64(2048) * sectorSize
	if off != want {
		t.Errorf("got %d, want %d", off, want)
	}
}

func TestPartitionOffset_GPT_SpecificIndex(t *testing.T) {
	var efiGUID [16]byte
	efiGUID[0] = 0xC1 // some non-Linux GUID

	entries := []struct {
		typeGUID [16]byte
		startLBA uint64
	}{
		{efiGUID, 2048},
		{linuxPartTypeGPT, 1052672},
	}
	img := buildGPT(entries)
	// Ask for index 1 explicitly.
	off, err := partitionOffset(bytes.NewReader(img), testDeviceSize, 1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	want := int64(1052672) * sectorSize
	if off != want {
		t.Errorf("got %d, want %d", off, want)
	}
}

func TestPartitionOffset_GPT_AutoSelectSkipsEFI(t *testing.T) {
	var efiGUID [16]byte
	efiGUID[0] = 0xC1

	entries := []struct {
		typeGUID [16]byte
		startLBA uint64
	}{
		{efiGUID, 2048},
		{linuxPartTypeGPT, 4096},
	}
	img := buildGPT(entries)
	off, err := partitionOffset(bytes.NewReader(img), testDeviceSize, -1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	want := int64(4096) * sectorSize
	if off != want {
		t.Errorf("got %d, want %d", off, want)
	}
}

func TestPartitionOffset_GPT_IndexNotFound(t *testing.T) {
	entries := []struct {
		typeGUID [16]byte
		startLBA uint64
	}{
		{linuxPartTypeGPT, 2048},
	}
	img := buildGPT(entries)
	_, err := partitionOffset(bytes.NewReader(img), testDeviceSize, 5)
	if err == nil {
		t.Error("expected error for out-of-range GPT index, got nil")
	}
}

func TestPartitionOffset_GPT_SkipsEmptyTypeGUID(t *testing.T) {
	var emptyGUID [16]byte

	entries := []struct {
		typeGUID [16]byte
		startLBA uint64
	}{
		{emptyGUID, 2048}, // unused slot
		{linuxPartTypeGPT, 4096},
	}
	img := buildGPT(entries)
	off, err := partitionOffset(bytes.NewReader(img), testDeviceSize, -1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	want := int64(4096) * sectorSize
	if off != want {
		t.Errorf("got %d, want %d", off, want)
	}
}

// TestPartitionOffset_GPT_NoLinuxFallback covers a GPT whose only populated
// entry is a non-Linux type: the auto-select falls back to the first populated
// partition.
func TestPartitionOffset_GPT_NoLinuxFallback(t *testing.T) {
	var efiGUID [16]byte
	efiGUID[0] = 0xC1
	img := buildGPT([]struct {
		typeGUID [16]byte
		startLBA uint64
	}{
		{efiGUID, 8192},
	})
	off, err := partitionOffset(bytes.NewReader(img), testDeviceSize, -1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	want := int64(8192) * sectorSize
	if off != want {
		t.Errorf("got %d, want %d", off, want)
	}
}

// TestPartitionOffset_GPT_Malformed feeds a hostile entry size so the hardened
// parser rejects the table; partitionOffset must surface the error, not panic.
func TestPartitionOffset_GPT_Malformed(t *testing.T) {
	img := buildGPT(nil)
	binary.LittleEndian.PutUint32(img[512+84:], 64) // entry size below MinEntrySize
	if _, err := partitionOffset(bytes.NewReader(img), testDeviceSize, -1); err == nil {
		t.Fatal("expected GPT entry-size validation error, got nil")
	}
}

// TestPartitionOffset_HeaderReadError ensures a backend that fails the header
// read returns an error rather than panicking.
func TestPartitionOffset_HeaderReadError(t *testing.T) {
	// A short device whose GPT signature is present but header is truncated.
	img := make([]byte, 600)
	copy(img[512:], "EFI PART")
	if _, err := partitionOffset(bytes.NewReader(img), int64(len(img)), -1); err == nil {
		t.Fatal("expected truncated-header error, got nil")
	}
}
