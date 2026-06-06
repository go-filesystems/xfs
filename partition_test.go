// partition_test.go — whitebox unit tests for MBR/GPT partition detection.
package filesystem_xfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// ──────────────────── helpers ──────────────────────────────────────────────

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
	}
	return img
}

// buildGPT returns a byte slice large enough to hold GPT header at LBA 1 and
// the partition table at partEntryLBA=2. Each entry occupies entrySize bytes.
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
	off, err := partitionOffset(bytes.NewReader(img), -1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	if off != 0 {
		t.Errorf("got %d, want 0 for bare image", off)
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
	off, err := partitionOffset(bytes.NewReader(img), -1)
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
	off, err := partitionOffset(bytes.NewReader(img), 0)
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
	off, err := partitionOffset(bytes.NewReader(img), -1)
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
	_, err := partitionOffset(bytes.NewReader(img), 3)
	if err == nil {
		t.Error("expected error for out-of-range index, got nil")
	}
}

func TestPartitionOffset_MBR_NoLinuxPartition(t *testing.T) {
	img := buildMBR([]struct {
		ptype    uint8
		startLBA uint32
	}{
		{0x82, 2048}, // swap only
	})
	_, err := partitionOffset(bytes.NewReader(img), -1)
	if err == nil {
		t.Error("expected error for MBR with no Linux partition, got nil")
	}
}

func TestPartitionOffset_MBR_ZeroStartLBASkipped(t *testing.T) {
	// A zero-startLBA entry must be ignored even if ptype is 0x83.
	img := buildMBR([]struct {
		ptype    uint8
		startLBA uint32
	}{
		{0x83, 0},    // invalid
		{0x83, 2048}, // valid
	})
	off, err := partitionOffset(bytes.NewReader(img), -1)
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
	off, err := partitionOffset(bytes.NewReader(img), -1)
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
	off, err := partitionOffset(bytes.NewReader(img), 1)
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
	off, err := partitionOffset(bytes.NewReader(img), -1)
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
	_, err := partitionOffset(bytes.NewReader(img), 5)
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
	off, err := partitionOffset(bytes.NewReader(img), -1)
	if err != nil {
		t.Fatalf("partitionOffset: %v", err)
	}
	want := int64(4096) * sectorSize
	if off != want {
		t.Errorf("got %d, want %d", off, want)
	}
}

type partitionErrReaderAt struct {
	err error
}

func (r partitionErrReaderAt) ReadAt([]byte, int64) (int, error) {
	return 0, r.err
}

func TestPartitionOffset_GPTAdditionalErrors(t *testing.T) {
	if _, err := gptPartOffset(partitionErrReaderAt{err: errBoom}, -1); !errors.Is(err, errBoom) {
		t.Fatalf("expected GPT header read error %v, got %v", errBoom, err)
	}

	img := buildGPT(nil)
	binary.LittleEndian.PutUint32(img[512+84:], 64)
	if _, err := gptPartOffset(bytes.NewReader(img), -1); err == nil {
		t.Fatal("expected GPT entry size validation error")
	}

	var efiGUID [16]byte
	efiGUID[0] = 0xC1
	img = buildGPT([]struct {
		typeGUID [16]byte
		startLBA uint64
	}{
		{efiGUID, 2048},
	})
	if _, err := gptPartOffset(bytes.NewReader(img), -1); err == nil {
		t.Fatal("expected GPT auto-select to fail with no Linux data partition")
	}
}

func TestPartitionOffset_MBRAdditionalReadError(t *testing.T) {
	if _, err := mbrPartOffset(partitionErrReaderAt{err: errBoom}, -1); !errors.Is(err, errBoom) {
		t.Fatalf("expected MBR table read error %v, got %v", errBoom, err)
	}
}
