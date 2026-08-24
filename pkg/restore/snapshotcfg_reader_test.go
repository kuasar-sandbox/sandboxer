package restore

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

const testSnapshotCfg = "resources:\n  capacity:\n    cpu: 2\n    memory: 8MiB\nmetadata:\n  test: value\nboot: {}\n"

func TestSnapshotCfgReaderLocalFormsAndIdentity(t *testing.T) {
	zipBody := testSnapshotZIP(t, testSnapshotCfg)
	dir, ref, path := writeTestSnapshotBundle(t, zipBody, nil, false)

	t.Run("raw absolute", func(t *testing.T) {
		reader := newTestProcessReader(t, nil)
		document, err := reader.Read(context.Background(), path, SnapshotCfgReadOptions{})
		assertTestSnapshotDocument(t, document, err)
	})

	t.Run("raw relative with explicit directory", func(t *testing.T) {
		reader := newTestProcessReader(t, nil)
		document, err := reader.Read(context.Background(), filepath.Base(path), SnapshotCfgReadOptions{RelativeDir: dir})
		assertTestSnapshotDocument(t, document, err)
	})

	t.Run("portable sha256 file relative", func(t *testing.T) {
		if !strings.Contains(ref, "@sha256:") {
			t.Fatalf("plain ref = %q, want sha256 identity", ref)
		}
		reader := newTestProcessReader(t, nil)
		document, err := reader.Read(context.Background(), ref, SnapshotCfgReadOptions{RelativeDir: dir})
		assertTestSnapshotDocument(t, document, err)
	})

	t.Run("located root uses path map", func(t *testing.T) {
		parsed, err := manifest.ParseRef(ref)
		if err != nil {
			t.Fatal(err)
		}
		parsed.Location = "shared"
		reader := newTestProcessReader(t, nil)
		document, err := reader.Read(context.Background(), parsed.String(), SnapshotCfgReadOptions{
			RefLocations: config.RefLocations{"shared": dir},
			RelativeDir:  "/must/not/be/used",
		})
		assertTestSnapshotDocument(t, document, err)
	})
}

func TestSnapshotCfgReaderHMACAndWrongKey(t *testing.T) {
	key := [32]byte{0x41, 0x42, 0x43}
	codec, err := manifestcrypto.NewTarStreamCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	dir, ref, _ := writeTestSnapshotBundle(t, testSnapshotZIP(t, testSnapshotCfg), codec, true)
	if !strings.Contains(ref, "@hmac:") {
		t.Fatalf("encrypted ref = %q, want hmac identity", ref)
	}

	t.Setenv("MANIFEST_KEY", hex.EncodeToString(key[:]))
	reader := newTestProcessReader(t, &config.ManifestConfig{Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalRequired}})
	document, err := reader.Read(context.Background(), ref, SnapshotCfgReadOptions{RelativeDir: dir})
	assertTestSnapshotDocument(t, document, err)

	wrong := [32]byte{0xff, 0xee}
	t.Setenv("MANIFEST_KEY", hex.EncodeToString(wrong[:]))
	wrongReader := newTestProcessReader(t, &config.ManifestConfig{Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalRequired}})
	if _, err := wrongReader.Read(context.Background(), ref, SnapshotCfgReadOptions{RelativeDir: dir}); err == nil {
		t.Fatal("encrypted snapshot accepted the wrong MANIFEST_KEY")
	} else if strings.Contains(err.Error(), hex.EncodeToString(key[:])) || strings.Contains(err.Error(), hex.EncodeToString(wrong[:])) {
		t.Fatalf("error exposed key material: %v", err)
	}
}

