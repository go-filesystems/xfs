package filesystem_xfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const xfsTestSize = int64(fmtAgBlocks) * fmtBlockSize // 4 MiB (= fmtMinSize, 1 AG)

var errXfsBoom = errors.New("xfs format injected error")

// ── Validation errors ─────────────────────────────────────────────────────

func TestXfsFmt_NotMultipleOfBlockSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.img")
	if _, err := Format(path, xfsTestSize+1, FormatConfig{}); err == nil {
		t.Error("expected error: size not a multiple of block size")
	}
}

func TestXfsFmt_TooSmall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiny.img")
	if _, err := Format(path, fmtBlockSize, FormatConfig{}); err == nil {
		t.Error("expected error: size too small")
	}
}

// ── Happy-path basics ─────────────────────────────────────────────────────

func TestXfsFmt_CreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.img")
	fs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("image file not created: %v", err)
	}
}

func TestXfsFmt_FileSizePreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != xfsTestSize {
		t.Errorf("size = %d, want %d", info.Size(), xfsTestSize)
	}
}

func TestXfsFmt_TruncatesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.img")
	if err := os.WriteFile(path, make([]byte, 512*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	fs.Close()
}

func TestXfsFmt_StatRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	st, err := fs.Stat("/")
	if err != nil {
		t.Fatalf("Stat /: %v", err)
	}
	if st.Mode()&0xF000 != 0x4000 {
		t.Errorf("root mode 0x%04X is not a directory", st.Mode())
	}
}

func TestXfsFmt_ListDirRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	entries, err := fs.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir /: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty root dir, got %d entries", len(entries))
	}
}

func TestXfsFmt_WriteReadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	defer fs.Close()
	const data = "hello from xfs Format\n"
	if err := fs.WriteFile("/hello.txt", []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := fs.ReadFile("/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != data {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestXfsFmt_MultiAG(t *testing.T) {
	// 8 MiB = 2 AGs
	path := filepath.Join(t.TempDir(), "multi.img")
	fs, err := Format(path, 2*xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatalf("Format (2 AGs): %v", err)
	}
	fs.Close()
}

func TestXfsFmt_CustomLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, xfsTestSize, FormatConfig{Label: "testlabel"})
	if err != nil {
		t.Fatalf("Format with label: %v", err)
	}
	fs.Close()
	fs2, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open after Format: %v", err)
	}
	defer fs2.Close()
}

func TestXfsFmt_CustomUUID(t *testing.T) {
	var uuid [16]byte
	for i := range uuid {
		uuid[i] = byte(i + 1)
	}
	path := filepath.Join(t.TempDir(), "disk.img")
	fs, err := Format(path, xfsTestSize, FormatConfig{UUID: uuid})
	if err != nil {
		t.Fatalf("Format with UUID: %v", err)
	}
	fs.Close()
}

func TestXfsFmt_ReOpenAndWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	{
		fs, err := Format(path, xfsTestSize, FormatConfig{})
		if err != nil {
			t.Fatalf("Format: %v", err)
		}
		if err := fs.WriteFile("/data.bin", []byte("original"), 0o600); err != nil {
			fs.Close()
			t.Fatalf("WriteFile: %v", err)
		}
		fs.Close()
	}
	fs, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer fs.Close()
	got, err := fs.ReadFile("/data.bin")
	if err != nil {
		t.Fatalf("ReadFile after re-open: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("got %q, want %q", got, "original")
	}
}

// ── Error injection ───────────────────────────────────────────────────────

type xfsCountingFile struct {
	inner     fmtFile
	writeCall int
	failAt    int
}

func (f *xfsCountingFile) WriteAt(p []byte, off int64) (int, error) {
	f.writeCall++
	if f.writeCall == f.failAt {
		return 0, errXfsBoom
	}
	return f.inner.WriteAt(p, off)
}
func (f *xfsCountingFile) Truncate(n int64) error { return f.inner.Truncate(n) }
func (f *xfsCountingFile) Close() error           { return f.inner.Close() }

type xfsTruncFailFile struct{}

