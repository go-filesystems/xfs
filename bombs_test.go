package filesystem_xfs

import (
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/go-volumes/safeio"
)

// Memory-bomb regression tests: every input-driven allocation must be refused
// BEFORE it happens. Each test builds a tiny hostile structure and asserts the
// error and that the whole call allocated (runtime.MemStats.TotalAlloc is
// cumulative) or read (countReader) little.

const bombAllocLimit = 1 << 20

func allocDelta(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// countReader counts the bytes read through it.
type countReader struct {
	r io.ReaderAt
	n int64
}

func (c *countReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}

// putForkExtents writes extents into the inode's data fork (extents format).
func putForkExtents(in *inode, exts ...extent) {
	fork := in.dataFork()
	for i, e := range exts {
		rec := encodeExtent(e)
		copy(fork[i*16:], rec[:])
	}
	in.nExts = uint32(len(exts))
}

// An inode whose di_nblocks is forged to 2^40 (it bounds the buffer, and is
// itself read from the image) and whose di_size is 2 GiB, on a 8 KiB image.
func TestBomb_ForgedNBlocksHugeSize(t *testing.T) {
	sb := defaultSB()
	sb.dBlocks = 2 // the superblock says the filesystem has two blocks
	in := newTestInode(10, 0x8000, inodeFmtExtents, 2<<30)
	setInodeNBlocks(in, 1<<40)
	putForkExtents(in, extent{startOff: 0, startBlock: 1, count: 1})
	var err error
	got := allocDelta(func() { _, err = readFileData(newMemRW(int(sb.blockSize*2)), 0, sb, in) })
	t.Logf("allocated %d bytes, err=%v", got, err)
	if !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes before refusing", got)
	}
}

// Same forged nblocks against a superblock that declares a huge filesystem:
// the read limit is what stops it.
func TestBomb_DeclaredHugeFilesystemHugeSize(t *testing.T) {
	sb := defaultSB()
	sb.dBlocks = 1 << 40
	in := newTestInode(10, 0x8000, inodeFmtExtents, 2<<30)
	setInodeNBlocks(in, 1<<40)
	putForkExtents(in, extent{startOff: 0, startBlock: 1, count: 1})
	var err error
	got := allocDelta(func() { _, err = readFileData(newMemRW(int(sb.blockSize*2)), 0, sb, in) })
	t.Logf("allocated %d bytes, err=%v", got, err)
	if !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got > bombAllocLimit {
		t.Fatalf("allocated %d bytes before refusing", got)
	}
}

// Extents all claiming the same logical and physical blocks are read once per
// extent.
func TestBomb_OverlappingExtentsRefused(t *testing.T) {
	sb := defaultSB()
	const n = 8
	in := newTestInode(10, 0x8000, inodeFmtExtents, 4*4096)
	setInodeNBlocks(in, 4)
	exts := make([]extent, n)
	for i := range exts {
		exts[i] = extent{startOff: 0, startBlock: 1, count: 4}
	}
	putForkExtents(in, exts...)
	cr := &countReader{r: newMemRW(int(sb.blockSize * 8))}
	_, err := readFileData(cr, 0, sb, in)
	t.Logf("read %d bytes for a %d-byte file, err=%v", cr.n, in.size, err)
	if err == nil {
		t.Fatal("overlapping extents accepted")
	}
	if cr.n > 2*int64(in.size) {
		t.Fatalf("read %d bytes for a %d-byte file", cr.n, in.size)
	}
}

// btreeLeafExtents sized its slice from the record count in the block header,
// not from the bytes the block holds.
func TestBomb_BtreeLeafCountFromHeader(t *testing.T) {
	data := make([]byte, 4096-56)
	got := allocDelta(func() { _ = btreeLeafExtents(data, 65535) })
	t.Logf("allocated %d bytes", got)
	if got > bombAllocLimit/8 {
		t.Fatalf("allocated %d bytes for a %d-byte block", got, len(data))
	}
}

// reflinkCopyLocal allocated di_size before looking at the fork.
func TestBomb_ReflinkCopyLocalSize(t *testing.T) {
	sb := defaultSB()
	src := newTestInode(10, 0x8000, inodeFmtLocal, 2<<30)
	dir := newTestInode(2, 0x4000, inodeFmtLocal, 0)
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = errors.New("panic")
				t.Logf("panic: %v", r)
			}
		}()
		got := allocDelta(func() { err = reflinkCopyLocal(newMemRW(int(sb.blockSize*8)), 0, sb, src, dir, "x") })
		t.Logf("allocated %d bytes, err=%v", got, err)
		if got > bombAllocLimit {
			t.Errorf("allocated %d bytes", got)
		}
	}()
	if err == nil {
		t.Fatal("inline source larger than its fork accepted")
	}
	if err.Error() == "panic" {
		t.Fatal("panicked")
	}
}

// The read limit is exact: di_size == limit is read, limit+1 is refused.
func TestBomb_MaxReadSizeExact(t *testing.T) {
	sb := defaultSB()
	fs := &xfsFS{sb: sb}
	fs.SetMaxReadSize(4096)
	if fs.MaxReadSize() != 4096 {
		t.Fatalf("MaxReadSize = %d", fs.MaxReadSize())
	}
	in := newTestInode(10, 0x8000, inodeFmtExtents, 4096)
	setInodeNBlocks(in, 1<<40)
	putForkExtents(in, extent{startOff: 0, startBlock: 1, count: 1})
	rw := newMemRW(int(sb.blockSize * 2))
	if out, err := readFileData(rw, 0, sb, in); err != nil || len(out) != 4096 {
		t.Fatalf("at the limit: len=%d err=%v", len(out), err)
	}
	in.size = 4097
	if _, err := readFileData(rw, 0, sb, in); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("one over the limit: err = %v, want ErrTooLarge", err)
	}
	fs.SetMaxReadSize(-1)
	if fs.MaxReadSize() != DefaultMaxReadSize {
		t.Fatalf("default not restored: %d", fs.MaxReadSize())
	}
}

