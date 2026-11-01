package filesystem_xfs

import (
	"encoding/binary"
	"io"
	"testing"
)

// fuzzAllocBudget is what one fuzz iteration may allocate: a fixed allowance
// plus a small multiple of the input. A bomb is an iteration whose allocation
// has no relation to the input's size.
func fuzzAllocBudget(inputLen int) uint64 { return 16<<20 + 64*uint64(inputLen) }

// FuzzReadFileBomb reads a regular file whose inode fields (di_size,
// di_nblocks, the data fork, the superblock's sb_dblocks) and device all come
// from the input. The allocation must stay proportional to the input once the
// read limit is lowered to 1 MiB, and the bytes read from the device must stay
// proportional to the file.
func FuzzReadFileBomb(f *testing.F) {
	seed := make([]byte, 8+8+8+1+16)
	binary.LittleEndian.PutUint64(seed[0:], 4096)
	binary.LittleEndian.PutUint64(seed[8:], 1)
	f.Add(seed)
	f.Add(append(append([]byte{}, seed...), make([]byte, 8192)...))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 25 {
			return
		}
		size := binary.LittleEndian.Uint64(data[0:])
		nblocks := binary.LittleEndian.Uint64(data[8:])
		dblocks := binary.LittleEndian.Uint64(data[16:])
		fmtByte := data[24]
		rest := data[25:]
		sb := defaultSB()
		sb.dBlocks = dblocks
		sb.maxRead = 1 << 20
		format := []uint8{inodeFmtLocal, inodeFmtExtents, inodeFmtBtree}[int(fmtByte)%3]
		in := newTestInode(10, 0x8000, format, size)
		setInodeNBlocks(in, nblocks)
		fork := in.dataFork()
		n := copy(fork, rest)
		in.nExts = uint32(len(rest[:n]) / 16)
		img := rest[n:]
		cr := &countReader{r: &bombDevice{buf: img}}
		got := allocDelta(func() { _, _ = readFileData(cr, 0, sb, in) })
		if got > fuzzAllocBudget(len(data)) {
			t.Fatalf("allocated %d bytes for a %d-byte input", got, len(data))
		}
		// Reads are bounded by the file (<= 1 MiB here) plus tree blocks, which
		// are bounded by the image: one read of each block, never one per extent.
		if limit := int64(1<<20) + 4*int64(len(img)) + 1<<20; cr.n > limit {
			t.Fatalf("read %d bytes for a %d-byte image", cr.n, len(img))
		}
	})
}

// bombDevice is a read-only device with io.ReaderAt semantics.
type bombDevice struct{ buf []byte }

func (b *bombDevice) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b.buf)) {
		return 0, io.EOF
	}
	n := copy(p, b.buf[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// FuzzValidateExtents sorts arbitrary records and checks the invariant the
// validator exists for: whatever it accepts has no overlap in either order.
func FuzzValidateExtents(f *testing.F) {
	f.Add(make([]byte, 64))
	f.Fuzz(func(t *testing.T, data []byte) {
		var exts []extent
		for i := 0; i+16 <= len(data) && i < 16*64; i += 16 {
			exts = append(exts, decodeExtent(data[i:]))
		}
		if validateExtents(exts, 1) != nil {
			return
		}
		for i := range exts {
			for j := i + 1; j < len(exts); j++ {
				a, b := exts[i], exts[j]
				if a.count == 0 || b.count == 0 {
					continue
				}
				if a.startOff < b.startOff+uint64(b.count) && b.startOff < a.startOff+uint64(a.count) {
					t.Fatalf("accepted file-offset overlap: %+v %+v", a, b)
				}
				if a.startBlock < b.startBlock+uint64(b.count) && b.startBlock < a.startBlock+uint64(a.count) {
					t.Fatalf("accepted physical overlap: %+v %+v", a, b)
				}
			}
		}
	})
}
