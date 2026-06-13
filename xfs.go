// Package xfs provides read/write access to XFS filesystem images without
// requiring root privileges or external tools. It targets XFS v5
// (CRC-enabled superblock, v3 inodes) as used by Rocky Linux 9, AlmaLinux 9,
// and other RHEL-family distributions. Version 4 filesystems are also
// supported for reading.
//
// Partition tables (MBR/GPT) are detected automatically; pass partIndex = -1
// to auto-select the first Linux data partition.
//
// Supported operations:
//   - ReadFile, ListDir, Stat — complete for all directory formats.
//   - WriteFile — creates or overwrites regular files. For existing files the
//     data is rewritten in-place when the new content fits the current block
//     allocation; larger rewrites allocate additional blocks. New files are
//     created by allocating an inode and data blocks via the AG free-space
//     B-trees and inserting an entry in the parent directory.
//   - DeleteFile — removes a file and frees its inode and data blocks.
package filesystem_xfs

// On-disk magic numbers (all big-endian).
const (
	magicSB    uint32 = 0x58465342 // "XFSB" — superblock
	magicAGF   uint32 = 0x58414746 // "XAGF" — AG free-space header
	magicAGI   uint32 = 0x58414749 // "XAGI" — AG inode header
	magicAGFL  uint32 = 0x5841464C // "XAFL" — AG free-list block (v5)
	magicInode uint16 = 0x494E     // "IN"   — inode

	// Directory data-block magic numbers.
	magicDir3Block uint32 = 0x58443342 // "XD3B" — v5 single-block directory
	magicDir3Data  uint32 = 0x58443344 // "XD3D" — v5 multi-block directory data
	magicDir2Block uint32 = 0x58443221 // v4 single-block directory
	magicDir2Data  uint32 = 0x58443231 // v4 multi-block directory data

	// AG free-space B-tree magic numbers.
	magicABTB   uint32 = 0x41423342 // "AB3B" — v5 bno B-tree
	magicABTC   uint32 = 0x41423343 // "AB3C" — v5 cnt B-tree
	magicABTBv4 uint32 = 0x41425442 // "ABTB" — v4 bno B-tree
	magicABTCv4 uint32 = 0x41425443 // "ABTC" — v4 cnt B-tree

	// Inode B-tree magic numbers.
	magicIAB3   uint32 = 0x49414233 // "IAB3" — v5 inobt
	magicIABTv4 uint32 = 0x54494142 // "TIAB" — v4 inobt

	// Inode data-fork formats (di_format).
	inodeFmtLocal   = 1 // inline data / short-form directory
	inodeFmtExtents = 2 // flat extent array in data fork
	inodeFmtBtree   = 3 // B-tree of extents; root in data fork

	// Byte value marking an unused directory-entry slot.
	dirFreeTag uint16 = 0xFFFF

	// Logical byte offset at which directory leaf blocks begin.
	// Data blocks live at logical blocks [0, dirLeafByteOffset/blockSize).
	dirLeafByteOffset uint64 = 1 << 35 // 32 GiB
)

// DirEntry is a parsed directory entry.
type DirEntry struct {
	Inode    uint64
	Name     string
	FileType uint8
}

// Stat holds basic metadata for a filesystem path.
type Stat struct {
	Mode  uint16
	Size  uint64
	Inode uint64
}