func TestSnapshotCfgReaderManifestAndCloseLifecycle(t *testing.T) {
	_, _, path := writeTestSnapshotBundle(t, testSnapshotZIP(t, testSnapshotCfg), nil, false)
	fetcher := &testSnapshotFetcher{path: path}
	storage := &testSnapshotStorage{fetcher: fetcher}
	reader := newSnapshotCfgReader(storage)
	root := "manifest://" + strings.Repeat("ab", 32)
	document, err := reader.Read(context.Background(), root, SnapshotCfgReadOptions{})
	assertTestSnapshotDocument(t, document, err)
	if fetcher.opens != 1 || fetcher.streamCloses != 1 {
		t.Fatalf("manifest opens=%d stream closes=%d, want 1/1", fetcher.opens, fetcher.streamCloses)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if storage.closes != 1 {
		t.Fatalf("storage closes=%d, want 1", storage.closes)
	}
}

func TestSnapshotCfgReaderManifestBundleBySymlinkAndSelector(t *testing.T) {
	dir := t.TempDir()
	key := [32]byte{0x61, 0x62, 0x63}
	cfg := &config.ManifestConfig{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	sink, err := snapshot.NewBundleSink(context.Background(), dir, "reader-bundle", cfg,
		func() ([32]byte, error) { return key, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootRef, path, err := sink.AbsorbBundle(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x19}, 8192)), nil,
		bytes.NewReader(testSnapshotZIP(t, testSnapshotCfg)))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MANIFEST_KEY", hex.EncodeToString(key[:]))
	reader := newTestProcessReader(t, cfg)

	document, err := reader.Read(context.Background(), filepath.Join(dir, "reader-bundle.snapshot"), SnapshotCfgReadOptions{})
	assertTestSnapshotDocument(t, document, err)
	rootKey, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	explicit := "file://" + path + "@manifest:" + manifest.HexKey(rootKey)
	document, err = reader.Read(context.Background(), explicit, SnapshotCfgReadOptions{})
	assertTestSnapshotDocument(t, document, err)
}

func TestSnapshotCfgReaderRejectsOversizedExplicitFileBundle(t *testing.T) {
	stream := &oversizedSnapshotStream{}
	storage := &oversizedSnapshotStorage{stream: stream}
	reader := newSnapshotCfgReader(storage)
	root := "file://root.bundle@manifest:" + strings.Repeat("ab", 32)
	if _, err := reader.Read(context.Background(), root, SnapshotCfgReadOptions{}); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("Read error = %v, want oversized Bundle rejection", err)
	}
	if stream.closes != 1 {
		t.Fatalf("stream closes = %d, want 1", stream.closes)
	}
}

func TestSnapshotCfgReaderManifestCryptoAndWrongKey(t *testing.T) {
	customerKey := [32]byte{0x10, 0x20, 0x30}
	cryptoCfg := manifestcrypto.Config{Chunk: "aes", Manifest: "aes"}
	encryptor, decryptor, err := manifestcrypto.New(cryptoCfg)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := chunker.New(chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}})
	if err != nil {
		t.Fatal(err)
	}
	storage := newMemoryManifestStore()
	payload := append(bytes.Repeat([]byte{0x37}, 4096), testSnapshotZIP(t, testSnapshotCfg)...)
	ingester := ingest.NewIngester(
		func() ([32]byte, error) { return customerKey, nil },
		nil,
		storage,
		chunks,
		encryptor,
	)
	result, err := ingester.Ingest(context.Background(), sparse.Dense(bytes.NewReader(payload), uint64(len(payload))), ingest.IngestOption{})
	if err != nil {
		t.Fatal(err)
	}
	root := "manifest://" + hex.EncodeToString(result.ManifestKey[:])

	reader := newSnapshotCfgReader(&testSnapshotStorage{fetcher: fetch.NewFetcher(customerKey, storage, decryptor)})
	document, err := reader.Read(context.Background(), root, SnapshotCfgReadOptions{})
	assertTestSnapshotDocument(t, document, err)

	wrongKey := [32]byte{0xff, 0xee}
	wrongReader := newSnapshotCfgReader(&testSnapshotStorage{fetcher: fetch.NewFetcher(wrongKey, storage, decryptor)})
	if _, err := wrongReader.Read(context.Background(), root, SnapshotCfgReadOptions{}); err == nil {
		t.Fatal("manifest snapshot accepted the wrong customer key")
	} else if strings.Contains(err.Error(), hex.EncodeToString(customerKey[:])) || strings.Contains(err.Error(), hex.EncodeToString(wrongKey[:])) {
		t.Fatalf("error exposed key material: %v", err)
	}
}

