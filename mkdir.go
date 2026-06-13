package filesystem_xfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
)

var makeDirPathLookup = pathLookup
var makeDirLookupInDir = lookupInDir
var makeDirAllocInode = allocInode
var makeDirWriteInode = writeInode
var makeDirAddDirEntry = addDirEntry

// makeDir creates a new directory at p within the XFS filesystem.
// The parent directory must already exist. Returns an error if p already
// exists.
func makeDir(rw readerWriterAt, partOff int64, sb *superblock, p string, perm os.FileMode) error {
	p = path.Clean(p)
	parentPath, name := path.Split(p)
	if name == "" {
		return fmt.Errorf("xfs: invalid path %q", p)
	}
	parentPath = strings.TrimSuffix(parentPath, "/")
	if parentPath == "" {
		parentPath = "/"
	}

	dirIn, err := makeDirPathLookup(rw, partOff, sb, parentPath)
	if err != nil {
		return fmt.Errorf("xfs: parent %q: %w", parentPath, err)
	}
	if !dirIn.isDir() {
		return fmt.Errorf("xfs: %q is not a directory", parentPath)
	}

	// Reject if name already exists.
	_, err = makeDirLookupInDir(rw, partOff, sb, dirIn, name)
	if err == nil {
		return fmt.Errorf("xfs: %q already exists", p)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}

	// Allocate a new inode.
	ag := inoAG(sb, dirIn.num)
	ino, err := makeDirAllocInode(rw, partOff, sb, ag)
	if err != nil {
		for a := uint32(0); a < sb.agCount; a++ {
			if a == ag {
				continue
			}
			ino, err = makeDirAllocInode(rw, partOff, sb, a)
			if err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("xfs: makeDir: no free inode: %w", err)
		}
	}

	mode := uint16(perm&0o777) | 0x4000 // directory

	// Build the inode with a short-form (local) directory in the data fork.
	// A fresh empty directory has nlink=2: the parent's entry pointing at
	// it + the "." self-reference inside its own short-form data fork.
	inodeBuf := make([]byte, sb.inodeSize)
	initInodeV3(inodeBuf, ino, mode, sb.inodeSize, 2, sb.uuid)

	newIn := &inode{
		num:    ino,
		mode:   mode,
		raw:    inodeBuf,
		format: inodeFmtLocal,
	}

	// Build a minimal short-form directory header in the data fork.
	// Layout: [count=0][i8count=0][parent_ino: 4 or 8 bytes]
	fork := newIn.dataFork()
	be := binary.BigEndian
	fork[0] = 0 // count
	fork[1] = 0 // i8count
	if dirIn.num > 0xFFFFFFFF {
		be.PutUint64(fork[2:], dirIn.num)
		setInodeSize(newIn, 10) // 2 + 8
	} else {
		be.PutUint32(fork[2:], uint32(dirIn.num))
		setInodeSize(newIn, 6) // 2 + 4
	}
	setInodeFormat(newIn, inodeFmtLocal)

	if err := makeDirWriteInode(rw, partOff, sb, newIn); err != nil {
		return err
	}

	// Add entry to the parent directory (ftype 2 = directory).
	if err := makeDirAddDirEntry(rw, partOff, sb, dirIn, ino, name, 2 /* DT_DIR */); err != nil {
		return err
	}

	// A new subdirectory's ".." entry references the parent, so the parent's
	// link count gains one. xfs_repair flags a parent whose nlink doesn't
	// account for its child directories ("would reset inode N nlinks").
	be.PutUint32(dirIn.raw[inoOffNLink:], be.Uint32(dirIn.raw[inoOffNLink:])+1)
	return makeDirWriteInode(rw, partOff, sb, dirIn)
}
