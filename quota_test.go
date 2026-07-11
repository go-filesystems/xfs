package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"
)

func formatQuota(t *testing.T, cfg QuotaConfig) *xfsFS {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quota.img")
	fs, err := Format(path, 3*testOneAG, FormatConfig{Label: "quota", Quota: cfg})
	if err != nil {
		t.Fatalf("Format(quota): %v", err)
	}
	return fs.(*xfsFS)
}

func TestQuotaConfigFlags(t *testing.T) {
	if !(QuotaConfig{}).isZero() {
		t.Fatal("zero QuotaConfig not isZero")
	}
	if (QuotaConfig{User: true}).isZero() {
		t.Fatal("User quota reported isZero")
	}
	// Accounting + checked, no enforcement.
	q := QuotaConfig{User: true, Group: true, Project: true}
	got := q.qflags()
	want := uint16(xfsUQuotaAcct | xfsUQuotaChkd | xfsGQuotaAcct | xfsGQuotaChkd | xfsPQuotaAcct | xfsPQuotaChkd)
	if got != want {
		t.Fatalf("qflags no-enforce = 0x%x want 0x%x", got, want)
	}
	// With enforcement (matches the mkfs reference 0x2cb ... plus CHKD bits).
	q.Enforce = true
	if q.qflags()&(xfsUQuotaEnfd|xfsGQuotaEnfd|xfsPQuotaEnfd) == 0 {
		t.Fatal("enforce flags not set")
	}
}

func TestQuotaSuperblockFields(t *testing.T) {
	fs := formatQuota(t, QuotaConfig{User: true, Group: true, Project: true, Enforce: true})
	defer fs.Close()

	// Re-read the on-disk superblock to confirm the quota fields persisted.
	sb, err := readSuperblock(fs.f, fs.partOffset)
	if err != nil {
		t.Fatal(err)
	}
	if sb.uQuotino == 0 || sb.gQuotino == 0 || sb.pQuotino == 0 {
		t.Fatalf("quota inodes unset: u=%d g=%d p=%d", sb.uQuotino, sb.gQuotino, sb.pQuotino)
	}
	if sb.quotaFlags == 0 {
		t.Fatal("qflags = 0")
	}
	// QUOTABIT must be set in the version number.
	buf := make([]byte, 512)
	if err := readBytes(fs.f, fs.partOffset, buf); err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(buf[sbOffVersionNum:])&xfsSBVersionQuotaBit == 0 {
		t.Fatal("QUOTABIT not set in sb_versionnum")
	}

	// The quota inodes are valid regular files.
	for _, ino := range []uint64{sb.uQuotino, sb.gQuotino, sb.pQuotino} {
		in, err := readInode(fs.f, fs.partOffset, fs.sb, ino)
		if err != nil {
			t.Fatalf("read quota inode %d: %v", ino, err)
		}
		if !in.isRegular() {
			t.Fatalf("quota inode %d not a regular file (mode 0%o)", ino, in.mode)
		}
	}
}

func TestQuotaOnlyUser(t *testing.T) {
	fs := formatQuota(t, QuotaConfig{User: true})
	defer fs.Close()
	sb, _ := readSuperblock(fs.f, fs.partOffset)
	if sb.uQuotino == 0 {
		t.Fatal("uquotino unset")
	}
	if sb.gQuotino != 0 || sb.pQuotino != 0 {
		t.Fatalf("group/project quota inodes should be unset: g=%d p=%d", sb.gQuotino, sb.pQuotino)
	}
}

func TestQuotaAccountingAfterWrite(t *testing.T) {
	fs := formatQuota(t, QuotaConfig{User: true, Group: true, Project: true})
	defer fs.Close()
	// Writing files (owned by id 0) must keep the dquots consistent via the
	// afterMutation quotacheck; the id-0 user dquot must reflect the usage.
	if err := fs.WriteFile("/a.dat", []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/b.dat", []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb := fs.sb
	in, err := readInode(fs.f, fs.partOffset, sb, sb.uQuotino)
	if err != nil {
		t.Fatal(err)
	}
	exts, err := inlineExtents(in)
	if err != nil || len(exts) == 0 {
		t.Fatalf("quota inode extents: %v", err)
	}
	blk, err := readRawBlock(fs.f, fs.partOffset, sb, exts[0].startBlock)
	if err != nil {
		t.Fatal(err)
	}
	// id-0 dquot lives at offset 0. icount must be >= 5 (root, rbm, rsum, 2 files).
	icount := binary.BigEndian.Uint64(blk[dqOffICount:])
	if icount < 5 {
		t.Fatalf("id-0 user icount = %d, want >= 5", icount)
	}
	if binary.BigEndian.Uint16(blk[dqOffMagic:]) != dqMagic {
		t.Fatal("dquot magic wrong")
	}
}

func TestBuildDquotCRC(t *testing.T) {
	sb := &superblock{hasCRC: true, blockSize: 4096}
	copy(sb.uuid[:], []byte("0123456789abcdef"))
	buf := make([]byte, 4096)
	buildDquot(buf, sb, dqTypeUser, 7, 3, 4)
	if !verifyCRC(buf[:dqBlkSize], dqOffCRC, dqBlkSize) {
		t.Fatal("dquot CRC not self-consistent")
	}
	if binary.BigEndian.Uint64(buf[dqOffBCount:]) != 3 || binary.BigEndian.Uint64(buf[dqOffICount:]) != 4 {
		t.Fatal("dquot counts not stored")
	}
	// A v4 (no-CRC) dquot leaves the CRC/uuid area zero.
	sb.hasCRC = false
	buildDquot(buf, sb, dqTypeGroup, 0, 0, 0)
	if binary.BigEndian.Uint32(buf[dqOffCRC:]) != 0 {
		t.Fatal("v4 dquot should not carry a CRC")
	}
}

func TestQuotaHighIDUnsupported(t *testing.T) {
	fs := formatQuota(t, QuotaConfig{User: true})
	defer fs.Close()
	if err := fs.WriteFile("/f", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Chowning to an id beyond the single dquot block's capacity triggers the
	// documented multi-block-quota-file limitation through the quotacheck.
	err := fs.Chown("/f", uint32(dquotsPerBlock(fs.sb)+1), 0)
	if !errors.Is(err, errQuotaIDRange) {
		t.Fatalf("Chown high uid = %v, want errQuotaIDRange", err)
	}
}
