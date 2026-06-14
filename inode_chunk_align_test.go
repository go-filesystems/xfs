package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestInodeChunkAlignment creates enough files to force growInobt to allocate
// several new inode chunks, then walks every AG's inobt and asserts each
// record's start inode is a multiple of XFS_INODES_PER_CHUNK (64). An
// unaligned chunk makes xfs_repair map inodes onto unrelated (file-data)
// blocks — the corruption this regression guards against.
func TestInodeChunkAlignment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "align.xfs")
	const oneAG = 16384 * 4096
	fs, err := Format(path, 4*oneAG, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	// >64 files in the root chunk forces at least three more 64-inode chunks.
	for i := 0; i < 200; i++ {
		if err := fs.WriteFile(fmt.Sprintf("/f%04d", i), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("WriteFile %d: %v", i, err)
		}
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sb, err := readSuperblock(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	be := binary.BigEndian
	hdr := sb.agBTreeHdrSize()
	chunks := 0
	for ag := uint32(0); ag < sb.agCount; ag++ {
		agi, err := agiBlock(f, 0, sb, ag)
		if err != nil {
			t.Fatal(err)
		}
		root := be.Uint32(agi[agiOffRoot:])
		if be.Uint32(agi[agiOffLevel:]) != 1 {
			continue // multi-level inobt not produced at this scale
		}
		leaf, err := readAGBlock(f, 0, sb, ag, root)
		if err != nil {
			t.Fatal(err)
		}
		numrecs := int(be.Uint16(leaf[6:]))
		for i := 0; i < numrecs; i++ {
			startIno := be.Uint32(leaf[hdr+i*inobtRecSize:])
			if startIno%inobtChunkInodes != 0 {
				t.Errorf("ag %d inobt rec %d: startino %d not %d-aligned",
					ag, i, startIno, inobtChunkInodes)
			}
			chunks++
		}
	}
	if chunks < 4 {
		t.Fatalf("expected several inode chunks (growInobt exercised), got %d", chunks)
	}
}
