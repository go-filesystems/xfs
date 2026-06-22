package filesystem_xfs

import (
	"errors"
	"io"
	"os"
	"testing"
)

func TestOpenAdditional(t *testing.T) {
	t.Run("open error", func(t *testing.T) {
		_, err := Open(t.TempDir()+"/missing", -1)
		if err == nil {
			t.Fatal("expected Open to fail for a missing image")
		}
	})

	t.Run("partition error closes file", func(t *testing.T) {
		f := newTempFile(t)
		oldOpen := openFile
		oldPart := openPartitionOffset
		openFile = func(string, int, os.FileMode) (*os.File, error) { return f, nil }
		openPartitionOffset = func(io.ReaderAt, int64, int) (int64, error) { return 0, errBoom }
		t.Cleanup(func() {
			openFile = oldOpen
			openPartitionOffset = oldPart
		})

		_, err := Open("ignored", -1)
		if !errors.Is(err, errBoom) {
			t.Fatalf("expected partition error %v, got %v", errBoom, err)
		}
		if _, statErr := f.Stat(); statErr == nil {
			t.Fatal("expected Open to close the file on partition errors")
		}
	})

	t.Run("superblock error closes file", func(t *testing.T) {
		f := newTempFile(t)
		oldOpen := openFile
		oldPart := openPartitionOffset
		oldSB := openReadSuperblock
		openFile = func(string, int, os.FileMode) (*os.File, error) { return f, nil }
		openPartitionOffset = func(io.ReaderAt, int64, int) (int64, error) { return 64, nil }
		openReadSuperblock = func(io.ReaderAt, int64) (*superblock, error) { return nil, errBoom }
		t.Cleanup(func() {
			openFile = oldOpen
			openPartitionOffset = oldPart
			openReadSuperblock = oldSB
		})

		_, err := Open("ignored", -1)
		if !errors.Is(err, errBoom) {
			t.Fatalf("expected superblock error %v, got %v", errBoom, err)
		}
		if _, statErr := f.Stat(); statErr == nil {
			t.Fatal("expected Open to close the file on superblock errors")
		}
	})

	t.Run("success", func(t *testing.T) {
		f := newTempFile(t)
		sb := defaultSB()
		oldOpen := openFile
		oldPart := openPartitionOffset
		oldSB := openReadSuperblock
		openFile = func(string, int, os.FileMode) (*os.File, error) { return f, nil }
		openPartitionOffset = func(io.ReaderAt, int64, int) (int64, error) { return 128, nil }
		openReadSuperblock = func(io.ReaderAt, int64) (*superblock, error) { return sb, nil }
		t.Cleanup(func() {
			openFile = oldOpen
			openPartitionOffset = oldPart
			openReadSuperblock = oldSB
		})

		ifs, err := Open("ignored", -1)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		fs := ifs.(*xfsFS)
		ofb, ok := fs.f.(*osFileBackend)
		if !ok || fs.partOffset != 128 || fs.sb != sb || ofb.f != f {
			t.Fatal("Open did not propagate the opened file and detected metadata")
		}
	})
}

func TestCloseAdditional(t *testing.T) {
	fs := newTestFS(t)
	innerFile := fs.f.(*osFileBackend).f
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := innerFile.Stat(); err == nil {
		t.Fatal("expected Close to close the underlying file")
	}
}

