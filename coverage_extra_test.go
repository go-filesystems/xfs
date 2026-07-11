package filesystem_xfs

import (
	"io"
	"testing"
)

// readFaultBackend fails ReadAt at/after a given call count.
type readFaultBackend struct {
	inner    blockBackend
	failRead int // -1 disabled; else fail once reads exceed this count
	reads    int
}

func (b *readFaultBackend) ReadAt(p []byte, off int64) (int, error) {
	b.reads++
	if b.failRead >= 0 && b.reads > b.failRead {
		return 0, errBoom
	}
	return b.inner.ReadAt(p, off)
}
func (b *readFaultBackend) WriteAt(p []byte, off int64) (int, error) { return b.inner.WriteAt(p, off) }
func (b *readFaultBackend) Truncate(n int64) error                   { return b.inner.Truncate(n) }
func (b *readFaultBackend) Sync() error                              { return b.inner.Sync() }
func (b *readFaultBackend) Size() (int64, error)                     { return b.inner.Size() }
func (b *readFaultBackend) Close() error                             { return b.inner.Close() }

func TestInobtEnumerateLeafReadFailure(t *testing.T) {
	fs := formatQuota(t, QuotaConfig{User: true})
	defer fs.Close()
	// Let the AGI read through, fail the inobt leaf read.
	fb := &readFaultBackend{inner: fs.f, failRead: 1}
	err := inobtEnumerate(fb, fs.partOffset, fs.sb, 0, func(uint32, uint64) error { return nil })
	if err == nil {
		t.Fatal("inobtEnumerate leaf read failure: want error")
	}
}

func TestQuotaRecomputeEnumerateFailure(t *testing.T) {
	fs := formatQuota(t, QuotaConfig{User: true})
	defer fs.Close()
	fb := &readFaultBackend{inner: fs.f, failRead: 1}
	if err := quotaRecompute(fb, fs.partOffset, fs.sb); err == nil {
		t.Fatal("quotaRecompute enumerate failure: want error")
	}
}

func TestRefcIncrLeadingAndTrailingGap(t *testing.T) {
	// A range that starts before and ends after an existing record exercises
	// both the leading-gap and trailing-gap fills in refcClipInside.
	recs := []refcRec{{100, 10, 3}}
	out := refcIncrRange(recs, 95, 25) // covers [95,120): gap, record, gap
	want := []refcRec{{95, 5, 2}, {100, 10, 4}, {110, 10, 2}}
	if !equalRecs(out, want) {
		t.Fatalf("leading/trailing gap: got %+v want %+v", out, want)
	}
}

func TestRefcountShareReadFailure(t *testing.T) {
	fs := formatReflink(t, 1)
	defer fs.Close()
	orig := reflinkAGFBlock
	defer func() { reflinkAGFBlock = orig }()
	reflinkAGFBlock = func(io.ReaderAt, int64, *superblock, uint32) ([]byte, error) {
		return nil, errInjected
	}
	blk := fs.sb.agAbsBlock(0, 100)
	if err := refcountShareExtent(fs.f, fs.partOffset, fs.sb, blk, 4); err == nil {
		t.Fatal("share read failure: want error")
	}
}

func TestResetFreedInodeNrext64(t *testing.T) {
	// An inode carrying the NREXT64 flag has its big-extent counter cleared on
	// reset.
	raw := make([]byte, 512)
	in := &inode{raw: raw}
	// Set NREXT64 in di_flags2 and a nonzero big-extent count.
	f := uint64(xfsDiflag2Nrext64)
	putBE64(raw[inoOffFlags2:], f)
	putBE64(raw[inoOffBigNExtents:], 42)
	resetFreedInodeFork(in)
	if getBE64(raw[inoOffBigNExtents:]) != 0 {
		t.Fatal("big-extent count not cleared")
	}
}

func putBE64(b []byte, v uint64) {
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
}

func getBE64(b []byte) uint64 {
	var v uint64
	for i := 0; i < 8; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v
}
