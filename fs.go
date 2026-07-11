package filesystem_xfs

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

var openFile = os.OpenFile
var openPartitionOffset = partitionOffset
var openReadSuperblock = readSuperblock
var fsPathLookup = pathLookup
var fsReadDir = readDir
var fsWriteFile = writeFile
var fsDeleteFile = deleteFile
var fsLookupInDir = lookupInDir
var fsReadInode = readInode
var fsReadFileData = readFileData
var fsReadSymlinkTarget = readSymlinkTarget
var fsMakeDir = makeDir
var fsDeleteDir = deleteDir
var fsRenameEntry = renameEntry
var fsSymlinkInode = symlinkInode
var fsTruncateInode = truncateInode
var fsLinkInode = linkInode
var fsReflinkFile = reflinkFile
var fsQuotaRecompute = quotaRecompute

// readerWriterAt combines io.ReaderAt and io.WriterAt.
type readerWriterAt interface {
	io.ReaderAt
	io.WriterAt
}

// blockBackend is the interface backing xfsFS. The XFS read /
// write paths already speak io.ReaderAt + io.WriterAt; this
// interface adds the lifecycle + capacity methods (Sync / Size
// / Truncate / Close) needed by Grow and Close so any layered
// block source (LUKS Device, qcow2 wrapper, in-memory fixture)
// can back an XFS filesystem.
type blockBackend interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Size() (int64, error)
	Truncate(size int64) error
	io.Closer
}

// BlockBackend is the exported alias of blockBackend, so external
// packages can implement it and feed instances to OpenFromDevice.
// Same shape as ext4.BlockDevice and zfs.BlockBackend — keeping
// the interface uniform across go-filesystems lets a single
// adapter (e.g. luksAsBlock) serve every FS.
type BlockBackend = blockBackend

// osFileBackend wraps *os.File so plain-disk-image opens go
// through the same blockBackend path as layered ones.
type osFileBackend struct{ f *os.File }

