package filesystem_xfs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestXfsGrowTo verifies that GrowTo extends the backing file, updates the
// AG count and can be re-opened successfully.
func TestXfsGrowTo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xfs-grow.img")
	ifs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer ifs.Close()
	fs := ifs.(*xfsFS)

	initialAgCount := fs.sb.agCount
	newSize := xfsTestSize * 2
	if err := fs.GrowTo(newSize); err != nil {
		t.Fatalf("GrowTo: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() != newSize {
		t.Fatalf("size = %d, want %d", st.Size(), newSize)
	}

	ifs2, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open after grow: %v", err)
	}
	defer ifs2.Close()
	fs2 := ifs2.(*xfsFS)
	if fs2.sb.agCount != initialAgCount*2 {
		t.Fatalf("ag count = %d, want %d", fs2.sb.agCount, initialAgCount*2)
	}
}