func TestReadFileAdditional(t *testing.T) {
	fs := newTestFS(t)

	t.Run("lookup error", func(t *testing.T) {
		oldLookup := fsPathLookup
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return nil, errBoom }
		t.Cleanup(func() { fsPathLookup = oldLookup })

		if _, err := fs.ReadFile("/file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected lookup error %v, got %v", errBoom, err)
		}
	})

	t.Run("non regular inode", func(t *testing.T) {
		oldLookup := fsPathLookup
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(7, 0x4000, inodeFmtLocal, 0), nil
		}
		t.Cleanup(func() { fsPathLookup = oldLookup })

		if _, err := fs.ReadFile("/dir"); err == nil {
			t.Fatal("expected ReadFile to reject directories")
		}
	})

	t.Run("data error", func(t *testing.T) {
		oldLookup := fsPathLookup
		oldRead := fsReadFileData
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(8, 0x8000, inodeFmtLocal, 0), nil
		}
		fsReadFileData = func(io.ReaderAt, int64, *superblock, *inode) ([]byte, error) { return nil, errBoom }
		t.Cleanup(func() {
			fsPathLookup = oldLookup
			fsReadFileData = oldRead
		})

		if _, err := fs.ReadFile("/file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected read error %v, got %v", errBoom, err)
		}
	})

	t.Run("success", func(t *testing.T) {
		oldLookup := fsPathLookup
		oldRead := fsReadFileData
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(9, 0x8000, inodeFmtLocal, 0), nil
		}
		fsReadFileData = func(io.ReaderAt, int64, *superblock, *inode) ([]byte, error) { return []byte("abc"), nil }
		t.Cleanup(func() {
			fsPathLookup = oldLookup
			fsReadFileData = oldRead
		})

		data, err := fs.ReadFile("/file")
		if err != nil || string(data) != "abc" {
			t.Fatalf("ReadFile = %q, %v; want abc, nil", data, err)
		}
	})
}

func TestListDirAdditional(t *testing.T) {
	fs := newTestFS(t)

	t.Run("lookup error", func(t *testing.T) {
		oldLookup := fsPathLookup
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return nil, errBoom }
		t.Cleanup(func() { fsPathLookup = oldLookup })

		if _, err := fs.ListDir("/"); !errors.Is(err, errBoom) {
			t.Fatalf("expected lookup error %v, got %v", errBoom, err)
		}
	})

	t.Run("non directory inode", func(t *testing.T) {
		oldLookup := fsPathLookup
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(10, 0x8000, inodeFmtLocal, 0), nil
		}
		t.Cleanup(func() { fsPathLookup = oldLookup })

		if _, err := fs.ListDir("/file"); err == nil {
			t.Fatal("expected ListDir to reject regular files")
		}
	})

	t.Run("read error", func(t *testing.T) {
		oldLookup := fsPathLookup
		oldRead := fsReadDir
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(11, 0x4000, inodeFmtLocal, 0), nil
		}
		fsReadDir = func(io.ReaderAt, int64, *superblock, *inode) ([]DirEntry, error) { return nil, errBoom }
		t.Cleanup(func() {
			fsPathLookup = oldLookup
			fsReadDir = oldRead
		})

		if _, err := fs.ListDir("/dir"); !errors.Is(err, errBoom) {
			t.Fatalf("expected readDir error %v, got %v", errBoom, err)
		}
	})

	t.Run("success", func(t *testing.T) {
		oldLookup := fsPathLookup
		oldRead := fsReadDir
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(12, 0x4000, inodeFmtLocal, 0), nil
		}
		fsReadDir = func(io.ReaderAt, int64, *superblock, *inode) ([]DirEntry, error) {
			return []DirEntry{{Inode: 7, Name: "etc", FileType: 2}}, nil
		}
		t.Cleanup(func() {
			fsPathLookup = oldLookup
			fsReadDir = oldRead
		})

		entries, err := fs.ListDir("/dir")
		if err != nil {
			t.Fatalf("ListDir: %v", err)
		}
		if len(entries) != 1 || entries[0].Name() != "etc" || entries[0].Inode() != 7 || entries[0].FileType() != 2 {
			t.Fatalf("unexpected ListDir output: %+v", entries)
		}
	})
}

func TestStatAdditional(t *testing.T) {
	fs := newTestFS(t)

	t.Run("lookup error", func(t *testing.T) {
		oldLookup := fsPathLookup
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return nil, errBoom }
		t.Cleanup(func() { fsPathLookup = oldLookup })

		if _, err := fs.Stat("/file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected lookup error %v, got %v", errBoom, err)
		}
	})

	t.Run("success", func(t *testing.T) {
		oldLookup := fsPathLookup
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(13, 0x81A4, inodeFmtLocal, 77), nil
		}
		t.Cleanup(func() { fsPathLookup = oldLookup })

		st, err := fs.Stat("/file")
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if st.Inode() != 13 || st.Mode() != 0x81A4 || st.Size() != 77 {
			t.Fatalf("unexpected Stat output: inode=%d mode=%#x size=%d", st.Inode(), st.Mode(), st.Size())
		}
	})
}

