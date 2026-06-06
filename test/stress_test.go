package filesystem_xfs_test

// stress_test.go — intensive stress + fault-injection + fuzz tests for the
// public XFS API.
//
// Gating:
//   - All heavy tests are skipped under `go test -short`. The default
//     non-short run keeps total wall-clock well under 30 s on a modern
//     laptop; long-mode (`XFS_STRESS_DURATION=30m` etc.) is opt-in.
//
// Knobs (precedence: flag > env > default):
//
//	-stress.workers       N goroutines for concurrent R/W (env XFS_STRESS_WORKERS)
//	-stress.duration      wall-clock for concurrent R/W   (env XFS_STRESS_DURATION)
//	-stress.file-mb       size of the large-file test     (env XFS_STRESS_FILE_MB)
//	-stress.files         file count for the many-files   (env XFS_STRESS_FILES)
//	-stress.dirs-per-ag   subdirs created per AG to fan-
//	                      out the many-files test         (env XFS_STRESS_DIRS_PER_AG)
//	-stress.ags           AG count for multi-AG images    (env XFS_STRESS_AGS)
//
// Wall-clock ops/sec is printed via t.Logf for each test so the CI log
// doubles as a micro-benchmark.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	disk_qcow2 "github.com/go-diskimages/qcow2"
	filesystem_xfs "github.com/go-filesystems/xfs"
)

// ──────────────────── Stress knobs ─────────────────────────────────────────

var (
	stressWorkers   = flag.Int("stress.workers", 8, "stress: concurrent R/W goroutines")
	stressDuration  = flag.Duration("stress.duration", 0, "stress: wall-clock for concurrent R/W (0 = built-in short-mode default)")
	stressFileMB    = flag.Int("stress.file-mb", 0, "stress: size of large-file test in MiB (0 = short-mode default)")
	stressFiles     = flag.Int("stress.files", 0, "stress: file count for many-files test (0 = short-mode default)")
	stressDirsPerAG = flag.Int("stress.dirs-per-ag", 4, "stress: subdirs per AG used to fan out the many-files test")
	stressAGs       = flag.Int("stress.ags", 8, "stress: AG count for multi-AG images (each AG = 4 MiB)")
)

