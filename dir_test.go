// dir_test.go — whitebox unit tests for directory-related functions.
package filesystem_xfs

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// ──────────────────── dirEntrySize ─────────────────────────────────────────

func TestDirEntrySize_NoFType(t *testing.T) {
	// (8+1+namelen+2+7)&^7 without ftype
	tests := []struct{ nl, want int }{
		{1, 16},  // (8+1+1+2+7)=19 &^7=16
		{3, 16},  // (8+1+3+2+7)=21 &^7=16
		{7, 24},  // (8+1+7+2+7)=25 &^7=24
		{8, 24},  // (8+1+8+2+7)=26 &^7=24
		{12, 24}, // (8+1+12+2+7)=30 &^7=24
		{14, 32}, // (8+1+14+2+7)=32 &^7=32
	}
	for _, tc := range tests {
		got := dirEntrySize(tc.nl, false)
		if got != tc.want {
			t.Errorf("dirEntrySize(%d, false) = %d, want %d", tc.nl, got, tc.want)
		}
	}
}

func TestDirEntrySize_WithFType(t *testing.T) {
	// Additional ftype byte shifts the rounding boundary.
	tests := []struct{ nl, want int }{
		{1, 16},  // (8+1+1+1+2+7)=20 &^7=16
		{7, 24},  // (8+1+7+1+2+7)=26 &^7=24
		{8, 24},  // (8+1+8+1+2+7)=27 &^7=24
		{11, 24}, // (8+1+11+1+2+7)=30 &^7=24
		{13, 32}, // (8+1+13+1+2+7)=32 &^7=32
	}
	for _, tc := range tests {
		got := dirEntrySize(tc.nl, true)
		if got != tc.want {
			t.Errorf("dirEntrySize(%d, true) = %d, want %d", tc.nl, got, tc.want)
		}
	}
}

// ──────────────────── sfLookup ─────────────────────────────────────────────

// buildSFDir constructs a short-form directory data fork for testing.
// All inodes fit in 4 bytes (i8count=0).
// entries: each is (name, ino) pair; parent is the parent ino.
func buildSFDir(parent uint32, entries []struct {
	name string
	ino  uint32
}, hasFType bool) []byte {
	var buf bytes.Buffer
	buf.WriteByte(byte(len(entries))) // count
	buf.WriteByte(0)                  // i8count = 0

	// parent inode (4 bytes, BE)
	var pb [4]byte
	binary.BigEndian.PutUint32(pb[:], parent)
	buf.Write(pb[:])

	for _, e := range entries {
		// namelen(1) + sfOffset(2) + name + ftype?(1) + ino(4)
		buf.WriteByte(byte(len(e.name)))
		buf.WriteByte(0) // sfOffset high
		buf.WriteByte(byte(buf.Len()))
		buf.WriteString(e.name)
		if hasFType {
			buf.WriteByte(1) // DT_REG
		}
		var ib [4]byte
		binary.BigEndian.PutUint32(ib[:], e.ino)
		buf.Write(ib[:])
	}
	return buf.Bytes()
}

func TestSFLookup_Found(t *testing.T) {
	entries := []struct {
		name string
		ino  uint32
	}{
		{"grub.cfg", 256},
		{"vmlinuz", 512},
	}
	fork := buildSFDir(128, entries, true)

	ino, err := sfLookup(fork, "grub.cfg", true)
	if err != nil {
		t.Fatalf("sfLookup: %v", err)
	}
	if ino != 256 {
		t.Errorf("ino: got %d, want 256", ino)
	}
}

func TestSFLookup_SecondEntry(t *testing.T) {
	entries := []struct {
		name string
		ino  uint32
	}{
		{"grub.cfg", 256},
		{"vmlinuz", 512},
	}
	fork := buildSFDir(128, entries, true)

	ino, err := sfLookup(fork, "vmlinuz", true)
	if err != nil {
		t.Fatalf("sfLookup: %v", err)
	}
	if ino != 512 {
		t.Errorf("ino: got %d, want 512", ino)
	}
}