func TestSnapshotCfgReaderClosesStreamOnParseFailure(t *testing.T) {
	_, _, path := writeTestSnapshotBundle(t, testSnapshotZIP(t, "resources: ["), nil, false)
	fetcher := &testSnapshotFetcher{path: path}
	reader := newSnapshotCfgReader(&testSnapshotStorage{fetcher: fetcher})
	if _, err := reader.Read(context.Background(), "manifest://"+strings.Repeat("cd", 32), SnapshotCfgReadOptions{}); err == nil {
		t.Fatal("malformed snapshot.cfg was accepted")
	}
	if fetcher.streamCloses != 1 {
		t.Fatalf("stream closes=%d, want 1", fetcher.streamCloses)
	}
}

func TestSnapshotCfgReaderPropagatesCloseErrors(t *testing.T) {
	_, _, path := writeTestSnapshotBundle(t, testSnapshotZIP(t, testSnapshotCfg), nil, false)
	t.Run("stream", func(t *testing.T) {
		closeErr := errors.New("stream close failed")
		fetcher := &testSnapshotFetcher{path: path, streamCloseErr: closeErr}
		reader := newSnapshotCfgReader(&testSnapshotStorage{fetcher: fetcher})
		_, err := reader.Read(context.Background(), "manifest://"+strings.Repeat("11", 32), SnapshotCfgReadOptions{})
		if !errors.Is(err, closeErr) {
			t.Fatalf("Read error = %v, want stream close error", err)
		}
		if fetcher.streamCloses != 1 {
			t.Fatalf("stream closes=%d, want 1", fetcher.streamCloses)
		}
	})
	t.Run("storage", func(t *testing.T) {
		closeErr := errors.New("storage close failed")
		reader := newSnapshotCfgReader(&testSnapshotStorage{err: closeErr})
		if err := reader.Close(); !errors.Is(err, closeErr) {
			t.Fatalf("Close error = %v, want storage close error", err)
		}
	})
}

func TestSnapshotCfgReaderContextCancellationClosesStream(t *testing.T) {
	_, _, path := writeTestSnapshotBundle(t, testSnapshotZIP(t, testSnapshotCfg), nil, false)
	ctx, cancel := context.WithCancel(context.Background())
	fetcher := &testSnapshotFetcher{path: path, wrap: func(stream fetch.Stream) fetch.Stream {
		return &cancelOnReadStream{Stream: stream, cancel: cancel}
	}}
	reader := newSnapshotCfgReader(&testSnapshotStorage{fetcher: fetcher})
	_, err := reader.Read(ctx, "manifest://"+strings.Repeat("ef", 32), SnapshotCfgReadOptions{})
	if err == nil {
		t.Fatal("read succeeded after its context was canceled")
	}
	if fetcher.streamCloses != 1 {
		t.Fatalf("stream closes=%d, want 1", fetcher.streamCloses)
	}
}