// The declared filesystem size is an exact ceiling too (2 blocks of 4 KiB).
func TestBomb_DeclaredSizeExact(t *testing.T) {
	sb := defaultSB()
	sb.dBlocks = 2
	in := newTestInode(10, 0x8000, inodeFmtExtents, 8192)
	setInodeNBlocks(in, 1<<40)
	putForkExtents(in, extent{startOff: 0, startBlock: 1, count: 1})
	rw := newMemRW(int(sb.blockSize * 4))
	if _, err := readFileData(rw, 0, sb, in); err != nil {
		t.Fatalf("at the declared size: %v", err)
	}
	in.size = 8193
	if _, err := readFileData(rw, 0, sb, in); !errors.Is(err, safeio.ErrTooLarge) {
		t.Fatalf("one over: err = %v, want ErrTooLarge", err)
	}
}

func TestBomb_DeclaredBytesOverflow(t *testing.T) {
	sb := &superblock{blockSize: 4096, dBlocks: 1 << 62}
	if n, ok := sb.declaredBytes(); !ok || n <= 0 {
		t.Fatalf("declaredBytes = %d, %v", n, ok)
	}
	if _, ok := (&superblock{blockSize: 4096}).declaredBytes(); ok {
		t.Fatal("zero dBlocks reported as known")
	}
}

func TestBomb_ValidateExtents(t *testing.T) {
	ok := [][]extent{
		nil,
		{{startOff: 0, startBlock: 10, count: 4}},
		{{startOff: 0, startBlock: 10, count: 4}, {startOff: 4, startBlock: 14, count: 4}}, // adjacent
		{{startOff: 8, startBlock: 30, count: 4}, {startOff: 0, startBlock: 10, count: 4}}, // disordered, disjoint
		{{startOff: 0, startBlock: 10, count: 4}, {startOff: 2, startBlock: 10, count: 0}}, // zero length maps nothing
		{{startOff: 0, startBlock: 10, count: 4}, {startOff: 100, startBlock: 20, count: 4}},
	}
	for i, l := range ok {
		if err := validateExtents(l, 2); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := [][]extent{
		{{startOff: 0, startBlock: 10, count: 4}, {startOff: 3, startBlock: 50, count: 4}},   // file offset overlap by one
		{{startOff: 0, startBlock: 10, count: 4}, {startOff: 10, startBlock: 13, count: 4}},  // physical overlap by one
		{{startOff: 10, startBlock: 50, count: 4}, {startOff: 0, startBlock: 10, count: 11}}, // disordered, overlapping
		{{startOff: 0, startBlock: 10, count: 4}, {startOff: 0, startBlock: 10, count: 4}},   // identical
	}
	for i, l := range bad {
		if err := validateExtents(l, 2); err == nil {
			t.Errorf("bad[%d]: accepted", i)
		}
	}
	l := []extent{{startOff: 8, startBlock: 30, count: 4}, {startOff: 0, startBlock: 10, count: 4}}
	_ = validateExtents(l, 2)
	if l[0].startOff != 8 {
		t.Fatal("validateExtents reordered its argument")
	}
}

// A btree root forging overlapping extents is refused the same way.
func TestBomb_BtreeLeafRecordsAreCapped(t *testing.T) {
	data := make([]byte, 16*10)
	for i := 0; i < 10; i++ {
		rec := encodeExtent(extent{startOff: uint64(i), startBlock: uint64(100 + i), count: 1})
		copy(data[i*16:], rec[:])
	}
	if got := btreeLeafExtents(data, 10); len(got) != 10 {
		t.Fatalf("exact fit: %d records", len(got))
	}
	if got := btreeLeafExtents(data, 65535); len(got) != 10 {
		t.Fatalf("header claiming more than the block holds: %d records", len(got))
	}
	if got := btreeLeafExtents(data, 3); len(got) != 3 {
		t.Fatalf("header claiming fewer: %d records", len(got))
	}
}

// Every AG start must lie inside the device; the bound is exact.
func TestBomb_GeometryFitsDevice(t *testing.T) {
	sb := defaultSB() // 2 AGs of 16384 4 KiB blocks: AG 1 starts at block 16384
	const atBound = 16384 * 4096
	if err := checkGeometryFitsDevice(sb, atBound); err != nil {
		t.Fatalf("last AG starting exactly at the end of the device block range: %v", err)
	}
	if err := checkGeometryFitsDevice(sb, atBound-4096); !errors.Is(err, safeio.ErrOutOfBounds) {
		t.Fatalf("one block short: err = %v", err)
	}
	if err := checkGeometryFitsDevice(sb, 0); err != nil {
		t.Fatalf("unknown device size must not refuse: %v", err)
	}
	huge := defaultSB()
	huge.agCount = 0xFFFFFFFF
	if err := checkGeometryFitsDevice(huge, 1<<30); !errors.Is(err, safeio.ErrOutOfBounds) {
		t.Fatalf("2^32 AGs on a 1 GiB device: err = %v", err)
	}
	// 3 AGs of 2^31 blocks on an 8 KiB image: the product must not wrap.
	if err := checkGeometryFitsDevice(&superblock{blockSize: 4096, agBlocks: 1 << 31, agCount: 3}, 8192); err == nil {
		t.Fatal("tiny image declaring 3 AGs of 2^31 blocks accepted")
	}
}
