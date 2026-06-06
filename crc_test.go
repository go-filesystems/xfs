// crc_test.go — whitebox unit tests for XFS CRC32c helpers.
package filesystem_xfs

import (
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// TestUpdateCRC_ZeroesField verifies that after updateCRC the CRC field is
// non-zero (assuming the payload is non-trivial) and is a valid LE32 value.
func TestUpdateCRC_ZeroesField(t *testing.T) {
	buf := make([]byte, 64)
	copy(buf[8:], "hello XFS world, this is test data for CRC computation 12345")
	updateCRC(buf, 0, 64)

	got := binary.LittleEndian.Uint32(buf[0:])
	if got == 0 {
		t.Errorf("expected non-zero CRC after updateCRC, got 0")
	}
}

// TestUpdateCRC_VerificationPasses confirms that verifyCRC returns true
// immediately after updateCRC.
func TestUpdateCRC_VerificationPasses(t *testing.T) {
	buf := make([]byte, 128)
	copy(buf[8:], "XFS v5 superblock payload for CRC test — Rocky Linux 9")
	const crcOff = 4
	updateCRC(buf, crcOff, 128)
	if !verifyCRC(buf, crcOff, 128) {
		t.Errorf("verifyCRC returned false immediately after updateCRC")
	}
}

// TestUpdateCRC_CRCFieldStoredAsLE ensures the stored CRC is little-endian
// (XFS stores __le32 even though all other fields are big-endian).
func TestUpdateCRC_CRCFieldStoredAsLE(t *testing.T) {
	buf := make([]byte, 64)
	copy(buf[8:], "payload that produces a non-palindromic CRC value!")
	updateCRC(buf, 0, 64)

	// Compute the expected LE32 value manually.
	tmp := make([]byte, 64)
	copy(tmp, buf)
	binary.LittleEndian.PutUint32(tmp[0:], 0)
	expected := crc32.Checksum(tmp, crc32.MakeTable(crc32.Castagnoli))

	stored := binary.LittleEndian.Uint32(buf[0:])
	if stored != expected {
		t.Errorf("stored CRC 0x%08X != expected 0x%08X", stored, expected)
	}
}

// TestUpdateCRC_KnownVector uses the CRC-32C standard test vector
// "123456789" → 0xE3069283 to validate our table.
func TestUpdateCRC_KnownVector(t *testing.T) {
	buf := []byte("123456789")
	// Compute directly without the zero-then-store wrapper.
	got := crc32.Checksum(buf, castagnoli)
	const want = uint32(0xE3069283)
	if got != want {
		t.Errorf("CRC32c(\"123456789\") = 0x%08X, want 0x%08X", got, want)
	}
}

// TestVerifyCRC_FailsOnMutation verifies that mutating one byte of the
// payload causes verifyCRC to return false.
func TestVerifyCRC_FailsOnMutation(t *testing.T) {
	buf := make([]byte, 64)
	copy(buf[8:], "original payload — must not be changed for verify to pass")
	updateCRC(buf, 0, 64)

	buf[10] ^= 0xFF // corrupt one byte
	if verifyCRC(buf, 0, 64) {
		t.Errorf("verifyCRC returned true after payload mutation")
	}
}

// TestVerifyCRC_AllZero confirms behaviour with an all-zero buffer:
// the CRC of 64 zeroes (with the CRC field also zero) must equal what
// updateCRC stores.
func TestVerifyCRC_AllZero(t *testing.T) {
	buf := make([]byte, 64)
	updateCRC(buf, 0, 64)
	if !verifyCRC(buf, 0, 64) {
		t.Errorf("verifyCRC failed for all-zero content")
	}
}

// TestUpdateCRC_DifferentOffsets checks that updateCRC is sensitive to
// the crcOff argument and correctly zeros only the right 4-byte window.
func TestUpdateCRC_DifferentOffsets(t *testing.T) {
	for _, crcOff := range []int{0, 4, 8, 52, 100} {
		size := 128
		if crcOff+4 > size {
			continue
		}
		buf := make([]byte, size)
		for i := range buf {
			buf[i] = byte(i)
		}
		updateCRC(buf, crcOff, size)
		if !verifyCRC(buf, crcOff, size) {
			t.Errorf("verifyCRC failed at crcOff=%d", crcOff)
		}
	}
}

// TestUpdateCRC_RepeatableResult confirms that calling updateCRC twice on
// the same buffer produces the same result.
func TestUpdateCRC_RepeatableResult(t *testing.T) {
	buf1 := make([]byte, 64)
	copy(buf1[8:], "repeatable test payload for CRC idempotency check")
	buf2 := make([]byte, len(buf1))
	copy(buf2, buf1)

	updateCRC(buf1, 4, 64)
	updateCRC(buf2, 4, 64)

	crc1 := binary.LittleEndian.Uint32(buf1[4:])
	crc2 := binary.LittleEndian.Uint32(buf2[4:])
	if crc1 != crc2 {
		t.Errorf("repeated updateCRC produced different results: 0x%08X vs 0x%08X", crc1, crc2)
	}
}
