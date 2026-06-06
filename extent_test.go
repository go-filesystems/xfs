// extent_test.go — whitebox unit tests for extent encode/decode.
package filesystem_xfs

import (
	"encoding/binary"
	"testing"
)

// TestDecodeEncodeRoundtrip verifies that encodeExtent(decodeExtent(rec)) == rec
// for a variety of hand-crafted 128-bit bmbt records.
func TestDecodeEncodeRoundtrip(t *testing.T) {
	tests := []struct {
		name       string
		startOff   uint64
		startBlock uint64
		count      uint32
		unwritten  bool
	}{
		{"zero", 0, 0, 0, false},
		{"simple", 0, 1024, 8, false},
		{"unwritten", 4, 2048, 16, true},
		{"max_count", 0, 0, (1 << 21) - 1, false},
		{"large_startoff", (1 << 54) - 1, 0, 1, false},
		{"large_startblock", 0, (1 << 52) - 1, 1, false},
		{"all_fields", 7, 12345678, 42, false},
		{"unwritten_large", (1 << 54) - 1, (1 << 52) - 1, (1 << 21) - 1, true},
		{"startblock_high_bits", 0, 0x1FF << 43, 1, false},
		{"startblock_low_bits", 0, 0x7FFFFFFFFFF, 1, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := extent{
				startOff:   tc.startOff,
				startBlock: tc.startBlock,
				count:      tc.count,
				unwritten:  tc.unwritten,
			}
			rec := encodeExtent(e)
			got := decodeExtent(rec[:])

			if got.startOff != tc.startOff {
				t.Errorf("startOff: got %d, want %d", got.startOff, tc.startOff)
			}
			if got.startBlock != tc.startBlock {
				t.Errorf("startBlock: got %d, want %d", got.startBlock, tc.startBlock)
			}
			if got.count != tc.count {
				t.Errorf("count: got %d, want %d", got.count, tc.count)
			}
			if got.unwritten != tc.unwritten {
				t.Errorf("unwritten: got %v, want %v", got.unwritten, tc.unwritten)
			}
		})
	}
}

// TestDecodeExtent_KnownBytes verifies decoding of a hand-crafted byte sequence.
// Extent: startOff=0, startBlock=0x400 (1024), count=8, unwritten=false.
//
// r0 = 0 | (0 << 9) | (1024 >> 43) = 0
// r1 = (1024 & mask43) << 21 | 8 = 1024<<21 | 8 = 0x0000080000000008
func TestDecodeExtent_KnownBytes(t *testing.T) {
	var rec [16]byte
	binary.BigEndian.PutUint64(rec[0:], 0)
	binary.BigEndian.PutUint64(rec[8:], uint64(1024)<<21|8)

	e := decodeExtent(rec[:])
	if e.startOff != 0 {
		t.Errorf("startOff: got %d, want 0", e.startOff)
	}
	if e.startBlock != 1024 {
		t.Errorf("startBlock: got %d, want 1024", e.startBlock)
	}
	if e.count != 8 {
		t.Errorf("count: got %d, want 8", e.count)
	}
	if e.unwritten {
		t.Errorf("unwritten: got true, want false")
	}
}

// TestDecodeExtent_UnwrittenFlag checks that the unwritten flag is read
// from the MSB of the high 64-bit word.
func TestDecodeExtent_UnwrittenFlag(t *testing.T) {
	var rec [16]byte
	// Set only the MSB (unwritten flag).
	binary.BigEndian.PutUint64(rec[0:], uint64(1)<<63)
	binary.BigEndian.PutUint64(rec[8:], 1) // count=1

	e := decodeExtent(rec[:])
	if !e.unwritten {
		t.Errorf("unwritten: got false, want true")
	}
	if e.startOff != 0 {
		t.Errorf("startOff: got %d, want 0", e.startOff)
	}
	if e.count != 1 {
		t.Errorf("count: got %d, want 1", e.count)
	}
}

// TestDecodeExtent_MaxValues exercises the case where all fields are at
// their maximum representable value.
func TestDecodeExtent_MaxValues(t *testing.T) {
	maxStartOff := uint64((1 << 54) - 1)
	maxStartBlock := uint64((1 << 52) - 1)
	maxCount := uint32((1 << 21) - 1)

	e := extent{
		startOff:   maxStartOff,
		startBlock: maxStartBlock,
		count:      maxCount,
		unwritten:  true,
	}
	rec := encodeExtent(e)
	got := decodeExtent(rec[:])

	if got.startOff != maxStartOff {
		t.Errorf("startOff: got 0x%X, want 0x%X", got.startOff, maxStartOff)
	}
	if got.startBlock != maxStartBlock {
		t.Errorf("startBlock: got 0x%X, want 0x%X", got.startBlock, maxStartBlock)
	}
	if got.count != maxCount {
		t.Errorf("count: got %d, want %d", got.count, maxCount)
	}
}

// TestBtreeLeafExtents verifies that btreeLeafExtents correctly extracts
// n records from a buffer.
func TestBtreeLeafExtents(t *testing.T) {
	// Build 3 consecutive 16-byte extents in a buffer.
	exts := []extent{
		{startOff: 0, startBlock: 100, count: 8, unwritten: false},
		{startOff: 8, startBlock: 200, count: 4, unwritten: false},
		{startOff: 12, startBlock: 300, count: 2, unwritten: true},
	}
	var buf [48]byte
	for i, e := range exts {
		rec := encodeExtent(e)
		copy(buf[i*16:], rec[:])
	}

	got := btreeLeafExtents(buf[:], 3)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	for i, want := range exts {
		if got[i].startOff != want.startOff ||
			got[i].startBlock != want.startBlock ||
			got[i].count != want.count ||
			got[i].unwritten != want.unwritten {
			t.Errorf("extent[%d]: got %+v, want %+v", i, got[i], want)
		}
	}
}

// TestBtreeLeafExtents_TruncatedBuffer confirms that btreeLeafExtents stops
// reading when the buffer is too small for the requested count.
func TestBtreeLeafExtents_TruncatedBuffer(t *testing.T) {
	// Only 1 full record in a 24-byte buffer; request 3.
	var buf [24]byte
	rec := encodeExtent(extent{startOff: 0, startBlock: 42, count: 5})
	copy(buf[0:], rec[:])
	// Second record fits.
	rec2 := encodeExtent(extent{startOff: 5, startBlock: 47, count: 3})
	copy(buf[16:], rec2[:8]) // deliberately truncate the second record

	got := btreeLeafExtents(buf[:], 3)
	// Only 1 complete record (buf[0:16] is complete, buf[16:24] is only 8 bytes < 16).
	if len(got) != 1 {
		t.Errorf("len(got) = %d, want 1", len(got))
	}
}
