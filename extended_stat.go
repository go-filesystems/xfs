package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
	"path"
	"strings"
	"time"
)

// InodeStat is the rich metadata view of an XFS inode. The common
// filesystem.Stat only exposes mode/size/inode; callers that want
// uid/gid, timestamps, link count or the inode generation number use
// ExtendedStat.
//
// All timestamps are returned in UTC.
type InodeStat struct {
	Inode      uint64
	Mode       uint16
	Size       uint64
	NBlocks    uint64 // di_nblocks: count of filesystem blocks allocated
	NLink      uint32
	UID        uint32
	GID        uint32
	Generation uint32
	ATime      time.Time
	CTime      time.Time
	MTime      time.Time
	CRTime     time.Time // v3 only — birth time; zero on v1/v2 inodes
}

// IsRegular / IsDir / IsSymlink mirror the on-disk mode predicates.
func (s *InodeStat) IsRegular() bool { return s.Mode&0xF000 == 0x8000 }
func (s *InodeStat) IsDir() bool     { return s.Mode&0xF000 == 0x4000 }
func (s *InodeStat) IsSymlink() bool { return s.Mode&0xF000 == 0xA000 }

// ExtendedStat returns the full inode metadata for path inside the XFS
// image. xfs-specific — not part of filesystem.Filesystem.
func (fs *xfsFS) ExtendedStat(p string) (*InodeStat, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	in, err := lstatLookup(fs.f, fs.partOffset, fs.sb, p)
	if err != nil {
		return nil, fmt.Errorf("xfs ExtendedStat %q: %w", p, err)
	}
	be := binary.BigEndian
	bigtime := inodeHasBigtime(in.raw)
	readT := func(off int) time.Time {
		return decodeInodeTime(in.raw, off, bigtime)
	}
	out := &InodeStat{
		Inode:      in.num,
		Mode:       in.mode,
		Size:       in.size,
		NBlocks:    in.nBlocks,
		NLink:      be.Uint32(in.raw[inoOffNLink:]),
		UID:        be.Uint32(in.raw[inoOffUID:]),
		GID:        be.Uint32(in.raw[inoOffGID:]),
		Generation: be.Uint32(in.raw[inoOffGen:]),
		ATime:      readT(inoOffATime),
		MTime:      readT(inoOffMTime),
		CTime:      readT(inoOffCTime),
	}
	// di_crtime only lives in v3 inodes — guarded by hasCRC since v3 is
	// what introduced CRCs.
	if fs.sb.hasCRC {
		out.CRTime = readT(inoOffCRTime)
	}
	return out, nil
}

// lstatLookup resolves p to its inode using lstat semantics: intermediate
// path components follow symlinks (via pathLookup), but the final component
// is read raw — so calling it on a symlink returns the symlink inode itself,
// not its target.
func lstatLookup(rw readerWriterAt, partOff int64, sb *superblock, p string) (*inode, error) {
	p = path.Clean(p)
	if p == "/" || p == "." {
		return pathLookup(rw, partOff, sb, p)
	}
	parentPath, name := path.Split(p)
	parentPath = strings.TrimSuffix(parentPath, "/")
	if parentPath == "" {
		parentPath = "/"
	}
	parentIn, err := pathLookup(rw, partOff, sb, parentPath)
	if err != nil {
		return nil, err
	}
	if !parentIn.isDir() {
		return nil, fmt.Errorf("xfs: %q is not a directory", parentPath)
	}
	childIno, err := lookupInDir(rw, partOff, sb, parentIn, name)
	if err != nil {
		return nil, err
	}
	return readInode(rw, partOff, sb, childIno)
}
