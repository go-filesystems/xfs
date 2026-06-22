package filesystem_xfs

import (
	"errors"
	"fmt"
	"io"

	"github.com/go-volumes/gpt"
)

// sectorSize is the LBA size (512) used throughout for partition-table offset
// arithmetic and the AG-header sector layout. Matches gpt.SectorSize.
const sectorSize = gpt.SectorSize

// partitionOffset returns the byte offset from the image start to the
// requested partition. partIndex selects a specific partition (0-based);
// pass -1 to auto-select the first Linux data partition. Returns 0 when no
// partition table is detected (bare filesystem image).
//
// H1: the bespoke MBR/GPT parser this used to carry (unbounded entry size and
// count, no device-size validation) is replaced by the shared, hardened
// go-volumes/gpt parser. deviceSize bounds every partition extent so a forged
// table cannot point the filesystem at an out-of-range offset; pass the
// backing device/image size in bytes.
func partitionOffset(r io.ReaderAt, deviceSize int64, partIndex int) (int64, error) {
	if deviceSize <= 0 {
		// Without a usable device size we cannot validate partition extents.
		// Treat as a bare filesystem image (offset 0) rather than guessing.
		return 0, nil
	}

	var p gpt.Partition
	var err error
	if partIndex >= 0 {
		p, err = gpt.ByIndex(r, deviceSize, partIndex)
	} else {
		p, err = gpt.ByType(r, deviceSize, gpt.LinuxFilesystemGUID)
		// Auto-select fallback: a Linux-typed GPT entry is preferred, but an
		// MBR table (or a GPT lacking the Linux-fs type GUID) still carries a
		// usable data partition. Fall back to the first populated entry —
		// matching the historical "first Linux/0x83, else nothing" behaviour
		// while staying bounds-checked.
		if errors.Is(err, gpt.ErrNotFound) {
			p, err = gptFirstLinux(r, deviceSize)
		}
	}
	if err != nil {
		// No partition table at all → bare filesystem image at offset 0,
		// preserving the historical behaviour for raw mkfs.xfs images.
		if errors.Is(err, gpt.ErrNoTable) {
			return 0, nil
		}
		return 0, fmt.Errorf("xfs: partition table: %w", err)
	}
	return p.StartOffset, nil
}

// gptFirstLinux returns the first MBR Linux (type 0x83) partition, or the first
// populated partition of any scheme, used as the auto-select fallback when no
// GPT Linux-filesystem-typed entry is present.
func gptFirstLinux(r io.ReaderAt, deviceSize int64) (gpt.Partition, error) {
	parts, err := gpt.List(r, deviceSize)
	if err != nil {
		return gpt.Partition{}, err
	}
	for _, p := range parts {
		if p.Scheme == gpt.SchemeMBR && p.MBRType == 0x83 {
			return p, nil
		}
	}
	if len(parts) > 0 {
		return parts[0], nil
	}
	return gpt.Partition{}, fmt.Errorf("%w: no populated partition", gpt.ErrNotFound)
}
