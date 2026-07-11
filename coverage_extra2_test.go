package filesystem_xfs

import (
	"encoding/binary"
	"io"
	"path/filepath"
	"testing"
)

// TestSetupQuotaGroupProjectFailure fails the second/third quota-inode
// allocation so the group and project error branches of setupQuota run.
func TestSetupQuotaGroupProjectFailure(t *testing.T) {
	for _, failOn := range []int{2, 3} {
		orig := quotaAllocInode
		count := 0
		quotaAllocInode = func(rw readerWriterAt, off int64, sb *superblock, ag uint32) (uint64, error) {
			count++
			if count >= failOn {
				return 0, errInjected
			}
			return orig(rw, off, sb, ag)
		}
		path := filepath.Join(t.TempDir(), "q.img")
		fs, err := Format(path, 3*testOneAG, FormatConfig{Quota: QuotaConfig{User: true, Group: true, Project: true}})
		quotaAllocInode = orig
		if err == nil {
			fs.Close()
			t.Fatalf("failOn=%d: expected error", failOn)
		}
	}
}

// TestRefcountReadNumrecsOverflow crafts a refcount root whose numrecs would
// run past the block, which the reader must reject.
func TestRefcountReadNumrecsOverflow(t *testing.T) {
	fs := formatReflink(t, 1)
	defer fs.Close()
	orig := reflinkReadAGBlock
	defer func() { reflinkReadAGBlock = orig }()
	reflinkReadAGBlock = func(r io.ReaderAt, off int64, sb *superblock, ag, rel uint32) ([]byte, error) {
		blk, err := orig(r, off, sb, ag, rel)
		if err != nil {
			return nil, err
		}
		binary.BigEndian.PutUint16(blk[6:], 0xFFFF) // impossibly many records
		return blk, nil
	}
	if _, err := refcountReadRecs(fs.f, fs.partOffset, fs.sb, 0); err == nil {
		t.Fatal("numrecs overflow: want error")
	}
}

// TestRefcountWriteOverflowGuard exercises the max-records guard in
// refcountWriteRecs via the exported single-block capacity.
func TestRefcountWriteFitsExactly(t *testing.T) {
	fs := formatReflink(t, 1)
	defer fs.Close()
	n := refcMaxRecs(fs.sb)
	recs := make([]refcRec, n)
	for i := range recs {
		recs[i] = refcRec{uint32(i*2 + 100), 1, 2}
	}
	if err := refcountWriteRecs(fs.f, fs.partOffset, fs.sb, 0, recs); err != nil {
		t.Fatalf("write max records: %v", err)
	}
	got, err := refcountReadRecs(fs.f, fs.partOffset, fs.sb, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("read back %d records, want %d", len(got), n)
	}
}
