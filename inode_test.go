// inode_test.go — whitebox unit tests for inode number arithmetic and
// inode read/write helpers.
package filesystem_xfs

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// defaultSB returns a superblock typical of a Rocky Linux 9 XFS partition:
// 4 KiB blocks, 512-byte inodes, 8 inodes/block, 16384 blocks per AG.
func defaultSB() *superblock {
	return &superblock{
		blockSize: 4096,
		agBlocks:  16384,
		agCount:   2,
		rootIno:   128,
		inodeSize: 512,
		inopBlock: 8,
		inopBLog:  3,  // log2(8)
		agBlkLog:  14, // ceil(log2(16384))
		dirBlkLog: 0,
		hasCRC:    true,
		hasFType:  true,
	}
}

// ──────────────────── inoAG / inoAGRel / inoFromAGRel ──────────────────────

func TestInoAG_AG0(t *testing.T) {
	sb := defaultSB()
	// ino 128: ag = 128 >> (14+3) = 128 >> 17 = 0
	ag := inoAG(sb, 128)
	if ag != 0 {
		t.Errorf("inoAG(128) = %d, want 0", ag)
	}
}

func TestInoAG_AG1(t *testing.T) {
	sb := defaultSB()
	// First inode in AG 1: 1 << (agBlkLog + inopBLog) = 1 << 17 = 131072
	ino := uint64(1) << (uint64(sb.agBlkLog) + uint64(sb.inopBLog))
	ag := inoAG(sb, ino)
	if ag != 1 {
		t.Errorf("inoAG(first-AG1) = %d, want 1", ag)
	}
}

func TestInoAGRel(t *testing.T) {
	sb := defaultSB()
	// ino 128 is entirely within AG 0: agRel = 128 & ((1<<17)-1) = 128
	agRel := inoAGRel(sb, 128)
	if agRel != 128 {
		t.Errorf("inoAGRel(128) = %d, want 128", agRel)
	}
}

func TestInoAGRel_SecondAG(t *testing.T) {
	sb := defaultSB()
	// ino = first-of-AG1 + 5
	base := uint64(1) << (uint64(sb.agBlkLog) + uint64(sb.inopBLog))
	ino := base + 5
	agRel := inoAGRel(sb, ino)
	if agRel != 5 {
		t.Errorf("inoAGRel(base+5) = %d, want 5", agRel)
	}
}

func TestInoFromAGRel_Roundtrip(t *testing.T) {
	sb := defaultSB()
	for _, ino := range []uint64{128, 256, 0, 1, 999, 131072, 131200} {
		ag := inoAG(sb, ino)
		agRel := inoAGRel(sb, ino)
		got := inoFromAGRel(sb, ag, agRel)
		if got != ino {
			t.Errorf("inoFromAGRel roundtrip(%d): got %d", ino, got)
		}
	}
}

// ──────────────────── inodeByteOff ─────────────────────────────────────────

func TestInodeByteOff_Root(t *testing.T) {
	sb := defaultSB()
	// ino=128, blockSize=4096, agBlocks=16384, inopBLog=3, agBlkLog=14.
	// ag=0, agRel=128, agBlock=128>>3=16, blkOff=128&7=0
	// byteOff = 0 + 0 + 16*4096 + 0 = 65536
	off := inodeByteOff(sb, 0, 128)
	if off != 65536 {
		t.Errorf("inodeByteOff(0, 128) = %d, want 65536", off)
	}
}

func TestInodeByteOff_PartOffset(t *testing.T) {
	sb := defaultSB()
	const partOff int64 = 1048576 // 1 MiB partition offset
	off := inodeByteOff(sb, partOff, 128)
	want := partOff + 65536
	if off != want {
		t.Errorf("inodeByteOff(1MiB, 128) = %d, want %d", off, want)
	}
}

func TestInodeByteOff_BlkOff(t *testing.T) {
	sb := defaultSB()
	// ino=129: same agBlock=16, blkOff=1 → +512 bytes
	off0 := inodeByteOff(sb, 0, 128)
	off1 := inodeByteOff(sb, 0, 129)
	if off1 != off0+512 {
		t.Errorf("inode 129 should be 512 bytes after 128: got %d, want %d", off1, off0+512)
	}
}

