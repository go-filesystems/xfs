package filesystem_xfs

import (
	"encoding/binary"
	"fmt"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// MaxLabelLen is the on-disk size of the XFS volume label (sb_fname,
// at superblock offset 108).
const MaxLabelLen = 12

// Compile-time assertion: xfsFS implements filesystem.Labeller.
var _ filesystem.Labeller = (*xfsFS)(nil)

// labelFieldOffset is the byte offset of sb_fname inside the on-disk
// superblock (12 bytes wide).
const labelFieldOffset = 108

// Label returns the current volume label, decoded from sb_fname. An
// empty string means the filesystem has no label set.
func (fs *xfsFS) Label() string {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.sb.label
}

// SetLabel writes a new volume label into the primary superblock and
// every AG secondary superblock so they stay in sync (fsck and the
// kernel both warn on label divergence). The label must be at most
// MaxLabelLen bytes; shorter labels are null-padded.
//
// Concurrency: SetLabel takes the FS write lock for the duration of
// the operation. Like ext4's SetLabel it does not go through any
// journaling — it's a direct ReadAt / mutate / WriteAt over a 512-byte
// superblock per AG. Use only on a filesystem no other writer is
// touching.
func (fs *xfsFS) SetLabel(label string) error {
	b := []byte(label)
	if len(b) > MaxLabelLen {
		return fmt.Errorf("xfs: label %q is %d bytes, exceeds maximum %d", label, len(b), MaxLabelLen)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	for ag := uint32(0); ag < fs.sb.agCount; ag++ {
		sbOff := fs.partOffset + fs.sb.agByteOffset(ag)
		buf := make([]byte, 512)
		if _, err := fs.f.ReadAt(buf, sbOff); err != nil {
			return fmt.Errorf("xfs SetLabel: read AG %d superblock: %w", ag, err)
		}
		if binary.BigEndian.Uint32(buf[sbOffMagic:]) != magicSB {
			return fmt.Errorf("xfs SetLabel: bad magic in AG %d superblock", ag)
		}

		// Zero the label slot, then copy. Keeps the trailing bytes
		// clean when the new label is shorter than the previous one.
		for i := 0; i < MaxLabelLen; i++ {
			buf[labelFieldOffset+i] = 0
		}
		copy(buf[labelFieldOffset:], b)

		if fs.sb.hasCRC {
			updateCRC(buf, sbOffCRC, sbCRCLen)
		}

		if _, err := fs.f.WriteAt(buf, sbOff); err != nil {
			return fmt.Errorf("xfs SetLabel: write AG %d superblock: %w", ag, err)
		}
	}
	if err := fs.f.Sync(); err != nil {
		return fmt.Errorf("xfs SetLabel: sync: %w", err)
	}

	fs.sb.label = strings.TrimRight(string(b), "\x00 \t\n\r")
	return nil
}