func TestMutatingMethodDelegation(t *testing.T) {
	fs := newTestFS(t)

	t.Run("WriteFile", func(t *testing.T) {
		oldWrite := fsWriteFile
		called := false
		fsWriteFile = func(readerWriterAt, int64, *superblock, string, []byte, os.FileMode) error {
			called = true
			return errBoom
		}
		t.Cleanup(func() { fsWriteFile = oldWrite })

		if err := fs.WriteFile("/file", []byte("x"), 0o644); !errors.Is(err, errBoom) || !called {
			t.Fatal("WriteFile did not delegate to writeFile")
		}
	})

	t.Run("DeleteFile", func(t *testing.T) {
		oldDelete := fsDeleteFile
		called := false
		fsDeleteFile = func(readerWriterAt, int64, *superblock, string) error { called = true; return errBoom }
		t.Cleanup(func() { fsDeleteFile = oldDelete })

		if err := fs.DeleteFile("/file"); !errors.Is(err, errBoom) || !called {
			t.Fatal("DeleteFile did not delegate to deleteFile")
		}
	})

	t.Run("MkDir", func(t *testing.T) {
		oldMkDir := fsMakeDir
		called := false
		fsMakeDir = func(readerWriterAt, int64, *superblock, string, os.FileMode) error { called = true; return errBoom }
		t.Cleanup(func() { fsMakeDir = oldMkDir })

		if err := fs.MkDir("/dir", 0o755); !errors.Is(err, errBoom) || !called {
			t.Fatal("MkDir did not delegate to makeDir")
		}
	})

	t.Run("DeleteDir", func(t *testing.T) {
		oldDelete := fsDeleteDir
		called := false
		fsDeleteDir = func(readerWriterAt, int64, *superblock, string) error { called = true; return errBoom }
		t.Cleanup(func() { fsDeleteDir = oldDelete })

		if err := fs.DeleteDir("/dir"); !errors.Is(err, errBoom) || !called {
			t.Fatal("DeleteDir did not delegate to deleteDir")
		}
	})

	t.Run("Rename", func(t *testing.T) {
		oldRename := fsRenameEntry
		called := false
		fsRenameEntry = func(readerWriterAt, int64, *superblock, string, string) error { called = true; return errBoom }
		t.Cleanup(func() { fsRenameEntry = oldRename })

		if err := fs.Rename("/old", "/new"); !errors.Is(err, errBoom) || !called {
			t.Fatal("Rename did not delegate to renameEntry")
		}
	})
}

