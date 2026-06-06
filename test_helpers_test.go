package filesystem_xfs

import (
	"errors"
	"io"
	"os"
	"testing"
)

var errBoom = errors.New("boom")

type memRW struct {
	data      []byte
	readHook  func(int64, []byte) error
	writeHook func(int64, []byte) error
}

func newMemRW(size int) *memRW {
	return &memRW{data: make([]byte, size)}
}

func (m *memRW) ReadAt(p []byte, off int64) (int, error) {
	if m.readHook != nil {
		if err := m.readHook(off, p); err != nil {
			return 0, err
		}
	}
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memRW) WriteAt(p []byte, off int64) (int, error) {
	if m.writeHook != nil {
		if err := m.writeHook(off, p); err != nil {
			return 0, err
		}
	}
	need := int(off) + len(p)
	if need > len(m.data) {
		grown := make([]byte, need)
		copy(grown, m.data)
		m.data = grown
	}
	copy(m.data[off:], p)
	return len(p), nil
}

func newTempFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "xfs-*.img")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func newTestFS(t *testing.T) *xfsFS {
	t.Helper()
	return &xfsFS{f: &osFileBackend{f: newTempFile(t)}, sb: defaultSB()}
}

func newTestInode(num uint64, mode uint16, format uint8, size uint64) *inode {
	raw := buildInodeBuf(num, mode, format, size)
	return &inode{num: num, mode: mode, format: format, size: size, raw: raw}
}
