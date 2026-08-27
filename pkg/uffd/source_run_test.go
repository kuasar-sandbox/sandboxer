package uffd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/codec"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func TestZeroSourceRunContract(t *testing.T) {
	run, err := (ZeroSource{}).RunAt(123, 17)
	if err != nil {
		t.Fatal(err)
	}
	if run.Offset() != 123 || run.End() != 140 || run.Kind() != sparse.Zero {
		t.Fatalf("run = [%d,%d) %v", run.Offset(), run.End(), run.Kind())
	}
	buf := bytes.Repeat([]byte{0xff}, 8)
	if n, err := run.ReadAt(context.Background(), buf, 4); n != len(buf) || err != nil {
		t.Fatalf("ReadAt = (%d,%v)", n, err)
	}
	if !bytes.Equal(buf, make([]byte, len(buf))) {
		t.Fatal("Zero Run did not clear destination")
	}
	if _, err := run.ReadAt(context.Background(), make([]byte, 8), 10); err == nil {
		t.Fatal("out-of-range Zero Run read succeeded")
	}
	if _, err := (ZeroSource{}).RunAt(0, 0); err == nil {
		t.Fatal("zero limit succeeded")
	}
	if _, err := (ZeroSource{}).RunAt(math.MaxUint64-1, 2); err == nil {
		t.Fatal("overflowing limit succeeded")
	}
}

