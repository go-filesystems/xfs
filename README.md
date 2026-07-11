<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-xfs.png" alt="go-filesystems/xfs" width="720"></p>

# xfs

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/xfs.svg)](https://pkg.go.dev/github.com/go-filesystems/xfs)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/xfs/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/xfs/actions/workflows/ci.yml)

Pure-Go read/write access to XFS filesystem images — no root privileges, no external tools, no CGO.

Supports v5 XFS images (CRC32c metadata checksums, ftype). MBR/GPT partition tables are auto-detected.

## References

https://docs.kernel.org/filesystems/xfs/index.html

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | Supports v5 XFS images (ftype, CRC32c) |
| Format | ✅ | Creates XFS images |
| Grow / Resize | ✅ | Whole-AG **and partial last-AG** growth (`xfs_growfs`-style); shrink returns `ErrShrinkUnsupported` |
| Reflink (shared extents / COW) | ✅ | `FormatConfig.Reflink`; per-AG refcount B-tree, `FS.Reflink` clone, COW-aware delete/overwrite |
| Quotas (user / group / project) | ✅ | `FormatConfig.Quota`; classic quota inodes + dquots, kept consistent by an inode-scan quotacheck |
| ReadFile | ✅ | Full file reads supported, incl. `NREXT64` (64-bit extent counts) images from modern `mkfs.xfs` |
| WriteFile | ✅ | Full file writes supported |
| MkDir / Delete / Rename | ✅ | Directory and rename operations supported |
| Stat / timestamps | ✅ | Legacy and `BIGTIME` (64-bit) timestamps, as written by modern `mkfs.xfs` |
| ReadLink / Symlinks | ✅ | Inline and remote (extent-based) targets, incl. the v5 `xfs_dsymlink_hdr` written by the kernel |
| Partitioned images | ✅ | MBR/GPT auto-detected |

All three advanced features above produce `xfs_repair -n`-clean images (validated
against `xfsprogs` 6.13 in CI on native amd64/arm64 runners).

## Advanced features

### Reflink (shared extents, copy-on-write)

Format with `FormatConfig{Reflink: true}` to enable the
`XFS_SB_FEAT_RO_COMPAT_REFLINK` feature bit and a per-AG reference-count B-tree
(`refcountbt`, matching `mkfs.xfs -m reflink=1,rmapbt=0`). `FS.Reflink(src, dst)`
then clones a file so it shares physical extents; deleting or overwriting a
reflinked file is copy-on-write (the shared extents' reference counts are
decremented and blocks are freed only when no longer shared).

### Quotas

Format with `FormatConfig{Quota: QuotaConfig{User, Group, Project, Enforce}}` to
create the classic (non-metadir) quota inodes (`sb_uquotino`/`sb_gquotino`/
`sb_pquotino`), seed their dquot clusters, and set `sb_qflags` + the
`XFS_SB_VERSION_QUOTABIT`. Block/inode accounting is kept consistent after every
mutating operation by a full inode-scan quotacheck.

## Limitations

- Snapshots are an LVM/device-mapper concern, not an XFS on-disk feature, and are
  out of scope for this filesystem-format library.
- Residuals in the advanced features: the reference-count B-tree is single-level
  (a share touching more distinct extents than fit one root block returns an
  error rather than growing the tree); growing a filesystem whose current last AG
  is already partial (in-place extension of that AG) is not supported — the last
  AG must be full before the next grow; quota files are a single dquot block, so
  identities beyond ~30 (one block of dquots) are not tracked.
- Intended for testing and tooling; not recommended for production workloads.

## Module

```
github.com/go-filesystems/xfs
```

## Supported operations

| Operation    | Status         |
|--------------|----------------|
| Open / Close | ✅ implemented |
| Format       | ✅ implemented |
| Stat         | ✅ implemented |
| ListDir      | ✅ implemented |
| ReadFile     | ✅ implemented |
| WriteFile    | ✅ implemented |
| MkDir        | ✅ implemented |
| DeleteFile   | ✅ implemented |
| DeleteDir    | ✅ implemented |
| Rename       | ✅ implemented |
| ReadLink     | ✅ implemented |
| Reflink      | ✅ implemented |
| Grow / Resize | ✅ implemented |

## API

### Format

```go
type FormatConfig struct {
    UUID    [16]byte // zero = randomly generated
    Label   string
    Reflink bool        // enable shared-extent (COW) support
    Quota   QuotaConfig // enable user/group/project quota accounting
}

type QuotaConfig struct {
    User, Group, Project bool // which quota types to account
    Enforce              bool // also set the *_ENFD (limit enforcement) flags
}

func Format(path string, sizeBytes int64, cfg FormatConfig) (*FS, error)

// Reflink clones srcPath into a new file dstPath sharing srcPath's extents
// (requires FormatConfig.Reflink); HasReflink reports the feature bit.
func (fs *FS) Reflink(srcPath, dstPath string) error
func (fs *FS) HasReflink() bool
```

### Open

```go
func Open(imagePath string, partIndex int) (*FS, error)
func (fs *FS) Close() error
```

### Read

```go
func (fs *FS) Stat(path string) (filesystem.Stat, error)
func (fs *FS) ListDir(path string) ([]filesystem.DirEntry, error)
func (fs *FS) ReadFile(path string) ([]byte, error)
func (fs *FS) ReadLink(path string) (string, error)
```

### Write

```go
func (fs *FS) WriteFile(path string, data []byte, perm os.FileMode) error
func (fs *FS) MkDir(path string, perm os.FileMode) error
func (fs *FS) DeleteFile(path string) error
func (fs *FS) DeleteDir(path string) error
func (fs *FS) Rename(oldPath, newPath string) error
```

## Implements

This package implements the `filesystem.Filesystem` contract defined in the
`github.com/go-filesystems/interface` module. Use the interface to write
generic tooling that works with other filesystem implementations in this
repository. Example:

```go
import (
    filesystem     "github.com/go-filesystems/interface"
    filesystem_xfs "github.com/go-filesystems/xfs"
)

f, _ := filesystem_xfs.Open("image.img", -1)
defer f.Close()
var fs filesystem.Filesystem = f
_, _ = fs.ReadFile("/hello.txt")
```
