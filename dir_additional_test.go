package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func restoreDirHooks(t *testing.T) {
	oldReadInode := dirReadInode
	oldLookupInDir := dirLookupInDirHook
	oldReadFileData := dirReadSymlinkTarget
	oldBlockDirLookup := dirBlockDirLookup
	oldInlineExtents := dirInlineExtents
	oldBtreeExtents := dirBtreeExtents
	oldReadRawBlock := dirReadRawBlock
	oldBlockReadDir := dirBlockReadDir
	oldPathLookup := dirPathLookup
	t.Cleanup(func() {
		dirReadInode = oldReadInode
		dirLookupInDirHook = oldLookupInDir
		dirReadSymlinkTarget = oldReadFileData
		dirBlockDirLookup = oldBlockDirLookup
		dirInlineExtents = oldInlineExtents
		dirBtreeExtents = oldBtreeExtents
		dirReadRawBlock = oldReadRawBlock
		dirBlockReadDir = oldBlockReadDir
		dirPathLookup = oldPathLookup
	})
}

func TestPathLookupAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)
	root := newTestInode(sb.rootIno, 0x4000, inodeFmtLocal, 0)
	childDir := newTestInode(10, 0x4000, inodeFmtLocal, 0)
	childFile := newTestInode(11, 0x8000, inodeFmtLocal, 0)
	linkIn := newTestInode(12, 0xa000, inodeFmtLocal, 0)

	t.Run("root and normal lookup", func(t *testing.T) {
		restoreDirHooks(t)
		dirReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			switch ino {
			case sb.rootIno:
				return root, nil
			case 10:
				return childDir, nil
			case 11:
				return childFile, nil
			default:
				return nil, ErrNotFound
			}
		}
		dirLookupInDirHook = func(_ io.ReaderAt, _ int64, _ *superblock, in *inode, name string) (uint64, error) {
			if in.num == sb.rootIno && name == "dir" {
				return 10, nil
			}
			if in.num == 10 && name == "file" {
				return 11, nil
			}
			return 0, ErrNotFound
		}

		got, err := pathLookup(rw, 0, sb, ".")
		if err != nil || got != root {
			t.Fatalf("pathLookup root: got (%v, %v), want (%v, nil)", got, err, root)
		}
		got, err = pathLookup(rw, 0, sb, "/dir/file")
		if err != nil || got != childFile {
			t.Fatalf("pathLookup normal: got (%v, %v), want (%v, nil)", got, err, childFile)
		}
	})

	t.Run("read and lookup errors", func(t *testing.T) {
		restoreDirHooks(t)
		dirReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint64) (*inode, error) {
			return nil, errBoom
		}
		if _, err := pathLookup(rw, 0, sb, "/"); !errors.Is(err, errBoom) {
			t.Fatalf("expected root read error %v, got %v", errBoom, err)
		}

		dirReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			if ino == sb.rootIno {
				return root, nil
			}
			return nil, errBoom
		}
		dirLookupInDirHook = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, _ string) (uint64, error) {
			return 0, errBoom
		}
		if _, err := pathLookup(rw, 0, sb, "/missing"); !errors.Is(err, errBoom) {
			t.Fatalf("expected lookup error %v, got %v", errBoom, err)
		}

		dirLookupInDirHook = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, _ string) (uint64, error) {
			return 10, nil
		}
		if _, err := pathLookup(rw, 0, sb, "/child"); !errors.Is(err, errBoom) {
			t.Fatalf("expected child read error %v, got %v", errBoom, err)
		}
	})

	t.Run("symlink follow", func(t *testing.T) {
		restoreDirHooks(t)
		dirReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
			if ino == sb.rootIno {
				return root, nil
			}
			return linkIn, nil
		}
		dirLookupInDirHook = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, _ string) (uint64, error) {
			return 12, nil
		}
		dirReadSymlinkTarget = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]byte, error) {
			return nil, errBoom
		}
		if _, err := pathLookup(rw, 0, sb, "/link"); !errors.Is(err, errBoom) {
			t.Fatalf("expected symlink read error %v, got %v", errBoom, err)
		}

		dirReadSymlinkTarget = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]byte, error) {
			return []byte("/target"), nil
		}
		dirPathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, _ string, _ int) (*inode, error) {
			return nil, errBoom
		}
		if _, err := pathLookup(rw, 0, sb, "/link"); !errors.Is(err, errBoom) {
			t.Fatalf("expected recursive path lookup error %v, got %v", errBoom, err)
		}

		dirPathLookup = func(_ io.ReaderAt, _ int64, _ *superblock, p string, _ int) (*inode, error) {
			if p != "/target" {
				t.Fatalf("unexpected symlink target path %q", p)
			}
			return childFile, nil
		}
		got, err := pathLookup(rw, 0, sb, "/link")
		if err != nil || got != childFile {
			t.Fatalf("pathLookup symlink: got (%v, %v), want (%v, nil)", got, err, childFile)
		}
	})
}

