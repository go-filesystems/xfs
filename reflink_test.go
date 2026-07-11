package filesystem_xfs

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

const testOneAG = int64(16384 * 4096)

// formatReflink formats a reflink-enabled image and returns the open FS.
func formatReflink(t *testing.T, ags int64) *xfsFS {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reflink.img")
	fs, err := Format(path, ags*testOneAG, FormatConfig{Label: "reflink", Reflink: true})
	if err != nil {
		t.Fatalf("Format(reflink): %v", err)
	}
	xfs, ok := fs.(*xfsFS)
	if !ok {
		t.Fatalf("Format returned %T, want *xfsFS", fs)
	}
	return xfs
}

func TestReflinkFeatureBit(t *testing.T) {
	fs := formatReflink(t, 2)
	defer fs.Close()
	if !fs.HasReflink() {
		t.Fatal("HasReflink() = false on a reflink-formatted fs")
	}
	if !fs.sb.hasReflink {
		t.Fatal("sb.hasReflink = false")
	}
}

func TestReflinkCloneShareDeleteOverwrite(t *testing.T) {
	fs := formatReflink(t, 3)
	defer fs.Close()

	orig := bytes.Repeat([]byte("REFLINKDATA"), 6000) // ~66 KiB => multiple blocks
	if err := fs.WriteFile("/orig.dat", orig, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Reflink("/orig.dat", "/clone.dat"); err != nil {
		t.Fatalf("Reflink: %v", err)
	}
	if err := fs.Reflink("/orig.dat", "/clone2.dat"); err != nil {
		t.Fatalf("Reflink 2: %v", err)
	}

	// Both clones read identical to the original (shared extents).
	for _, p := range []string{"/clone.dat", "/clone2.dat"} {
		got, err := fs.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", p, err)
		}
		if !bytes.Equal(got, orig) {
			t.Fatalf("%s content mismatch (%d vs %d bytes)", p, len(got), len(orig))
		}
	}

	// The reflink flag is set on the source and both clones.
	for _, p := range []string{"/orig.dat", "/clone.dat", "/clone2.dat"} {
		in, err := fsPathLookup(fs.f, fs.partOffset, fs.sb, p)
		if err != nil {
			t.Fatal(err)
		}
		if !inodeIsReflinked(in) {
			t.Fatalf("%s not marked reflinked", p)
		}
	}

	// COW delete: removing clone2 must not disturb orig/clone.
	if err := fs.DeleteFile("/clone2.dat"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if got, _ := fs.ReadFile("/orig.dat"); !bytes.Equal(got, orig) {
		t.Fatal("orig corrupted after COW delete")
	}
	if got, _ := fs.ReadFile("/clone.dat"); !bytes.Equal(got, orig) {
		t.Fatal("clone corrupted after COW delete")
	}

	// COW break: overwriting clone allocates private blocks, orig unaffected.
	newData := bytes.Repeat([]byte("NEWCONTENT!"), 4000)
	if err := fs.WriteFile("/clone.dat", newData, 0o644); err != nil {
		t.Fatalf("overwrite clone: %v", err)
	}
	if got, _ := fs.ReadFile("/clone.dat"); !bytes.Equal(got, newData) {
		t.Fatal("clone overwrite mismatch")
	}
	if got, _ := fs.ReadFile("/orig.dat"); !bytes.Equal(got, orig) {
		t.Fatal("orig corrupted by clone overwrite")
	}
	// After the COW break clone.dat no longer shares extents.
	in, _ := fsPathLookup(fs.f, fs.partOffset, fs.sb, "/clone.dat")
	if inodeIsReflinked(in) {
		t.Fatal("clone still marked reflinked after COW break")
	}
}

func TestReflinkLocalSourceIsPlainCopy(t *testing.T) {
	fs := formatReflink(t, 2)
	defer fs.Close()
	// Small payload stays inline (local format).
	if err := fs.WriteFile("/small.txt", []byte("tiny"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.Reflink("/small.txt", "/small-clone.txt"); err != nil {
		t.Fatalf("Reflink(local): %v", err)
	}
	got, err := fs.ReadFile("/small-clone.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "tiny" {
		t.Fatalf("local clone = %q, want %q", got, "tiny")
	}
}

func TestReflinkErrors(t *testing.T) {
	// reflink not enabled.
	path := filepath.Join(t.TempDir(), "plain.img")
	plain, err := Format(path, 2*testOneAG, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if err := plain.WriteFile("/a", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := plain.Reflink("/a", "/b"); err == nil {
		t.Fatal("Reflink on non-reflink fs: want error")
	}

	fs := formatReflink(t, 2)
	defer fs.Close()
	if err := fs.WriteFile("/f", bytes.Repeat([]byte("z"), 9000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fs.MkDir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, src, dst string }{
		{"src missing", "/nope", "/x"},
		{"src is dir", "/d", "/x"},
		{"dst exists", "/f", "/f"},
		{"dst parent missing", "/f", "/nodir/x"},
		{"dst parent not dir", "/f", "/f/x"},
		{"dst empty name", "/f", "/"},
	}
	for _, c := range cases {
		if err := fs.Reflink(c.src, c.dst); err == nil {
			t.Errorf("%s: Reflink(%q,%q) = nil, want error", c.name, c.src, c.dst)
		}
	}
}

func TestRefcountIntervalMath(t *testing.T) {
	// incr over empty tree: whole range becomes refcount 2.
	recs := refcIncrRange(nil, 100, 10)
	if len(recs) != 1 || recs[0] != (refcRec{100, 10, 2}) {
		t.Fatalf("incr empty: %+v", recs)
	}
	// incr again over a sub-range: [100,105) -> 3, [105,110) stays 2.
	recs = refcIncrRange(recs, 100, 5)
	want := []refcRec{{100, 5, 3}, {105, 5, 2}}
	if !equalRecs(recs, want) {
		t.Fatalf("incr sub-range: got %+v want %+v", recs, want)
	}
	// decr [100,105): 3->2 (kept), so all becomes 2 and merges.
	recs, free := refcDecrRange(recs, 100, 5)
	if len(free) != 0 {
		t.Fatalf("decr shared: unexpected free %+v", free)
	}
	if !equalRecs(recs, []refcRec{{100, 10, 2}}) {
		t.Fatalf("decr merge: %+v", recs)
	}
	// decr the whole range: 2->1 everywhere => tree empties, nothing freed
	// (blocks still owned by the remaining inode).
	recs, free = refcDecrRange(recs, 100, 10)
	if len(recs) != 0 || len(free) != 0 {
		t.Fatalf("decr to 1: recs=%+v free=%+v", recs, free)
	}
	// decr a range NOT in the tree: the whole range is freed.
	_, free = refcDecrRange(nil, 200, 4)
	if !equalRecs(free, []refcRec{{200, 4, 0}}) {
		t.Fatalf("decr unshared: free=%+v", free)
	}
	// mixed: [50,55) shared (refc 2), [55,60) unshared. decr [50,60):
	// shared drops to 1 (removed), unshared freed.
	mixed := []refcRec{{50, 5, 2}}
	out, free := refcDecrRange(mixed, 50, 10)
	if len(out) != 0 {
		t.Fatalf("mixed decr recs=%+v", out)
	}
	if !equalRecs(free, []refcRec{{55, 5, 0}}) {
		t.Fatalf("mixed decr free=%+v", free)
	}
}

func TestRefcountReadWriteRoundTrip(t *testing.T) {
	fs := formatReflink(t, 1)
	defer fs.Close()
	recs, err := refcountReadRecs(fs.f, fs.partOffset, fs.sb, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("fresh refcountbt not empty: %+v", recs)
	}
	want := []refcRec{{10, 3, 2}, {20, 5, 4}}
	if err := refcountWriteRecs(fs.f, fs.partOffset, fs.sb, 0, want); err != nil {
		t.Fatal(err)
	}
	got, err := refcountReadRecs(fs.f, fs.partOffset, fs.sb, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !equalRecs(got, want) {
		t.Fatalf("round-trip: got %+v want %+v", got, want)
	}
	// Overflow: more records than a single block holds.
	big := make([]refcRec, refcMaxRecs(fs.sb)+1)
	if err := refcountWriteRecs(fs.f, fs.partOffset, fs.sb, 0, big); !errors.Is(err, errRefcountFull) {
		t.Fatalf("overflow write = %v, want errRefcountFull", err)
	}
}

func TestRefcountShareExtentCrossAG(t *testing.T) {
	fs := formatReflink(t, 2)
	defer fs.Close()
	// An extent that runs off the end of AG 0 must be rejected.
	bad := fs.sb.agAbsBlock(0, fs.sb.agBlocks-2)
	if err := refcountShareExtent(fs.f, fs.partOffset, fs.sb, bad, 8); err == nil {
		t.Fatal("cross-AG share: want error")
	}
	if err := refcountFreeExtent(fs.f, fs.partOffset, fs.sb, bad, 8); err == nil {
		t.Fatal("cross-AG free: want error")
	}
}

func equalRecs(a, b []refcRec) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
