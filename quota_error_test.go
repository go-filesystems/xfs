package filesystem_xfs

import (
	"io"
	"path/filepath"
	"testing"
)

// withQuotaFormat formats a quota image after installing the given seam
// overrides, restoring them afterwards, and returns the Format error.
func quotaFormatErr(t *testing.T, install func()) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "q.img")
	install()
	fs, err := Format(path, 3*testOneAG, FormatConfig{Quota: QuotaConfig{User: true}})
	if err == nil {
		fs.Close()
	}
	return err
}

func TestSetupQuotaAllocInodeFailure(t *testing.T) {
	orig := quotaAllocInode
	defer func() { quotaAllocInode = orig }()
	err := quotaFormatErr(t, func() {
		quotaAllocInode = func(readerWriterAt, int64, *superblock, uint32) (uint64, error) {
			return 0, errInjected
		}
	})
	if err == nil {
		t.Fatal("quota alloc inode failure: want error")
	}
}

func TestSetupQuotaAllocBlockFailure(t *testing.T) {
	orig := quotaAllocBlocks
	defer func() { quotaAllocBlocks = orig }()
	err := quotaFormatErr(t, func() {
		quotaAllocBlocks = func(readerWriterAt, int64, *superblock, uint32, uint32) (uint64, error) {
			return 0, errInjected
		}
	})
	if err == nil {
		t.Fatal("quota alloc block failure: want error")
	}
}

func TestSetupQuotaWriteInodeFailure(t *testing.T) {
	orig := quotaWriteInode
	defer func() { quotaWriteInode = orig }()
	err := quotaFormatErr(t, func() {
		quotaWriteInode = func(io.WriterAt, int64, *superblock, *inode) error { return errInjected }
	})
	if err == nil {
		t.Fatal("quota write inode failure: want error")
	}
}

func TestSetupQuotaWriteBlocksFailure(t *testing.T) {
	orig := quotaWriteBlocks
	defer func() { quotaWriteBlocks = orig }()
	err := quotaFormatErr(t, func() {
		quotaWriteBlocks = func(io.WriterAt, int64, *superblock, uint64, uint32, []byte) error {
			return errInjected
		}
	})
	if err == nil {
		t.Fatal("quota write blocks failure: want error")
	}
}

func TestQuotaRecomputeReadInodeFailure(t *testing.T) {
	fs := formatQuota(t, QuotaConfig{User: true})
	defer fs.Close()
	orig := quotaReadInode
	defer func() { quotaReadInode = orig }()
	calls := 0
	quotaReadInode = func(r io.ReaderAt, off int64, sb *superblock, ino uint64) (*inode, error) {
		calls++
		if calls > 1 {
			return nil, errInjected
		}
		return orig(r, off, sb, ino)
	}
	if err := quotaRecompute(fs.f, fs.partOffset, fs.sb); err == nil {
		t.Fatal("quotaRecompute read failure: want error")
	}
}

func TestQuotaRecomputeDisabledNoop(t *testing.T) {
	// A filesystem without quotas: recompute is a no-op.
	path := filepath.Join(t.TempDir(), "noq.img")
	fs, err := Format(path, testOneAG, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	xfs := fs.(*xfsFS)
	if err := quotaRecompute(xfs.f, xfs.partOffset, xfs.sb); err != nil {
		t.Fatalf("recompute on non-quota fs: %v", err)
	}
}

func TestInobtEnumerateAGIFailure(t *testing.T) {
	fs := formatQuota(t, QuotaConfig{User: true})
	defer fs.Close()
	// Enumerating a nonexistent AG surfaces the AGI read/parse error.
	err := inobtEnumerate(fs.f, fs.partOffset, fs.sb, fs.sb.agCount+5, func(uint32, uint64) error { return nil })
	if err == nil {
		t.Fatal("inobtEnumerate bad AG: want error")
	}
}