func (f *xfsTruncFailFile) WriteAt([]byte, int64) (int, error) { return 0, nil }
func (f *xfsTruncFailFile) Truncate(int64) error               { return errXfsBoom }
func (f *xfsTruncFailFile) Close() error                       { return nil }

type xfsCloseFailFile struct{ inner fmtFile }

func (f *xfsCloseFailFile) WriteAt(p []byte, off int64) (int, error) { return f.inner.WriteAt(p, off) }
func (f *xfsCloseFailFile) Truncate(n int64) error                   { return f.inner.Truncate(n) }
func (f *xfsCloseFailFile) Close() error                             { return errXfsBoom }

func injectXfsCounting(t *testing.T, failAt int) {
	t.Helper()
	old := fmtOpenFile
	fmtOpenFile = func(path string) (fmtFile, error) {
		inner, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		return &xfsCountingFile{inner: inner, failAt: failAt}, nil
	}
	t.Cleanup(func() { fmtOpenFile = old })
}

func xfsExpectBoom(t *testing.T) {
	t.Helper()
	if _, err := Format(filepath.Join(t.TempDir(), "x.img"), xfsTestSize, FormatConfig{}); !errors.Is(err, errXfsBoom) {
		t.Fatalf("expected errXfsBoom, got %v", err)
	}
}

func TestXfsFmt_OpenFileFails(t *testing.T) {
	old := fmtOpenFile
	fmtOpenFile = func(string) (fmtFile, error) { return nil, errXfsBoom }
	t.Cleanup(func() { fmtOpenFile = old })
	xfsExpectBoom(t)
}

func TestXfsFmt_TruncateFails(t *testing.T) {
	old := fmtOpenFile
	fmtOpenFile = func(string) (fmtFile, error) { return &xfsTruncFailFile{}, nil }
	t.Cleanup(func() { fmtOpenFile = old })
	xfsExpectBoom(t)
}

func TestXfsFmt_RandReadFails(t *testing.T) {
	old := fmtRandRead
	fmtRandRead = func([]byte) (int, error) { return 0, errXfsBoom }
	t.Cleanup(func() { fmtRandRead = old })
	if _, err := Format(filepath.Join(t.TempDir(), "x.img"), xfsTestSize, FormatConfig{}); !errors.Is(err, errXfsBoom) {
		t.Fatalf("expected errXfsBoom, got %v", err)
	}
}

// For 1 AG (xfsTestSize = 4 MiB): write order is
//
//	AG0: 1=AGF, 2=AGI, 3=bno, 4=cnt, 5=inobt
//	6=rootInode, 7=superblock
func TestXfsFmt_WriteAGFFails(t *testing.T)        { injectXfsCounting(t, 1); xfsExpectBoom(t) }
func TestXfsFmt_WriteAGIFails(t *testing.T)        { injectXfsCounting(t, 2); xfsExpectBoom(t) }
func TestXfsFmt_WriteBnoLeafFails(t *testing.T)    { injectXfsCounting(t, 3); xfsExpectBoom(t) }
func TestXfsFmt_WriteCntLeafFails(t *testing.T)    { injectXfsCounting(t, 4); xfsExpectBoom(t) }
func TestXfsFmt_WriteInobtLeafFails(t *testing.T)  { injectXfsCounting(t, 5); xfsExpectBoom(t) }
func TestXfsFmt_WriteRootInodeFails(t *testing.T)  { injectXfsCounting(t, 6); xfsExpectBoom(t) }
func TestXfsFmt_WriteSuperblockFails(t *testing.T) { injectXfsCounting(t, 7); xfsExpectBoom(t) }

func TestXfsFmt_CloseFails(t *testing.T) {
	old := fmtOpenFile
	fmtOpenFile = func(path string) (fmtFile, error) {
		inner, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		return &xfsCloseFailFile{inner: inner}, nil
	}
	t.Cleanup(func() { fmtOpenFile = old })
	xfsExpectBoom(t)
}

func TestXfsFmt_OpenFSFails(t *testing.T) {
	old := fmtOpenFS
	fmtOpenFS = func(string, int) (FS, error) { return nil, errXfsBoom }
	t.Cleanup(func() { fmtOpenFS = old })
	xfsExpectBoom(t)
}