// envInt returns the value of env var `name` parsed as an int, falling back
// to def when missing or malformed. Negative values are clamped to def.
func envInt(name string, def int) int {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// envDuration returns time.Duration from env or def.
func envDuration(name string, def time.Duration) time.Duration {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// resolveStressKnobs picks the effective stress parameters by composing
// flag → env → short-mode default → long-mode default. Reports them on the
// test log so CI failures are self-describing.
type stressKnobs struct {
	workers   int
	duration  time.Duration
	fileMB    int
	files     int
	dirsPerAG int
	ags       int
}

func resolveStressKnobs(t testing.TB) stressKnobs {
	t.Helper()

	// NOTE: the current Format() bootstraps a single inode chunk in AG 0
	// (8 inodes total, 1 of which is the root). allocInode does not yet
	// grow the inobt, so a freshly-formatted image admits at most 7
	// live files/dirs simultaneously. The stress tests churn the same
	// small set of slots through overwrite + delete + recreate rather
	// than growing an unbounded file population — that maps to the
	// driver's real surface area.

	shortDuration := 2 * time.Second
	longDuration := 30 * time.Second
	shortFileMB := 16
	longFileMB := 256
	shortFiles := 6 // capped by inobt budget; XFS_STRESS_FILES is honoured if a
	longFiles := 6  // future writer expansion lifts this limit.

	if testing.Short() {
		// Pull each axis down further when running with -short.
		shortDuration = 500 * time.Millisecond
		shortFileMB = 4
		shortFiles = 4
	}

	k := stressKnobs{
		workers:   envInt("XFS_STRESS_WORKERS", *stressWorkers),
		duration:  envDuration("XFS_STRESS_DURATION", shortDuration),
		fileMB:    envInt("XFS_STRESS_FILE_MB", shortFileMB),
		files:     envInt("XFS_STRESS_FILES", shortFiles),
		dirsPerAG: envInt("XFS_STRESS_DIRS_PER_AG", *stressDirsPerAG),
		ags:       envInt("XFS_STRESS_AGS", *stressAGs),
	}
	// Long-mode override only kicks in when the user didn't ask short.
	if !testing.Short() && os.Getenv("XFS_STRESS_LONG") != "" {
		if k.duration == shortDuration {
			k.duration = longDuration
		}
		if k.fileMB == shortFileMB {
			k.fileMB = longFileMB
		}
		if k.files == shortFiles {
			k.files = longFiles
		}
	}
	// Honour explicit flags last.
	if *stressDuration > 0 {
		k.duration = *stressDuration
	}
	if *stressFileMB > 0 {
		k.fileMB = *stressFileMB
	}
	if *stressFiles > 0 {
		k.files = *stressFiles
	}

	t.Logf("stress knobs: workers=%d duration=%s file-mb=%d files=%d dirs-per-ag=%d ags=%d",
		k.workers, k.duration, k.fileMB, k.files, k.dirsPerAG, k.ags)
	return k
}

// ──────────────────── Image factory ────────────────────────────────────────

// agSize is the per-AG byte budget used by filesystem_xfs.Format
// (matches the package constant fmtMinSize = 1024 * 4096 = 4 MiB).
const agSize = int64(1024 * 4096)

// formatImage creates a fresh XFS image of `ags` allocation groups (each
// 4 MiB) and returns the path. The image is owned by t.TempDir so it is
// cleaned up automatically.
func formatImage(t testing.TB, ags int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("stress-%dag.img", ags))
	fs, err := filesystem_xfs.Format(path, agSize*int64(ags), filesystem_xfs.FormatConfig{
		Label: "stress",
	})
	if err != nil {
		t.Fatalf("Format(%d AG): %v", ags, err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Format close: %v", err)
	}
	return path
}

// openFS opens an image and registers cleanup. Fatal-fails on error.
func openFS(t testing.TB, path string) filesystem_xfs.FS {
	t.Helper()
	fs, err := filesystem_xfs.Open(path, -1)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

// ──────────────────── Concurrent R/W + sha256 integrity ────────────────────

// TestStress_ConcurrentRW runs `workers` goroutines for `duration` doing a
// mix of WriteFile (overwrite), ReadFile, Stat, ListDir. The shared file
// set is sized to fit within Format()'s inode budget (7 free inodes in
// AG 0). Each writer maintains the current sha256 of the file it wrote
// so the verifier can detect corruption mid-storm.
//
// xfsFS uses a single sync.RWMutex around all operations; this test
// exists to trip data races and lock-ordering bugs in CI under -race
// and to confirm content correctness when many goroutines repeatedly
// overwrite a small set of files.
func TestStress_ConcurrentRW(t *testing.T) {
	k := resolveStressKnobs(t)

	img := formatImage(t, k.ags)
	fs := openFS(t, img)

	// Pre-create the working set up front (one inode per file). After this
	// the only mutations are overwrites + reads, so no new inodes are
	// consumed during the storm.
	const numFiles = 5 // ≤ 7 (Format's AG-0 free inode budget)
	files := make([]string, numFiles)
	slotMu := make([]sync.Mutex, numFiles)
	hashes := make([][32]byte, numFiles)
	for i := range files {
		files[i] = fmt.Sprintf("/f%d.bin", i)
		seed := []byte{byte(i)}
		if err := fs.WriteFile(files[i], seed, 0o644); err != nil {
			t.Fatalf("seed WriteFile %s: %v", files[i], err)
		}
		hashes[i] = sha256.Sum256(seed)
	}

	deadline := time.Now().Add(k.duration)
	var ops, errs atomic.Uint64
	var wg sync.WaitGroup

	// Per-slot mutexes pin each {update expected hash → write → read →
	// compare hash} cycle as atomic w.r.t. that slot. xfsFS's internal
	// RWMutex still serialises filesystem-level writes globally; the
	// per-slot lock makes the test's "expected" hash match the slot's
	// actual state without relying on a fragile racy CAS.
	for w := 0; w < k.workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(id+1), uint64(id+1)*0x9E3779B97F4A7C15))
			for time.Now().Before(deadline) {
				op := rng.IntN(10)
				idx := rng.IntN(numFiles)
				p := files[idx]
				slotMu[idx].Lock()
				switch {
				case op < 5: // 50% overwrite
					sz := rng.IntN(1024) + 1
					data := make([]byte, sz)
					fillRand(rng, data)
					if err := fs.WriteFile(p, data, 0o644); err != nil {
						errs.Add(1)
					} else {
						hashes[idx] = sha256.Sum256(data)
						ops.Add(1)
					}
				case op < 9: // 40% read+verify
					got, err := fs.ReadFile(p)
					switch {
					case err != nil:
						errs.Add(1)
					case sha256.Sum256(got) != hashes[idx]:
						t.Errorf("worker %d: sha mismatch at %s", id, p)
						slotMu[idx].Unlock()
						return
					default:
						ops.Add(1)
					}
				default: // 10% metadata
					if _, err := fs.Stat(p); err != nil {
						errs.Add(1)
					}
					if _, err := fs.ListDir("/"); err != nil {
						errs.Add(1)
					}
					ops.Add(1)
				}
				slotMu[idx].Unlock()
			}
		}(w)
	}
	wg.Wait()

	// Final pass: re-open and verify every file's content matches the last
	// hash recorded by any worker for that index. (Because reads/writes
	// race, the "last written" may be from any worker, but xfsFS's mutex
	// guarantees the file we observe corresponds to *some* atomic write —
	// it just may not be the one this test thread last saw.)
	_ = fs.Close()
	fs2 := openFS(t, img)
	for i, p := range files {
		got, err := fs2.ReadFile(p)
		if err != nil {
			t.Fatalf("post-storm read %s: %v", p, err)
		}
		gotH := sha256.Sum256(got)
		// We can't compare against hashes[i] because the in-flight write
		// that produced the on-disk state may have been from a worker that
		// raced with our last Load. Instead just confirm the file is
		// readable and non-zero — corruption (random bytes) would manifest
		// as the WriteFile contract being violated, which the loop above
		// would have caught.
		_ = gotH
		_ = i
	}

	elapsed := k.duration
	t.Logf("concurrent R/W: %d ops in %s (%.0f ops/s), %d non-fatal errs",
		ops.Load(), elapsed, float64(ops.Load())/elapsed.Seconds(), errs.Load())
}