func TestLookupInDirAdditional(t *testing.T) {
	sb := defaultSB()
	local := newTestInode(20, 0x4000, inodeFmtLocal, 0)
	fork := buildSFDir(1, []struct {
		name string
		ino  uint32
	}{{"file", 22}}, sb.hasFType)
	copy(local.dataFork(), fork)

	ino, err := lookupInDir(newMemRW(0), 0, sb, local, "file")
	if err != nil || ino != 22 {
		t.Fatalf("lookupInDir local: got (%d, %v), want (22, nil)", ino, err)
	}

	restoreDirHooks(t)
	extentDir := newTestInode(21, 0x4000, inodeFmtExtents, 0)
	dirBlockDirLookup = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode, name string) (uint64, error) {
		if name != "entry" {
			return 0, ErrNotFound
		}
		return 33, nil
	}
	ino, err = lookupInDir(newMemRW(0), 0, sb, extentDir, "entry")
	if err != nil || ino != 33 {
		t.Fatalf("lookupInDir extents: got (%d, %v), want (33, nil)", ino, err)
	}

	bad := newTestInode(22, 0x4000, 99, 0)
	if _, err := lookupInDir(newMemRW(0), 0, sb, bad, "x"); err == nil {
		t.Fatal("expected unsupported format error")
	}
}

func TestBlockDirLookupAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)
	dirIn := newTestInode(23, 0x4000, inodeFmtExtents, 0)
	leafLogBlock := dirLeafByteOffset / uint64(sb.blockSize)

	t.Run("dir extents and block read errors", func(t *testing.T) {
		restoreDirHooks(t)
		dirInlineExtents = func(*inode) ([]extent, error) { return nil, errBoom }
		if _, err := blockDirLookup(rw, 0, sb, dirIn, "file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected dirExtents error %v, got %v", errBoom, err)
		}

		dirInlineExtents = func(*inode) ([]extent, error) {
			return []extent{{startOff: 0, startBlock: 3, count: 1}}, nil
		}
		dirReadRawBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint64) ([]byte, error) {
			return nil, errBoom
		}
		if _, err := blockDirLookup(rw, 0, sb, dirIn, "file"); !errors.Is(err, errBoom) {
			t.Fatalf("expected readRawBlock error %v, got %v", errBoom, err)
		}
	})

	t.Run("success, not found and leaf skip", func(t *testing.T) {
		restoreDirHooks(t)
		blk := buildDirBlock([]struct {
			ino  uint64
			name string
			ft   uint8
		}{{ino: 44, name: "file", ft: 1}}, sb.hasFType)
		dirInlineExtents = func(*inode) ([]extent, error) {
			return []extent{{startOff: leafLogBlock, startBlock: 1, count: 1}, {startOff: 0, startBlock: 2, count: 1}}, nil
		}
		dirReadRawBlock = func(_ io.ReaderAt, _ int64, _ *superblock, block uint64) ([]byte, error) {
			if block != 2 {
				return make([]byte, sb.blockSize), nil
			}
			return append([]byte(nil), blk...), nil
		}
		ino, err := blockDirLookup(rw, 0, sb, dirIn, "file")
		if err != nil || ino != 44 {
			t.Fatalf("blockDirLookup success: got (%d, %v), want (44, nil)", ino, err)
		}
		if _, err := blockDirLookup(rw, 0, sb, dirIn, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
}

func TestDirExtentsAdditional(t *testing.T) {
	sb := defaultSB()
	restoreDirHooks(t)
	extentDir := newTestInode(24, 0x4000, inodeFmtExtents, 0)
	btreeDir := newTestInode(25, 0x4000, inodeFmtBtree, 0)
	want := []extent{{startOff: 0, startBlock: 1, count: 2}}
	dirInlineExtents = func(*inode) ([]extent, error) { return want, nil }
	dirBtreeExtents = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]extent, error) { return want, nil }

	got, err := dirExtents(newMemRW(0), 0, sb, extentDir)
	if err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("dirExtents extents: got (%v, %v), want (%v, nil)", got, err, want)
	}
	got, err = dirExtents(newMemRW(0), 0, sb, btreeDir)
	if err != nil || len(got) != 1 || got[0] != want[0] {
		t.Fatalf("dirExtents btree: got (%v, %v), want (%v, nil)", got, err, want)
	}

	bad := newTestInode(26, 0x4000, 99, 0)
	if _, err := dirExtents(newMemRW(0), 0, sb, bad); err == nil {
		t.Fatal("expected unsupported dirExtents format error")
	}
}

