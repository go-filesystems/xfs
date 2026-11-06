package filesystem_xfs

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

func offsetPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>12)
	}
	return b
}

// bigXFS is an image holding /big (fileSize bytes, one extent: the writer
// allocates a file in one extent, so it must fit in an allocation group of
// 64 MiB) and /small (inline data).
func bigXFS(t testing.TB, fileSize int) (FS, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.img")
	fs, err := Format(path, 4*testOneAG, FormatConfig{Label: "big"})
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	data := offsetPattern(fileSize)
	if err := fs.WriteFile("/big", data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.WriteFile("/small", []byte("hello world"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Cleanup(func() { fs.Close() })
	return fs, data
}

func checkRange(t *testing.T, f filesystem.File, want []byte, size, off int64) {
	t.Helper()
	buf := make([]byte, 4096)
	n, err := f.ReadAt(buf, off)
	wantN := min(int64(len(buf)), size-off)
	if int64(n) != wantN || (n < len(buf)) != errors.Is(err, io.EOF) || (n == len(buf) && err != nil) {
		t.Fatalf("ReadAt(%d) = %d, %v; want %d", off, n, err, wantN)
	}
	if !bytes.Equal(buf[:n], want[off:off+int64(n)]) {
		t.Fatalf("ReadAt(%d): wrong bytes", off)
	}
}

// Ranges of a real file, compared with what was written.
func TestOpenFileReadsRanges(t *testing.T) {
	const fileSize = 40 << 20
	fs, want := bigXFS(t, fileSize)
	o, ok := fs.(filesystem.Opener)
	if !ok {
		t.Fatal("the driver is not a filesystem.Opener")
	}
	f, err := o.OpenFile("/big")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Size() != fileSize {
		t.Fatalf("Size = %d", f.Size())
	}
	for _, off := range []int64{0, 4095, 1 << 20, 33<<20 + 123, fileSize - 10} {
		checkRange(t, f, want, fileSize, off)
	}
	if n, err := f.ReadAt(make([]byte, 8), fileSize); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end = %d, %v", n, err)
	}
	if _, err := f.ReadAt(make([]byte, 8), -1); err == nil {
		t.Error("a negative offset was read")
	}
}

// The writer puts a file in one extent of at most one allocation group
// (64 MiB), so a file past 64 MiB is made here from the real one: the same
// blocks, mapped at a file offset past 64 MiB, in a file declared 72 MiB long.
// A READ at 66 MiB then reads the extent's bytes without loading anything else.
func TestOpenFileReadsAnOffsetPast64MiB(t *testing.T) {
	const fileSize = 4 << 20
	fs, want := bigXFS(t, fileSize)
	real, err := fs.(filesystem.Opener).OpenFile("/big")
	if err != nil {
		t.Fatal(err)
	}
	r := real.(*xfsFile)
	if len(r.exts) == 0 {
		t.Fatal("no extents")
	}
	const base = 66 << 20
	shifted := &xfsFile{fs: r.fs, size: 72 << 20}
	for _, e := range r.exts {
		e.startOff += base / 4096
		shifted.exts = append(shifted.exts, e)
	}
	// Past the extent (a hole after it) and before it (a hole in front of it).
	for _, tc := range []struct{ off, from int64 }{{base + 123, 123}, {base + 1<<20, 1 << 20}, {base + fileSize - 100, fileSize - 100}} {
		buf := make([]byte, 4096)
		n, err := shifted.ReadAt(buf, tc.off)
		wantN := min(int64(4096), int64(shifted.size)-tc.off)
		if int64(n) != wantN || (err != nil && !errors.Is(err, io.EOF)) {
			t.Fatalf("ReadAt(%d) = %d, %v", tc.off, n, err)
		}
		inExtent := min(int64(n), fileSize-tc.from)
		if !bytes.Equal(buf[:inExtent], want[tc.from:tc.from+inExtent]) || !bytes.Equal(buf[inExtent:n], make([]byte, int64(n)-inExtent)) {
			t.Fatalf("ReadAt(%d): wrong bytes", tc.off)
		}
	}
	front := make([]byte, 64)
	if n, err := shifted.ReadAt(front, 1<<20); n != 64 || err != nil || !bytes.Equal(front, make([]byte, 64)) {
		t.Fatalf("the hole in front of the extent = %d %v", n, err)
	}
}

func TestOpenFileInlineAndRefusals(t *testing.T) {
	fs, _ := bigXFS(t, 1<<20)
	o := fs.(filesystem.Opener)
	f, err := o.OpenFile("/small")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if n, err := f.ReadAt(buf, 6); n != 5 || err != nil || string(buf) != "world" {
		t.Errorf("inline ReadAt = %d %v %q", n, err, buf)
	}
	if n, err := f.ReadAt(make([]byte, 20), 0); n != 11 || !errors.Is(err, io.EOF) {
		t.Errorf("inline ReadAt past the end = %d %v", n, err)
	}
	if err := f.Close(); err != nil {
		t.Error(err)
	}
	if _, err := o.OpenFile("/nope"); err == nil {
		t.Error("a missing file was opened")
	}
	if _, err := o.OpenFile("/"); err == nil {
		t.Error("a directory was opened")
	}
}

// An extent a forged inode places far past the window is skipped (and its file
// offset cannot wrap), and a hole reads as zeros.
func TestOpenFileForgedExtents(t *testing.T) {
	f := &xfsFile{fs: &xfsFS{sb: &superblock{blockSize: 4096}}, size: 1 << 20,
		exts: []extent{{startOff: 1 << 60, count: 1}}}
	buf := bytes.Repeat([]byte{0xff}, 64)
	if n, err := f.ReadAt(buf, 4096); n != 64 || err != nil || !bytes.Equal(buf, make([]byte, 64)) {
		t.Fatalf("a hole = %d %v %v", n, err, buf[:4])
	}
}

// BenchmarkReadRangeOfBigFile: a 1 MiB read at 50 MiB of a 56 MiB file, through
// OpenFile/ReadAt and through ReadFile. Allocations are exact; times are not
// under load.
func BenchmarkReadRangeOfBigFile(b *testing.B) {
	fs, _ := bigXFS(b, 56<<20)
	b.Run("OpenFile-ReadAt", func(b *testing.B) {
		f, err := fs.(filesystem.Opener).OpenFile("/big")
		if err != nil {
			b.Fatal(err)
		}
		buf := make([]byte, 1<<20)
		b.SetBytes(int64(len(buf)))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := f.ReadAt(buf, 50<<20); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ReadFile-whole", func(b *testing.B) {
		b.SetBytes(1 << 20)
		b.ReportAllocs()
		for b.Loop() {
			data, err := fs.ReadFile("/big")
			if err != nil {
				b.Fatal(err)
			}
			_ = data[50<<20 : 51<<20]
		}
	})
}
