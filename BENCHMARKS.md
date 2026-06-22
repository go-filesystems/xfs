# Performance parity — go-filesystems/xfs vs mkfs.xfs / kernel xfs  (2026-06-22)

## Methodology

- **Where**: the `debian` Tart VM (linux/arm64) on an Apple-silicon (M4) host.
  Our pure-Go driver and the reference C tools run in the same VM, same kernel,
  same hardware. Reads are **cold** (`echo 3 > /proc/sys/vm/drop_caches` before
  every iteration).
- **CPU / kernel**: 4 vCPU aarch64, Linux 6.12.74 (Debian 13).
- **Go**: 1.26.4 linux/arm64, CGO disabled.
- **Reference tools**: xfsprogs 6.13.0 (`mkfs.xfs`), in-tree kernel XFS.
- **Image set**: 2008 files — 2000 small (1–4 KiB) + 8 large (4 MiB) ≈ 38 MB of
  file data in a 513 MiB image (XFS requires > 300 MiB).
- **Sampling**: best-of-5; format and read timed separately; read cold;
  throughput on the ~38 MB payload.
- **Format**: ours `xfs.Format(path, size, cfg)` vs `truncate` + `mkfs.xfs`.
- **Read**: image created+populated by `mkfs.xfs` + loop-mount + `cp -a`, then
  read by ours (`Open`+walk) and the kernel (`mount -o loop` + `tar`). No
  general-purpose userspace XFS reader is shipped in Debian, so no peer column.
- **Correctness gate (verified)**: our extraction returns exactly 2008 files
  byte-for-byte; **our `xfs.Format` output loop-mounts and is `xfs_repair -n`
  clean.**

> **Important geometry note.** The read image is created with
> `mkfs.xfs -d agsize=134217728` (128 MiB AGs → **power-of-2 agblocks**). Our
> reader currently assumes `fsblock == agno*agblocks + agbno`, which only holds
> when `agblocks` is a power of two. `mkfs.xfs`'s *default* agsize is **not** a
> power of two (e.g. 32832 blocks), and our reader then computes an
> out-of-range physical block and fails (`read extent block …: EOF`). See the
> action items — this is the top correctness gap.

## Results

| op | size | ours (MB/s, wall) | reference (MB/s, wall) | ratio | verdict |
|----|------|-------------------|------------------------|-------|---------|
| Format | 513 MiB | — , **0.118 ms** | mkfs.xfs: — , 13.99 ms | **0.008×** | ours 119× faster† |
| Read (cold) | 38 MB | **1564 MB/s, 23.5 ms** | kernel: 647 MB/s, 56.9 ms | **0.41×** | **ours 2.4× faster** |

† See caveat below.

## Summary

- **Read: we BEAT the kernel path 2.4× (1564 vs 647 MB/s).** Our reader does a
  tight sequential extent parse; the kernel comparison pays loop-device setup +
  VFS/`mount` overhead + `tar` traversal. On the data-only metric our pure-Go
  XFS reader is genuinely fast here. (Caveat: this holds for the power-of-2-AG
  geometry; the multi-AG default geometry currently fails — see action items.)
- **Format: nominally 119× faster, but not like-for-like.** Our `Format` writes
  a sparse, metadata-only image; `mkfs.xfs` zeroes/initialises more AG metadata
  and writes secondary superblocks. Our output is `xfs_repair`-clean and
  mountable, so it is a valid (and very fast) provisioning path, but the raw
  ratio overstates the difference in *work done*.

### Root causes / gaps

1. **fsblock→physical conversion is wrong for non-power-of-2 agblocks**
   (`read.go` → `blockByteOffset` treats `e.startBlock` as a flat block number).
   This is a correctness bug, not a perf one, and it is the headline item.
2. Read path is already competitive; the main perf headroom is buffer pooling
   and SIMD crc32c, shared with the other drivers.

### Action items

- [ ] **FIX (correctness):** decode the packed XFS fsblock properly —
      `agno = fsb >> sb.agBlkLog`, `agbno = fsb & ((1<<agBlkLog)-1)`,
      `linear = agno*agBlocks + agbno` — in `blockByteOffset` and everywhere an
      extent `startBlock` is turned into a byte offset. Add a regression test
      against a real `mkfs.xfs` default-geometry image (non-power-of-2 agblocks).
- [ ] Pool read buffers; cache the AG headers across the walk.
- [ ] SIMD crc32c via go-asmgen (shared across go-filesystems).

## Reproduce

```sh
sudo ./benchmarks/run.sh xfs <repo_dir> <work_dir> 5
```

`benchmarks/run.sh` is shared across the go-filesystems drivers;
`benchmarks/bench.go` is the xfs harness. Standalone `main` package, excluded
from the coverage gate.
