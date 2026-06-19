<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-xfs.png" alt="go-filesystems/xfs" width="720"></p>

# xfs

Pure-Go read/write access to XFS filesystem images — no root privileges, no external tools, no CGO.

Supports v5 XFS images (CRC32c metadata checksums, ftype). MBR/GPT partition tables are auto-detected.

## References

https://docs.kernel.org/filesystems/xfs/index.html

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | Supports v5 XFS images (ftype, CRC32c) |
| Format | ✅ | Creates XFS images |
| Grow / Resize | ✅ | Grow supported; shrink returns `ErrShrinkUnsupported` |
| ReadFile | ✅ | Full file reads supported, incl. `NREXT64` (64-bit extent counts) images from modern `mkfs.xfs` |
| WriteFile | ✅ | Full file writes supported |
| MkDir / Delete / Rename | ✅ | Directory and rename operations supported |
| Stat / timestamps | ✅ | Legacy and `BIGTIME` (64-bit) timestamps, as written by modern `mkfs.xfs` |
| ReadLink / Symlinks | ✅ | Inline and remote (extent-based) targets, incl. the v5 `xfs_dsymlink_hdr` written by the kernel |
| Partitioned images | ✅ | MBR/GPT auto-detected |

## Limitations

- Advanced XFS features (online resizing, quotas, reflink, snapshots) are not implemented.
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

## API

### Format

```go
type FormatConfig struct {
    UUID  [16]byte // zero = randomly generated
    Label string
}

func Format(path string, sizeBytes int64, cfg FormatConfig) (*FS, error)
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
