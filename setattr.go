package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"
)

// writeXfsLegacyTimespec writes an XFS legacy on-disk timestamp at dst[0:8]:
// big-endian seconds since the epoch (be32, signed) + nanoseconds (be32).
// The bigtime variant (introduced in newer XFS) packs a single int64 of
// nanoseconds; we don't emit that here — every image we open today still
// reports legacy timestamps and the kernel mounts both interchangeably.
func writeXfsLegacyTimespec(dst []byte, t time.Time) {
	be := binary.BigEndian
	be.PutUint32(dst[0:], uint32(t.Unix()))
	be.PutUint32(dst[4:], uint32(t.Nanosecond()))
}

// bumpCTime stamps the inode's ctime field with the current time. Called
// from every setattr path: POSIX considers any inode-metadata change a
// "change time" event.
func bumpCTime(in *inode) {
	writeXfsLegacyTimespec(in.raw[inoOffCTime:], time.Now().UTC())
}

// chownInode updates the uid/gid of the inode at path. ctime is refreshed.
// Mode bits, file body, atime and mtime are left intact.
func chownInode(rw readerWriterAt, partOff int64, sb *superblock, path string, uid, gid uint32) error {
	in, err := pathLookup(rw, partOff, sb, path)
	if err != nil {
		return fmt.Errorf("xfs chown: %q: %w", path, err)
	}
	be := binary.BigEndian
	be.PutUint32(in.raw[inoOffUID:], uid)
	be.PutUint32(in.raw[inoOffGID:], gid)
	bumpCTime(in)
	return writeInode(rw, partOff, sb, in)
}

// chmodInode replaces the permission bits of the inode at path, preserving
// the file-type bits (regular/dir/symlink). ctime is refreshed.
func chmodInode(rw readerWriterAt, partOff int64, sb *superblock, path string, perm os.FileMode) error {
	in, err := pathLookup(rw, partOff, sb, path)
	if err != nil {
		return fmt.Errorf("xfs chmod: %q: %w", path, err)
	}
	be := binary.BigEndian
	cur := be.Uint16(in.raw[inoOffMode:])
	newMode := (cur &^ 0o7777) | (uint16(perm) & 0o7777)
	be.PutUint16(in.raw[inoOffMode:], newMode)
	in.mode = newMode
	bumpCTime(in)
	return writeInode(rw, partOff, sb, in)
}

// chtimesInode replaces atime and mtime on the inode at path. ctime is
// refreshed to "now" per POSIX. crtime (birth time) is left untouched.
func chtimesInode(rw readerWriterAt, partOff int64, sb *superblock, path string, atime, mtime time.Time) error {
	in, err := pathLookup(rw, partOff, sb, path)
	if err != nil {
		return fmt.Errorf("xfs chtimes: %q: %w", path, err)
	}
	writeXfsLegacyTimespec(in.raw[inoOffATime:], atime.UTC())
	writeXfsLegacyTimespec(in.raw[inoOffMTime:], mtime.UTC())
	bumpCTime(in)
	return writeInode(rw, partOff, sb, in)
}