func TestReadLinkAdditional(t *testing.T) {
	fs := newTestFS(t)

	t.Run("root is not a symlink", func(t *testing.T) {
		if _, err := fs.ReadLink("/"); err == nil {
			t.Fatal("expected ReadLink to reject the root path")
		}
	})

	t.Run("parent lookup error", func(t *testing.T) {
		oldLookup := fsPathLookup
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) { return nil, errBoom }
		t.Cleanup(func() { fsPathLookup = oldLookup })

		if _, err := fs.ReadLink("/ln"); !errors.Is(err, errBoom) {
			t.Fatalf("expected parent lookup error %v, got %v", errBoom, err)
		}
	})

	t.Run("lookupInDir error", func(t *testing.T) {
		oldLookup := fsPathLookup
		oldDir := fsLookupInDir
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(14, 0x4000, inodeFmtLocal, 0), nil
		}
		fsLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 0, errBoom }
		t.Cleanup(func() {
			fsPathLookup = oldLookup
			fsLookupInDir = oldDir
		})

		if _, err := fs.ReadLink("/ln"); !errors.Is(err, errBoom) {
			t.Fatalf("expected lookupInDir error %v, got %v", errBoom, err)
		}
	})

	t.Run("inode read error", func(t *testing.T) {
		oldLookup := fsPathLookup
		oldDir := fsLookupInDir
		oldRead := fsReadInode
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(15, 0x4000, inodeFmtLocal, 0), nil
		}
		fsLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 99, nil }
		fsReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) { return nil, errBoom }
		t.Cleanup(func() {
			fsPathLookup = oldLookup
			fsLookupInDir = oldDir
			fsReadInode = oldRead
		})

		if _, err := fs.ReadLink("/ln"); !errors.Is(err, errBoom) {
			t.Fatalf("expected readInode error %v, got %v", errBoom, err)
		}
	})

	t.Run("non symlink inode", func(t *testing.T) {
		oldLookup := fsPathLookup
		oldDir := fsLookupInDir
		oldRead := fsReadInode
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(16, 0x4000, inodeFmtLocal, 0), nil
		}
		fsLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 100, nil }
		fsReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			return newTestInode(100, 0x8000, inodeFmtLocal, 0), nil
		}
		t.Cleanup(func() {
			fsPathLookup = oldLookup
			fsLookupInDir = oldDir
			fsReadInode = oldRead
		})

		if _, err := fs.ReadLink("/ln"); err == nil {
			t.Fatal("expected ReadLink to reject regular files")
		}
	})

	t.Run("target read error", func(t *testing.T) {
		oldLookup := fsPathLookup
		oldDir := fsLookupInDir
		oldRead := fsReadInode
		oldData := fsReadSymlinkTarget
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(17, 0x4000, inodeFmtLocal, 0), nil
		}
		fsLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 101, nil }
		fsReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			return newTestInode(101, 0xA000, inodeFmtLocal, 0), nil
		}
		fsReadSymlinkTarget = func(io.ReaderAt, int64, *superblock, *inode) ([]byte, error) { return nil, errBoom }
		t.Cleanup(func() {
			fsPathLookup = oldLookup
			fsLookupInDir = oldDir
			fsReadInode = oldRead
			fsReadSymlinkTarget = oldData
		})

		if _, err := fs.ReadLink("/ln"); !errors.Is(err, errBoom) {
			t.Fatalf("expected read target error %v, got %v", errBoom, err)
		}
	})

	t.Run("success", func(t *testing.T) {
		oldLookup := fsPathLookup
		oldDir := fsLookupInDir
		oldRead := fsReadInode
		oldData := fsReadSymlinkTarget
		fsPathLookup = func(io.ReaderAt, int64, *superblock, string) (*inode, error) {
			return newTestInode(18, 0x4000, inodeFmtLocal, 0), nil
		}
		fsLookupInDir = func(io.ReaderAt, int64, *superblock, *inode, string) (uint64, error) { return 102, nil }
		fsReadInode = func(io.ReaderAt, int64, *superblock, uint64) (*inode, error) {
			return newTestInode(102, 0xA000, inodeFmtLocal, 0), nil
		}
		fsReadSymlinkTarget = func(io.ReaderAt, int64, *superblock, *inode) ([]byte, error) { return []byte("/target"), nil }
		t.Cleanup(func() {
			fsPathLookup = oldLookup
			fsLookupInDir = oldDir
			fsReadInode = oldRead
			fsReadSymlinkTarget = oldData
		})

		target, err := fs.ReadLink("/ln")
		if err != nil || target != "/target" {
			t.Fatalf("ReadLink = %q, %v; want /target, nil", target, err)
		}
	})
}

func TestRawBlockHelpers(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(int(sb.blockSize * 2))
	copy(rw.data[:sb.blockSize], []byte("hello"))

	blk, err := readRawBlock(rw, 0, sb, 0)
	if err != nil || string(blk[:5]) != "hello" {
		t.Fatalf("readRawBlock = %q, %v; want hello, nil", blk[:5], err)
	}
	if err := writeRawBlock(rw, 0, sb, 1, []byte("world")); err != nil {
		t.Fatalf("writeRawBlock: %v", err)
	}
	if string(rw.data[sb.blockSize:sb.blockSize+5]) != "world" {
		t.Fatal("writeRawBlock did not write at the expected block offset")
	}

	rw.readHook = func(int64, []byte) error { return errBoom }
	if _, err := readRawBlock(rw, 0, sb, 0); !errors.Is(err, errBoom) {
		t.Fatalf("expected readRawBlock error %v, got %v", errBoom, err)
	}
	rw.readHook = nil
	rw.writeHook = func(int64, []byte) error { return errBoom }
	if err := writeRawBlock(rw, 0, sb, 0, make([]byte, sb.blockSize)); !errors.Is(err, errBoom) {
		t.Fatalf("expected writeRawBlock error %v, got %v", errBoom, err)
	}
}