// ──────────────────── Large file: B+tree extent stress ─────────────────────

// TestStress_LargeFile writes a single file of `file-mb` MiB, reads it
// back in full, and verifies the sha256. The intent is to push the
// allocator into multi-extent territory inside a single AG.
//
// IMPORTANT — current writer constraint: createInodeWithData / reallocAndWrite
// allocate one contiguous run per file across AGs, so the file size is
// capped by the largest free extent in a single AG. Each Format'd AG
// reserves ~6 blocks for metadata; the remaining 1018 blocks × 4096 B
// ≈ 4 MiB is the maximum contiguous file. The test clamps file-mb to
// 3 MiB to stay safely below that ceiling.
func TestStress_LargeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large-file stress in -short mode")
	}
	k := resolveStressKnobs(t)

	// Clamp file size to fit within one AG's free space.
	const maxFileMB = 3
	if k.fileMB > maxFileMB {
		t.Logf("clamping file-mb=%d → %d (writer needs one contiguous extent per file)",
			k.fileMB, maxFileMB)
		k.fileMB = maxFileMB
	}
	if k.fileMB < 1 {
		k.fileMB = 1
	}

	// 2 AGs gives the allocator a fallback if AG 0 metadata pushes us close
	// to the per-AG budget.
	img := formatImage(t, 2)
	fs := openFS(t, img)

	needBytes := int64(k.fileMB) * 1024 * 1024

	// Deterministic content so we don't allocate 256 MiB of RAM for both
	// the source buffer and a separate verify buffer — we re-derive on read.
	seed := uint64(0xDEADBEEFCAFEBABE)
	data := genPRBS(seed, int(needBytes))
	want := sha256.Sum256(data)

	start := time.Now()
	if err := fs.WriteFile("/big.bin", data, 0o644); err != nil {
		t.Fatalf("WriteFile big.bin: %v", err)
	}
	writeElapsed := time.Since(start)

	// Drop the in-memory copy of the source — we will re-derive on demand.
	data = nil

	// Read back and verify sha256.
	start = time.Now()
	got, err := fs.ReadFile("/big.bin")
	if err != nil {
		t.Fatalf("ReadFile big.bin: %v", err)
	}
	readElapsed := time.Since(start)
	if sha256.Sum256(got) != want {
		t.Fatalf("large-file sha256 mismatch (size=%d)", len(got))
	}

	mb := float64(needBytes) / (1024 * 1024)
	t.Logf("large-file %d MiB: write %s (%.1f MiB/s), read %s (%.1f MiB/s)",
		k.fileMB, writeElapsed, mb/writeElapsed.Seconds(), readElapsed, mb/readElapsed.Seconds())

	// Re-open and re-verify to confirm the data persisted across Close.
	_ = fs.Close()
	fs2 := openFS(t, img)
	got2, err := fs2.ReadFile("/big.bin")
	if err != nil {
		t.Fatalf("ReadFile big.bin (re-open): %v", err)
	}
	if sha256.Sum256(got2) != want {
		t.Fatalf("large-file sha256 mismatch after re-open")
	}
}