func TestInodeByteOff_NextBlock(t *testing.T) {
	sb := defaultSB()
	// ino=136 is in block 17 (agBlock=17), blkOff=0.
	// 136>>3=17, 136&7=0
	off136 := inodeByteOff(sb, 0, 136)
	want := int64(17 * 4096)
	if off136 != want {
		t.Errorf("inodeByteOff(0, 136) = %d, want %d", off136, want)
	}
}

// ──────────────────── buildInodeBuf helper ─────────────────────────────────

// buildInodeBuf constructs a 512-byte v3 inode buffer with the given fields.
func buildInodeBuf(num uint64, mode uint16, format uint8, size uint64) []byte {
	buf := make([]byte, 512)
	be := binary.BigEndian
	be.PutUint16(buf[inoOffMagic:], magicInode)
	be.PutUint16(buf[inoOffMode:], mode)
	buf[inoOffVersion] = 3
	buf[inoOffFormat] = format
	be.PutUint64(buf[inoOffSize:], size)
	be.PutUint64(buf[inoOffIno:], num)
	return buf
}

// ──────────────────── readInode parsing ────────────────────────────────────

func TestReadInode_Basic(t *testing.T) {
	sb := defaultSB()
	// Place inode 128 at the expected offset (65536) in a reader.
	const inodeOff = 65536
	imageSize := inodeOff + 512
	image := make([]byte, imageSize)
	inoBuf := buildInodeBuf(128, 0x81A4 /* 0100644 */, inodeFmtLocal, 42)
	copy(image[inodeOff:], inoBuf)

	r := bytes.NewReader(image)
	in, err := readInode(r, 0, sb, 128)
	if err != nil {
		t.Fatalf("readInode: %v", err)
	}
	if in.num != 128 {
		t.Errorf("num: got %d, want 128", in.num)
	}
	if in.mode != 0x81A4 {
		t.Errorf("mode: got 0x%04X, want 0x81A4", in.mode)
	}
	if in.format != inodeFmtLocal {
		t.Errorf("format: got %d, want %d", in.format, inodeFmtLocal)
	}
	if in.size != 42 {
		t.Errorf("size: got %d, want 42", in.size)
	}
	if !in.isRegular() {
		t.Errorf("expected isRegular() == true")
	}
}

func TestReadInode_BadMagic(t *testing.T) {
	sb := defaultSB()
	const inodeOff = 65536
	image := make([]byte, inodeOff+512)
	// Leave magic as 0x0000 — should trigger an error.

	r := bytes.NewReader(image)
	_, err := readInode(r, 0, sb, 128)
	if err == nil {
		t.Error("expected error for bad inode magic, got nil")
	}
}

func TestReadInode_IsDir(t *testing.T) {
	sb := defaultSB()
	const inodeOff = 65536
	image := make([]byte, inodeOff+512)
	inoBuf := buildInodeBuf(128, 0x41ED /* 0040755 */, inodeFmtLocal, 0)
	copy(image[inodeOff:], inoBuf)

	r := bytes.NewReader(image)
	in, err := readInode(r, 0, sb, 128)
	if err != nil {
		t.Fatalf("readInode: %v", err)
	}
	if !in.isDir() {
		t.Errorf("expected isDir() == true for mode 0x%04X", in.mode)
	}
	if in.isRegular() {
		t.Errorf("expected isRegular() == false for dir inode")
	}
}

func TestReadInode_IsSymlink(t *testing.T) {
	sb := defaultSB()
	const inodeOff = 65536
	image := make([]byte, inodeOff+512)
	inoBuf := buildInodeBuf(128, 0xA1FF /* symlink */, inodeFmtLocal, 0)
	copy(image[inodeOff:], inoBuf)

	r := bytes.NewReader(image)
	in, err := readInode(r, 0, sb, 128)
	if err != nil {
		t.Fatalf("readInode: %v", err)
	}
	if !in.isSymlink() {
		t.Errorf("expected isSymlink() == true")
	}
}

// ──────────────────── inode setter helpers ─────────────────────────────────

