package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestReadFileDataAdditional(t *testing.T) {
	sb := defaultSB()

	t.Run("local size exceeds fork", func(t *testing.T) {
		in := newTestInode(1, 0x8000, inodeFmtLocal, uint64(len(newTestInode(1, 0x8000, inodeFmtLocal, 0).dataFork())+1))
		if _, err := readFileData(newMemRW(0), 0, sb, in); err == nil {
			t.Fatal("expected inline read to fail when inode size exceeds the data fork")
		}
	})

	t.Run("extents error", func(t *testing.T) {
		in := newTestInode(2, 0x8000, inodeFmtExtents, 1)
		in.nExts = 64
		if _, err := readFileData(newMemRW(0), 0, sb, in); err == nil {
			t.Fatal("expected readFileData to propagate inline extent parsing failures")
		}
	})

	t.Run("extents success", func(t *testing.T) {
		in := newTestInode(3, 0x8000, inodeFmtExtents, 5)
		in.nExts = 1
		rec := encodeExtent(extent{startOff: 0, startBlock: 1, count: 1})
		copy(in.dataFork(), rec[:])
		rw := newMemRW(int(sb.blockSize * 2))
		copy(rw.data[sb.blockSize:], []byte("hello"))

		data, err := readFileData(rw, 0, sb, in)
		if err != nil || string(data) != "hello" {
			t.Fatalf("readFileData = %q, %v; want hello, nil", data, err)
		}
	})

	t.Run("btree error", func(t *testing.T) {
		in := &inode{num: 4, mode: 0x8000, format: inodeFmtBtree, raw: make([]byte, inodeCoreSize+1)}
		if _, err := readFileData(newMemRW(0), 0, sb, in); err == nil {
			t.Fatal("expected readFileData to propagate btree extent parsing failures")
		}
	})

	t.Run("unsupported format", func(t *testing.T) {
		in := newTestInode(5, 0x8000, 99, 0)
		if _, err := readFileData(newMemRW(0), 0, sb, in); err == nil {
			t.Fatal("expected readFileData to reject unsupported inode formats")
		}
	})
}

func TestBtreeExtentsAdditional(t *testing.T) {
	sb := defaultSB()

	t.Run("root too small", func(t *testing.T) {
		in := newTestInode(6, 0x8000, inodeFmtBtree, 0)
		fork := in.dataFork()
		binary.BigEndian.PutUint16(fork[0:], 1)
		binary.BigEndian.PutUint16(fork[2:], 1)
		if _, err := btreeExtents(newMemRW(0), 0, sb, in); err == nil {
			t.Fatal("expected btreeExtents to reject truncated roots")
		}
	})

	t.Run("leaf root", func(t *testing.T) {
		in := newTestInode(7, 0x8000, inodeFmtBtree, 0)
		fork := in.dataFork()
		binary.BigEndian.PutUint16(fork[0:], 0)
		binary.BigEndian.PutUint16(fork[2:], 1)
		rec := encodeExtent(extent{startOff: 0, startBlock: 2, count: 3})
		copy(fork[4:], rec[:])

		exts, err := btreeExtents(newMemRW(0), 0, sb, in)
		if err != nil || len(exts) != 1 || exts[0].startBlock != 2 || exts[0].count != 3 {
			t.Fatalf("unexpected leaf-root extents: %+v, %v", exts, err)
		}
	})

	t.Run("read leaf error", func(t *testing.T) {
		in := newTestInode(8, 0x8000, inodeFmtBtree, 0)
		fork := in.dataFork()
		binary.BigEndian.PutUint16(fork[0:], 1)
		binary.BigEndian.PutUint16(fork[2:], 1)
		binary.BigEndian.PutUint64(fork[20:], 3)
		rw := newMemRW(int(sb.blockSize * 4))
		rw.readHook = func(off int64, _ []byte) error {
			if off == int64(3)*int64(sb.blockSize) {
				return errBoom
			}
			return nil
		}
		if _, err := btreeExtents(rw, 0, sb, in); !errors.Is(err, errBoom) {
			t.Fatalf("expected leaf read error %v, got %v", errBoom, err)
		}
	})

	t.Run("internal root with sibling leaves", func(t *testing.T) {
		in := newTestInode(9, 0x8000, inodeFmtBtree, 0)
		fork := in.dataFork()
		binary.BigEndian.PutUint16(fork[0:], 1)
		binary.BigEndian.PutUint16(fork[2:], 1)
		binary.BigEndian.PutUint64(fork[20:], 3)
		rw := newMemRW(int(sb.blockSize * 5))
		hdr := btreeHdrSize(sb.hasCRC)
		leaf1 := rw.data[int(3*sb.blockSize):int(4*sb.blockSize)]
		binary.BigEndian.PutUint16(leaf1[4:], 0)
		binary.BigEndian.PutUint16(leaf1[6:], 1)
		binary.BigEndian.PutUint32(leaf1[12:], 4)
		rec1 := encodeExtent(extent{startOff: 0, startBlock: 10, count: 1})
		copy(leaf1[hdr:], rec1[:])
		leaf2 := rw.data[int(4*sb.blockSize):int(5*sb.blockSize)]
		binary.BigEndian.PutUint16(leaf2[4:], 0)
		binary.BigEndian.PutUint16(leaf2[6:], 1)
		binary.BigEndian.PutUint32(leaf2[12:], 0xFFFFFFFF)
		rec2 := encodeExtent(extent{startOff: 1, startBlock: 11, count: 2})
		copy(leaf2[hdr:], rec2[:])

		exts, err := btreeExtents(rw, 0, sb, in)
		if err != nil || len(exts) != 2 || exts[1].startBlock != 11 {
			t.Fatalf("unexpected chained extents: %+v, %v", exts, err)
		}
	})
}

