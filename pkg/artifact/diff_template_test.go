package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/emptyext4"
	"golang.org/x/sys/unix"
)

type templateObject struct {
	partition store.Partition
	key       store.ContentKey
}
type templateStore struct {
	mu        sync.Mutex
	admission store.WriteAdmission
	objects   map[templateObject][]byte
}

func (s *templateStore) AdmitWrite(context.Context) (store.WriteAdmission, error) {
	return s.admission, nil
}
func (s *templateStore) Put(_ context.Context, _ store.WriteAdmission, p store.Partition, k store.ContentKey, data []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sha256.Sum256(data) != k {
		return false, errors.New("invalid fixture content key")
	}
	key := templateObject{p, k}
	_, exists := s.objects[key]
	s.objects[key] = bytes.Clone(data)
	return !exists, nil
}
func (s *templateStore) Get(_ context.Context, p store.Partition, k store.ContentKey) (cache.CacheResult, cache.Blob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[templateObject{p, k}]
	if !ok {
		return cache.CacheMiss, nil, nil
	}
	return cache.CacheHit, cache.NewMemBlob(bytes.Clone(data)), nil
}

type strictTemplateFetcher struct {
	fetch.Fetcher
	opens int
}

func (f *strictTemplateFetcher) OpenVerifiedManifest(ctx context.Context, key store.ContentKey) (fetch.Stream, error) {
	f.opens++
	return f.Fetcher.OpenManifest(ctx, key)
}

type templateFixture struct {
	dir                           string
	source                        *emptyext4.Source
	storage                       *ProcessStorage
	store                         *templateStore
	strict                        *strictTemplateFetcher
	key                           [32]byte
	root                          store.ContentKey
	plain, encrypted, bundle, raw string
	plainDigest, hmac             string
}

func newTemplateFixture(t *testing.T) *templateFixture {
	t.Helper()
	f := &templateFixture{dir: t.TempDir(), key: sha256.Sum256([]byte("public-test-only/diff-template"))}
	var err error
	f.source, err = emptyext4.New(8 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.source.Close() })
	keyFn := func() ([32]byte, error) { return f.key, nil }
	verify := false // A template must not inherit this ordinary-read policy.
	cfg := &config.ManifestConfig{Crypto: manifestcrypto.Config{Chunk: "aes", Manifest: "aes", Local: "auto", Ref: "auto"}}
	cfg.Manifest.VerifyContent = &verify
	f.storage, err = NewProcessStorageWithCustomerKey(cfg, keyFn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.storage.Close() })
	f.raw = filepath.Join(f.dir, "empty.ext4")
	raw, err := os.Create(f.raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Truncate(int64(f.source.Size())); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 128*1024)
	for offset := uint64(0); offset < f.source.Size(); {
		run, err := f.source.RunAt(offset, f.source.Size()-offset)
		if err != nil {
			t.Fatal(err)
		}
		if run.Kind() == sparse.Data {
			for at := offset; at < run.End(); {
				n := min(uint64(len(buffer)), run.End()-at)
				if _, err := f.source.ReadAt(context.Background(), buffer[:n], at); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.WriteAt(buffer[:n], int64(at)); err != nil {
					t.Fatal(err)
				}
				at += n
			}
		}
		offset = run.End()
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	f.plain, f.encrypted = filepath.Join(f.dir, "empty.tar"), filepath.Join(f.dir, "encrypted.tar")
	for _, encrypted := range []bool{false, true} {
		var carrier bytes.Buffer
		var options []tarstream.WriteOption
		if encrypted {
			options = append(options, tarstream.WithCodec(f.storage.LocalCodec(), true))
		}
		_, digest, err := tarstream.WriteTo(context.Background(), &carrier, "disk", f.source, options...)
		if err != nil {
			t.Fatal(err)
		}
		path := f.plain
		if encrypted {
			path, f.hmac = f.encrypted, digest
		} else {
			f.plainDigest = digest
		}
		if err := os.WriteFile(path, carrier.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	salt, err := store.SaltForGeneration("NONE")
	if err != nil {
		t.Fatal(err)
	}
	f.store = &templateStore{admission: store.WriteAdmission{Generation: "NONE", Salt: salt}, objects: make(map[templateObject][]byte)}
	chunks, err := chunker.New(chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}})
	if err != nil {
		t.Fatal(err)
	}
	enc, dec, err := manifestcrypto.New(cfg.Crypto)
	if err != nil {
		t.Fatal(err)
	}
	ing := ingest.NewIngester(keyFn, nil, f.store, chunks, enc)
	result, err := ing.Ingest(context.Background(), f.source, ingest.IngestOption{})
	if err != nil {
		t.Fatal(err)
	}
	f.root = result.ManifestKey
	// A selected template is allowed to coexist with unrelated Manifest roots.
	if _, err := ing.Ingest(context.Background(), sparse.Dense(strings.NewReader("unrelated"), 9), ingest.IngestOption{}); err != nil {
		t.Fatal(err)
	}
	f.bundle = f.writeBundle(t, "empty.bundle", f.root, false, false, false)
	f.strict = &strictTemplateFetcher{Fetcher: fetch.NewFetcher(f.key, f.store, dec)}
	return f
}

