package filesystem_xfs

import "testing"

// openTestFS opens the image at path and returns the concrete *xfsFS
// implementation for package-internal tests. It fails the test on error and
// registers a cleanup to Close the filesystem.
func openTestFS(t testing.TB, path string) *xfsFS {
	t.Helper()
	fsIfc, err := Open(path, -1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = fsIfc.Close() })
	return fsIfc.(*xfsFS)
}
