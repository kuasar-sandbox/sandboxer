package uffd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/codec"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

// passthroughDecryptor returns the ciphertext bytes verbatim.
type passthroughDecryptor struct{}

func (passthroughDecryptor) Encrypt(_ [32]byte, p []byte) ([]byte, [32]byte, [32]byte) {
	return p, [32]byte{}, [32]byte{}
}
func (passthroughDecryptor) Decrypt(_ [32]byte, c []byte) ([]byte, error)        { return c, nil }
func (passthroughDecryptor) DecryptInPlace(_ [32]byte, b []byte) ([]byte, error) { return b, nil }

// buildPageStream makes a one-page-per-chunk manifest Stream over numPages
// pages. fills[i] != 0 → page i is a data chunk filled with fills[i]; fills[i]
// == 0 → page i is a declared hole.
func buildPageStream(t *testing.T, numPages int, fills []byte) fetch.Stream {
	t.Helper()
	ps := int(PageSize)
	m := &codec.Manifest{Version: codec.Version1, ImageSize: uint64(numPages * ps)}
	g := mapGetter{}
	for i := 0; i < numPages; i++ {
		off := uint64(i * ps)
		if fills[i] == 0 {
			m.Holes = append(m.Holes, sparse.Extent{Offset: off, Size: uint64(ps)})
			continue
		}
		blob := bytes.Repeat([]byte{fills[i]}, ps)
		key := store.ContentKey(sha256.Sum256(blob))
		g[key] = blob
		m.Entries = append(m.Entries, codec.ChunkEntry{
			Offset: off, Size: uint32(ps), CiphertextHash: key,
		})
	}
	return fetch.NewStream(m, make([][32]byte, len(m.Entries)), g, passthroughDecryptor{})
}

// drainSource walks src page-by-page exactly as the uffd handler does:
// PageSize-aligned ReadAt, zero runs leave the destination zero, data runs are
// copied. Returns the reconstructed image.
func drainSource(t *testing.T, src SnapshotReader, size int) []byte {
	t.Helper()
	out := make([]byte, size)
	off := 0
	for off < size {
		buf := make([]byte, size-off)
		n, zero, err := src.ReadAt(buf, uint64(off))
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadAt @ %d: %v", off, err)
		}
		if n == 0 {
			t.Fatalf("ReadAt @ %d made no progress", off)
		}
		if !zero {
			copy(out[off:off+n], buf[:n])
		}
		off += n
	}
	return out
}

// mapGetter returns a chunk's full bytes by content key.
type mapGetter map[store.ContentKey][]byte

func (g mapGetter) Get(_ context.Context, _ store.Partition, k store.ContentKey) (cache.CacheResult, cache.Blob, error) {
	if d, ok := g[k]; ok {
		return cache.CacheHit, cache.NewMemBlob(d), nil
	}
	return cache.CacheMiss, nil, nil
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
	m := &codec.Manifest{Version: codec.Version1, ImageSize: uint64(numPages * ps)}
	g := mapGetter{}
	for _, e := range exts {
		off := uint64(e.startPage * ps)
		sz := e.numPages * ps
		if e.fill == 0 {
			m.Holes = append(m.Holes, sparse.Extent{Offset: off, Size: uint64(sz)})
			continue
		}
		blob := bytes.Repeat([]byte{e.fill}, sz)
		k := store.ContentKey(sha256.Sum256(blob))
		g[k] = blob
		m.Entries = append(m.Entries, codec.ChunkEntry{Offset: off, Size: uint32(sz), CiphertextHash: k})
	}
	return fetch.NewStream(m, make([][32]byte, len(m.Entries)), g, passthroughDecryptor{})
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

	src, err := NewStreamSnapshotSource(context.Background(), fetch.NewLayered(top, base), uint64(n)*PageSize)
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

	src, err := NewStreamSnapshotSource(context.Background(), fetch.NewLayered(top, base), uint64(n)*PageSize)
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