func TestStreamSnapshotSourcePreservesChunkRunWithoutPayloadIO(t *testing.T) {
	plain := bytes.Repeat([]byte{0x5a}, 3*PageSize)
	m := &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(plain)),
		Entries: []codec.ChunkEntry{{
			Offset:         0,
			Size:           uint32(len(plain)),
			CiphertextHash: sha256.Sum256(plain),
		}},
	}
	stream, getter := openSnapshotManifest(t, m, map[store.ContentKey][]byte{sha256.Sum256(plain): plain})
	source, err := NewStreamSnapshotSource(stream, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.RunAt(PageSize, 2*PageSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := run.(fetch.ChunkRun); !ok {
		t.Fatalf("Data Run type %T lost fetch.ChunkRun", run)
	}
	if run.Offset() != PageSize || run.End() != 3*PageSize || run.Kind() != sparse.Data {
		t.Fatalf("run = [%d,%d) %v", run.Offset(), run.End(), run.Kind())
	}
	if got := getter.chunkCalls.Load(); got != 0 {
		t.Fatalf("RunAt performed %d chunk Get calls", got)
	}
	buf := make([]byte, PageSize)
	if n, err := run.ReadAt(context.Background(), buf, 0); n != len(buf) || err != nil {
		t.Fatalf("Run.ReadAt = (%d,%v)", n, err)
	}
	if got := getter.chunkCalls.Load(); got != 1 {
		t.Fatalf("Run.ReadAt chunk Get calls = %d, want 1", got)
	}
}

func TestSnapshotMemorySectionPreservesManifestChunkRun(t *testing.T) {
	memory := bytes.Repeat([]byte{0x6d}, 3*PageSize)
	tail, err := snapshotfile.BuildZIP(
		[]byte(`{"memory":{"size":12288}}`),
		[]byte(`{"state":"ok"}`),
		[]byte("version: 1\nsandbox_ref: manifest://0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	logical := append(append([]byte(nil), memory...), tail...)
	key := store.ContentKey(sha256.Sum256(logical))
	stream, getter := openSnapshotManifest(t, &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(logical)),
		Entries: []codec.ChunkEntry{{
			Offset:         0,
			Size:           uint32(len(logical)),
			CiphertextHash: key,
		}},
	}, map[store.ContentKey][]byte{key: logical})
	root, err := snapshotfile.Open(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	source, err := NewStreamSnapshotSource(root.Memory, uint64(len(memory)))
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.RunAt(PageSize, 2*PageSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := run.(fetch.ChunkRun); !ok {
		t.Fatalf("Snapshot memory Data Run type %T lost fetch.ChunkRun", run)
	}
	if run.End() > uint64(len(memory)) {
		t.Fatalf("Snapshot memory Run exposed ZIP tail: end=%d memory=%d", run.End(), len(memory))
	}
	if got := getter.chunkCalls.Load(); got == 0 {
		// Opening the strict ZIP necessarily reads carrier data. This assertion
		// only documents that the test exercised the manifest-backed carrier.
		t.Fatal("opening Snapshot did not read its manifest chunk")
	}
}

func TestStreamSnapshotSourceMergesFinalHoleAndZero(t *testing.T) {
	m := &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: 3 * PageSize,
		Holes:     []sparse.Extent{{Offset: 0, Size: PageSize}},
		Entries: []codec.ChunkEntry{
			{Offset: PageSize, Size: PageSize, IsZero: true},
			{Offset: 2 * PageSize, Size: PageSize, IsZero: true},
		},
	}
	stream, getter := openSnapshotManifest(t, m, nil)
	source, err := NewStreamSnapshotSource(stream, m.ImageSize)
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.RunAt(0, m.ImageSize)
	if err != nil {
		t.Fatal(err)
	}
	if run.Kind() != sparse.Zero || run.End() != m.ImageSize {
		t.Fatalf("merged no-read Run = [%d,%d) %v", run.Offset(), run.End(), run.Kind())
	}
	if _, ok := run.(fetch.ChunkRun); ok {
		t.Fatalf("Zero Run type %T implements ChunkRun", run)
	}
	if got := getter.chunkCalls.Load(); got != 0 {
		t.Fatalf("no-read RunAt performed %d chunk Gets", got)
	}
}

func TestStreamSnapshotSourcePageBoundarySafety(t *testing.T) {
	tests := []struct {
		name  string
		data  []byte
		holes []sparse.Extent
		want  []byte
	}{
		{
			name:  "hole to data",
			data:  append(make([]byte, PageSize/2), bytes.Repeat([]byte{0x31}, PageSize+PageSize/2)...),
			holes: []sparse.Extent{{Offset: 0, Size: PageSize / 2}},
		},
		{
			name:  "data to hole",
			data:  append(bytes.Repeat([]byte{0x72}, PageSize/2), make([]byte, PageSize+PageSize/2)...),
			holes: []sparse.Extent{{Offset: PageSize / 2, Size: PageSize + PageSize/2}},
		},
	}
	for i := range tests {
		tt := &tests[i]
		tt.want = append([]byte(nil), tt.data[:PageSize]...)
		t.Run(tt.name, func(t *testing.T) {
			s, err := sparse.NewSource(bytes.NewReader(tt.data), uint64(len(tt.data)), tt.holes)
			if err != nil {
				t.Fatal(err)
			}
			source, err := NewStreamSnapshotSource(testStream{Source: s}, uint64(len(tt.data)))
			if err != nil {
				t.Fatal(err)
			}
			run, err := source.RunAt(0, uint64(len(tt.data)))
			if err != nil {
				t.Fatal(err)
			}
			if run.Kind() != sparse.Data || run.End() != PageSize {
				t.Fatalf("straddling Run = [%d,%d) %v, want one Data page", run.Offset(), run.End(), run.Kind())
			}
			if _, ok := run.(fetch.ChunkRun); ok {
				t.Fatalf("composite page Run type %T implements ChunkRun", run)
			}
			buf := make([]byte, PageSize)
			if n, err := run.ReadAt(context.Background(), buf, 0); n != len(buf) || err != nil {
				t.Fatalf("ReadAt = (%d,%v)", n, err)
			}
			if !bytes.Equal(buf, tt.want) {
				t.Fatal("straddling page data mismatch")
			}
		})
	}
}

func TestStreamSnapshotSourceLayeredCapability(t *testing.T) {
	plain := bytes.Repeat([]byte{0x44}, 2*PageSize)
	m := &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(plain)),
		Entries: []codec.ChunkEntry{{
			Offset:         0,
			Size:           uint32(len(plain)),
			CiphertextHash: sha256.Sum256(plain),
		}},
	}
	lower, _ := openSnapshotManifest(t, m, map[store.ContentKey][]byte{sha256.Sum256(plain): plain})
	holeSource, err := sparse.NewSource(bytes.NewReader(make([]byte, len(plain))), uint64(len(plain)), []sparse.Extent{{Offset: 0, Size: uint64(len(plain))}})
	if err != nil {
		t.Fatal(err)
	}
	outerHole, err := sparse.NewSource(bytes.NewReader(make([]byte, len(plain))), uint64(len(plain)), []sparse.Extent{{Offset: 0, Size: uint64(len(plain))}})
	if err != nil {
		t.Fatal(err)
	}
	nested := fetch.NewLayered(testStream{Source: outerHole}, fetch.NewLayered(testStream{Source: holeSource}, lower))
	source, err := NewStreamSnapshotSource(nested, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.RunAt(0, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := run.(fetch.ChunkRun); !ok {
		t.Fatalf("layered lower manifest Run type %T lost ChunkRun", run)
	}

	upperData, err := sparse.NewSource(bytes.NewReader(plain), uint64(len(plain)), nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err = NewStreamSnapshotSource(fetch.NewLayered(testStream{Source: upperData}, lower), uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	run, err = source.RunAt(0, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := run.(fetch.ChunkRun); ok {
		t.Fatalf("ordinary upper Data Run type %T unexpectedly implements ChunkRun", run)
	}

	zeroTop, _ := openSnapshotManifest(t, &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(plain)),
		Entries:   []codec.ChunkEntry{{Offset: 0, Size: uint32(len(plain)), IsZero: true}},
	}, nil)
	source, err = NewStreamSnapshotSource(fetch.NewLayered(zeroTop, lower), uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	run, err = source.RunAt(0, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	if run.Kind() != sparse.Zero {
		t.Fatalf("upper Zero resolved as %v", run.Kind())
	}
	if _, ok := run.(fetch.ChunkRun); ok {
		t.Fatalf("upper Zero Run type %T implements ChunkRun", run)
	}
}

func TestStreamSnapshotSourceLayeredVisibilityClipsChunkRun(t *testing.T) {
	plain := bytes.Repeat([]byte{0x66}, 3*PageSize)
	m := &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(plain)),
		Entries: []codec.ChunkEntry{{
			Offset:         0,
			Size:           uint32(len(plain)),
			CiphertextHash: sha256.Sum256(plain),
		}},
	}
	lower, _ := openSnapshotManifest(t, m, map[store.ContentKey][]byte{sha256.Sum256(plain): plain})
	upperBytes := append(make([]byte, PageSize), bytes.Repeat([]byte{0x99}, 2*PageSize)...)
	upper, err := sparse.NewSource(bytes.NewReader(upperBytes), uint64(len(upperBytes)), []sparse.Extent{{Offset: 0, Size: PageSize}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewStreamSnapshotSource(fetch.NewLayered(testStream{Source: upper}, lower), uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.RunAt(0, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := run.(fetch.ChunkRun); !ok {
		t.Fatalf("clipped lower Run type %T lost ChunkRun", run)
	}
	if run.End() != PageSize {
		t.Fatalf("lower chunk reappeared past upper boundary: end=%d", run.End())
	}
}

func TestStreamSnapshotSourcePlaintextAndEncryptedTar(t *testing.T) {
	logical := bytes.Repeat([]byte{0x83}, 4*PageSize)
	clear(logical[2*PageSize : 3*PageSize])
	holes := []sparse.Extent{{Offset: 2 * PageSize, Size: PageSize}}
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			source, err := sparse.NewSource(bytes.NewReader(logical), uint64(len(logical)), holes)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "memory.snapshot")
			out, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			var writeOptions []tarstream.WriteOption
			var readOptions []tarstream.ReadOption
			if encrypted {
				codec, err := manifestcrypto.NewTarStreamCodec([32]byte{1, 3, 5, 7})
				if err != nil {
					t.Fatal(err)
				}
				writeOptions = append(writeOptions, tarstream.WithCodec(codec, false))
				readOptions = append(readOptions, tarstream.WithCodec(codec, true))
			}
			scheme, digest, err := tarstream.WriteTo(context.Background(), out, "memory", source, writeOptions...)
			if err != nil {
				t.Fatal(err)
			}
			if err := out.Close(); err != nil {
				t.Fatal(err)
			}
			readOptions = append(readOptions, tarstream.WithExpectedDigest(scheme, digest))
			stream, err := fetch.OpenTarStream(path, readOptions...)
			if err != nil {
				t.Fatal(err)
			}
			upper, err := sparse.NewSource(bytes.NewReader(make([]byte, len(logical))), uint64(len(logical)), []sparse.Extent{{Offset: 0, Size: uint64(len(logical))}})
			if err != nil {
				t.Fatal(err)
			}
			layered := fetch.NewLayered(testStream{Source: upper}, stream)
			t.Cleanup(func() { _ = layered.Close() })
			snapshot, err := NewStreamSnapshotSource(layered, uint64(len(logical)))
			if err != nil {
				t.Fatal(err)
			}

			run, err := snapshot.RunAt(0, uint64(len(logical)))
			if err != nil {
				t.Fatal(err)
			}
			if run.Kind() != sparse.Data {
				t.Fatalf("first Run kind = %v", run.Kind())
			}
			if _, ok := run.(fetch.ChunkRun); ok {
				t.Fatalf("tar Run type %T implements ChunkRun", run)
			}
			buf := make([]byte, PageSize)
			if n, err := run.ReadAt(context.Background(), buf, 0); n != len(buf) || err != nil {
				t.Fatalf("Run.ReadAt = (%d,%v)", n, err)
			}
			if !bytes.Equal(buf, logical[:PageSize]) {
				t.Fatal("tar Data Run mismatch")
			}

			run, err = snapshot.RunAt(2*PageSize, 2*PageSize)
			if err != nil {
				t.Fatal(err)
			}
			if run.Kind() != sparse.Zero || run.End() != 3*PageSize {
				t.Fatalf("tar hole Run = [%d,%d) %v", run.Offset(), run.End(), run.Kind())
			}
		})
	}
}

func TestStreamSnapshotSourceBounds(t *testing.T) {
	data := make([]byte, 2*PageSize)
	s, err := sparse.NewSource(bytes.NewReader(data), uint64(len(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewStreamSnapshotSource(testStream{Source: s}, PageSize+17)
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.RunAt(PageSize, math.MaxUint64)
	if err != nil {
		t.Fatal(err)
	}
	if run.End() != PageSize+17 {
		t.Fatalf("Run end = %d, want RAM end %d", run.End(), PageSize+17)
	}
	if _, err := source.RunAt(PageSize+17, 1); !errors.Is(err, io.EOF) {
		t.Fatalf("RunAt RAM end error = %v, want io.EOF", err)
	}
}

type snapshotManifestGetter struct {
	manifestKey  store.ContentKey
	manifestData []byte
	chunks       map[store.ContentKey][]byte
	chunkCalls   atomic.Uint64
	reuseChunk   bool
}

func (g *snapshotManifestGetter) Get(_ context.Context, partition store.Partition, key store.ContentKey) (cache.CacheResult, cache.Blob, error) {
	switch partition {
	case store.PartitionManifest:
		if key != g.manifestKey {
			return cache.CacheMiss, nil, nil
		}
		return cache.CacheHit, cache.NewMemBlob(append([]byte(nil), g.manifestData...)), nil
	case store.PartitionChunk:
		chunk, ok := g.chunks[key]
		if !ok {
			return cache.CacheMiss, nil, nil
		}
		g.chunkCalls.Add(1)
		if !g.reuseChunk {
			chunk = append([]byte(nil), chunk...)
		}
		return cache.CacheHit, cache.NewMemBlob(chunk), nil
	default:
		return cache.CacheMiss, nil, errors.New("unexpected partition")
	}
}

type snapshotTestDecryptor struct{ keyBytes int }

func (snapshotTestDecryptor) DecryptChunkTo(_ context.Context, _ [32]byte, ciphertext, dst []byte) error {
	if len(ciphertext) != len(dst) {
		return errors.New("snapshot test decryptor: ciphertext and destination sizes differ")
	}
	copy(dst, ciphertext)
	return nil
}

func (d snapshotTestDecryptor) UnsealKeyTable(_ [32]byte, _, _ []byte) ([]byte, error) {
	return make([]byte, d.keyBytes), nil
}

var _ manifestcrypto.Decryptor = snapshotTestDecryptor{}

type testTB interface {
	Helper()
	Fatal(args ...any)
	Cleanup(func())
}

func openSnapshotManifest(t testTB, m *codec.Manifest, chunks map[store.ContentKey][]byte) (fetch.Stream, *snapshotManifestGetter) {
	t.Helper()
	manifest := *m
	manifest.Entries = append([]codec.ChunkEntry(nil), m.Entries...)
	for _, entry := range manifest.Entries {
		if entry.Size > manifest.MaxChunkSize {
			manifest.MaxChunkSize = entry.Size
		}
	}
	data, err := codec.Marshal(&manifest, []byte("sealed-test-keys"))
	if err != nil {
		t.Fatal(err)
	}
	manifestKey := store.ContentKey(sha256.Sum256(data))
	nonZero := 0
	for _, entry := range manifest.Entries {
		if !entry.IsZero {
			nonZero++
		}
	}
	getter := &snapshotManifestGetter{manifestKey: manifestKey, manifestData: data, chunks: chunks}
	stream, err := fetch.NewFetcher([32]byte{}, getter, snapshotTestDecryptor{keyBytes: nonZero * 32}).OpenManifest(context.Background(), manifestKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	return stream, getter
}