func TestReadDirAdditional(t *testing.T) {
	sb := defaultSB()
	local := newTestInode(27, 0x4000, inodeFmtLocal, 0)
	fork := buildSFDir(1, []struct {
		name string
		ino  uint32
	}{{"file", 30}}, sb.hasFType)
	copy(local.dataFork(), fork)
	entries, err := readDir(newMemRW(0), 0, sb, local)
	if err != nil || len(entries) != 1 || entries[0].Name != "file" {
		t.Fatalf("readDir local: got (%v, %v)", entries, err)
	}

	restoreDirHooks(t)
	extentDir := newTestInode(28, 0x4000, inodeFmtExtents, 0)
	dirBlockReadDir = func(_ io.ReaderAt, _ int64, _ *superblock, _ *inode) ([]DirEntry, error) {
		return []DirEntry{{Inode: 31, Name: "block", FileType: 1}}, nil
	}
	entries, err = readDir(newMemRW(0), 0, sb, extentDir)
	if err != nil || len(entries) != 1 || entries[0].Name != "block" {
		t.Fatalf("readDir extents: got (%v, %v)", entries, err)
	}

	bad := newTestInode(29, 0x4000, 99, 0)
	if _, err := readDir(newMemRW(0), 0, sb, bad); err == nil {
		t.Fatal("expected unsupported readDir format error")
	}
}

func TestBlockReadDirAdditional(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)
	dirIn := newTestInode(30, 0x4000, inodeFmtExtents, 0)
	leafLogBlock := dirLeafByteOffset / uint64(sb.blockSize)

	t.Run("dir extents and block read errors", func(t *testing.T) {
		restoreDirHooks(t)
		dirInlineExtents = func(*inode) ([]extent, error) { return nil, errBoom }
		if _, err := blockReadDir(rw, 0, sb, dirIn); !errors.Is(err, errBoom) {
			t.Fatalf("expected dirExtents error %v, got %v", errBoom, err)
		}

		dirInlineExtents = func(*inode) ([]extent, error) {
			return []extent{{startOff: 0, startBlock: 3, count: 1}}, nil
		}
		dirReadRawBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint64) ([]byte, error) {
			return nil, errBoom
		}
		if _, err := blockReadDir(rw, 0, sb, dirIn); !errors.Is(err, errBoom) {
			t.Fatalf("expected readRawBlock error %v, got %v", errBoom, err)
		}
	})

	t.Run("success across data blocks", func(t *testing.T) {
		restoreDirHooks(t)
		blk1 := buildDirBlock([]struct {
			ino  uint64
			name string
			ft   uint8
		}{{ino: 1, name: ".", ft: 2}, {ino: 2, name: "one", ft: 1}}, sb.hasFType)
		blk2 := buildDirBlock([]struct {
			ino  uint64
			name string
			ft   uint8
		}{{ino: 1, name: "..", ft: 2}, {ino: 3, name: "two", ft: 1}}, sb.hasFType)
		dirInlineExtents = func(*inode) ([]extent, error) {
			return []extent{{startOff: leafLogBlock, startBlock: 1, count: 1}, {startOff: 0, startBlock: 4, count: 2}}, nil
		}
		dirReadRawBlock = func(_ io.ReaderAt, _ int64, _ *superblock, block uint64) ([]byte, error) {
			switch block {
			case 4:
				return append([]byte(nil), blk1...), nil
			case 5:
				return append([]byte(nil), blk2...), nil
			default:
				return make([]byte, sb.blockSize), nil
			}
		}
		entries, err := blockReadDir(rw, 0, sb, dirIn)
		if err != nil {
			t.Fatalf("blockReadDir: %v", err)
		}
		if len(entries) != 2 || entries[0].Name != "one" || entries[1].Name != "two" {
			t.Fatalf("unexpected blockReadDir entries: %v", entries)
		}
	})
}