func TestReadSnapshotCfgEntryValidation(t *testing.T) {
	t.Run("missing exact entry", func(t *testing.T) {
		body := rawZIP(t, []zipEntry{{name: "nested/snapshot.cfg", body: []byte(testSnapshotCfg)}})
		if _, err := readSnapshotCfgEntry(bytes.NewReader(body), int64(len(body))); err == nil || !strings.Contains(err.Error(), "no snapshot.cfg") {
			t.Fatalf("error = %v, want missing snapshot.cfg", err)
		}
	})

	t.Run("duplicate exact entry", func(t *testing.T) {
		body := rawZIP(t, []zipEntry{
			{name: "snapshot.cfg", body: []byte(testSnapshotCfg)},
			{name: "snapshot.cfg", body: []byte(testSnapshotCfg)},
		})
		if _, err := readSnapshotCfgEntry(bytes.NewReader(body), int64(len(body))); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("error = %v, want duplicate snapshot.cfg", err)
		}
	})

	t.Run("declared size exceeds limit", func(t *testing.T) {
		body := rawStoredZIP(t, []byte("x"), MaxSnapshotCfgSize+1)
		if _, err := readSnapshotCfgEntry(bytes.NewReader(body), int64(len(body))); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("error = %v, want declared size limit", err)
		}
	})

	t.Run("forged actual decompressed size is rejected", func(t *testing.T) {
		body := rawDeflateZIP(t, bytes.Repeat([]byte{'x'}, MaxSnapshotCfgSize+1), 1)
		if _, err := readSnapshotCfgEntry(bytes.NewReader(body), int64(len(body))); err == nil {
			t.Fatal("forged uncompressed size was accepted")
		}
	})

	t.Run("bad zip", func(t *testing.T) {
		body := []byte("not a zip")
		if _, err := readSnapshotCfgEntry(bytes.NewReader(body), int64(len(body))); err == nil || !strings.Contains(err.Error(), "read snapshot zip") {
			t.Fatalf("error = %v, want bad zip", err)
		}
	})

	t.Run("truncated zip", func(t *testing.T) {
		body := rawZIP(t, []zipEntry{{name: "snapshot.cfg", body: []byte(testSnapshotCfg)}})
		body = body[:len(body)-12]
		if _, err := readSnapshotCfgEntry(bytes.NewReader(body), int64(len(body))); err == nil {
			t.Fatal("truncated zip was accepted")
		}
	})
}

func newTestProcessReader(t *testing.T, cfg *config.ManifestConfig) *SnapshotCfgReader {
	t.Helper()
	reader, err := NewSnapshotCfgReader(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close reader: %v", err)
		}
	})
	return reader
}

func assertTestSnapshotDocument(t *testing.T, document *SnapshotCfgDocument, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if document == nil || document.Config == nil {
		t.Fatal("reader returned no canonical config")
	}
	if document.Config.Resources.Capacity.CPU != 2 || document.Config.Resources.Capacity.Memory != "8MiB" {
		t.Fatalf("capacity = %#v", document.Config.Resources.Capacity)
	}
	if string(document.Raw) != testSnapshotCfg {
		t.Fatalf("raw snapshot.cfg = %q", document.Raw)
	}
}

