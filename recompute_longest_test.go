package filesystem_xfs

import (
	"io"
	"testing"
)

// TestAgfRecomputeLongest checks that agfRecomputeLongest returns the true
// maximum free-extent count across the cnt/bno B-tree, regardless of record
// sort order, across single-leaf, sibling-chained, and multi-level trees.
func TestAgfRecomputeLongest(t *testing.T) {
	sb := allocTestSB()

	t.Run("single leaf takes the max count", func(t *testing.T) {
		restoreAllocHooks(t)
		// Records intentionally unsorted by count; max is 42.
		leaf := makeAllocLeaf(sb, 0xFFFFFFFF,
			allocRec{start: 10, count: 5},
			allocRec{start: 30, count: 42},
			allocRec{start: 80, count: 7},
		)
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, rel uint32) ([]byte, error) {
			if rel != 3 {
				t.Fatalf("unexpected block read %d", rel)
			}
			return leaf, nil
		}
		got, err := agfRecomputeLongest(newMemRW(0), 0, sb, 0, 3, 1)
		if err != nil || got != 42 {
			t.Fatalf("recompute = (%d, %v), want (42, nil)", got, err)
		}
	})

	t.Run("walks right siblings", func(t *testing.T) {
		restoreAllocHooks(t)
		// leaf 3 -> leaf 4 -> end; the largest extent (99) lives in the sibling.
		leaf3 := makeAllocLeaf(sb, 4, allocRec{start: 10, count: 5})
		leaf4 := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 50, count: 99}, allocRec{start: 70, count: 3})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, rel uint32) ([]byte, error) {
			switch rel {
			case 3:
				return leaf3, nil
			case 4:
				return leaf4, nil
			}
			t.Fatalf("unexpected block read %d", rel)
			return nil, nil
		}
		got, err := agfRecomputeLongest(newMemRW(0), 0, sb, 0, 3, 1)
		if err != nil || got != 99 {
			t.Fatalf("recompute = (%d, %v), want (99, nil)", got, err)
		}
	})

	t.Run("descends internal node to leftmost leaf", func(t *testing.T) {
		restoreAllocHooks(t)
		// root (internal, block 2) -> leftmost child 3 -> sibling 4.
		root := makeAllocInternal(sb, []allocRec{{start: 0, count: 5}, {start: 50, count: 99}}, []uint32{3, 5})
		leaf3 := makeAllocLeaf(sb, 4, allocRec{start: 10, count: 5})
		leaf4 := makeAllocLeaf(sb, 0xFFFFFFFF, allocRec{start: 50, count: 99})
		allocReadAGBlock = func(_ io.ReaderAt, _ int64, _ *superblock, _ uint32, rel uint32) ([]byte, error) {
			switch rel {
			case 2:
				return root, nil
			case 3:
				return leaf3, nil
			case 4:
				return leaf4, nil
			}
			t.Fatalf("unexpected block read %d", rel)
			return nil, nil
		}
		got, err := agfRecomputeLongest(newMemRW(0), 0, sb, 0, 2, 2)
		if err != nil || got != 99 {
			t.Fatalf("recompute = (%d, %v), want (99, nil)", got, err)
		}
	})

	t.Run("propagates read errors", func(t *testing.T) {
		restoreAllocHooks(t)
		allocReadAGBlock = func(io.ReaderAt, int64, *superblock, uint32, uint32) ([]byte, error) {
			return nil, errBoom
		}
		if _, err := agfRecomputeLongest(newMemRW(0), 0, sb, 0, 3, 1); err == nil {
			t.Fatal("expected read error")
		}
	})
	_ = io.EOF
}