func (o *osFileBackend) ReadAt(p []byte, off int64) (int, error)  { return o.f.ReadAt(p, off) }
func (o *osFileBackend) WriteAt(p []byte, off int64) (int, error) { return o.f.WriteAt(p, off) }
func (o *osFileBackend) Sync() error                              { return o.f.Sync() }
func (o *osFileBackend) Truncate(size int64) error                { return o.f.Truncate(size) }
func (o *osFileBackend) Close() error                             { return o.f.Close() }
func (o *osFileBackend) Size() (int64, error) {
	fi, err := o.f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// FS represents an open XFS filesystem image.
type xfsFS struct {
	f          blockBackend
	partOffset int64 // byte offset from image start to the XFS partition
	sb         *superblock
	mu         sync.RWMutex
}

// Verify implementation of the common filesystem interface + every
// capability interface xfsFS supports. These assertions are the
// load-bearing part of the contract — if a method signature drifts,
// the build fails here rather than at the type assertion in a caller.
var (
	_ filesystem.Filesystem     = (*xfsFS)(nil)
	_ filesystem.Symlinker      = (*xfsFS)(nil)
	_ filesystem.HardLinker     = (*xfsFS)(nil)
	_ filesystem.Truncater      = (*xfsFS)(nil)
	_ filesystem.MetadataSetter = (*xfsFS)(nil)
	_ filesystem.Grower         = (*xfsFS)(nil)
	_ filesystem.Resizer        = (*xfsFS)(nil)
)

// FS is the public interface returned by Open. Extends
// filesystem.Filesystem with the XFS-specific operations — image
// resize (Grow/Resize/GrowTo) plus inode metadata setters and the
// rich ExtendedStat reader.
type FS interface {
	filesystem.Filesystem
	// Grow is the canonical "extend to newSizeBytes" entry point. It
	// requires newSizeBytes to be at least the current filesystem size
	// and to align to a whole number of allocation groups (each AG =
	// sb_agblocks × blockSize). Equal sizes are a no-op.
	Grow(newSizeBytes int64) error
	// Resize routes the uniform filesystem.Resizer call to Grow when
	// expanding; shrink is intentionally not supported in XFS (matches
	// the kernel + xfsprogs semantics) and returns
	// filesystem.ErrShrinkUnsupported when newSizeBytes < current.
	Resize(newSizeBytes int64) error
	// GrowTo is the historical name kept for callers that target the
	// filesystem.Grower interface. It is a thin alias of Grow.
	GrowTo(newSizeBytes int64) error

	ExtendedStat(path string) (*InodeStat, error)
	Chown(path string, uid, gid uint32) error
	Chmod(path string, perm os.FileMode) error
	Chtimes(path string, atime, mtime time.Time) error
	Symlink(target, linkPath string) error
	Truncate(path string, newSize int64) error
	Link(oldPath, newPath string) error
	// Reflink clones srcPath into a new file dstPath that shares srcPath's
	// data extents copy-on-write (the equivalent of `cp --reflink`). It
	// requires the filesystem to have been formatted with reflink support
	// (FormatConfig.Reflink); dstPath must not already exist.
	Reflink(srcPath, dstPath string) error
	// HasReflink reports whether the filesystem carries the reflink feature.
	HasReflink() bool
	Label() string
	SetLabel(label string) error
}

var _ FS = (*xfsFS)(nil)

// Open opens an XFS filesystem image, auto-detecting the partition table and
// selecting the first Linux data partition. Pass partIndex ≥ 0 to pick a
// specific (0-based) partition entry; pass -1 for auto-detection.
func Open(imagePath string, partIndex int) (FS, error) {
	f, err := openFile(imagePath, os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("xfs: open %s: %w", imagePath, err)
	}
	return OpenFromDevice(&osFileBackend{f: f}, partIndex)
}

// OpenFromDevice opens an XFS filesystem backed by an arbitrary
// blockBackend. Layered callers (LUKS / qcow2 / in-memory) feed
// the FS without an *os.File-backed image.
func OpenFromDevice(dev BlockBackend, partIndex int) (FS, error) {
	// The hardened partition parser validates every offset against the device
	// size; a backend that cannot report its size (or reports 0) is treated as
	// a bare filesystem image at offset 0.
	devSize, _ := dev.Size()
	off, err := openPartitionOffset(dev, devSize, partIndex)
	if err != nil {
		dev.Close()
		return nil, err
	}
	sb, err := openReadSuperblock(dev, off)
	if err != nil {
		dev.Close()
		return nil, err
	}
	return &xfsFS{f: dev, partOffset: off, sb: sb}, nil
}

// Close releases the file handle.
func (fs *xfsFS) Close() error { return fs.f.Close() }

// Grow extends the filesystem to the absolute size newSizeBytes,
// matching the canonical xfs_growfs semantics:
//
//  1. newSizeBytes must be a multiple of the filesystem block size and
//     of the per-AG byte size (sb_agblocks × blockSize). Partial-AG
//     growth is rejected — the XFS spec only adds whole AGs.
//  2. newSizeBytes < current size returns an error. The kernel's XFS
//     and xfsprogs both forbid shrink; use Resize() for the typed
//     filesystem.ErrShrinkUnsupported variant.
//  3. The backing file is extended to newSizeBytes via Truncate.
//  4. For each newly-appended AG, the per-AG metadata is written
//     (secondary superblock at block 0, AGF/AGI at blocks 1/2, bno/cnt/
//     inobt leaves at blocks 3/4/5).
//  5. The primary superblock's sb_agcount is rewritten and re-CRC'd.
//
// Concurrency: takes the FS write lock for the duration of the call.
// On any I/O failure mid-grow the on-disk state is left as far as the
// operation got; callers may re-run Grow to retry — the writer is
// idempotent on a per-AG basis (each AG is initialised from scratch).
func (fs *xfsFS) Grow(newSizeBytes int64) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	blockSize := int64(fs.sb.blockSize)
	if newSizeBytes%blockSize != 0 {
		return fmt.Errorf("xfs: grow: size %d is not a multiple of block size %d", newSizeBytes, fs.sb.blockSize)
	}
	agBlocks := int64(fs.sb.agBlocks)

	curDBlocks := int64(fs.sb.dBlocks)
	if curDBlocks == 0 {
		curDBlocks = int64(fs.sb.agCount) * agBlocks
	}
	newDBlocks := newSizeBytes / blockSize
	curSize := curDBlocks * blockSize
	if newDBlocks == curDBlocks {
		return nil
	}
	if newDBlocks < curDBlocks {
		return fmt.Errorf("xfs: grow: size %d < current size %d (shrink not supported)", newSizeBytes, curSize)
	}

	// Growing a filesystem whose current last AG is already partial would
	// require rewriting that AG's AGF/AGI/free-space B-trees in place (extending
	// the final AG). That in-place extension is not implemented; the last AG
	// must currently be full before growing.
	if curDBlocks%agBlocks != 0 {
		return fmt.Errorf("xfs: grow: current filesystem ends in a partial AG (%d blocks); in-place extension of a partial last AG is not supported", curDBlocks)
	}

	// New AG count: ceil(newDBlocks / agBlocks). The final AG may be partial.
	newAgCount := uint32((newDBlocks + agBlocks - 1) / agBlocks)
	// Reject a partial last AG too small to hold its own metadata plus a free
	// extent — xfs_repair rejects an undersized AG.
	if lastLen := newDBlocks - int64(newAgCount-1)*agBlocks; lastLen < growMinAGBlocks {
		return fmt.Errorf("xfs: grow: final partial AG of %d blocks is smaller than the minimum %d", lastLen, growMinAGBlocks)
	}

	// Extend backing file to the new size.
	if err := fs.f.Truncate(fs.partOffset + newSizeBytes); err != nil {
		return fmt.Errorf("xfs: grow: truncate: %w", err)
	}

	// Publish the new geometry before writing the AGs so agLength() and the
	// superblock buffers reflect the final block count (and the partial last AG).
	fs.sb.dBlocks = uint64(newDBlocks)
	fs.sb.agCount = newAgCount

	// Initialize each newly appended AG (full, except possibly the last) and its
	// secondary superblock.
	for ag := uint32(0); ag < newAgCount; ag++ {
		if int64(ag)*agBlocks < curDBlocks {
			continue // pre-existing AG
		}
		if err := fmtWriteAG(fs.f, fs.sb, ag, fs.sb.uuid); err != nil {
			return fmt.Errorf("xfs: grow: write AG %d: %w", ag, err)
		}
		if err := fmtWriteSecondarySB(fs.f, fs.partOffset, fs.sb, ag, newAgCount, fs.sb.uuid, fs.sb.label); err != nil {
			return fmt.Errorf("xfs: grow: write AG %d secondary SB: %w", ag, err)
		}
	}

	// Rewrite primary superblock with the updated geometry and re-CRC.
	if err := fmtWriteSuperblock(fs.f, fs.sb, newAgCount, fs.sb.uuid, fs.sb.label); err != nil {
		return fmt.Errorf("xfs: grow: write superblock: %w", err)
	}
	if err := fs.f.Sync(); err != nil {
		return fmt.Errorf("xfs: grow: sync: %w", err)
	}
	return nil
}