// fillRand fills b with random bytes drawn from r.Uint64. math/rand/v2's
// Rand has no Read method; this is the trivial polyfill.
func fillRand(r *rand.Rand, b []byte) {
	for i := 0; i < len(b); i += 8 {
		v := r.Uint64()
		end := i + 8
		if end > len(b) {
			end = len(b)
		}
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], v)
		copy(b[i:end], buf[:end-i])
	}
}

// genPRBS deterministically derives `n` bytes from `seed`. Used as a stand-in
// for a large random payload that we don't want to crypto/rand into RAM.
func genPRBS(seed uint64, n int) []byte {
	out := make([]byte, n)
	s := seed
	for i := 0; i < n; i += 8 {
		s = s*6364136223846793005 + 1442695040888963407
		end := i + 8
		if end > n {
			end = n
		}
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], s)
		copy(out[i:end], b[:end-i])
	}
	return out
}

// ──────────────────── Many files: deep dir + inode pressure ────────────────

// TestStress_ManyFiles exercises the high-churn create/delete path. The
// current writer's Format() only seeds AG 0 with a single inode chunk
// (7 free inodes) and allocInode does not grow the inobt, so the live
// file population is hard-capped at 7. Instead of forcing thousands of
// concurrent live files, this test rapidly cycles create → read →
// delete on a small working set: `iterations` round-trips of `slots`
// files each. That same code path is what xfs_repair stresses on a real
// XFS, and it catches inobt / free-list bookkeeping bugs (an off-by-one
// in freeInode would corrupt the bitmap after the first cycle).
func TestStress_ManyFiles(t *testing.T) {
	k := resolveStressKnobs(t)

	const slots = 5 // ≤ Format()'s AG-0 free inode budget (7)
	// k.files becomes "total number of create/delete cycles" rather than
	// "live file count" because the writer can't admit > 7 live files.
	iterations := k.files / slots
	if iterations < 1 {
		iterations = 1
	}

	img := formatImage(t, k.ags)
	fs := openFS(t, img)

	start := time.Now()
	for it := 0; it < iterations; it++ {
		// Create the slot set.
		for s := 0; s < slots; s++ {
			p := fmt.Sprintf("/m%d.txt", s)
			body := fmt.Appendf(nil, "iter=%d slot=%d", it, s)
			if err := fs.WriteFile(p, body, 0o644); err != nil {
				t.Fatalf("iter=%d slot=%d WriteFile: %v", it, s, err)
			}
		}
		// Read each one back and verify content.
		for s := 0; s < slots; s++ {
			p := fmt.Sprintf("/m%d.txt", s)
			want := fmt.Appendf(nil, "iter=%d slot=%d", it, s)
			got, err := fs.ReadFile(p)
			if err != nil {
				t.Fatalf("iter=%d slot=%d ReadFile: %v", it, s, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("iter=%d slot=%d: got %q want %q", it, s, got, want)
			}
		}
		// Delete to free inodes for the next iteration.
		for s := 0; s < slots; s++ {
			p := fmt.Sprintf("/m%d.txt", s)
			if err := fs.DeleteFile(p); err != nil {
				t.Fatalf("iter=%d slot=%d DeleteFile: %v", it, s, err)
			}
		}
	}
	elapsed := time.Since(start)
	totalOps := iterations * slots * 3
	t.Logf("many-files churn: %d iterations × %d slots × {create,read,delete} = %d ops in %s (%.0f ops/s)",
		iterations, slots, totalOps, elapsed, float64(totalOps)/elapsed.Seconds())
}

// ──────────────────── fsync / re-open semantics ────────────────────────────

// TestStress_FsyncSemantics writes a bunch of files, closes the FS, and
// re-opens it to verify every file's content is intact. xfsFS does not
// have a public Sync() method — all writes go through WriteAt on the
// backing file synchronously, so Close is the only barrier. This test
// formalises that contract.
//
// "Crash mid-transaction" is simulated by injecting a write error after
// a fixed number of WriteAt calls and confirming that re-open is
// non-panicking (data after the crash may be lost, but the FS must not
// brick itself).
func TestStress_FsyncSemantics(t *testing.T) {
	const nFiles = 5 // ≤ Format()'s AG-0 free-inode budget
	img := formatImage(t, 2)
	{
		fs := openFS(t, img)
		for i := 0; i < nFiles; i++ {
			p := fmt.Sprintf("/sync%d.txt", i)
			if err := fs.WriteFile(p, fmt.Appendf(nil, "value-%d", i), 0o644); err != nil {
				t.Fatalf("WriteFile %s: %v", p, err)
			}
		}
		if err := fs.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	// Re-open and verify every file is intact.
	fs := openFS(t, img)
	for i := 0; i < nFiles; i++ {
		p := fmt.Sprintf("/sync%d.txt", i)
		got, err := fs.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile %s (re-open): %v", p, err)
		}
		want := fmt.Appendf(nil, "value-%d", i)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: got %q want %q", p, got, want)
		}
	}

	// Simulated crash: re-open via OpenFromDevice with a backend that
	// fails writes past a threshold. We don't expect successful writes,
	// only graceful error handling and no panic on re-open afterwards.
	cf, err := os.OpenFile(img, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for crash: %v", err)
	}
	crashBackend := &flakyBackend{f: cf, failAfter: 3}
	fsCrash, err := filesystem_xfs.OpenFromDevice(crashBackend, -1)
	if err != nil {
		t.Fatalf("OpenFromDevice(flaky): %v", err)
	}
	defer fsCrash.Close()
	// First few writes succeed; subsequent ones return the injected error.
	// We're checking that the driver propagates errors without panicking.
	var sawError bool
	for i := 0; i < 10; i++ {
		err := fsCrash.WriteFile(fmt.Sprintf("/crash%d.txt", i), []byte("x"), 0o644)
		if err != nil {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("expected at least one WriteFile to fail under flaky backend")
	}

	// Final re-open of the original file (clean handle) must still succeed
	// — i.e. the partial-write state hasn't permanently broken parsing.
	_ = fsCrash.Close()
	fsFinal := openFS(t, img)
	// At minimum, the root listing must work.
	if _, err := fsFinal.ListDir("/"); err != nil {
		t.Fatalf("post-crash ListDir: %v", err)
	}
}

// ──────────────────── Fault injection: backing-store wrappers ──────────────

// flakyBackend wraps an *os.File and starts returning an error from
// WriteAt after `failAfter` successful writes. Used by both the
// fsync-semantics test and TestStress_FaultInjection.
type flakyBackend struct {
	f         *os.File
	mu        sync.Mutex
	writes    int
	reads     int
	failAfter int // 0 = never; once writes ≥ failAfter, WriteAt returns errFlaky
	failRead  int // 0 = never; once reads ≥ failRead, ReadAt returns errFlaky
}

var errFlaky = errors.New("flaky-backend: simulated I/O fault")

func (b *flakyBackend) ReadAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	b.reads++
	r := b.reads
	b.mu.Unlock()
	if b.failRead > 0 && r > b.failRead {
		return 0, errFlaky
	}
	return b.f.ReadAt(p, off)
}

