package uffd

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

type testStream struct{ sparse.Source }

func (testStream) Close() error { return nil }

// buildPageStream makes a one-page-per-chunk manifest Stream over numPages
// pages. fills[i] != 0 → page i is a data chunk filled with fills[i]; fills[i]
// == 0 → page i is a declared hole.
func buildPageStream(t *testing.T, numPages int, fills []byte) fetch.Stream {
	t.Helper()
	ps := int(PageSize)
	data := make([]byte, numPages*ps)
	var holes []sparse.Extent
	for i := 0; i < numPages; i++ {
		off := uint64(i * ps)
		if fills[i] == 0 {
			holes = append(holes, sparse.Extent{Offset: off, Size: uint64(ps)})
			continue
		}
		copy(data[i*ps:(i+1)*ps], bytes.Repeat([]byte{fills[i]}, ps))
	}
	src, err := sparse.NewSource(bytes.NewReader(data), uint64(len(data)), holes)
	if err != nil {
		t.Fatal(err)
	}
	return testStream{Source: src}
}

// drainSource walks the executable Runs exposed by SnapshotReader.
func drainSource(t *testing.T, src SnapshotReader, size int) []byte {
	t.Helper()
	out := make([]byte, size)
	off := 0
	for off < size {
		run, err := src.RunAt(uint64(off), uint64(size-off))
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("RunAt @ %d: %v", off, err)
		}
		n := int(run.End() - run.Offset())
		if n == 0 {
			t.Fatalf("RunAt @ %d made no progress", off)
		}
		if run.Kind() == sparse.Data {
			if got, readErr := run.ReadAt(context.Background(), out[off:off+n], 0); readErr != nil || got != n {
				t.Fatalf("Run.ReadAt @ %d = (%d, %v), want (%d, nil)", off, got, readErr, n)
			}
		}
		off += n
	}
	return out
}

// extent is one [startPage, startPage+numPages) region; fill==0 → declared hole.
type extent struct {
	startPage, numPages int
	fill                byte
}

// buildExtentStream builds a Stream whose chunks may span multiple pages
// (variable-size, like real CDC), interleaved with declared holes.
func buildExtentStream(t *testing.T, numPages int, exts []extent) fetch.Stream {
	t.Helper()
	ps := int(PageSize)
	data := make([]byte, numPages*ps)
	var holes []sparse.Extent
	for _, e := range exts {
		off := uint64(e.startPage * ps)
		sz := e.numPages * ps
		if e.fill == 0 {
			holes = append(holes, sparse.Extent{Offset: off, Size: uint64(sz)})
			continue
		}
		copy(data[e.startPage*ps:(e.startPage+e.numPages)*ps], bytes.Repeat([]byte{e.fill}, sz))
	}
	src, err := sparse.NewSource(bytes.NewReader(data), uint64(len(data)), holes)
	if err != nil {
		t.Fatal(err)
	}
	return testStream{Source: src}
}

// TestStreamSnapshotSource_LayeredMultiPageChunks exercises the realistic case:
// multi-page (variable-size) chunks, sub-chunk fall-through (a top hole over the
// middle of a larger base chunk), and a merged hole — driven page-by-page AND
// re-driven with a single large buffer (the data-run-extension path).
func TestStreamSnapshotSource_LayeredMultiPageChunks(t *testing.T) {
	const n = 8
	// base: one 7-page data chunk [0,7), hole at page 7.
	base := buildExtentStream(t, n, []extent{{0, 7, 0xB0}, {7, 1, 0}})
	// top: 2-page chunk [0,2), 1-page chunk [4,5); holes at 2,3,5,6,7.
	top := buildExtentStream(t, n, []extent{
		{0, 2, 0xA0}, {2, 2, 0}, {4, 1, 0xA1}, {5, 3, 0},
	})
	// effective: 0,1=top(A0); 2,3=base(B0); 4=top(A1); 5,6=base(B0); 7=merged hole.
	want := []byte{0xA0, 0xA0, 0xB0, 0xB0, 0xA1, 0xB0, 0xB0, 0x00}

	check := func(name string, got []byte) {
		ps := int(PageSize)
		for i := 0; i < n; i++ {
			page := got[i*ps : (i+1)*ps]
			exp := bytes.Repeat([]byte{want[i]}, ps)
			if !bytes.Equal(page, exp) {
				t.Errorf("%s: page %d = %#x want %#x", name, i, page[0], want[i])
			}
		}
	}

	src, err := NewStreamSnapshotSource(fetch.NewLayered(top, base), uint64(n)*PageSize)
	if err != nil {
		t.Fatalf("NewStreamSnapshotSource: %v", err)
	}
	check("multi-page", drainSource(t, src, n*int(PageSize)))
}

// TestStreamSnapshotSource_LayeredManifest reproduces the chained-restore
// memory path: a top manifest with holes overlaid on a base manifest, driven
// page-by-page through StreamSnapshotSource. Each effective page must equal
// top-if-data, else base-if-data, else zero.
func TestStreamSnapshotSource_LayeredManifest(t *testing.T) {
	const n = 8
	// base: all data except page 7 (hole) — page 7 is hole in BOTH → merged.
	base := buildPageStream(t, n, []byte{0xB0, 0xB1, 0xB2, 0xB3, 0xB4, 0xB5, 0xB6, 0x00})
	// top: data on 0,2,4; holes elsewhere (fall through to base).
	top := buildPageStream(t, n, []byte{0xA0, 0x00, 0xA2, 0x00, 0xA4, 0x00, 0x00, 0x00})

	src, err := NewStreamSnapshotSource(fetch.NewLayered(top, base), uint64(n)*PageSize)
	if err != nil {
		t.Fatalf("NewStreamSnapshotSource: %v", err)
	}
	got := drainSource(t, src, n*int(PageSize))

	want := map[int]byte{0: 0xA0, 1: 0xB1, 2: 0xA2, 3: 0xB3, 4: 0xA4, 5: 0xB5, 6: 0xB6, 7: 0x00}
	ps := int(PageSize)
	for i := 0; i < n; i++ {
		page := got[i*ps : (i+1)*ps]
		exp := bytes.Repeat([]byte{want[i]}, ps)
		if !bytes.Equal(page, exp) {
			t.Errorf("page %d: got %#x want %#x (layered fall-through wrong)", i, page[0], want[i])
		}
	}
}