// growMinAGBlocks is the smallest number of blocks a (partial) final AG may
// have. It must comfortably exceed the fixed per-AG metadata footprint (B-tree
// roots + AGFL + optional refcount root) so the AG still carries a usable free
// extent; XFS itself rejects tiny allocation groups.
const growMinAGBlocks = 256

// GrowTo is the historical name for Grow and is kept for callers that
// target the filesystem.Grower interface. New code should call Grow.
func (fs *xfsFS) GrowTo(newSizeBytes int64) error {
	return fs.Grow(newSizeBytes)
}

// Resize implements filesystem.Resizer. For XFS shrink is intentionally
// not supported (matching the kernel + xfsprogs semantics); the function
// returns filesystem.ErrShrinkUnsupported when newSizeBytes is smaller
// than the current filesystem size. Equal sizes are a no-op; larger
// sizes route to Grow.
func (fs *xfsFS) Resize(newSizeBytes int64) error {
	fs.mu.RLock()
	curDBlocks := int64(fs.sb.dBlocks)
	if curDBlocks == 0 {
		curDBlocks = int64(fs.sb.agCount) * int64(fs.sb.agBlocks)
	}
	curSize := curDBlocks * int64(fs.sb.blockSize)
	fs.mu.RUnlock()
	if newSizeBytes < curSize {
		return filesystem.ErrShrinkUnsupported
	}
	return fs.Grow(newSizeBytes)
}

// ReadFile reads and returns the full contents of the regular file at path.
func (fs *xfsFS) ReadFile(path string) ([]byte, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	in, err := fsPathLookup(fs.f, fs.partOffset, fs.sb, path)
	if err != nil {
		return nil, err
	}
	if !in.isRegular() {
		return nil, fmt.Errorf("xfs: %q is not a regular file", path)
	}
	return fsReadFileData(fs.f, fs.partOffset, fs.sb, in)
}

// ListDir returns all entries of the directory at path.
func (fs *xfsFS) ListDir(path string) ([]filesystem.DirEntry, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	in, err := fsPathLookup(fs.f, fs.partOffset, fs.sb, path)
	if err != nil {
		return nil, err
	}
	if !in.isDir() {
		return nil, fmt.Errorf("xfs: %q is not a directory", path)
	}
	entries, err := fsReadDir(fs.f, fs.partOffset, fs.sb, in)
	if err != nil {
		return nil, err
	}
	out := make([]filesystem.DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, filesystem.NewDirEntry(e.Inode, e.Name, e.FileType))
	}
	return out, nil
}

