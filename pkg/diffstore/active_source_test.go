package diffstore

import (
	"context"
	"io"
	"slices"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

func TestBitmapRunEndBoundaries(t *testing.T) {
	patterns := [][]uint64{
		{0, 0, 0},
		{^uint64(0), ^uint64(0), ^uint64(0)},
		{1 << 63, 1, ^uint64(1)},
		{^uint64(1 << 63), ^uint64(1), 1},
		{0xaaaaaaaaaaaaaaaa, 0x5555555555555555, 1},
	}
	for _, bitmap := range patterns {
		// The logical tail is one bit into the third word. Check every bounded
		// range against a per-bit oracle, including starts inside a word.
		for first := uint64(0); first < 129; first++ {
			for limit := first + 1; limit <= 129; limit++ {
				present := (bitmap[first/64] >> (first % 64)) & 1
				want := first + 1
				for want < limit && (bitmap[want/64]>>(want%64))&1 == present {
					want++
				}
				if got := bitmapRunEnd(bitmap, first, limit); got != want {
					t.Fatalf("bitmap=%x range=[%d,%d): end=%d want=%d", bitmap, first, limit, got, want)
				}
			}
		}
	}
}

func TestSnapshotHolesWordBoundaries(t *testing.T) {
	const size = 129 * cowBlockSize
	bitmap := []uint64{1 << 63, 1, ^uint64(1)}
	want := []sparse.Extent{
		{Offset: 0, Size: 63 * cowBlockSize},
		{Offset: 65 * cowBlockSize, Size: 64 * cowBlockSize},
	}
	if got := snapshotHoles(bitmap, size); !slices.Equal(got, want) {
		t.Fatalf("holes=%v want=%v", got, want)
	}
	if got := snapshotHoles(nil, 0); len(got) != 0 {
		t.Fatalf("empty snapshot holes=%v", got)
	}
}

func TestActiveExportFreezesPresence(t *testing.T) {
	const size = 6 * cowBlockSize
	// Only metadata is used: the fixture has no payload file or writeback worker.
	cow := &Disk{
		size: size, bitmap: []uint64{0b010101}, lifeCtx: context.Background(),
		cache: &Cache{}, diff: &diffFile{logicalSize: size, payload: []uint64{0b010001}},
	}
	view, err := cow.Export(context.Background(), ActiveDiff)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	cow.bitmapMu.Lock()
	clear(cow.bitmap)
	cow.bitmapMu.Unlock()
	for page, want := range []sparse.RunKind{sparse.Data, sparse.Hole, sparse.Zero, sparse.Hole, sparse.Data, sparse.Hole} {
		off := uint64(page * cowBlockSize)
		run, err := view.RunAt(off, size-off)
		if err != nil {
			t.Fatal(err)
		}
		if run.Offset() != off || run.End() != off+cowBlockSize || run.Kind() != want {
			t.Fatalf("page=%d run=[%d,%d) kind=%v want=%v", page, run.Offset(), run.End(), run.Kind(), want)
		}
	}
}

func TestActiveSourceRunBounds(t *testing.T) {
	const pages = 3 * maxWritebackPages
	const size = pages * cowBlockSize
	bitmap := make([]uint64, pages/64)
	for i := 1; i < len(bitmap); i++ {
		bitmap[i] = ^uint64(0)
	}
	payload := make([]uint64, len(bitmap))
	payload[1] = 1
	cow := &Disk{
		size: size, bitmap: bitmap, lifeCtx: context.Background(),
		cache: &Cache{}, diff: &diffFile{logicalSize: size, payload: payload},
	}
	view, err := cow.Export(context.Background(), ActiveDiff)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	for _, tc := range []struct {
		off, limit, end uint64
		kind            sparse.RunKind
	}{
		{0, 64*cowBlockSize - 13, 64*cowBlockSize - 13, sparse.Hole},
		{63*cowBlockSize + 17, 2 * cowBlockSize, 64 * cowBlockSize, sparse.Hole},
		{64*cowBlockSize + 17, size, 65 * cowBlockSize, sparse.Data},
		{65*cowBlockSize + 17, size, (65 + maxWritebackPages) * cowBlockSize, sparse.Zero},
		{size - 17, 64, size, sparse.Zero},
		{65*cowBlockSize + 5, 3, 65*cowBlockSize + 8, sparse.Zero},
	} {
		run, err := view.RunAt(tc.off, tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		if run.Offset() != tc.off || run.End() != tc.end || run.Kind() != tc.kind {
			t.Fatalf("off=%d limit=%d: run=[%d,%d) kind=%v want end=%d kind=%v", tc.off, tc.limit, run.Offset(), run.End(), run.Kind(), tc.end, tc.kind)
		}
	}
	if _, err := view.RunAt(size, 1); err != io.EOF {
		t.Fatalf("past end: %v", err)
	}
	if _, err := view.RunAt(0, 0); err == nil {
		t.Fatal("accepted a zero run limit")
	}
}
