// SPDX-License-Identifier: BSD-3-Clause

package filesystem_xfs

import (
	"errors"
	iofs "io/fs"
	"path/filepath"
	"testing"
)

// ⛔ The error contract from go-filesystems/interface: a path that is not
// there must satisfy errors.Is(err, fs.ErrNotExist).
//
// xfs raises ErrNotFound from its directory lookups, which are 404s, and
// lookupInDir ALSO returns a plain error for an unsupported directory format
// -- an inode claiming a format this driver cannot read, which is a broken
// image, not a missing file. The marking below therefore tests the error
// rather than the call site, so the corrupt case keeps its own shape.
//
// ⚠ DeleteFile is NOT in this list, and that is a finding rather than an
// omission. xfs documents it as idempotent -- deleting a path that is not
// there returns nil -- and so does ext4. The other eleven drivers in the
// family report an error. Both readings are defensible on their own (os.Remove
// answers ENOENT, os.RemoveAll answers nil), but a caller written against the
// interface cannot rely on either while they disagree, and the interface says
// nothing about it. Picking one here would change documented behaviour in two
// drivers on my own authority, so it is reported instead.
func TestMissingPathsSatisfyErrNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fs.img")
	fsys, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()

	for _, tc := range []struct {
		what string
		err  error
	}{
		{"Stat", func() error { _, e := fsys.Stat("/nope.txt"); return e }()},
		{"ReadFile", func() error { _, e := fsys.ReadFile("/nope.txt"); return e }()},
		{"ListDir", func() error { _, e := fsys.ListDir("/nope"); return e }()},
		{"ReadLink", func() error { _, e := fsys.ReadLink("/nope"); return e }()},
		{"Rename", fsys.Rename("/nope.txt", "/other.txt")},
	} {
		if tc.err == nil {
			t.Errorf("%s on a missing path returned no error at all", tc.what)
			continue
		}
		if !errors.Is(tc.err, iofs.ErrNotExist) {
			t.Errorf("%s: errors.Is(err, fs.ErrNotExist) is false for %q", tc.what, tc.err)
		}
	}
}

// TestAnUnreadableDirFormatIsNotA404 guards the other side. An inode whose
// format this driver cannot read is a corrupt or unsupported image; if that
// ever starts satisfying fs.ErrNotExist, every server in this family will
// report it as a missing file and the fault will go unnoticed.
func TestAnUnreadableDirFormatIsNotA404(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fs.img")
	fsys, err := Format(path, xfsTestSize, FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	x, ok := fsys.(*xfsFS)
	if !ok {
		t.Fatalf("Format returned %T, not the concrete filesystem this test drives", fsys)
	}

	root, err := dirReadInode(x.f, x.partOffset, x.sb, x.sb.rootIno)
	if err != nil {
		t.Fatal(err)
	}
	bogus := *root
	bogus.format = 0xEE // no such directory format

	_, err = lookupInDir(x.f, x.partOffset, x.sb, &bogus, "anything")
	if err == nil {
		t.Fatal("an inode claiming an unknown directory format returned no error")
	}
	if errors.Is(err, iofs.ErrNotExist) {
		t.Fatalf("an unreadable directory format now reads as fs.ErrNotExist (%v): "+
			"a server will answer 404 for a corrupt image", err)
	}
}
