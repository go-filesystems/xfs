package filesystem_xfs

import (
	"bytes"
	"testing"
)

func TestReadInodeReadError(t *testing.T) {
	sb := defaultSB()
	if _, err := readInode(bytes.NewReader(nil), 0, sb, 128); err == nil {
		t.Fatal("expected readInode to fail when the inode bytes are unavailable")
	}
}

func TestWriteInodeAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(int(inodeByteOff(sb, 0, 128) + int64(sb.inodeSize)))
	in := newTestInode(128, 0x81A4, inodeFmtLocal, 0)

	if err := writeInode(rw, 0, sb, in); err != nil {
		t.Fatalf("writeInode: %v", err)
	}
	start := inodeByteOff(sb, 0, 128)
	if rw.data[start+inoOffMagic] != in.raw[inoOffMagic] {
		t.Fatal("writeInode did not persist the inode bytes at the computed offset")
	}
	if in.raw[inoOffCRC] == 0 && in.raw[inoOffCRC+1] == 0 && in.raw[inoOffCRC+2] == 0 && in.raw[inoOffCRC+3] == 0 {
		t.Fatal("writeInode did not refresh the inode CRC for a v5 superblock")
	}

	rw.writeHook = func(int64, []byte) error { return errBoom }
	if err := writeInode(rw, 0, sb, in); err == nil {
		t.Fatal("expected writeInode to propagate write failures")
	}
}