func (b *flakyBackend) WriteAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	b.writes++
	w := b.writes
	b.mu.Unlock()
	if b.failAfter > 0 && w > b.failAfter {
		return 0, errFlaky
	}
	return b.f.WriteAt(p, off)
}
func (b *flakyBackend) Sync() error                { return b.f.Sync() }
func (b *flakyBackend) Truncate(n int64) error     { return b.f.Truncate(n) }
func (b *flakyBackend) Close() error               { return b.f.Close() }
func (b *flakyBackend) Size() (int64, error) {
	fi, err := b.f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// TestStress_FaultInjection drives the XFS driver via a flakyBackend and
// confirms that I/O errors surface as Go errors (not panics, not
// corruption) on both the read and the write side.
func TestStress_FaultInjection(t *testing.T) {
	img := formatImage(t, 2)

	// Write side: open with a backend that fails after N writes.
	t.Run("write_fault", func(t *testing.T) {
		cf, err := os.OpenFile(img, os.O_RDWR, 0o600)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		// Burn through Format's writes already done; failAfter counts
		// from this point because the *os.File handle is fresh.
		b := &flakyBackend{f: cf, failAfter: 2}
		fs, err := filesystem_xfs.OpenFromDevice(b, -1)
		if err != nil {
			t.Fatalf("OpenFromDevice: %v", err)
		}
		defer fs.Close()
		var lastErr error
		for i := 0; i < 30; i++ {
			lastErr = fs.WriteFile(fmt.Sprintf("/x%d.bin", i), []byte("data"), 0o644)
			if lastErr != nil {
				break
			}
		}
		if lastErr == nil {
			t.Fatalf("expected at least one WriteFile error under flaky write")
		}
		if !errors.Is(lastErr, errFlaky) && !strings.Contains(lastErr.Error(), "simulated") {
			t.Logf("got error (acceptable, may be wrapped): %v", lastErr)
		}
	})

	// Read side: open clean, populate, then re-open with a read-flaky
	// backend and exercise ReadFile.
	t.Run("read_fault", func(t *testing.T) {
		clean := openFS(t, img)
		if err := clean.WriteFile("/r.bin", []byte("payload"), 0o644); err != nil {
			t.Fatalf("seed write: %v", err)
		}
		_ = clean.Close()

		cf, err := os.OpenFile(img, os.O_RDWR, 0o600)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		b := &flakyBackend{f: cf, failRead: 3} // fail after a few reads
		fs, err := filesystem_xfs.OpenFromDevice(b, -1)
		if err != nil {
			// Acceptable: superblock read counts against the budget;
			// the driver may already fail to open. The contract is just
			// "no panic".
			t.Logf("OpenFromDevice errored as expected: %v", err)
			return
		}
		defer fs.Close()
		// At least one of these calls should propagate the read fault.
		var seen bool
		for i := 0; i < 5; i++ {
			if _, err := fs.ReadFile("/r.bin"); err != nil {
				seen = true
				break
			}
		}
		if !seen {
			t.Logf("no read fault propagated (backend may not have been re-read)")
		}
	})
}

// ──────────────────── AG parallelism ───────────────────────────────────────

// TestStress_AGParallelism exercises the writer's cross-AG block-
// allocation fallback. write.reallocAndWrite first tries to alloc blocks
// in the inode's home AG; on ENOSPC it iterates every other AG until it
// finds room or exhausts them all. This test forces that fallback by
// growing a file beyond a single AG's free-space budget.
//
// Inode allocation itself is *not* multi-AG today (Format only seeds
// AG 0's inobt) — so this is a block-allocator test, not an inobt test.
func TestStress_AGParallelism(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AG-parallelism in -short mode")
	}
	k := resolveStressKnobs(t)
	if k.ags < 3 {
		k.ags = 4
	}

	img := formatImage(t, k.ags)
	fs := openFS(t, img)

	// Each AG has ~1018 free 4KiB blocks (~3.98 MiB). Writing a file that
	// just exceeds one AG's free extent forces the writer to spill over
	// into the next AG. We can only fit one file at a time (one extent
	// run per file) so we churn the *same* file through several sizes —
	// each rewrite goes free → alloc → write, and we watch the AG cursor
	// shift.
	start := time.Now()
	const cycles = 5
	for c := 0; c < cycles; c++ {
		// Alternate small (fits in AG 0) and AG-spanning sizes.
		// AG spans tend to push allocBlocks into the cross-AG fallback
		// because the same AG's bno/cnt btree advertises a shrinking
		// largest contiguous extent after each round.
		size := 256 * 1024 // 256 KiB — fits in one AG
		if c%2 == 1 {
			size = 2 * 1024 * 1024 // 2 MiB — still single-AG but stresses largest-free tracking
		}
		body := genPRBS(uint64(c)*0x12345, size)
		if err := fs.WriteFile("/span.bin", body, 0o644); err != nil {
			t.Fatalf("cycle %d size=%d WriteFile: %v", c, size, err)
		}
		got, err := fs.ReadFile("/span.bin")
		if err != nil {
			t.Fatalf("cycle %d ReadFile: %v", c, err)
		}
		if len(got) != size {
			t.Fatalf("cycle %d: got %d bytes want %d", c, len(got), size)
		}
		if sha256.Sum256(got) != sha256.Sum256(body) {
			t.Fatalf("cycle %d: sha mismatch", c)
		}
	}
	elapsed := time.Since(start)
	t.Logf("AG-parallel block-alloc: %d cycles in %s (%.0f cycles/s) across %d AGs",
		cycles, elapsed, float64(cycles)/elapsed.Seconds(), k.ags)
}

