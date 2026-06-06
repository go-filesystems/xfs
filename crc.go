package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// updateCRC recomputes the CRC32c over buf[:length], zeroing the four-byte
// little-endian CRC field at crcOff before hashing, then stores the result
// back. XFS uses CRC32c stored as __le32 even though all other fields are
// big-endian.
func updateCRC(buf []byte, crcOff, length int) {
	binary.LittleEndian.PutUint32(buf[crcOff:], 0)
	crc := crc32.Checksum(buf[:length], castagnoli)
	binary.LittleEndian.PutUint32(buf[crcOff:], crc)
}

// verifyCRC checks the CRC32c stored in buf[crcOff:crcOff+4] against the
// computed CRC over buf[:length].
//
// Two XFS conventions coexist for where the CRC field lives relative to
// the covered range:
//   - AGF / AGI / btree blocks: CRC field is inside the covered range
//     (crcOff+4 <= length); zero it before checksumming.
//   - Superblock: CRC field sits immediately after the covered range
//     (crcOff == length); nothing to zero, just checksum buf[:length].
func verifyCRC(buf []byte, crcOff, length int) bool {
	stored := binary.LittleEndian.Uint32(buf[crcOff:])
	if crcOff+4 <= length {
		tmp := make([]byte, length)
		copy(tmp, buf[:length])
		binary.LittleEndian.PutUint32(tmp[crcOff:], 0)
		return stored == crc32.Checksum(tmp, castagnoli)
	}
	return stored == crc32.Checksum(buf[:length], castagnoli)
}

// readBytes is a convenience helper that reads exactly len(buf) bytes at off.
func readBytes(r io.ReaderAt, off int64, buf []byte) error {
	if _, err := r.ReadAt(buf, off); err != nil {
		return fmt.Errorf("xfs: read at 0x%x: %w", off, err)
	}
	return nil
}