func testSnapshotZIP(t *testing.T, cfg string) []byte {
	t.Helper()
	body, err := snapshot.BuildZIP(map[string][]byte{
		"config.json":  {},
		"snapshot.cfg": []byte(cfg),
		"state.json":   {},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func writeTestSnapshotBundle(t *testing.T, zipBody []byte, codec tarstream.Codec, required bool) (dir, ref, path string) {
	t.Helper()
	dir = t.TempDir()
	ref, path, err := snapshot.NewFileSink(dir, "reader-test", codec, required, nil).AbsorbBundle(
		context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x29}, 4096)), nil, bytes.NewReader(zipBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	return dir, ref, path
}

type testSnapshotStorage struct {
	fetcher fetch.Fetcher
	codec   tarstream.Codec
	require bool
	closes  int
	err     error
}

func (s *testSnapshotStorage) Fetcher() fetch.Fetcher      { return s.fetcher }
func (s *testSnapshotStorage) LocalCodec() tarstream.Codec { return s.codec }
func (s *testSnapshotStorage) LocalRequired() bool         { return s.require }
func (s *testSnapshotStorage) Close() error                { s.closes++; return s.err }

type oversizedSnapshotStorage struct {
	testSnapshotStorage
	stream *oversizedSnapshotStream
}

func (s *oversizedSnapshotStorage) OpenFile(context.Context, string, manifest.Ref) (*artifact.OpenedFile, error) {
	return &artifact.OpenedFile{Stream: s.stream}, nil
}

type oversizedSnapshotStream struct{ closes int }

func (*oversizedSnapshotStream) Size() uint64 { return uint64(math.MaxInt64) + 1 }

func (*oversizedSnapshotStream) RunAt(uint64, uint64) (sparse.Run, error) {
	return nil, errors.New("unexpected RunAt")
}

func (*oversizedSnapshotStream) ReadAt(context.Context, []byte, uint64) (int, error) {
	return 0, errors.New("unexpected ReadAt")
}

func (s *oversizedSnapshotStream) Close() error { s.closes++; return nil }

type testSnapshotFetcher struct {
	path           string
	opens          int
	streamCloses   int
	streamCloseErr error
	wrap           func(fetch.Stream) fetch.Stream
}

func (f *testSnapshotFetcher) OpenManifest(ctx context.Context, _ store.ContentKey) (fetch.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.opens++
	stream, err := fetch.OpenTarStream(f.path)
	if err != nil {
		return nil, err
	}
	tracked := fetch.Stream(&closeTrackingStream{Stream: stream, closes: &f.streamCloses, err: f.streamCloseErr})
	if f.wrap != nil {
		tracked = f.wrap(tracked)
	}
	return tracked, nil
}

type closeTrackingStream struct {
	fetch.Stream
	closes *int
	err    error
}

func (s *closeTrackingStream) Close() error {
	*s.closes++
	return errors.Join(s.Stream.Close(), s.err)
}

type cancelOnReadStream struct {
	fetch.Stream
	cancel context.CancelFunc
	once   sync.Once
}

type memoryManifestStore struct {
	mu      sync.Mutex
	objects map[store.Partition]map[store.ContentKey][]byte
}

func newMemoryManifestStore() *memoryManifestStore {
	return &memoryManifestStore{objects: make(map[store.Partition]map[store.ContentKey][]byte)}
}

func (s *memoryManifestStore) AdmitWrite(context.Context) (store.WriteAdmission, error) {
	return store.WriteAdmission{Generation: "test", Salt: [32]byte{0x51}}, nil
}

func (s *memoryManifestStore) Put(_ context.Context, _ store.WriteAdmission, partition store.Partition, key store.ContentKey, data []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	objects := s.objects[partition]
	if objects == nil {
		objects = make(map[store.ContentKey][]byte)
		s.objects[partition] = objects
	}
	if _, exists := objects[key]; exists {
		return false, nil
	}
	objects[key] = append([]byte(nil), data...)
	return true, nil
}

func (s *memoryManifestStore) Get(_ context.Context, partition store.Partition, key store.ContentKey) (cache.CacheResult, cache.Blob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, exists := s.objects[partition][key]
	if !exists {
		return cache.CacheMiss, nil, nil
	}
	return cache.CacheHit, cache.NewMemBlob(append([]byte(nil), data...)), nil
}

func (s *cancelOnReadStream) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	s.once.Do(s.cancel)
	return s.Stream.ReadAt(ctx, p, off)
}

type zipEntry struct {
	name string
	body []byte
}

func rawZIP(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	for _, entry := range entries {
		part, err := writer.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func rawStoredZIP(t *testing.T, actual []byte, declared uint64) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	header := &zip.FileHeader{
		Name:               "snapshot.cfg",
		Method:             zip.Store,
		CRC32:              crc32.ChecksumIEEE(actual),
		CompressedSize64:   uint64(len(actual)),
		UncompressedSize64: declared,
	}
	part, err := writer.CreateRaw(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(actual); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func rawDeflateZIP(t *testing.T, actual []byte, declared uint64) []byte {
	t.Helper()
	var compressed bytes.Buffer
	deflater, err := flate.NewWriter(&compressed, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deflater.Write(actual); err != nil {
		t.Fatal(err)
	}
	if err := deflater.Close(); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	header := &zip.FileHeader{
		Name:               "snapshot.cfg",
		Method:             zip.Deflate,
		CRC32:              crc32.ChecksumIEEE(actual),
		CompressedSize64:   uint64(compressed.Len()),
		UncompressedSize64: declared,
	}
	part, err := writer.CreateRaw(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(part, &compressed); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

var _ fetch.Fetcher = (*testSnapshotFetcher)(nil)
var _ fetch.Stream = (*closeTrackingStream)(nil)
var _ fetch.Stream = (*cancelOnReadStream)(nil)
var _ ingest.StoreWriter = (*memoryManifestStore)(nil)
var _ cache.Getter = (*memoryManifestStore)(nil)
