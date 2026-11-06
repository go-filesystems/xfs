package filesystem_xfs

// openfile.go — filesystem.Opener: positional reads of a regular file. The
// extent list of the file is read once, at open (metadata, small); each ReadAt
// then reads only the blocks of its own window, so a server answering a READ of
// a few bytes of a large file (NFS, SFTP) does not materialise the whole file
// through ReadFile for every request.

import (
	"fmt"
	"io"
	"sort"

	filesystem "github.com/go-filesystems/interface"
)

var _ filesystem.Opener = (*xfsFS)(nil)

// OpenFile opens the regular file at path for positional reads. Holes and
// unwritten extents read as zeros, as in ReadFile.
func (fs *xfsFS) OpenFile(path string) (filesystem.File, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	in, err := fsPathLookup(fs.f, fs.partOffset, fs.sb, path)
	if err != nil {
		return nil, err
	}
	if !in.isRegular() {
		return nil, fmt.Errorf("xfs: %q is not a regular file", path)
	}
	size := in.size
	if size > 1<<62 { // a forged size is no size
		size = 1 << 62
	}
	f := &xfsFile{fs: fs, size: int64(size)}
	switch in.format {
	case inodeFmtLocal:
		if uint64(len(in.dataFork())) < in.size {
			return nil, fmt.Errorf("xfs: inline inode %d size %d exceeds fork space %d", in.num, in.size, len(in.dataFork()))
		}
		f.inline = append([]byte(nil), in.dataFork()[:in.size]...)
	case inodeFmtExtents:
		if f.exts, err = inlineExtents(in); err != nil {
			return nil, fmt.Errorf("xfs: inode %d extents: %w", in.num, err)
		}
	case inodeFmtBtree:
		if f.exts, err = btreeExtents(fs.f, fs.partOffset, fs.sb, in); err != nil {
			return nil, fmt.Errorf("xfs: inode %d btree extents: %w", in.num, err)
		}
	default:
		return nil, fmt.Errorf("xfs: inode %d unsupported fork format %d", in.num, in.format)
	}
	sort.Slice(f.exts, func(i, j int) bool { return f.exts[i].startOff < f.exts[j].startOff })
	return f, nil
}

type xfsFile struct {
	fs     *xfsFS
	size   int64
	inline []byte
	exts   []extent
}

func (f *xfsFile) Size() int64 { return f.size }

// ReadAt fills p from off: the extents that overlap the window are read into
// place, the rest is zero. It says io.EOF with the bytes there are when the
// window runs past the end of the file.
func (f *xfsFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("xfs: negative offset %d", off)
	}
	if off >= f.size {
		return 0, io.EOF
	}
	want := int64(len(p))
	if off+want > f.size {
		want = f.size - off
	}
	dst := p[:want]
	clear(dst)
	if f.inline != nil {
		copy(dst, f.inline[min(off, int64(len(f.inline))):])
	} else if err := f.readExtents(dst, off); err != nil {
		return 0, err
	}
	if want < int64(len(p)) {
		return int(want), io.EOF
	}
	return int(want), nil
}

func (f *xfsFile) readExtents(dst []byte, off int64) error {
	f.fs.mu.RLock()
	defer f.fs.mu.RUnlock()
	bs := int64(f.fs.sb.blockSize)
	hi := off + int64(len(dst))
	for _, e := range f.exts {
		// An extent whose file offset is past the window, or past the file, holds
		// nothing of it: checked before the multiply, so a forged offset cannot wrap.
		if e.startOff > uint64(hi)/uint64(bs)+1 {
			break // sorted by file offset
		}
		eStart := int64(e.startOff) * bs
		eEnd := eStart + int64(e.count)*bs
		lo, top := max(eStart, off), min(eEnd, hi)
		if top <= lo {
			continue
		}
		phys, err := blockByteOffset(f.fs.partOffset, f.fs.sb, e.startBlock)
		if err != nil {
			return err
		}
		if _, err := f.fs.f.ReadAt(dst[lo-off:top-off], phys+(lo-eStart)); err != nil {
			return fmt.Errorf("xfs: read extent block %d: %w", e.startBlock, err)
		}
	}
	return nil
}

func (f *xfsFile) Close() error { return nil }