func (f *templateFixture) writeBundle(t *testing.T, name string, root store.ContentKey, forgeRoot, corruptChunk, omitChunks bool) string {
	t.Helper()
	path := filepath.Join(f.dir, name)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := bundle.NewWriter(file, f.store.admission, bundle.WriterOptions{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	for key, data := range f.store.objects {
		if omitChunks && key.partition == store.PartitionChunk {
			continue
		}
		value := bytes.Clone(data)
		if corruptChunk && key.partition == store.PartitionChunk {
			value[len(value)/2] ^= 1
		}
		if forgeRoot && key.partition == store.PartitionManifest && key.key == f.root {
			key.key = root
		}
		if _, err := writer.Put(context.Background(), f.store.admission, key.partition, key.key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Finalize(root); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func compareTemplate(t *testing.T, got, want sparse.Source) {
	t.Helper()
	if got.Size() != want.Size() {
		t.Fatalf("size=%d want=%d", got.Size(), want.Size())
	}
	a, b := make([]byte, 128*1024), make([]byte, 128*1024)
	for off := uint64(0); off < got.Size(); {
		n := min(uint64(len(a)), got.Size()-off)
		if _, err := got.ReadAt(context.Background(), a[:n], off); err != nil {
			t.Fatal(err)
		}
		if _, err := want.ReadAt(context.Background(), b[:n], off); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a[:n], b[:n]) {
			t.Fatalf("logical content differs at %d", off)
		}
		off += n
	}
}

func TestDiffTemplateFiveSourcesReadSameLogicalDisk(t *testing.T) {
	f := newTemplateFixture(t)
	storeView, err := NewProcessStorageWithLocalAccess(f.storage.cfg, f.storage.keyFn, f.storage.LocalCodec(), false, f.strict)
	if err != nil {
		t.Fatal(err)
	}
	defer storeView.Close()
	for _, tc := range []struct {
		name, ref string
		storage   *ProcessStorage
	}{
		{"raw", "file://" + f.raw, nil},
		{"tar", "file://" + f.plain + "@digest:" + f.plainDigest, f.storage},
		{"hmac", "file://" + f.encrypted + "@hmac:" + f.hmac, f.storage},
		{"bundle", "file://" + f.bundle + "@manifest:" + manifest.HexKey(f.root), f.storage},
		{"store", "manifest://" + manifest.HexKey(f.root), storeView},
		{"mkfs", "mkfs://ext4?size=8MiB", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := tc.storage.OpenDiffTemplate(context.Background(), tc.ref, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			compareTemplate(t, source, f.source)
			if verifier, ok := source.(diffstore.TemplateVerifier); ok {
				if err := verifier.Verify(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestDiffTemplateDispatchNeverSniffsOrFallsBack(t *testing.T) {
	f := newTemplateFixture(t)
	for _, path := range []string{f.plain, f.encrypted, f.bundle} {
		source, err := f.storage.OpenDiffTemplate(context.Background(), "file://"+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if source.Size() != uint64(info.Size()) {
			t.Fatal("bare file was decoded as a container", path)
		}
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range []string{
		"file://" + f.plain + "@hmac:" + f.hmac,
		"file://" + f.encrypted + "@digest:" + f.plainDigest,
		"file://" + f.bundle + "@digest:" + f.plainDigest,
		"file://" + f.plain + "@manifest:" + manifest.HexKey(f.root),
		"file://" + f.plain + "@digest:" + strings.Repeat("0", 64),
		"file://" + f.bundle + "@manifest:" + strings.Repeat("0", 64),
	} {
		if source, err := f.storage.OpenDiffTemplate(context.Background(), ref, "", nil); err == nil {
			source.Close()
			t.Error("accepted mismatched source", ref)
		}
	}
	for _, version := range []uint16{1, 3, 4} {
		data := make([]byte, 4096)
		copy(data, []byte{0x89, 'K', 'D', 'X', 'T', 'S', '1', '\n'})
		binary.BigEndian.PutUint16(data[8:], version)
		path := filepath.Join(f.dir, fmt.Sprintf("legacy-%d.diff", version))
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if source, err := f.storage.OpenDiffTemplate(context.Background(), "file://"+path, "", nil); err == nil {
			source.Close()
			t.Error("accepted active DIFF as raw template", version)
		}
	}
}

func TestDiffTemplateTarVerifierPinsCarrierAndDetectsPayloadCorruption(t *testing.T) {
	f := newTemplateFixture(t)
	carrier, err := os.ReadFile(f.plain)
	if err != nil {
		t.Fatal(err)
	}
	uuid := []byte{0xae, 0xbc, 0x4e, 0x52, 0x12, 0x5c, 0x56, 0xe0, 0x9e, 0x34, 0x3e, 0x07, 0x32, 0xca, 0x28, 0x2f}
	at := bytes.Index(carrier, uuid)
	if at < 0 {
		t.Fatal("UUID not present in fixture")
	}
	carrier[at] ^= 1
	corrupt := filepath.Join(f.dir, "corrupt.tar")
	if err := os.WriteFile(corrupt, carrier, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := f.storage.OpenDiffTemplate(context.Background(), "file://"+corrupt+"@digest:"+f.plainDigest, "", nil)
	if err != nil {
		t.Fatal("random open should only check declaration", err)
	}
	if err := consumeLocationSource(context.Background(), source); err != nil {
		t.Fatal("random reads should not accumulate commitment", err)
	}
	if err := source.(diffstore.TemplateVerifier).Verify(context.Background()); !errors.Is(err, tarstream.ErrDigestMismatch) {
		t.Fatalf("full verifier err=%v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	for _, encrypted := range []bool{false, true} {
		path, qualifier, digest := f.plain, "digest", f.plainDigest
		if encrypted {
			path, qualifier, digest = f.encrypted, "hmac", f.hmac
		}
		alias := filepath.Join(f.dir, "alias-"+qualifier)
		if err := os.Symlink(path, alias); err != nil {
			t.Fatal(err)
		}
		opened, err := f.storage.OpenDiffTemplate(context.Background(), "file://"+alias+"@"+qualifier+":"+digest, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(corrupt, alias); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		readErrs := make(chan error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); readErrs <- opened.(diffstore.TemplateVerifier).Verify(context.Background()) }()
		go func() {
			defer wg.Done()
			var p [4096]byte
			for i := 0; i < 100; i++ {
				if _, err := opened.ReadAt(context.Background(), p[:], 0); err != nil {
					readErrs <- err
					return
				}
			}
			readErrs <- nil
		}()
		wg.Wait()
		close(readErrs)
		for err := range readErrs {
			if err != nil {
				t.Fatal("pinned/concurrent read failed", err)
			}
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal("close was not idempotent", err)
		}
	}
}

func TestDiffTemplateBundleForcesIdentityAndLocalClosure(t *testing.T) {
	f := newTemplateFixture(t)
	wrong := store.ContentKey{0xa5}
	forged := f.writeBundle(t, "forged.bundle", wrong, true, false, false)
	if source, err := f.storage.OpenDiffTemplate(context.Background(), "file://"+forged+"@manifest:"+manifest.HexKey(wrong), "", nil); err == nil {
		source.Close()
		t.Fatal("verify_content=false bypassed template identity")
	}
	for _, tc := range []struct {
		name          string
		corrupt, omit bool
	}{{"corrupt", true, false}, {"missing", false, true}} {
		path := f.writeBundle(t, tc.name+".bundle", f.root, false, tc.corrupt, tc.omit)
		// A complete remote source is available; selected Bundle errors must
		// never be repaired by borrowing its healthy chunks.
		borrowed := f.storage.BorrowWithFetcher(f.strict)
		opened, err := borrowed.OpenDiffTemplate(context.Background(), "file://"+path+"@manifest:"+manifest.HexKey(f.root), "", nil)
		if err == nil {
			err = opened.(diffstore.TemplateVerifier).Verify(context.Background())
			opened.Close()
		}
		if err == nil {
			t.Fatal("accepted broken Bundle closure", tc.name)
		}
		if f.strict.opens != 0 {
			t.Fatal("Bundle template fell back to borrowed Store")
		}
	}
}

func TestDiffTemplateBorrowedStoreCapabilityAndScopedSource(t *testing.T) {
	f := newTemplateFixture(t)
	unchecked, err := NewProcessStorageWithLocalAccess(nil, nil, nil, false, f.strict.Fetcher)
	if err != nil {
		t.Fatal(err)
	}
	defer unchecked.Close()
	ref := "manifest://" + manifest.HexKey(f.root)
	if source, err := unchecked.OpenDiffTemplate(context.Background(), ref, "", nil); err == nil {
		source.Close()
		t.Fatal("accepted opaque borrowed Fetcher")
	}
	verified, err := NewProcessStorageWithLocalAccess(nil, nil, nil, false, f.strict)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	// A base graph override must not change the separately selected template
	// Store capability, even if the override cannot serve this Manifest.
	view := verified.BorrowWithFetcher(&closeCountingFetcher{})
	opened, err := view.OpenDiffTemplate(context.Background(), ref, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.(diffstore.TemplateVerifier).Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.strict.opens != 1 {
		t.Fatal("template Store capability was not preserved")
	}
	opened.Close()
	view.Close()
}

func TestDiffTemplateOwnedStoreStrictViewAndClientLifetime(t *testing.T) {
	f := newTemplateFixture(t)
	_, dec, err := manifestcrypto.New(f.storage.cfg.Crypto)
	if err != nil {
		t.Fatal(err)
	}
	wrong := store.ContentKey{0xa5}
	f.store.objects[templateObject{store.PartitionManifest, wrong}] = bytes.Clone(f.store.objects[templateObject{store.PartitionManifest, f.root}])
	ordinary := &closeCountingFetcher{Fetcher: fetch.NewFetcherWithOptions(f.key, f.store, dec, fetch.Options{VerifyContent: false})}
	strict := &closeCountingFetcher{Fetcher: fetch.NewFetcher(f.key, f.store, dec)}
	onDemand := &onDemandManifestFetcher{cfg: f.storage.cfg, keyFn: f.storage.keyFn,
		inner: ordinary, closer: ordinary, strictInner: strict, strictCloser: strict}
	owner := &ProcessStorage{cfg: f.storage.cfg, keyFn: f.storage.keyFn, fetcher: onDemand, templateFetcher: onDemand}
	stream, err := owner.Fetcher().OpenManifest(context.Background(), wrong)
	if err != nil {
		t.Fatal("ordinary-read test setup", err)
	}
	stream.Close()
	borrowed := owner.BorrowWithFetcher(&closeCountingFetcher{})
	if source, err := borrowed.OpenDiffTemplate(context.Background(), "manifest://"+manifest.HexKey(wrong), "", nil); err == nil {
		source.Close()
		t.Fatal("template inherited unchecked ordinary Store")
	}
	if *f.storage.cfg.Manifest.VerifyContent {
		t.Fatal("template mutated ordinary-read policy")
	}
	if err := borrowed.Close(); err != nil {
		t.Fatal(err)
	}
	if ordinary.closes != 0 || strict.closes != 0 {
		t.Fatal("borrower closed process clients")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if ordinary.closes != 1 || strict.closes != 1 {
		t.Fatalf("client closes ordinary=%d strict=%d", ordinary.closes, strict.closes)
	}
}

func TestDiffTemplateNilOptionalFetcherPreservesOwnedStoreThroughBorrow(t *testing.T) {
	f := newTemplateFixture(t)
	owner, err := NewProcessStorageWithLocalAccess(f.storage.cfg, f.storage.keyFn, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// Use the actual strict Manifest/Chunk decoder with an in-memory Store;
	// only client transport construction is replaced for this lifecycle test.
	owner.fetcher.strictInner = f.strict.Fetcher
	view := owner.BorrowWithFetcher(owner.Fetcher())
	defer view.Close()
	opened, err := view.OpenDiffTemplate(context.Background(), "manifest://"+manifest.HexKey(f.root), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	compareTemplate(t, opened, f.source)
	if err := opened.(diffstore.TemplateVerifier).Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDiffTemplateStoreVerifierChecksUnreadChunks(t *testing.T) {
	f := newTemplateFixture(t)
	storage, err := NewProcessStorageWithLocalAccess(nil, nil, nil, false, f.strict)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	for key, value := range f.store.objects {
		if key.partition == store.PartitionChunk {
			value[len(value)/2] ^= 1
		}
	}
	opened, err := storage.OpenDiffTemplate(context.Background(), "manifest://"+manifest.HexKey(f.root), "", nil)
	if err != nil {
		t.Fatal("Manifest open unexpectedly consumed all chunks", err)
	}
	defer opened.Close()
	if err := opened.(diffstore.TemplateVerifier).Verify(context.Background()); err == nil {
		t.Fatal("unread corrupt Chunk escaped full verification")
	}
}

func TestDiffTemplateRawMkfsAndNamedPlaintextNeedNoKey(t *testing.T) {
	f := newTemplateFixture(t)
	calls := 0
	storage, err := NewProcessStorageWithCustomerKey(&config.ManifestConfig{}, func() ([32]byte, error) { calls++; return [32]byte{}, errors.New("key must remain unused") })
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	locations := config.RefLocations{"seeds": f.dir}
	for _, raw := range []string{"file://" + f.raw, "mkfs://ext4?size=8MiB", "file://empty.tar@digest:" + f.plainDigest + "@location:seeds"} {
		opened, err := storage.OpenDiffTemplate(context.Background(), raw, "", locations)
		if err != nil {
			t.Fatal(err)
		}
		if verifier, ok := opened.(diffstore.TemplateVerifier); ok {
			if err := verifier.Verify(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		opened.Close()
	}
	if calls != 0 {
		t.Fatal("unused source requested a key")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(f.dir, "escaped")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	if opened, err := storage.OpenDiffTemplate(context.Background(), "file://escaped@location:seeds", "", locations); err == nil {
		opened.Close()
		t.Fatal("named alias escaped directory")
	}
	for _, qualifier := range []string{"", "@digest:" + f.plainDigest, "@manifest:" + manifest.HexKey(f.root)} {
		fifo := filepath.Join(f.dir, "fifo")
		if err := unix.Mkfifo(fifo, 0600); err != nil && !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
		if opened, err := f.storage.OpenDiffTemplate(context.Background(), "file://"+fifo+qualifier, "", nil); err == nil {
			opened.Close()
			t.Fatal("accepted FIFO template")
		}
	}
}

func TestDiffTemplateLocalOpenRecoveryAndCancellation(t *testing.T) {
	f := newTemplateFixture(t)
	refs := map[string]string{
		"raw":    "file://" + f.raw,
		"digest": "file://" + f.plain + "@digest:" + f.plainDigest,
		"hmac":   "file://" + f.encrypted + "@hmac:" + f.hmac,
		"bundle": "file://" + f.bundle + "@manifest:" + manifest.HexKey(f.root),
	}
	for name, ref := range refs {
		t.Run(name+"/recover", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			attempts := 0
			opened, err := f.storage.openDiffTemplate(ctx, ref, "", nil, func(path string) (*os.File, os.FileInfo, error) {
				attempts++
				if attempts == 1 {
					return nil, nil, unix.EAGAIN
				}
				return openTemplateFile(path)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			compareTemplate(t, opened, f.source)
			if attempts != 2 {
				t.Fatalf("open attempts=%d", attempts)
			}
		})
		t.Run(name+"/cancel-after-open", func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			cause := errors.New("template operation ended")
			var carrier *os.File
			opened, err := f.storage.openDiffTemplate(ctx, ref, "", nil, func(path string) (*os.File, os.FileInfo, error) {
				file, info, err := openTemplateFile(path)
				carrier = file
				cancel(cause)
				return file, info, err
			})
			if opened != nil || !errors.Is(err, cause) || !readretry.IsTerminal(err) {
				t.Fatalf("canceled open returned source=%v err=%v", opened, err)
			}
			if carrier == nil {
				t.Fatal("fixture never opened its carrier")
			}
			if _, err := carrier.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("cancellation leaked opened carrier: %v", err)
			}
		})
	}
	t.Run("cancel-during-retry", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		cause := errors.New("stop waiting for template")
		started, done := make(chan struct{}), make(chan error, 1)
		var once sync.Once
		go func() {
			opened, err := f.storage.openDiffTemplate(ctx, refs["raw"], "", nil, func(string) (*os.File, os.FileInfo, error) {
				once.Do(func() { close(started) })
				return nil, nil, unix.EAGAIN
			})
			if opened != nil {
				_ = opened.Close()
			}
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("template open did not start")
		}
		cancel(cause)
		select {
		case err := <-done:
			if !errors.Is(err, cause) || !readretry.IsTerminal(err) {
				t.Fatalf("lost cancellation cause: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("template open retry ignored cancellation")
		}
	})
}

func TestDiffTemplatePermanentOpenFailuresReleaseCarrier(t *testing.T) {
	f := newTemplateFixture(t)
	cycle := filepath.Join(f.dir, "cycle")
	if err := os.Symlink(cycle, cycle); err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{
		"missing":         "file://" + filepath.Join(f.dir, "missing"),
		"symlink-cycle":   "file://" + cycle,
		"directory":       "file://" + f.dir,
		"digest-mismatch": "file://" + f.plain + "@digest:" + strings.Repeat("0", 64),
		"invalid-tar":     "file://" + f.raw + "@digest:" + f.plainDigest,
		"missing-root":    "file://" + f.bundle + "@manifest:" + strings.Repeat("0", 64),
		"invalid-mkfs":    "mkfs://ext4?size=1",
	}
	for name, ref := range refs {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var carriers []*os.File
			attempts := 0
			opened, err := f.storage.openDiffTemplate(ctx, ref, "", nil, func(path string) (*os.File, os.FileInfo, error) {
				attempts++
				file, info, err := openTemplateFile(path)
				if file != nil {
					carriers = append(carriers, file)
				}
				return file, info, err
			})
			if opened != nil || err == nil || !readerr.IsPermanent(err) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("permanent failure was retried or accepted: source=%v err=%v", opened, err)
			}
			if attempts > 1 {
				t.Fatalf("permanent failure used %d file-open attempts", attempts)
			}
			for _, carrier := range carriers {
				if _, err := carrier.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("failed open leaked carrier: %v", err)
				}
			}
		})
	}
}

type callbackVerifiedFetcher struct {
	fetch.Fetcher
	open func(context.Context, store.ContentKey) (fetch.Stream, error)
}

func (f *callbackVerifiedFetcher) OpenVerifiedManifest(ctx context.Context, key store.ContentKey) (fetch.Stream, error) {
	return f.open(ctx, key)
}

type countedTemplateStream struct {
	fetch.Stream
	closes *atomic.Int32
}

func (s *countedTemplateStream) Close() error {
	s.closes.Add(1)
	return s.Stream.Close()
}

func TestDiffTemplateStoreOpenRecoveryClosesEveryAttempt(t *testing.T) {
	f := newTemplateFixture(t)
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			cause := errors.New("stop opened Store template")
			attempts := 0
			var closes atomic.Int32
			fetcher := &callbackVerifiedFetcher{Fetcher: f.strict.Fetcher}
			fetcher.open = func(ctx context.Context, key store.ContentKey) (fetch.Stream, error) {
				stream, err := f.strict.Fetcher.OpenManifest(ctx, key)
				if err != nil {
					return stream, err
				}
				attempts++
				counted := &countedTemplateStream{Stream: stream, closes: &closes}
				if attempts == 1 {
					return counted, unix.EAGAIN
				}
				if canceled {
					cancel(cause)
				}
				return counted, nil
			}
			storage, err := NewProcessStorageWithLocalAccess(nil, nil, nil, false, fetcher)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			opened, err := storage.OpenDiffTemplate(ctx, "manifest://"+manifest.HexKey(f.root), "", nil)
			if canceled {
				if opened != nil || !errors.Is(err, cause) || !readretry.IsTerminal(err) {
					t.Fatalf("canceled Store open source=%v err=%v", opened, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				compareTemplate(t, opened, f.source)
				if err := opened.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if attempts != 2 || closes.Load() != 2 {
				t.Fatalf("Store attempts=%d closed=%d", attempts, closes.Load())
			}
		})
	}
}

type retryTemplateReaderAt struct {
	io.ReaderAt
	failed, replayed bool
	failAt           int64
	failedOffset     int64
	failedSize       int
}

func (r *retryTemplateReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if !r.failed && off >= r.failAt {
		r.failed, r.failedOffset, r.failedSize = true, off, len(p)
		for i := range p[:len(p)/2] {
			p[i] = 0xa5
		}
		return len(p) / 2, unix.EAGAIN
	}
	if r.failed && !r.replayed {
		if off != r.failedOffset || len(p) != r.failedSize {
			return 0, readerr.Mark(fmt.Errorf("partial range was not completely retried"), false)
		}
		r.replayed = true
	}
	return r.ReaderAt.ReadAt(p, off)
}

func TestDiffTemplateTarVerificationRetriesBeforeSequentialParsing(t *testing.T) {
	f := newTemplateFixture(t)
	for name, ref := range map[string]string{
		"digest": "file://" + f.plain + "@digest:" + f.plainDigest,
		"hmac":   "file://" + f.encrypted + "@hmac:" + f.hmac,
	} {
		for _, failAt := range []int64{0, 4096} {
			t.Run(fmt.Sprintf("%s/%d", name, failAt), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				opened, err := f.storage.OpenDiffTemplate(ctx, ref, "", nil)
				if err != nil {
					t.Fatal(err)
				}
				defer opened.Close()
				tarSource := opened.(*tarTemplateSource)
				reader := &retryTemplateReaderAt{ReaderAt: tarSource.file, failAt: failAt}
				if err := tarSource.verify(ctx, reader); err != nil {
					t.Fatal("verification did not recover from a partial physical read", err)
				}
				if !reader.failed || !reader.replayed {
					t.Fatal("verification did not retry the entire failed physical range")
				}
			})
		}
	}
}