func TestSetInodeSize(t *testing.T) {
	in := &inode{raw: make([]byte, 512)}
	setInodeSize(in, 99999)
	got := binary.BigEndian.Uint64(in.raw[inoOffSize:])
	if got != 99999 {
		t.Errorf("setInodeSize: raw field = %d, want 99999", got)
	}
	if in.size != 99999 {
		t.Errorf("setInodeSize: in.size = %d, want 99999", in.size)
	}
}

func TestSetInodeNBlocks(t *testing.T) {
	in := &inode{raw: make([]byte, 512)}
	setInodeNBlocks(in, 42)
	got := binary.BigEndian.Uint64(in.raw[inoOffNBlocks:])
	if got != 42 {
		t.Errorf("setInodeNBlocks: raw field = %d, want 42", got)
	}
}

func TestSetInodeNExtents(t *testing.T) {
	in := &inode{raw: make([]byte, 512)}
	setInodeNExtents(in, 7)
	got := binary.BigEndian.Uint32(in.raw[inoOffNExtents:])
	if got != 7 {
		t.Errorf("setInodeNExtents: raw field = %d, want 7", got)
	}
}

func TestSetInodeFormat(t *testing.T) {
	in := &inode{raw: make([]byte, 512)}
	setInodeFormat(in, inodeFmtExtents)
	if in.raw[inoOffFormat] != inodeFmtExtents {
		t.Errorf("setInodeFormat: raw field = %d, want %d", in.raw[inoOffFormat], inodeFmtExtents)
	}
	if in.format != inodeFmtExtents {
		t.Errorf("setInodeFormat: in.format = %d, want %d", in.format, inodeFmtExtents)
	}
}

func TestZeroInode(t *testing.T) {
	in := &inode{raw: make([]byte, 512), mode: 0x81A4}
	binary.BigEndian.PutUint16(in.raw[inoOffMode:], 0x81A4)
	zeroInode(in)
	got := binary.BigEndian.Uint16(in.raw[inoOffMode:])
	if got != 0 {
		t.Errorf("zeroInode: mode = 0x%04X, want 0", got)
	}
}

// ──────────────────── initInodeV3 ──────────────────────────────────────────

func TestInitInodeV3(t *testing.T) {
	buf := make([]byte, 512)
	initInodeV3(buf, 256, 0x81A4, 512, 1, [16]byte{})

	be := binary.BigEndian
	magic := be.Uint16(buf[inoOffMagic:])
	if magic != magicInode {
		t.Errorf("magic: got 0x%04X, want 0x%04X", magic, magicInode)
	}
	if buf[inoOffVersion] != 3 {
		t.Errorf("version: got %d, want 3", buf[inoOffVersion])
	}
	num := be.Uint64(buf[inoOffIno:])
	if num != 256 {
		t.Errorf("di_ino: got %d, want 256", num)
	}
	mode := be.Uint16(buf[inoOffMode:])
	if mode != 0x81A4 {
		t.Errorf("mode: got 0x%04X, want 0x81A4", mode)
	}
	if nlink := be.Uint32(buf[inoOffNLink:]); nlink != 1 {
		t.Errorf("nlink: got %d, want 1", nlink)
	}
	// All four timestamp fields must be stamped (not zero / epoch).
	for _, label := range []struct {
		off  int
		name string
	}{
		{inoOffATime, "atime"},
		{inoOffMTime, "mtime"},
		{inoOffCTime, "ctime"},
		{inoOffCRTime, "crtime"},
	} {
		if sec := be.Uint32(buf[label.off:]); sec == 0 {
			t.Errorf("%s seconds = 0; expected current time stamped by initInodeV3", label.name)
		}
	}
}

// ──────────────────── dataFork ─────────────────────────────────────────────

func TestInodeDataFork_StartsAtCore(t *testing.T) {
	buf := make([]byte, 512)
	for i := range buf {
		buf[i] = byte(i)
	}
	in := &inode{raw: buf}
	fork := in.dataFork()
	if len(fork) != 512-inodeCoreSize {
		t.Errorf("dataFork len = %d, want %d", len(fork), 512-inodeCoreSize)
	}
	if &fork[0] != &buf[inodeCoreSize] {
		t.Errorf("dataFork does not start at raw[%d]", inodeCoreSize)
	}
}
