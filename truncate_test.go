package filesystem_xfs

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

func openFreshWithFile(t *testing.T, name string, data []byte) (FS, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if err := fs.WriteFile(name, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return fs, p
}

func TestXfsTruncate_Grow(t *testing.T) {
	fs, _ := openFreshWithFile(t, "/f", []byte("abc"))
	defer fs.Close()
	if err := fs.Truncate("/f", 16); err != nil {
		t.Fatalf("Truncate grow: %v", err)
	}
	st, _ := fs.ExtendedStat("/f")
	if st.Size != 16 {
		t.Errorf("Size after grow = %d, want 16", st.Size)
	}
	got, err := fs.ReadFile("/f")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 16 {
		t.Fatalf("ReadFile len = %d, want 16", len(got))
	}
	if !bytes.Equal(got[:3], []byte("abc")) {
		t.Errorf("prefix = %q, want %q", got[:3], "abc")
	}
	for i := 3; i < 16; i++ {
		if got[i] != 0 {
			t.Errorf("byte %d = 0x%02x, want zero (sparse extension)", i, got[i])
			break
		}
	}
}

func TestXfsTruncate_ShrinkToZero(t *testing.T) {
	// Make the body big enough to span more than one block, so the shrink
	// path actually exercises freeBlocks rather than just trimming inline
	// content.
	data := bytes.Repeat([]byte("Z"), 8*1024)
	fs, _ := openFreshWithFile(t, "/big", data)
	defer fs.Close()
	if err := fs.Truncate("/big", 0); err != nil {
		t.Fatalf("Truncate to 0: %v", err)
	}
	st, _ := fs.ExtendedStat("/big")
	if st.Size != 0 {
		t.Errorf("Size = %d, want 0", st.Size)
	}
	if st.NBlocks != 0 {
		t.Errorf("NBlocks = %d, want 0 after shrink-to-zero", st.NBlocks)
	}
	got, err := fs.ReadFile("/big")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadFile len = %d, want 0", len(got))
	}
}

func TestXfsTruncate_ShrinkPartial(t *testing.T) {
	data := make([]byte, 6*1024)
	for i := range data {
		data[i] = byte(i)
	}
	fs, _ := openFreshWithFile(t, "/p", data)
	defer fs.Close()
	const newSize = 1500
	if err := fs.Truncate("/p", newSize); err != nil {
		t.Fatalf("Truncate partial: %v", err)
	}
	st, _ := fs.ExtendedStat("/p")
	if st.Size != newSize {
		t.Errorf("Size = %d, want %d", st.Size, newSize)
	}
	got, err := fs.ReadFile("/p")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != newSize {
		t.Fatalf("ReadFile len = %d, want %d", len(got), newSize)
	}
	if !bytes.Equal(got, data[:newSize]) {
		t.Errorf("content differs from original prefix")
	}
}

func TestXfsTruncate_BumpsMTimeAndCTime(t *testing.T) {
	fs, _ := openFreshWithFile(t, "/t", []byte("hello"))
	defer fs.Close()
	before, _ := fs.ExtendedStat("/t")
	time.Sleep(2 * time.Millisecond)
	if err := fs.Truncate("/t", 5); err != nil { // no-op size
		t.Fatalf("Truncate noop: %v", err)
	}
	after, _ := fs.ExtendedStat("/t")
	if !after.MTime.After(before.MTime) {
		t.Errorf("mtime not bumped on no-op truncate: %v -> %v", before.MTime, after.MTime)
	}
	if !after.CTime.After(before.CTime) {
		t.Errorf("ctime not bumped on no-op truncate: %v -> %v", before.CTime, after.CTime)
	}
}

func TestXfsTruncate_RejectsNegative(t *testing.T) {
	fs, _ := openFreshWithFile(t, "/n", []byte("x"))
	defer fs.Close()
	if err := fs.Truncate("/n", -1); err == nil {
		t.Error("Truncate with negative size unexpectedly succeeded")
	}
}

func TestXfsTruncate_RejectsNonRegular(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(p, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	if err := fs.MkDir("/d", 0o755); err != nil {
		t.Fatalf("MkDir: %v", err)
	}
	if err := fs.Truncate("/d", 0); err == nil {
		t.Error("Truncate on directory unexpectedly succeeded")
	}
}

func TestXfsTruncate_MissingPath(t *testing.T) {
	fs, _ := openFreshWithFile(t, "/exists", []byte("x"))
	defer fs.Close()
	if err := fs.Truncate("/no/such/file", 0); err == nil {
		t.Error("Truncate on missing path unexpectedly succeeded")
	}
}

func TestXfsTruncate_PersistsAcrossReopen(t *testing.T) {
	data := bytes.Repeat([]byte("A"), 5000)
	fs, img := openFreshWithFile(t, "/r", data)
	if err := fs.Truncate("/r", 1234); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	fs.Close()

	fs2, err := Open(img, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs2.Close()
	st, _ := fs2.ExtendedStat("/r")
	if st.Size != 1234 {
		t.Errorf("after reopen Size = %d, want 1234", st.Size)
	}
	got, err := fs2.ReadFile("/r")
	if err != nil {
		t.Fatalf("ReadFile after reopen: %v", err)
	}
	if len(got) != 1234 || !bytes.Equal(got, data[:1234]) {
		t.Errorf("after reopen content/length wrong: len=%d", len(got))
	}
}