func TestSFLookup_NotFound(t *testing.T) {
	entries := []struct {
		name string
		ino  uint32
	}{
		{"grub.cfg", 256},
	}
	fork := buildSFDir(128, entries, true)
	_, err := sfLookup(fork, "missing", true)
	if err == nil {
		t.Error("expected ErrNotFound, got nil")
	}
}

func TestSFLookup_NoFType(t *testing.T) {
	entries := []struct {
		name string
		ino  uint32
	}{
		{"os-release", 300},
	}
	fork := buildSFDir(128, entries, false)
	ino, err := sfLookup(fork, "os-release", false)
	if err != nil {
		t.Fatalf("sfLookup: %v", err)
	}
	if ino != 300 {
		t.Errorf("ino: got %d, want 300", ino)
	}
}

func TestSFLookup_TooShort(t *testing.T) {
	_, err := sfLookup([]byte{1, 0, 0, 0}, "x", true)
	if err == nil {
		t.Error("expected ErrNotFound for too-short fork, got nil")
	}
}

func TestSFLookup_Empty(t *testing.T) {
	fork := buildSFDir(128, nil, true)
	_, err := sfLookup(fork, "anything", true)
	if err == nil {
		t.Error("expected ErrNotFound for empty dir, got nil")
	}
}

// ──────────────────── sfReadDir ────────────────────────────────────────────