// Stat returns basic metadata for the file or directory at path.
func (fs *xfsFS) Stat(path string) (filesystem.Stat, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	in, err := fsPathLookup(fs.f, fs.partOffset, fs.sb, path)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(in.mode, in.size, in.num), nil
}

// afterMutation persists the filesystem-wide free counters to the primary
// superblock after a successful allocating/freeing operation, so the global
// counts stay consistent with the per-AG headers (xfs_repair cross-checks
// them). A nil-returning op whose count sync fails surfaces that error.
func (fs *xfsFS) afterMutation(err error) error {
	if err != nil {
		return err
	}
	if err := syncSuperblockCounts(fs.f, fs.partOffset, fs.sb); err != nil {
		return err
	}
	// Keep the quota accounting consistent when quotas are enabled (a full
	// quotacheck; a no-op otherwise).
	return fsQuotaRecompute(fs.f, fs.partOffset, fs.sb)
}

// WriteFile creates or overwrites the regular file at path with the given data
// and Unix permission bits. The parent directory must already exist.
func (fs *xfsFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.afterMutation(fsWriteFile(fs.f, fs.partOffset, fs.sb, path, data, perm))
}

// DeleteFile removes the regular file at path and frees its inode and data
// blocks. Returns nil if the path does not exist (idempotent).
func (fs *xfsFS) DeleteFile(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.afterMutation(fsDeleteFile(fs.f, fs.partOffset, fs.sb, path))
}

// Reflink clones srcPath into a new file dstPath that shares srcPath's data
// extents copy-on-write, maintaining the per-AG refcount B-tree. Requires the
// filesystem to have been formatted with reflink support; dstPath must not
// already exist.
func (fs *xfsFS) Reflink(srcPath, dstPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.afterMutation(fsReflinkFile(fs.f, fs.partOffset, fs.sb, srcPath, dstPath))
}

// HasReflink reports whether the filesystem carries the reflink feature bit.
func (fs *xfsFS) HasReflink() bool {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.sb.hasReflink
}

// Link creates a hardlink at newPath pointing at the inode at oldPath. The
// source must not be a directory (POSIX rule) and newPath must not already
// exist. nlink + ctime on the source inode are refreshed.
func (fs *xfsFS) Link(oldPath, newPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.afterMutation(fsLinkInode(fs.f, fs.partOffset, fs.sb, oldPath, newPath))
}

// Truncate resizes the regular file at path to newSize bytes. Growing a
// file produces an implicit sparse extension (no disk allocation); shrinking
// drops or trims extents past newSize. mtime + ctime are refreshed in all
// cases per POSIX. Symlinks on the path are followed.
func (fs *xfsFS) Truncate(path string, newSize int64) error {
	if newSize < 0 {
		return fmt.Errorf("xfs truncate: %q: negative size %d", path, newSize)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.afterMutation(fsTruncateInode(fs.f, fs.partOffset, fs.sb, path, uint64(newSize)))
}

// Symlink creates a symbolic link at linkPath whose target is the literal
// string `target`. The parent directory must exist; linkPath must not.
// Symlink targets are stored as-is and are not resolved at creation time.
func (fs *xfsFS) Symlink(target, linkPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.afterMutation(fsSymlinkInode(fs.f, fs.partOffset, fs.sb, target, linkPath))
}

// ReadLink returns the target of the symbolic link at path.
func (fs *xfsFS) ReadLink(path string) (string, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	// Resolve the parent directory (pathLookup follows symlinks automatically,
	// so we look up by component to avoid following the last symlink).
	p := path
	if p == "/" {
		return "", fmt.Errorf("xfs: %q is not a symbolic link", path)
	}
	var parentPath, name string
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			parentPath = p[:i]
			name = p[i+1:]
			break
		}
	}
	if parentPath == "" {
		parentPath = "/"
	}
	parentIn, err := fsPathLookup(fs.f, fs.partOffset, fs.sb, parentPath)
	if err != nil {
		return "", err
	}
	inoNum, err := fsLookupInDir(fs.f, fs.partOffset, fs.sb, parentIn, name)
	if err != nil {
		return "", err
	}
	in, err := fsReadInode(fs.f, fs.partOffset, fs.sb, inoNum)
	if err != nil {
		return "", err
	}
	if !in.isSymlink() {
		return "", fmt.Errorf("xfs: %q is not a symbolic link", path)
	}
	target, err := fsReadSymlinkTarget(fs.f, fs.partOffset, fs.sb, in)
	if err != nil {
		return "", err
	}
	return string(target), nil
}

