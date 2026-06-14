# Performance benchmarks

Two halves that measure the **same standard operations** so the pure-Go driver
can be read side by side with the in-kernel XFS implementation.

## Go-driver side (portable, runs anywhere)

```sh
go test -bench=. -benchmem -run='^$'
```

Benchmarks (in `../bench_test.go`, public-API only): `Format`, `WriteFileSeq`,
`ReadFileSeq`, `Stat`, `ListDir`, `CreateFiles`, `DeleteFiles`. File-backed
image under `b.TempDir()` so the numbers include real block I/O.

> **Note — directory batch size.** The file-count benchmarks use 100 entries
> rather than ext4's 200: the XFS writer currently only emits single-block
> directory form, which caps a directory at ~127 entries (leaf form is not yet
> implemented). The reference `compare.sh` uses the same `NFILES=100` so the
> two sides stay aligned.

## Reference side (in-kernel XFS, Linux only, needs root)

```sh
scp bench/compare.sh dc1-r1-h1:/tmp/ && ssh dc1-r1-h1 'sudo bash /tmp/compare.sh'
```

`compare.sh` runs the same ops via `mkfs.xfs` + `mount -o loop` + `dd`
(with `fsync`/`drop_caches`) + coreutils.

> **Caveat — not apples-to-apples.** The kernel has a page cache, log and
> writeback; the Go driver does synchronous user-space block I/O. Treat the
> kernel numbers as a rough upper-bound reference, not a literal target.

## First findings (2026-06, Apple M4 Max)

Go-driver side (`-benchtime=3x`, 256 MiB image, 8 MiB big file, 100-file batches):

| Operation        | go-filesystems/xfs    |
|------------------|-----------------------|
| Sequential read  | ~5.2–7.6 GB/s         |
| Sequential write | ~1.7–1.9 GB/s         |
| Create file      | ~48 µs/file           |
| Delete file      | ~29 µs/file           |
| Format           | ~5.9 ms               |
| Stat             | ~27–46 µs             |
| ListDir (100)    | ~43 µs                |

Both sequential paths are fast and allocation-light on the read side (7
allocs/op); the large write allocates one ~8 MiB buffer (≈2k allocs/op total).
The metadata mutations (create/delete) are dominated by per-file directory and
inode B-tree rewrites — the main optimization target, and also where the
single-block directory limit lives.

Profile with
`go test -bench=BenchmarkWriteFileSeq -cpuprofile=cpu.out -memprofile=mem.out`.
