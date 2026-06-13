// superblock_test.go — whitebox unit tests for readSuperblock.
package filesystem_xfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// buildSBBuf constructs a 512-byte XFS superblock buffer with the given
// field values. All multi-byte integers are big-endian except the v5 CRC.
func buildSBBuf(
	blockSize uint32,
	agBlocks uint32,
	agCount uint32,
	rootIno uint64,
	inodeSize uint16,
	inopBlock uint16,
	inopBLog uint8,
	agBlkLog uint8,
	dirBlkLog uint8,
	versionNum uint16, // low nibble = version (5 for v5)
	featIncompat uint32,
	feat2 uint32,
) []byte {
	buf := make([]byte, 512)
	be := binary.BigEndian
	be.PutUint32(buf[sbOffMagic:], magicSB)
	be.PutUint32(buf[sbOffBlockSize:], blockSize)
	be.PutUint64(buf[sbOffRootIno:], rootIno)
	be.PutUint32(buf[sbOffAgBlocks:], agBlocks)
	be.PutUint32(buf[sbOffAgCount:], agCount)
	be.PutUint16(buf[sbOffVersionNum:], versionNum)
	be.PutUint16(buf[sbOffInodeSize:], inodeSize)
	be.PutUint16(buf[sbOffInopBlock:], inopBlock)
	buf[sbOffInopBLog] = inopBLog
	buf[sbOffAgBlkLog] = agBlkLog
	buf[sbOffDirBlkLog] = dirBlkLog
	be.PutUint32(buf[sbOffFeatIncompat:], featIncompat)
	be.PutUint32(buf[sbOffFeatures2:], feat2)
	return buf
}

// v5SBBuf returns a buffer representative of a Rocky Linux 9 XFS superblock.
func v5SBBuf() []byte {
	return buildSBBuf(4096, 16384, 2, 128, 512, 8, 3, 14, 0,
		0x0005, // v5
		xfsSBFeatFType,
		0,
	)
}

// ──────────────────── readSuperblock ───────────────────────────────────────

func TestReadSuperblock_V5BasicFields(t *testing.T) {
	buf := v5SBBuf()
	r := bytes.NewReader(buf)
	sb, err := readSuperblock(r, 0)
	if err != nil {
		t.Fatalf("readSuperblock: %v", err)
	}

	if sb.blockSize != 4096 {
		t.Errorf("blockSize: got %d, want 4096", sb.blockSize)
	}
	if sb.agBlocks != 16384 {
		t.Errorf("agBlocks: got %d, want 16384", sb.agBlocks)
	}
	if sb.agCount != 2 {
		t.Errorf("agCount: got %d, want 2", sb.agCount)
	}
	if sb.rootIno != 128 {
		t.Errorf("rootIno: got %d, want 128", sb.rootIno)
	}
	if sb.inodeSize != 512 {
		t.Errorf("inodeSize: got %d, want 512", sb.inodeSize)
	}
	if sb.inopBlock != 8 {
		t.Errorf("inopBlock: got %d, want 8", sb.inopBlock)
	}
	if sb.inopBLog != 3 {
		t.Errorf("inopBLog: got %d, want 3", sb.inopBLog)
	}
	if sb.agBlkLog != 14 {
		t.Errorf("agBlkLog: got %d, want 14", sb.agBlkLog)
	}
}

func TestReadSuperblock_V5HasCRC(t *testing.T) {
	r := bytes.NewReader(v5SBBuf())
	sb, err := readSuperblock(r, 0)
	if err != nil {
		t.Fatalf("readSuperblock: %v", err)
	}
	if !sb.hasCRC {
		t.Errorf("hasCRC: got false, want true for v5")
	}
}

func TestReadSuperblock_V5HasFType(t *testing.T) {
	r := bytes.NewReader(v5SBBuf())
	sb, err := readSuperblock(r, 0)
	if err != nil {
		t.Fatalf("readSuperblock: %v", err)
	}
	if !sb.hasFType {
		t.Errorf("hasFType: got false, want true when XFS_SB_FEAT_INCOMPAT_FTYPE is set")
	}
}

func TestReadSuperblock_V5NoFType(t *testing.T) {
	buf := buildSBBuf(4096, 16384, 1, 128, 512, 8, 3, 14, 0,
		0x0005, 0 /* no ftype */, 0)
	r := bytes.NewReader(buf)
	sb, err := readSuperblock(r, 0)
	if err != nil {
		t.Fatalf("readSuperblock: %v", err)
	}
	if sb.hasFType {
		t.Errorf("hasFType: got true, want false when incompat ftype not set")
	}
}

func TestReadSuperblock_V4FType(t *testing.T) {
	// v4 superblock: versionNum low nibble != 5; ftype in features2.
	buf := buildSBBuf(4096, 16384, 1, 128, 512, 8, 3, 14, 0,
		0x0004, 0, xfsSBv4FeatFType /* feat2 */)
	r := bytes.NewReader(buf)
	sb, err := readSuperblock(r, 0)
	if err != nil {
		t.Fatalf("readSuperblock: %v", err)
	}
	if sb.hasCRC {
		t.Errorf("hasCRC: got true, want false for v4")
	}
	if !sb.hasFType {
		t.Errorf("hasFType: got false, want true for v4 with XFS_SB_VERSION2_FTYPE")
	}
}