func TestPathLookupSkipsEmptyComponents(t *testing.T) {
	sb := defaultSB()
	rw := newMemRW(0)
	root := newTestInode(sb.rootIno, 0x4000, inodeFmtLocal, 0)
	dir := newTestInode(90, 0x4000, inodeFmtLocal, 0)
	file := newTestInode(91, 0x8000, inodeFmtLocal, 0)
	restoreDirHooks(t)
	dirReadInode = func(_ io.ReaderAt, _ int64, _ *superblock, ino uint64) (*inode, error) {
		switch ino {
		case sb.rootIno:
			return root, nil
		case 90:
			return dir, nil
		case 91:
			return file, nil
		default:
			return nil, ErrNotFound
		}
	}
	dirLookupInDirHook = func(_ io.ReaderAt, _ int64, _ *superblock, in *inode, name string) (uint64, error) {
		if in.num == sb.rootIno && name == "dir" {
			return 90, nil
		}
		if in.num == 90 && name == "file" {
			return 91, nil
		}
		return 0, ErrNotFound
	}
	got, err := pathLookup(rw, 0, sb, "/dir//./file")
	if err != nil || got != file {
		t.Fatalf("pathLookup skipped components = (%v, %v), want (%v, nil)", got, err, file)
	}
}

func TestDirHelperRemainingBranches(t *testing.T) {
	sb := defaultSB()

	t.Run("sfLookup malformed and i8 paths", func(t *testing.T) {
		fork := make([]byte, 25)
		fork[0] = 1
		fork[1] = 1
		binary.BigEndian.PutUint64(fork[2:], 1)
		fork[10] = 3
		copy(fork[13:], []byte("big"))
		fork[16] = 1
		binary.BigEndian.PutUint64(fork[17:], 0x100000001)
		ino, err := sfLookup(fork, "big", true)
		if err != nil || ino != 0x100000001 {
			t.Fatalf("sfLookup i8 = (%d, %v), want (%d, nil)", ino, err, uint64(0x100000001))
		}

		if _, err := sfLookup([]byte{1, 0, 0, 0, 0, 0}, "x", true); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound for short entry, got %v", err)
		}
		badName := make([]byte, 9)
		badName[0] = 1
		badName[6] = 5
		if _, err := sfLookup(badName, "x", false); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound for truncated name, got %v", err)
		}
		badIno := make([]byte, 10)
		badIno[0] = 1
		badIno[6] = 1
		badIno[9] = 'a'
		if _, err := sfLookup(badIno, "a", false); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound for truncated inode, got %v", err)
		}
	})

	t.Run("sfReadDir malformed and i8 paths", func(t *testing.T) {
		fork := make([]byte, 25)
		fork[0] = 1
		fork[1] = 1
		binary.BigEndian.PutUint64(fork[2:], 1)
		fork[10] = 3
		copy(fork[13:], []byte("big"))
		fork[16] = 1
		binary.BigEndian.PutUint64(fork[17:], 0x100000001)
		entries, err := sfReadDir(fork, true)
		if err != nil || len(entries) != 1 || entries[0].Inode != 0x100000001 {
			t.Fatalf("sfReadDir i8 = (%v, %v)", entries, err)
		}
		entries, err = sfReadDir(make([]byte, 5), true)
		if err != nil || entries != nil {
			t.Fatalf("sfReadDir short = (%v, %v), want (nil, nil)", entries, err)
		}
		badName := make([]byte, 9)
		badName[0] = 1
		badName[6] = 5
		entries, err = sfReadDir(badName, false)
		if err != nil || len(entries) != 0 {
			t.Fatalf("sfReadDir truncated name = (%v, %v)", entries, err)
		}
		badFType := make([]byte, 10)
		badFType[0] = 1
		badFType[6] = 1
		badFType[9] = 'a'
		entries, err = sfReadDir(badFType, true)
		if err != nil || len(entries) != 0 {
			t.Fatalf("sfReadDir truncated ftype = (%v, %v)", entries, err)
		}
		entries, err = sfReadDir(badFType, false)
		if err != nil || len(entries) != 0 {
			t.Fatalf("sfReadDir truncated inode = (%v, %v)", entries, err)
		}
	})

	t.Run("dir block scanners edge cases", func(t *testing.T) {
		hdr := dirDataHdrSize(sb.hasCRC)
		if ino, found := searchDirBlock(make([]byte, hdr), "x", true, sb.hasCRC, true); found || ino != 0 {
			t.Fatalf("searchDirBlock short hdr = (%d, %v)", ino, found)
		}
		blk := make([]byte, hdr+10)
		binary.BigEndian.PutUint16(blk[hdr:], dirFreeTag)
		binary.BigEndian.PutUint16(blk[hdr+2:], 4)
		if ino, found := searchDirBlock(blk, "x", true, sb.hasCRC, true); found || ino != 0 {
			t.Fatalf("searchDirBlock short free = (%d, %v)", ino, found)
		}
		blk = make([]byte, hdr+9)
		if ino, found := searchDirBlock(blk, "x", true, sb.hasCRC, true); found || ino != 0 {
			t.Fatalf("searchDirBlock short entry = (%d, %v)", ino, found)
		}
		blk = make([]byte, hdr+24)
		if ino, found := searchDirBlock(blk, "x", true, sb.hasCRC, true); found || ino != 0 {
			t.Fatalf("searchDirBlock sentinel = (%d, %v)", ino, found)
		}
		blk = make([]byte, hdr+16)
		binary.BigEndian.PutUint64(blk[hdr:], 1)
		blk[hdr+8] = 8
		if ino, found := searchDirBlock(blk, "x", true, sb.hasCRC, true); found || ino != 0 {
			t.Fatalf("searchDirBlock truncated name = (%d, %v)", ino, found)
		}

		if got := parseDirBlock(make([]byte, hdr), true, sb.hasCRC); got != nil {
			t.Fatalf("parseDirBlock short hdr = %v", got)
		}
		blk = make([]byte, hdr+10)
		binary.BigEndian.PutUint16(blk[hdr:], dirFreeTag)
		binary.BigEndian.PutUint16(blk[hdr+2:], 4)
		if got := parseDirBlock(blk, true, sb.hasCRC); len(got) != 0 {
			t.Fatalf("parseDirBlock short free = %v", got)
		}
		blk = make([]byte, hdr+16)
		binary.BigEndian.PutUint64(blk[hdr:], 1)
		blk[hdr+8] = 0
		if got := parseDirBlock(blk, true, sb.hasCRC); len(got) != 0 {
			t.Fatalf("parseDirBlock zero namelen = %v", got)
		}
		blk = make([]byte, hdr+16)
		binary.BigEndian.PutUint64(blk[hdr:], 1)
		blk[hdr+8] = 8
		if got := parseDirBlock(blk, true, sb.hasCRC); len(got) != 0 {
			t.Fatalf("parseDirBlock truncated name = %v", got)
		}
		blk = make([]byte, hdr+10)
		binary.BigEndian.PutUint64(blk[hdr:], 1)
		blk[hdr+8] = 1
		if got := parseDirBlock(blk, true, sb.hasCRC); len(got) != 0 {
			t.Fatalf("parseDirBlock missing ftype = %v", got)
		}
	})

	t.Run("findFreeSlot and addEntryToSFDir edge cases", func(t *testing.T) {
		hdr := dirDataHdrSize(sb.hasCRC)
		blk := make([]byte, hdr+10)
		binary.BigEndian.PutUint16(blk[hdr:], dirFreeTag)
		binary.BigEndian.PutUint16(blk[hdr+2:], 4)
		if off := findFreeSlot(blk, 8, sb.hasFType, sb.hasCRC); off != -1 {
			t.Fatalf("findFreeSlot short free = %d, want -1", off)
		}
		blk = make([]byte, hdr+8)
		binary.BigEndian.PutUint64(blk[hdr:], 1)
		if off := findFreeSlot(blk, 8, sb.hasFType, sb.hasCRC); off != -1 {
			t.Fatalf("findFreeSlot short entry = %d, want -1", off)
		}
		blk = make([]byte, hdr+16)
		binary.BigEndian.PutUint64(blk[hdr:], 1)
		blk[hdr+8] = 0
		if off := findFreeSlot(blk, 8, sb.hasFType, sb.hasCRC); off != -1 {
			t.Fatalf("findFreeSlot zero namelen = %d, want -1", off)
		}

		short := newTestInode(92, 0x4000, inodeFmtLocal, 0)
		short.raw = short.raw[:inodeCoreSize+1]
		if addEntryToSFDir(short, 1, "x", 1, sb) {
			t.Fatal("expected addEntryToSFDir to reject a fork shorter than the header")
		}
		trunc := newTestInode(93, 0x4000, inodeFmtLocal, 0)
		trunc.raw = trunc.raw[:inodeCoreSize+6]
		trunc.dataFork()[0] = 1
		if addEntryToSFDir(trunc, 1, "x", 1, sb) {
			t.Fatal("expected addEntryToSFDir to reject truncated existing entries")
		}
		i8 := newTestInode(94, 0x4000, inodeFmtLocal, 0)
		i8.dataFork()[1] = 1
		if !addEntryToSFDir(i8, 7, "ok", 1, sb) {
			t.Fatal("expected addEntryToSFDir to support an 8-byte parent header")
		}
		if addEntryToSFDir(newTestInode(95, 0x4000, inodeFmtLocal, 0), 0x100000000, "big", 1, sb) {
			t.Fatal("expected addEntryToSFDir to reject 8-byte child inode upgrades")
		}
		forkCap := newTestInode(96, 0x4000, inodeFmtLocal, 0)
		forkCap.forkOff = 1
		if addEntryToSFDir(forkCap, 7, "toolong", 1, sb) {
			t.Fatal("expected addEntryToSFDir to reject entries that exceed forkOff capacity")
		}
	})
}

