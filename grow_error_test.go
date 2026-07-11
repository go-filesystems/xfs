package filesystem_xfs

import (
	"path/filepath"
	"testing"
)

// faultBackend wraps a real blockBackend and injects failures to exercise the
// I/O error branches of Grow.
type faultBackend struct {
	inner          blockBackend
	failTruncate   bool
	failSync       bool
	failWriteAfter int   // -1 = never; otherwise fail once writes exceed this count
	failWriteOff   int64 // -1 = disabled; otherwise fail a write at this offset
	writes         int
}

func (b *faultBackend) ReadAt(p []byte, off int64) (int, error) { return b.inner.ReadAt(p, off) }
func (b *faultBackend) WriteAt(p []byte, off int64) (int, error) {
	b.writes++
	if b.failWriteAfter >= 0 && b.writes > b.failWriteAfter {
		return 0, errBoom
	}
	if b.failWriteOff >= 0 && off == b.failWriteOff {
		return 0, errBoom
	}
	return b.inner.WriteAt(p, off)
}
func (b *faultBackend) Truncate(n int64) error {
	if b.failTruncate {
		return errBoom
	}
	return b.inner.Truncate(n)
}
func (b *faultBackend) Sync() error {
	if b.failSync {
		return errBoom
	}
	return b.inner.Sync()
}
func (b *faultBackend) Size() (int64, error) { return b.inner.Size() }
func (b *faultBackend) Close() error         { return b.inner.Close() }

func growFaultFS(t *testing.T) *xfsFS {
	t.Helper()
	path := filepath.Join(t.TempDir(), "g.img")
	fs, err := Format(path, testOneAG, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return fs.(*xfsFS)
}

func TestGrowTruncateFailure(t *testing.T) {
	fs := growFaultFS(t)
	defer fs.Close()
	fs.f = &faultBackend{inner: fs.f, failTruncate: true, failWriteAfter: -1, failWriteOff: -1}
	if err := fs.Grow(2 * testOneAG); err == nil {
		t.Fatal("grow truncate failure: want error")
	}
}

func TestGrowWriteAGFailure(t *testing.T) {
	fs := growFaultFS(t)
	defer fs.Close()
	// Allow the truncate, fail on the first AG write.
	fs.f = &faultBackend{inner: fs.f, failWriteAfter: 0, failWriteOff: -1}
	if err := fs.Grow(2 * testOneAG); err == nil {
		t.Fatal("grow AG write failure: want error")
	}
}

func TestGrowSyncFailure(t *testing.T) {
	fs := growFaultFS(t)
	defer fs.Close()
	fs.f = &faultBackend{inner: fs.f, failSync: true, failWriteAfter: -1, failWriteOff: -1}
	if err := fs.Grow(2 * testOneAG); err == nil {
		t.Fatal("grow sync failure: want error")
	}
}

func TestGrowSuperblockFailure(t *testing.T) {
	fs := growFaultFS(t)
	defer fs.Close()
	// Fail the primary superblock rewrite (offset 0), letting the per-AG writes
	// at higher offsets through.
	fs.f = &faultBackend{inner: fs.f, failWriteAfter: -1, failWriteOff: 0}
	if err := fs.Grow(2 * testOneAG); err == nil {
		t.Fatal("grow superblock failure: want error")
	}
}
