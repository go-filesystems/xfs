package filesystem_xfs

import "encoding/binary"

// extent is a decoded bmbt record: a contiguous run of file blocks mapped to
// a contiguous run of filesystem blocks.
type extent struct {
	startOff   uint64 // file block offset
	startBlock uint64 // absolute filesystem block number
	count      uint32
	unwritten  bool
}

// decodeExtent decodes a 16-byte big-endian bmbt record.
//
// Bit layout (high 64 bits = r0, low 64 bits = r1):
//
//	r0[63]       flag (unwritten)
//	r0[62:9]     startoff  (54 bits, file block offset)
//	r0[8:0]      startblock high 9 bits
//	r1[63:21]    startblock low 43 bits
//	r1[20:0]     blockcount (21 bits)
func decodeExtent(rec []byte) extent {
	r0 := binary.BigEndian.Uint64(rec[0:8])
	r1 := binary.BigEndian.Uint64(rec[8:16])
	return extent{
		unwritten:  (r0 >> 63) != 0,
		startOff:   (r0 >> 9) & ((1 << 54) - 1),
		startBlock: ((r0 & 0x1FF) << 43) | (r1 >> 21),
		count:      uint32(r1 & 0x1FFFFF),
	}
}

// encodeExtent encodes an extent back to a 16-byte bmbt record.
func encodeExtent(e extent) [16]byte {
	var flag uint64
	if e.unwritten {
		flag = 1
	}
	r0 := (flag << 63) | ((e.startOff & ((1 << 54) - 1)) << 9) | (e.startBlock >> 43)
	r1 := (e.startBlock&((1<<43)-1))<<21 | uint64(e.count)&0x1FFFFF
	var rec [16]byte
	binary.BigEndian.PutUint64(rec[0:8], r0)
	binary.BigEndian.PutUint64(rec[8:16], r1)
	return rec
}