func TestDirRemainingSpecifics(t *testing.T) {
	sb := defaultSB()

	t.Run("sf helpers truncated names", func(t *testing.T) {
		fork := make([]byte, 12)
		fork[0] = 1
		fork[6] = 5
		if _, err := sfLookup(fork, "x", false); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound for sfLookup truncated name, got %v", err)
		}
		entries, err := sfReadDir(fork, false)
		if err != nil || len(entries) != 0 {
			t.Fatalf("sfReadDir truncated name = (%v, %v), want (empty, nil)", entries, err)
		}
	})

	t.Run("parseDirBlock sentinel branch", func(t *testing.T) {
		hdr := dirDataHdrSize(sb.hasCRC)
		blk := make([]byte, hdr+24)
		if got := parseDirBlock(blk, true, sb.hasCRC); len(got) != 0 {
			t.Fatalf("parseDirBlock sentinel block = %v", got)
		}
	})

	t.Run("addEntryToSFDir existing i8 entries", func(t *testing.T) {
		in := newTestInode(99, 0x4000, inodeFmtLocal, 0)
		fork := in.dataFork()
		fork[0] = 1
		fork[1] = 1
		binary.BigEndian.PutUint64(fork[2:], 1)
		fork[10] = 3
		copy(fork[13:], []byte("old"))
		fork[16] = 1
		binary.BigEndian.PutUint64(fork[17:], 5)
		if !addEntryToSFDir(in, 7, "new", 1, sb) {
			t.Fatal("expected addEntryToSFDir to append after an existing i8 entry")
		}
		if fork[0] != 2 {
			t.Fatalf("expected updated short-form count 2, got %d", fork[0])
		}
	})
}