func TestSFReadDir_AllEntries(t *testing.T) {
	entries := []struct {
		name string
		ino  uint32
	}{
		{"grub.cfg", 256},
		{"vmlinuz", 512},
		{"initrd.img", 768},
	}
	fork := buildSFDir(128, entries, true)
	got, err := sfReadDir(fork, true)
	if err != nil {
		t.Fatalf("sfReadDir: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}

	want := map[string]uint64{"grub.cfg": 256, "vmlinuz": 512, "initrd.img": 768}
	for _, de := range got {
		wantIno, ok := want[de.Name]
		if !ok {
			t.Errorf("unexpected entry %q", de.Name)
			continue
		}
		if de.Inode != wantIno {
			t.Errorf("entry %q: ino=%d, want %d", de.Name, de.Inode, wantIno)
		}
		if de.FileType != 1 {
			t.Errorf("entry %q: ftype=%d, want 1", de.Name, de.FileType)
		}
	}
}

func TestSFReadDir_NoFType(t *testing.T) {
	entries := []struct {
		name string
		ino  uint32
	}{
		{"etc", 256},
	}
	fork := buildSFDir(128, entries, false)
	got, err := sfReadDir(fork, false)
	if err != nil {
		t.Fatalf("sfReadDir: %v", err)
	}
	if len(got) != 1 || got[0].Name != "etc" || got[0].Inode != 256 {
		t.Errorf("got %+v, want [{etc 256 0}]", got)
	}
}

func TestSFReadDir_Empty(t *testing.T) {
	fork := buildSFDir(128, nil, true)
	got, err := sfReadDir(fork, true)
	if err != nil {
		t.Fatalf("sfReadDir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 entries, got %d", len(got))
	}
}

// ──────────────────── parseDirBlock ────────────────────────────────────────

// buildDirBlock constructs a v5 directory data block (64-byte header
// followed by directory entries). All entries are "used" (no free slots).
func buildDirBlock(entries []struct {
	ino  uint64
	name string
	ft   uint8
}, hasFType bool) []byte {
	blk := make([]byte, 4096)
	// v5 header magic at offset 0.
	binary.BigEndian.PutUint32(blk[0:], magicDir3Data)
	// CRC at offset 4 — left as 0 for unit tests (not verified during parse).

	off := dir3DataHdrSize
	for _, e := range entries {
		sz := dirEntrySize(len(e.name), hasFType)
		binary.BigEndian.PutUint64(blk[off:], e.ino)
		blk[off+8] = byte(len(e.name))
		copy(blk[off+9:], e.name)
		if hasFType {
			blk[off+9+len(e.name)] = e.ft
		}
		// tag at the end of the entry.
		binary.BigEndian.PutUint16(blk[off+sz-2:], uint16(off))
		off += sz
	}

	// Mark remaining space as one big free slot.
	remain := 4096 - 8 - off // leave 8 bytes for block-form tail at end
	if remain >= 8 {
		binary.BigEndian.PutUint16(blk[off:], dirFreeTag)
		binary.BigEndian.PutUint16(blk[off+2:], uint16(remain))
	}
	return blk
}

func TestParseDirBlock_SingleEntry(t *testing.T) {
	entries := []struct {
		ino  uint64
		name string
		ft   uint8
	}{
		{256, "grub.cfg", 1},
	}
	blk := buildDirBlock(entries, true)
	got := parseDirBlock(blk, true, true)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Name != "grub.cfg" {
		t.Errorf("name: got %q, want %q", got[0].Name, "grub.cfg")
	}
	if got[0].Inode != 256 {
		t.Errorf("ino: got %d, want 256", got[0].Inode)
	}
	if got[0].FileType != 1 {
		t.Errorf("ftype: got %d, want 1", got[0].FileType)
	}
}

func TestParseDirBlock_MultipleEntries(t *testing.T) {
	entries := []struct {
		ino  uint64
		name string
		ft   uint8
	}{
		{100, "a", 1},
		{200, "bb", 2},
		{300, "ccc", 4},
	}
	blk := buildDirBlock(entries, true)
	got := parseDirBlock(blk, true, true)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	names := map[string]uint64{"a": 100, "bb": 200, "ccc": 300}
	for _, de := range got {
		wantIno, ok := names[de.Name]
		if !ok {
			t.Errorf("unexpected entry %q", de.Name)
		} else if de.Inode != wantIno {
			t.Errorf("%q: ino=%d, want %d", de.Name, de.Inode, wantIno)
		}
	}
}

func TestParseDirBlock_SkipsDotEntries(t *testing.T) {
	entries := []struct {
		ino  uint64
		name string
		ft   uint8
	}{
		{128, ".", 4},
		{64, "..", 4},
		{256, "grub.cfg", 1},
	}
	blk := buildDirBlock(entries, true)
	got := parseDirBlock(blk, true, true)
	// . and .. must be filtered out.
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (no . or ..)", len(got))
	}
	if got[0].Name != "grub.cfg" {
		t.Errorf("name: got %q, want grub.cfg", got[0].Name)
	}
}

func TestParseDirBlock_V4Header(t *testing.T) {
	// V4 blocks have a 16-byte header instead of 64.
	blk := make([]byte, 4096)
	binary.BigEndian.PutUint32(blk[0:], magicDir2Data)

	off := dir2DataHdrSize // 16
	name := "etc"
	ino := uint64(512)
	sz := dirEntrySize(len(name), false)
	binary.BigEndian.PutUint64(blk[off:], ino)
	blk[off+8] = byte(len(name))
	copy(blk[off+9:], name)
	binary.BigEndian.PutUint16(blk[off+sz-2:], uint16(off))
	off += sz

	remain := 4096 - 8 - off
	if remain >= 8 {
		binary.BigEndian.PutUint16(blk[off:], dirFreeTag)
		binary.BigEndian.PutUint16(blk[off+2:], uint16(remain))
	}

	got := parseDirBlock(blk, false /* hasFType */, false /* hasCRC */)
	if len(got) != 1 || got[0].Name != "etc" || got[0].Inode != 512 {
		t.Errorf("v4 block parse: got %+v", got)
	}
}

func TestParseDirBlock_EmptyBlock(t *testing.T) {
	blk := make([]byte, 4096)
	binary.BigEndian.PutUint32(blk[0:], magicDir3Data)
	// Mark entire data region as free.
	off := dir3DataHdrSize
	binary.BigEndian.PutUint16(blk[off:], dirFreeTag)
	binary.BigEndian.PutUint16(blk[off+2:], uint16(4096-off))

	got := parseDirBlock(blk, true, true)
	if len(got) != 0 {
		t.Errorf("expected 0 entries for empty block, got %d", len(got))
	}
}

// ──────────────────── searchDirBlock ───────────────────────────────────────

func TestSearchDirBlock_Found(t *testing.T) {
	entries := []struct {
		ino  uint64
		name string
		ft   uint8
	}{
		{256, "grub.cfg", 1},
		{512, "vmlinuz", 1},
	}
	blk := buildDirBlock(entries, true)
	ino, ok := searchDirBlock(blk, "vmlinuz", true, true, false)
	if !ok {
		t.Error("searchDirBlock returned false, want true")
	}
	if ino != 512 {
		t.Errorf("ino: got %d, want 512", ino)
	}
}

func TestSearchDirBlock_NotFound(t *testing.T) {
	entries := []struct {
		ino  uint64
		name string
		ft   uint8
	}{
		{256, "grub.cfg", 1},
	}
	blk := buildDirBlock(entries, true)
	_, ok := searchDirBlock(blk, "missing", true, true, false)
	if ok {
		t.Error("searchDirBlock returned true for missing entry, want false")
	}
}

// ──────────────────── readFileData (inodeFmtLocal) ─────────────────────────

func TestReadFileData_Local(t *testing.T) {
	content := []byte("GRUB_TIMEOUT=5\nGRUB_CMDLINE_LINUX=\"console=ttyS0\"\n")
	raw := make([]byte, 512)
	binary.BigEndian.PutUint16(raw[inoOffMagic:], magicInode)
	raw[inoOffVersion] = 3
	raw[inoOffFormat] = inodeFmtLocal
	binary.BigEndian.PutUint64(raw[inoOffSize:], uint64(len(content)))
	copy(raw[inodeCoreSize:], content)

	in := &inode{
		num:    42,
		mode:   0x81A4,
		format: inodeFmtLocal,
		size:   uint64(len(content)),
		raw:    raw,
	}

	sb := defaultSB()
	got, err := readFileData(bytes.NewReader(nil), 0, sb, in)
	if err != nil {
		t.Fatalf("readFileData: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", got, content)
	}
}

func TestReadFileData_Local_Empty(t *testing.T) {
	raw := make([]byte, 512)
	binary.BigEndian.PutUint16(raw[inoOffMagic:], magicInode)
	raw[inoOffVersion] = 3
	raw[inoOffFormat] = inodeFmtLocal

	in := &inode{format: inodeFmtLocal, size: 0, raw: raw}
	sb := defaultSB()
	got, err := readFileData(bytes.NewReader(nil), 0, sb, in)
	if err != nil {
		t.Fatalf("readFileData empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty data, got %d bytes", len(got))
	}
}

func TestReadFileData_UnsupportedFormat(t *testing.T) {
	raw := make([]byte, 512)
	in := &inode{format: 99, size: 10, raw: raw}
	sb := defaultSB()
	_, err := readFileData(bytes.NewReader(nil), 0, sb, in)
	if err == nil {
		t.Error("expected error for unsupported format, got nil")
	}
}

// TestReadFileData_ExtentsInline tests readFileData with inodeFmtExtents,
// one extent, data populated in a fake image buffer.
func TestReadFileData_ExtentsInline(t *testing.T) {
	sb := defaultSB()
	content := []byte("hello Rocky Linux")

	// Allocate data at block 32.
	const dataBlock = 32
	blockSize := int(sb.blockSize)
	imgSize := (dataBlock + 1) * blockSize
	image := make([]byte, imgSize)
	copy(image[dataBlock*blockSize:], content)

	// Build inode: format=extents, 1 extent, size=len(content).
	raw := make([]byte, 512)
	binary.BigEndian.PutUint16(raw[inoOffMagic:], magicInode)
	raw[inoOffVersion] = 3
	raw[inoOffFormat] = inodeFmtExtents
	binary.BigEndian.PutUint64(raw[inoOffSize:], uint64(len(content)))
	binary.BigEndian.PutUint32(raw[inoOffNExtents:], 1)

	e := extent{startOff: 0, startBlock: dataBlock, count: 1}
	rec := encodeExtent(e)
	copy(raw[inodeCoreSize:], rec[:])

	in := &inode{
		num:    256,
		mode:   0x81A4,
		format: inodeFmtExtents,
		size:   uint64(len(content)),
		nExts:  1,
		raw:    raw,
	}

	r := bytes.NewReader(image)
	got, err := readFileData(r, 0, sb, in)
	if err != nil {
		t.Fatalf("readFileData extents: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", got, content)
	}
}

// ──────────────────── writeDirEntry ────────────────────────────────────────

func TestWriteDirEntry_WithFType(t *testing.T) {
	blk := make([]byte, 4096)
	offset := 0
	ino := uint64(256)
	name := "grub.cfg"
	ftype := uint8(1)

	sz := writeDirEntry(blk, offset, ino, name, ftype, true)

	// Check inode.
	gotIno := binary.BigEndian.Uint64(blk[0:])
	if gotIno != ino {
		t.Errorf("ino: got %d, want %d", gotIno, ino)
	}
	// Check namelen.
	if blk[8] != byte(len(name)) {
		t.Errorf("namelen: got %d, want %d", blk[8], len(name))
	}
	// Check name bytes.
	if string(blk[9:9+len(name)]) != name {
		t.Errorf("name: got %q, want %q", string(blk[9:9+len(name)]), name)
	}
	// Check ftype.
	if blk[9+len(name)] != ftype {
		t.Errorf("ftype: got %d, want %d", blk[9+len(name)], ftype)
	}
	// Check tag == offset at end of entry.
	gotTag := binary.BigEndian.Uint16(blk[sz-2:])
	if gotTag != uint16(offset) {
		t.Errorf("tag: got %d, want %d", gotTag, offset)
	}
}

func TestWriteDirEntry_NoFType(t *testing.T) {
	blk := make([]byte, 4096)
	const offset = 64
	ino := uint64(512)
	name := "etc"

	sz := writeDirEntry(blk, offset, ino, name, 0, false)

	gotIno := binary.BigEndian.Uint64(blk[offset:])
	if gotIno != ino {
		t.Errorf("ino: got %d, want %d", gotIno, ino)
	}
	gotTag := binary.BigEndian.Uint16(blk[offset+sz-2:])
	if gotTag != offset {
		t.Errorf("tag: got %d, want %d", gotTag, offset)
	}
	wantSz := dirEntrySize(len(name), false)
	if sz != wantSz {
		t.Errorf("size: got %d, want %d", sz, wantSz)
	}
}

// ──────────────────── markSlotFree ─────────────────────────────────────────

func TestMarkSlotFree_Basic(t *testing.T) {
	blk := make([]byte, 4096)
	// Prefill some data that should be cleared.
	for i := 64; i < 64+24; i++ {
		blk[i] = 0xFF
	}
	offset, length := 64, 24

	markSlotFree(blk, offset, length)

	// freetag at offset.
	freetag := binary.BigEndian.Uint16(blk[offset:])
	if freetag != dirFreeTag {
		t.Errorf("freetag: got 0x%04X, want 0x%04X", freetag, dirFreeTag)
	}
	// length field at offset+2.
	gotLen := binary.BigEndian.Uint16(blk[offset+2:])
	if gotLen != uint16(length) {
		t.Errorf("length field: got %d, want %d", gotLen, length)
	}
	// tag at end.
	gotTag := binary.BigEndian.Uint16(blk[offset+length-2:])
	if gotTag != uint16(offset) {
		t.Errorf("tag: got %d, want %d", gotTag, offset)
	}
	// Everything between offset+4 and offset+length-2 should be zero.
	for i := offset + 4; i < offset+length-2; i++ {
		if blk[i] != 0 {
			t.Errorf("blk[%d] = 0x%02X, want 0x00 (slot not cleared)", i, blk[i])
		}
	}
}

// ──────────────────── findFreeSlot ─────────────────────────────────────────

func TestFindFreeSlot_Found(t *testing.T) {
	blk := make([]byte, 4096)
	binary.BigEndian.PutUint32(blk[0:], magicDir3Data)

	// Place a free slot of 32 bytes starting at offset dir3DataHdrSize.
	off := dir3DataHdrSize
	binary.BigEndian.PutUint16(blk[off:], dirFreeTag)
	binary.BigEndian.PutUint16(blk[off+2:], 32)
	binary.BigEndian.PutUint16(blk[off+30:], uint16(off))

	got := findFreeSlot(blk, 24, true, true)
	if got != off {
		t.Errorf("got offset %d, want %d", got, off)
	}
}

func TestFindFreeSlot_TooSmall(t *testing.T) {
	blk := make([]byte, 4096)
	binary.BigEndian.PutUint32(blk[0:], magicDir3Data)

	off := dir3DataHdrSize
	binary.BigEndian.PutUint16(blk[off:], dirFreeTag)
	binary.BigEndian.PutUint16(blk[off+2:], 16)
	binary.BigEndian.PutUint16(blk[off+14:], uint16(off))

	got := findFreeSlot(blk, 32, true, true)
	if got != -1 {
		t.Errorf("got %d, want -1 (slot too small)", got)
	}
}

func TestFindFreeSlot_AfterUsedEntry(t *testing.T) {
	blk := make([]byte, 4096)
	binary.BigEndian.PutUint32(blk[0:], magicDir3Data)
	off := dir3DataHdrSize

	// Write a used entry.
	n := writeDirEntry(blk, off, 256, "grub.cfg", 1, true)
	off += n

	// Place a free slot after it.
	binary.BigEndian.PutUint16(blk[off:], dirFreeTag)
	binary.BigEndian.PutUint16(blk[off+2:], 32)
	binary.BigEndian.PutUint16(blk[off+30:], uint16(off))

	got := findFreeSlot(blk, 24, true, true)
	if got != off {
		t.Errorf("got offset %d, want %d", got, off)
	}
}

// ──────────────────── findEntryInBlock ─────────────────────────────────────

func TestFindEntryInBlock_Found(t *testing.T) {
	blk := make([]byte, 4096)
	binary.BigEndian.PutUint32(blk[0:], magicDir3Data)
	off := dir3DataHdrSize

	sz1 := writeDirEntry(blk, off, 256, "grub.cfg", 1, true)
	off += sz1
	writeDirEntry(blk, off, 512, "vmlinuz", 1, true)

	foundOff, foundSz := findEntryInBlock(blk, "vmlinuz", true, true)
	if foundOff != off {
		t.Errorf("offset: got %d, want %d", foundOff, off)
	}
	if foundSz != dirEntrySize(len("vmlinuz"), true) {
		t.Errorf("size: got %d, want %d", foundSz, dirEntrySize(len("vmlinuz"), true))
	}
}

func TestFindEntryInBlock_NotFound(t *testing.T) {
	blk := make([]byte, 4096)
	binary.BigEndian.PutUint32(blk[0:], magicDir3Data)
	off := dir3DataHdrSize
	writeDirEntry(blk, off, 256, "grub.cfg", 1, true)

	foundOff, _ := findEntryInBlock(blk, "missing", true, true)
	if foundOff != -1 {
		t.Errorf("got %d, want -1", foundOff)
	}
}

func TestFindEntryInBlock_SkipsFreeSlot(t *testing.T) {
	blk := make([]byte, 4096)
	binary.BigEndian.PutUint32(blk[0:], magicDir3Data)
	off := dir3DataHdrSize

	// Free slot of 16 bytes.
	binary.BigEndian.PutUint16(blk[off:], dirFreeTag)
	binary.BigEndian.PutUint16(blk[off+2:], 16)
	binary.BigEndian.PutUint16(blk[off+14:], uint16(off))
	off += 16

	writeDirEntry(blk, off, 512, "vmlinuz", 1, true)

	foundOff, _ := findEntryInBlock(blk, "vmlinuz", true, true)
	if foundOff != off {
		t.Errorf("got %d, want %d", foundOff, off)
	}
}