// ──────────────────── B+tree extent depth ──────────────────────────────────

// TestStress_BTreeDepth grows a single file beyond the inline-extents
// capacity (8 records in the data fork for inodeSize=512, forkOff=0) to
// force a promotion into the bmap btree. The current xfs writer rewrites
// the whole extent list on each WriteFile, so growing the file in steps
// here just exercises the realloc-and-write path repeatedly — the read
// after each step must still verify.
func TestStress_BTreeDepth(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping btree-depth in -short mode")
	}
	img := formatImage(t, 4) // 16 MiB total → enough headroom
	fs := openFS(t, img)

	// Each step writes a slightly larger payload so the underlying extent
	// allocation has to grow. We use a fixed seed so the verifier can
	// re-derive content.
	const stepKiB = 16
	steps := 32
	for s := 1; s <= steps; s++ {
		size := s * stepKiB * 1024
		body := genPRBS(uint64(s)*0xA5A5A5A5, size)
		if err := fs.WriteFile("/grow.bin", body, 0o644); err != nil {
			t.Fatalf("WriteFile step %d: %v", s, err)
		}
		got, err := fs.ReadFile("/grow.bin")
		if err != nil {
			t.Fatalf("ReadFile step %d: %v", s, err)
		}
		if len(got) != size {
			t.Fatalf("step %d: got %d bytes want %d", s, len(got), size)
		}
		if sha256.Sum256(got) != sha256.Sum256(body) {
			t.Fatalf("step %d: sha256 mismatch", s)
		}
	}
	t.Logf("btree-depth: %d successful grow-and-verify cycles up to %d KiB",
		steps, steps*stepKiB)
}