// MkDir creates a new directory at path with the given Unix permission bits.
// The parent directory must already exist. Returns an error if path already
// exists.
func (fs *xfsFS) MkDir(path string, perm os.FileMode) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.afterMutation(fsMakeDir(fs.f, fs.partOffset, fs.sb, path, perm))
}

// DeleteDir removes the directory at path and all of its contents
// recursively. Returns nil if path does not exist (idempotent).
func (fs *xfsFS) DeleteDir(path string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.afterMutation(fsDeleteDir(fs.f, fs.partOffset, fs.sb, path))
}

// Rename moves (and optionally renames) the file or directory at oldPath to
// newPath. If newPath already exists it is replaced (directories must be
// empty).
func (fs *xfsFS) Rename(oldPath, newPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.afterMutation(fsRenameEntry(fs.f, fs.partOffset, fs.sb, oldPath, newPath))
}

// Chown changes the owner uid/gid of the inode at path. ctime is bumped
// to "now" (POSIX). Mode, file body, atime and mtime are untouched.
// xfs-specific — not part of filesystem.Filesystem.
func (fs *xfsFS) Chown(path string, uid, gid uint32) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	// Ownership changes reassign the inode's quota charge, so run the quota
	// reconciliation (a no-op when quotas are disabled).
	return fs.afterMutation(chownInode(fs.f, fs.partOffset, fs.sb, path, uid, gid))
}

// Chmod replaces the permission bits of the inode at path; the file-type
// bits (regular/dir/symlink) are preserved. ctime is bumped to "now".
// xfs-specific.
func (fs *xfsFS) Chmod(path string, perm os.FileMode) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return chmodInode(fs.f, fs.partOffset, fs.sb, path, perm)
}

// Chtimes sets atime and mtime on the inode at path; ctime bumps to
// "now" per POSIX. crtime (birth time) is preserved. xfs-specific.
func (fs *xfsFS) Chtimes(path string, atime, mtime time.Time) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return chtimesInode(fs.f, fs.partOffset, fs.sb, path, atime, mtime)
}

// blockByteOffset converts a packed on-disk filesystem block number (fsbno) to
// its byte offset. The fsbno is first decoded to a flat/physical block via
// fsbToPhysBlock — XFS packs fsbno = (agno<<agblklog)|agbno and the default
// mkfs.xfs agsize is not a power of two, so the raw fsbno is NOT a flat block
// index. It then rejects any block whose offset would overflow int64 (and
// therefore wrap negative). An attacker-controlled block pointer (from a forged
// extent or btree node) could otherwise drive a negative ReadAt offset; some
// backends panic on that rather than erroring. Returns ErrBlockOutOfRange when
// the offset cannot be represented.
func blockByteOffset(partOff int64, sb *superblock, fsbno uint64) (int64, error) {
	bs := int64(sb.blockSize)
	phys := sb.fsbToPhysBlock(fsbno)
	// phys*bs must not overflow int64, and partOff+that must not either.
	if bs <= 0 || phys > uint64((1<<63-1-partOff)/bs) {
		return 0, fmt.Errorf("xfs: block %d offset overflows: %w", fsbno, ErrBlockOutOfRange)
	}
	return partOff + int64(phys)*bs, nil
}

// ErrBlockOutOfRange is returned when a block number's byte offset would
// overflow int64 (a forged extent/btree pointer).
var ErrBlockOutOfRange = fmt.Errorf("xfs: block offset out of range")

// readRawBlock reads one filesystem block (sb.blockSize bytes) by absolute
// block number.
func readRawBlock(f io.ReaderAt, partOff int64, sb *superblock, blockNum uint64) ([]byte, error) {
	off, err := blockByteOffset(partOff, sb, blockNum)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, sb.blockSize)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("xfs: read block %d: %w", blockNum, err)
	}
	return buf, nil
}

// writeRawBlock writes one filesystem block by packed fsbno (see fsbToPhysBlock).
func writeRawBlock(f io.WriterAt, partOff int64, sb *superblock, blockNum uint64, data []byte) error {
	off := partOff + int64(sb.fsbToPhysBlock(blockNum))*int64(sb.blockSize)
	if _, err := f.WriteAt(data, off); err != nil {
		return fmt.Errorf("xfs: write block %d: %w", blockNum, err)
	}
	return nil
}