func TestReadSuperblock_BadMagic(t *testing.T) {
	buf := v5SBBuf()
	binary.BigEndian.PutUint32(buf[sbOffMagic:], 0xDEADBEEF)
	r := bytes.NewReader(buf)
	_, err := readSuperblock(r, 0)
	if err == nil {
		t.Error("expected error for bad magic, got nil")
	}
}

func TestReadSuperblock_InvalidGeometry_ZeroBlockSize(t *testing.T) {
	buf := buildSBBuf(0, 16384, 1, 128, 512, 8, 3, 14, 0, 0x0005, 0, 0)
	r := bytes.NewReader(buf)
	_, err := readSuperblock(r, 0)
	if err == nil {
		t.Error("expected error for blockSize=0, got nil")
	}
}

func TestReadSuperblock_InvalidGeometry_ZeroInodeSize(t *testing.T) {
	buf := buildSBBuf(4096, 16384, 1, 128, 0 /* inodeSize=0 */, 8, 3, 14, 0, 0x0005, 0, 0)
	r := bytes.NewReader(buf)
	_, err := readSuperblock(r, 0)
	if err == nil {
		t.Error("expected error for inodeSize=0, got nil")
	}
}

func TestReadSuperblock_DirBlkLog(t *testing.T) {
	buf := buildSBBuf(4096, 16384, 1, 128, 512, 8, 3, 14, 1 /* dirBlkLog=1 */, 0x0005, 0, 0)
	r := bytes.NewReader(buf)
	sb, err := readSuperblock(r, 0)
	if err != nil {
		t.Fatalf("readSuperblock: %v", err)
	}
	if sb.dirBlkLog != 1 {
		t.Errorf("dirBlkLog: got %d, want 1", sb.dirBlkLog)
	}
	if sb.dirFSBlocks() != 2 {
		t.Errorf("dirFSBlocks: got %d, want 2 (2^dirBlkLog=1)", sb.dirFSBlocks())
	}
}

func TestReadSuperblock_PartitionOffset(t *testing.T) {
	// Place the superblock at a non-zero partition offset (1 MiB).
	const partOff int64 = 1 << 20
	buf := v5SBBuf()
	image := make([]byte, int(partOff)+len(buf))
	copy(image[partOff:], buf)

	r := bytes.NewReader(image)
	sb, err := readSuperblock(r, partOff)
	if err != nil {
		t.Fatalf("readSuperblock at 1MiB offset: %v", err)
	}
	if sb.blockSize != 4096 {
		t.Errorf("blockSize: got %d, want 4096", sb.blockSize)
	}
}

// ──────────────────── superblock byte-offset helpers ───────────────────────

func TestAgFByteOffset(t *testing.T) {
	sb := defaultSB() // blockSize=4096, agBlocks=16384, sectorSize defaults to 512
	// The AG headers (SB/AGF/AGI/AGFL) are sector-aligned inside block 0, so
	// the AGF lives at sector 1 of the AG, not block 1.
	// AG 0 AGF: partOff + 0*16384*4096 + 512 = 512
	got := sb.agFByteOffset(0, 0)
	if got != 512 {
		t.Errorf("agFByteOffset(0,0) = %d, want 512", got)
	}
	// AG 1 AGF: partOff + 1*16384*4096 + 512
	got = sb.agFByteOffset(0, 1)
	want := int64(16384*4096) + 512
	if got != want {
		t.Errorf("agFByteOffset(0,1) = %d, want %d", got, want)
	}
}

func TestAgIByteOffset(t *testing.T) {
	sb := defaultSB()
	got := sb.agIByteOffset(0, 0)
	if got != 1024 { // sector 2 = 2 * 512
		t.Errorf("agIByteOffset(0,0) = %d, want 1024", got)
	}
}

func TestDirFSBlocks(t *testing.T) {
	sb := defaultSB()
	sb.dirBlkLog = 0
	if sb.dirFSBlocks() != 1 {
		t.Errorf("dirFSBlocks: got %d, want 1 for dirBlkLog=0", sb.dirFSBlocks())
	}
	sb.dirBlkLog = 2
	if sb.dirFSBlocks() != 4 {
		t.Errorf("dirFSBlocks: got %d, want 4 for dirBlkLog=2", sb.dirFSBlocks())
	}
}

type superblockErrReaderAt struct {
	err error
}

func (r superblockErrReaderAt) ReadAt([]byte, int64) (int, error) {
	return 0, r.err
}

func TestReadSuperblock_ReadError(t *testing.T) {
	if _, err := readSuperblock(superblockErrReaderAt{err: errBoom}, 4096); !errors.Is(err, errBoom) {
		t.Fatalf("expected readSuperblock read error %v, got %v", errBoom, err)
	}
}