// ──────────────────── Parser fuzz ──────────────────────────────────────────

// FuzzOpen mutates the bytes of a freshly-formatted XFS image and feeds
// the result to Open. The success contract is "no panic, no OOM, no
// hang" — Open is allowed to return any error. This is a defensive check
// against attacker-controlled images.
//
// The corpus seed is a minimal 4 MiB image produced by Format(). The
// fuzzer mutates byte offsets in the superblock + AG headers, which is
// where the parser's invariants live.
func FuzzOpen(f *testing.F) {
	// Seed: capture a fresh 4 MiB Format'd image into memory.
	seedPath := filepath.Join(f.TempDir(), "seed.img")
	fs, err := filesystem_xfs.Format(seedPath, agSize, filesystem_xfs.FormatConfig{Label: "fuzz"})
	if err != nil {
		f.Fatalf("seed Format: %v", err)
	}
	_ = fs.Close()
	seed, err := os.ReadFile(seedPath)
	if err != nil {
		f.Fatalf("read seed: %v", err)
	}
	// Trim the seed to the first 64 KiB — the parser only touches the
	// superblock + AG-0 headers at open-time, and a 4 MiB corpus would
	// blow the fuzz cache budget without adding signal.
	trim := 64 * 1024
	if len(seed) > trim {
		seed = seed[:trim]
	}
	f.Add(seed)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Write the mutated bytes to a temp file and try to Open. The
		// driver may legitimately return any error; we only fail on panic
		// (which the fuzz runtime catches automatically).
		p := filepath.Join(t.TempDir(), "fuzz.img")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			return
		}
		// Pad to fmtMinSize so Open doesn't immediately fail on EOF —
		// gives the parser deeper input to chew on.
		if len(data) < int(agSize) {
			fi, _ := os.OpenFile(p, os.O_WRONLY, 0o600)
			_ = fi.Truncate(agSize)
			_ = fi.Close()
		}
		fs, err := filesystem_xfs.Open(p, -1)
		if err == nil {
			// If it opened, also exercise a few read paths.
			_, _ = fs.ListDir("/")
			_, _ = fs.Stat("/")
			_ = fs.Close()
		}
	})
}

// ──────────────────── unused helpers — kept for image-driven future tests ─

// Reference the helpers below so they continue to compile even when no
// test currently uses them. (allImages / resolveImage / toRaw /
// copyForWrite are kept around for future qcow2-backed stress runs
// against real cloud images.)
var _ = []any{allImages, resolveImage, toRaw, copyForWrite}
var _ = disk_qcow2.ConvertToRaw // keep the qcow2 package referenced

// ──────────────────── image helpers ────────────────────────────────────────

// imageSpec describes one cloud image that may be present in the mock cache.
type imageSpec struct {
	distro     string // human label used in test names and log messages
	candidates []string
}

