package filesystem_xfs

import (
	"fmt"
	"math"
	"sort"
	"sync/atomic"

	"github.com/go-volumes/safeio"
)

// ErrTooLarge is returned, wrapped, when an on-disk size or count would make
// the driver allocate or walk more than the image can legitimately hold. It is
// safeio.ErrTooLarge, so errors.Is works against either name.
var ErrTooLarge = safeio.ErrTooLarge

// DefaultMaxReadSize is the largest file ReadFile will materialise in memory
// unless SetMaxReadSize says otherwise: 1 GiB.
//
// di_size, di_nblocks and the superblock's sb_dblocks all come from the image,
// so none of them can bound an allocation by itself: an inode forged to
// nblocks = 2^40 "owns" enough blocks for any size, and a superblock may
// declare a filesystem as large as it likes. Only the caller knows how much
// memory it is willing to spend, so there is a documented ceiling.
const DefaultMaxReadSize int64 = 1 << 30

// maxReadLimit is the effective ceiling on a materialised file.
func (sb *superblock) maxReadLimit() int64 {
	if m := atomic.LoadInt64(&sb.maxRead); m > 0 {
		return m
	}
	return DefaultMaxReadSize
}

// SetMaxReadSize sets the largest file ReadFile will materialise, in bytes.
// n <= 0 restores DefaultMaxReadSize. A file whose di_size exceeds it is
// refused with ErrTooLarge before any buffer is allocated.
func (fs *xfsFS) SetMaxReadSize(n int64) {
	if n < 0 {
		n = 0
	}
	atomic.StoreInt64(&fs.sb.maxRead, n)
}

// MaxReadSize reports the limit SetMaxReadSize configured (or the default).
func (fs *xfsFS) MaxReadSize() int64 { return fs.sb.maxReadLimit() }

// declaredBytes returns sb_dblocks * blocksize without overflowing, and false
// when the superblock does not say (synthetic superblocks only).
func (sb *superblock) declaredBytes() (int64, bool) {
	if sb.dBlocks == 0 || sb.blockSize == 0 {
		return 0, false
	}
	if sb.dBlocks > uint64(math.MaxInt64)/uint64(sb.blockSize) {
		return math.MaxInt64, true
	}
	return int64(sb.dBlocks) * int64(sb.blockSize), true
}

// fileCeiling bounds an allocation sized from di_size: the smaller of what the
// inode's own block count could hold (inodeMaxBytes), the declared filesystem
// size, and the read limit.
func fileCeiling(sb *superblock, in *inode) int64 {
	max := inodeMaxBytes(sb, in)
	if d, ok := sb.declaredBytes(); ok && d < max {
		max = d
	}
	if lim := sb.maxReadLimit(); lim < max {
		max = lim
	}
	return max
}

// validateExtents refuses an extent list whose records overlap, in file offset
// or in physical blocks. A valid fork maps each file block at most once and
// never maps two file blocks onto one physical block, so overlap is corruption
// -- and it is an amplifier: n records covering the same blocks make a reader
// read, copy and decode those blocks n times, so the work stops being
// proportional to the image. Records of zero length map nothing and are
// ignored.
//
// The common case, records already ascending in both orders, is one pass with
// no allocation; only a disordered list is sorted, on a copy.
func validateExtents(exts []extent, ino uint64) error {
	if len(exts) < 2 {
		return nil
	}
	if err := checkRuns(exts, ino, "file offset", func(e extent) uint64 { return e.startOff }); err != nil {
		return err
	}
	return checkRuns(exts, ino, "physical block", func(e extent) uint64 { return e.startBlock })
}

func checkRuns(exts []extent, ino uint64, kind string, start func(extent) uint64) error {
	ordered := true
	var prevEnd uint64
	have := false
	for _, e := range exts {
		if e.count == 0 {
			continue
		}
		s := start(e)
		if have && s < prevEnd {
			ordered = false
			break
		}
		prevEnd, have = s+uint64(e.count), true
	}
	if ordered {
		return nil
	}
	cp := make([]extent, 0, len(exts))
	for _, e := range exts {
		if e.count != 0 {
			cp = append(cp, e)
		}
	}
	sort.Slice(cp, func(i, j int) bool { return start(cp[i]) < start(cp[j]) })
	for i := 1; i < len(cp); i++ {
		if start(cp[i]) < start(cp[i-1])+uint64(cp[i-1].count) {
			return fmt.Errorf("xfs: inode %d has overlapping extents at %s %d: %w", ino, kind, start(cp[i]), safeio.ErrOutOfBounds)
		}
	}
	return nil
}

// checkGeometryFitsDevice refuses a superblock whose last allocation group
// starts beyond the end of the device. The per-AG loops (free-space scans,
// superblock-copy updates, inode allocation) run once per AG, and agcount is a
// 32-bit field of the image: without this a 512-byte superblock could ask for
// 2^32 iterations. devBytes is the size from the partition start; 0 means the
// device cannot say, and nothing is checked.
func checkGeometryFitsDevice(sb *superblock, devBytes int64) error {
	if devBytes <= 0 || sb.agCount == 0 {
		return nil
	}
	lastAG := uint64(sb.agCount-1) * uint64(sb.agBlocks)
	if lastAG > uint64(devBytes)/uint64(sb.blockSize) {
		return fmt.Errorf("xfs: allocation group %d starts at block %d, beyond the %d-byte device: %w",
			sb.agCount-1, lastAG, devBytes, safeio.ErrOutOfBounds)
	}
	return nil
}