func TestBtreeHdrSizeAdditional(t *testing.T) {
	if btreeHdrSize(true) != 56 || btreeHdrSize(false) != 16 {
		t.Fatal("btreeHdrSize returned unexpected values")
	}
}

func TestReadExtentsAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(int(sb.blockSize * 2))
	copy(rw.data[sb.blockSize:], []byte("payload"))
	exts := []extent{{startOff: 0, startBlock: 1, count: 1}, {startOff: 2, startBlock: 1, count: 1}}

	data, err := readExtents(rw, 0, sb, exts, 7)
	if err != nil || string(data) != "payload" {
		t.Fatalf("readExtents = %q, %v; want payload, nil", data, err)
	}

	rw.readHook = func(int64, []byte) error { return errBoom }
	if _, err := readExtents(rw, 0, sb, exts[:1], 7); !errors.Is(err, errBoom) {
		t.Fatalf("expected readExtents error %v, got %v", errBoom, err)
	}
}

func TestReadFileDataBtreeSuccess(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(10, 0x8000, inodeFmtBtree, 5)
	fork := in.dataFork()
	binary.BigEndian.PutUint16(fork[0:], 0)
	binary.BigEndian.PutUint16(fork[2:], 1)
	rec := encodeExtent(extent{startOff: 0, startBlock: 1, count: 1})
	copy(fork[4:], rec[:])
	rw := newMemRW(int(sb.blockSize * 2))
	copy(rw.data[sb.blockSize:], []byte("hello"))

	data, err := readFileData(rw, 0, sb, in)
	if err != nil || string(data) != "hello" {
		t.Fatalf("readFileData btree = %q, %v; want hello, nil", data, err)
	}
}

func TestBtreeExtentsRemainingBranches(t *testing.T) {
	sb := defaultSB()

	t.Run("internal node read error", func(t *testing.T) {
		in := newTestInode(11, 0x8000, inodeFmtBtree, 0)
		fork := in.dataFork()
		binary.BigEndian.PutUint16(fork[0:], 2)
		binary.BigEndian.PutUint16(fork[2:], 1)
		binary.BigEndian.PutUint64(fork[20:], 3)
		rw := newMemRW(int(sb.blockSize * 4))
		rw.readHook = func(off int64, _ []byte) error {
			if off == int64(3)*int64(sb.blockSize) {
				return errBoom
			}
			return nil
		}
		if _, err := btreeExtents(rw, 0, sb, in); !errors.Is(err, errBoom) {
			t.Fatalf("expected internal node read error %v, got %v", errBoom, err)
		}
	})

	t.Run("internal node too small", func(t *testing.T) {
		in := newTestInode(12, 0x8000, inodeFmtBtree, 0)
		fork := in.dataFork()
		binary.BigEndian.PutUint16(fork[0:], 2)
		binary.BigEndian.PutUint16(fork[2:], 1)
		binary.BigEndian.PutUint64(fork[20:], 3)
		rw := newMemRW(int(sb.blockSize * 4))
		blk := rw.data[int(3*sb.blockSize):int(4*sb.blockSize)]
		binary.BigEndian.PutUint16(blk[6:], 300)
		if _, err := btreeExtents(rw, 0, sb, in); err == nil {
			t.Fatal("expected btreeExtents to reject undersized internal nodes")
		}
	})

	t.Run("level two success", func(t *testing.T) {
		in := newTestInode(13, 0x8000, inodeFmtBtree, 0)
		fork := in.dataFork()
		binary.BigEndian.PutUint16(fork[0:], 2)
		binary.BigEndian.PutUint16(fork[2:], 1)
		binary.BigEndian.PutUint64(fork[20:], 3)
		rw := newMemRW(int(sb.blockSize * 5))
		hdr := btreeHdrSize(sb.hasCRC)
		internal := rw.data[int(3*sb.blockSize):int(4*sb.blockSize)]
		binary.BigEndian.PutUint16(internal[6:], 1)
		binary.BigEndian.PutUint64(internal[hdr+16:], 4)
		leaf := rw.data[int(4*sb.blockSize):int(5*sb.blockSize)]
		binary.BigEndian.PutUint16(leaf[6:], 1)
		binary.BigEndian.PutUint32(leaf[12:], 0xFFFFFFFF)
		rec := encodeExtent(extent{startOff: 0, startBlock: 9, count: 2})
		copy(leaf[hdr:], rec[:])

		exts, err := btreeExtents(rw, 0, sb, in)
		if err != nil || len(exts) != 1 || exts[0].startBlock != 9 || exts[0].count != 2 {
			t.Fatalf("unexpected level-two extents: %+v, %v", exts, err)
		}
	})
}

func TestBtreeExtentsRootTooSmall(t *testing.T) {
	sb := defaultSB()
	in := newTestInode(14, 0x8000, inodeFmtBtree, 0)
	in.raw = in.raw[:inodeCoreSize+27]
	fork := in.dataFork()
	binary.BigEndian.PutUint16(fork[0:], 1)
	binary.BigEndian.PutUint16(fork[2:], 1)
	if _, err := btreeExtents(newMemRW(0), 0, sb, in); err == nil {
		t.Fatal("expected btreeExtents to reject truncated roots with missing child pointers")
	}
}