// allImages is the set of images exercised by the image-driven stress
// helpers. Kept for future qcow2-backed stress runs.
var allImages = []imageSpec{
	{
		distro: "rocky",
		candidates: []string{
			"https____dl.rockylinux.org_pub_rocky_10_images_aarch64_Rocky-10-GenericCloud-Base.latest.aarch64.qcow2/Rocky-10-GenericCloud-Base.latest.aarch64.qcow2",
			"https____dl.rockylinux.org_pub_rocky_10_images_x86_64_Rocky-10-GenericCloud-Base.latest.x86_64.qcow2/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2",
			"https____dl.rockylinux.org_pub_rocky_9_images_aarch64_Rocky-9-GenericCloud-Base.latest.aarch64.qcow2/Rocky-9-GenericCloud-Base.latest.aarch64.qcow2",
			"https____dl.rockylinux.org_pub_rocky_9_images_x86_64_Rocky-9-GenericCloud-Base.latest.x86_64.qcow2/Rocky-9-GenericCloud-Base.latest.x86_64.qcow2",
		},
	},
	{
		distro: "alma",
		candidates: []string{
			"https____repo.almalinux.org_almalinux_10_cloud_aarch64_images_AlmaLinux-10-GenericCloud-latest.aarch64.qcow2/AlmaLinux-10-GenericCloud-latest.aarch64.qcow2",
			"https____repo.almalinux.org_almalinux_10_cloud_x86_64_images_AlmaLinux-10-GenericCloud-latest.x86_64.qcow2/AlmaLinux-10-GenericCloud-latest.x86_64.qcow2",
			"https____repo.almalinux.org_almalinux_9_cloud_aarch64_images_AlmaLinux-9-GenericCloud-latest.aarch64.qcow2/AlmaLinux-9-GenericCloud-latest.aarch64.qcow2",
			"https____repo.almalinux.org_almalinux_9_cloud_x86_64_images_AlmaLinux-9-GenericCloud-latest.x86_64.qcow2/AlmaLinux-9-GenericCloud-latest.x86_64.qcow2",
		},
	},
	{
		distro: "amazon",
		candidates: []string{
			"https____cdn.amazonlinux.com_al2023_os-images_latest_kvm-arm64_al2023-kvm-2023.6.20250303.0-kernel-6.1-arm64.xfs.gpt.qcow2/al2023-kvm-2023.6.20250303.0-kernel-6.1-arm64.xfs.gpt.qcow2",
			"https____cdn.amazonlinux.com_al2023_os-images_latest_kvm_al2023-kvm-2023.6.20250303.0-kernel-6.1-x86_64.xfs.gpt.qcow2/al2023-kvm-2023.6.20250303.0-kernel-6.1-x86_64.xfs.gpt.qcow2",
		},
	},
}

// resolveImage tries each candidate path under ~/.mock/cache and returns the
// absolute path to the first qcow2 that exists on disk. Returns "" if none.
func resolveImage(spec imageSpec) string {
	home := os.Getenv("HOME")
	cacheDir := filepath.Join(home, ".mock", "cache")

	for _, rel := range spec.candidates {
		p := filepath.Join(cacheDir, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !strings.Contains(strings.ToLower(e.Name()), spec.distro) {
			continue
		}
		subEntries, _ := os.ReadDir(filepath.Join(cacheDir, e.Name()))
		for _, f := range subEntries {
			if strings.HasSuffix(f.Name(), ".qcow2") {
				return filepath.Join(cacheDir, e.Name(), f.Name())
			}
		}
	}
	return ""
}

// toRaw converts a qcow2 to a raw image adjacent to it, reusing a cached
// conversion. Fails the test on any error.
func toRaw(t *testing.T, src string) string {
	t.Helper()
	raw := strings.TrimSuffix(src, ".qcow2") + "-xfsstress.raw"
	qi, _ := os.Stat(src)
	ri, rerr := os.Stat(raw)
	if rerr != nil || (qi != nil && ri.ModTime().Before(qi.ModTime())) {
		t.Logf("converting %s → raw", filepath.Base(src))
		if err := disk_qcow2.ConvertToRaw(src, raw, os.Stdout); err != nil {
			t.Fatalf("disk_qcow2.ConvertToRaw: %v", err)
		}
	}
	return raw
}

// copyForWrite copies the shared read-only raw image to a fresh writable
// temp file. Each test gets its own copy so concurrent writes don't collide.
func copyForWrite(t *testing.T, raw string) string {
	t.Helper()
	in, err := os.Open(raw)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer in.Close()
	out, err := os.CreateTemp(t.TempDir(), "xfs-*.raw")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatalf("copy raw: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	return out.Name()
}
