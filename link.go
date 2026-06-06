package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path"
	"strings"
)

// linkInode adds a directory entry at newPath that points at the same inode
// as oldPath, the POSIX hardlink operation. The source must not be a
// directory (POSIX rule) and newPath must not already exist. The source
// inode's nlink count is incremented and its ctime is refreshed.
//
// The symlink-following behavior on the source matches POSIX link(2): a
// path that resolves through symlinks ends up linking the resolved target,
// not the symlink itself. lstatLookup-style "no follow last" semantics are
// not provided here — callers that need them can use Symlink instead.
func linkInode(rw readerWriterAt, partOff int64, sb *superblock, oldPath, newPath string) error {
	src, err := pathLookup(rw, partOff, sb, oldPath)
	if err != nil {
		return fmt.Errorf("xfs link: source %q: %w", oldPath, err)
	}
	if src.isDir() {
		return fmt.Errorf("xfs link: %q is a directory; hardlinks to directories are not allowed", oldPath)
	}

	newPath = path.Clean(newPath)
	parentPath, name := path.Split(newPath)
	if name == "" {
		return fmt.Errorf("xfs link: invalid destination %q", newPath)
	}
	parentPath = strings.TrimSuffix(parentPath, "/")
	if parentPath == "" {
		parentPath = "/"
	}

	parentIn, err := pathLookup(rw, partOff, sb, parentPath)
	if err != nil {
		return fmt.Errorf("xfs link: dst parent %q: %w", parentPath, err)
	}
	if !parentIn.isDir() {
		return fmt.Errorf("xfs link: %q is not a directory", parentPath)
	}

	if _, err := lookupInDir(rw, partOff, sb, parentIn, name); err == nil {
		return fmt.Errorf("xfs link: %q already exists", newPath)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	// Bump nlink + ctime on the source inode.
	be := binary.BigEndian
	newNlink := be.Uint32(src.raw[inoOffNLink:]) + 1
	be.PutUint32(src.raw[inoOffNLink:], newNlink)
	bumpCTime(src)
	if err := writeInode(rw, partOff, sb, src); err != nil {
		return fmt.Errorf("xfs link: update source inode: %w", err)
	}

	// XFS directory-entry ftype matches the source's file-type bits.
	var ftype uint8
	switch {
	case src.isSymlink():
		ftype = 7 // DT_LNK
	default:
		ftype = 1 // DT_REG
	}
	return addDirEntry(rw, partOff, sb, parentIn, src.num, name, ftype)
}
