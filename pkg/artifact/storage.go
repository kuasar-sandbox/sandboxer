// Package artifact opens and publishes logical image, disk, Sandbox, and
// Snapshot artifacts independently of their physical carrier. It owns the
// customer-key binding, local tarstream codec, and lazily-created manifest
// client used by one task or tool process.
package artifact

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	cacheclient "github.com/kuasar-sandbox/accelerator/pkg/cache/client"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	storeclient "github.com/kuasar-sandbox/accelerator/pkg/store/client"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// ProcessStorage fixes the process-local customer key and owns any manifest
// client opened by the process. A nil manifest config is valid for local-only
// artifact access. Manifest clients are allocated only when Fetcher is first
// used, so file-only operations do not connect to cache or store.
type ProcessStorage struct {
	cfg           *config.ManifestConfig
	keyFn         ingest.CustomerKeyFunc
	localCodec    tarstream.Codec
	localRequired bool
	fetcher       *onDemandManifestFetcher
}

// NewProcessStorage initializes process-local artifact access from cfg and the
// existing MANIFEST_KEY environment convention.
func NewProcessStorage(cfg *config.ManifestConfig) (*ProcessStorage, error) {
	var keyFn ingest.CustomerKeyFunc
	if cfg != nil {
		keyFn = cfg.CustomerKey
	}
	return NewProcessStorageWithCustomerKey(cfg, keyFn)
}

// NewProcessStorageWithCustomerKey initializes process-local artifact access
// with an explicit customer-key resolver. It is intended for embedding callers
// whose authoritative key is task-scoped rather than process-global. The
// resolver is evaluated at most once and its result is fixed for every fetch,
// ingest, and local-codec operation owned by the returned storage.
func NewProcessStorageWithCustomerKey(cfg *config.ManifestConfig, customerKey ingest.CustomerKeyFunc) (*ProcessStorage, error) {
	s := &ProcessStorage{cfg: cfg}
	if cfg == nil {
		return s, nil
	}
	if customerKey == nil {
		return nil, fmt.Errorf("artifact storage: customer key resolver is required")
	}

	var (
		once   sync.Once
		key    [32]byte
		keyErr error
	)
	s.keyFn = func() ([32]byte, error) {
		once.Do(func() { key, keyErr = customerKey() })
		return key, keyErr
	}

	policy, err := cfg.Crypto.LocalPolicy()
	if err != nil {
		return nil, err
	}
	if policy != crypto.LocalOff {
		key, err = s.keyFn()
		if err != nil {
			return nil, err
		}
		s.localCodec, err = crypto.NewTarStreamCodec(key)
		if err != nil {
			return nil, err
		}
		s.localRequired = policy == crypto.LocalRequired
	}
	s.fetcher = &onDemandManifestFetcher{cfg: cfg, keyFn: s.keyFn}
	if !verificationOptions(cfg).VerifyContent {
		warnVerificationDisabled.Do(func() {
			log.Printf("WARNING: manifest.verify_content=false; ordinary remote and Bundle Manifest/Chunk SHA-256 verification is disabled")
		})
	}
	return s, nil
}

var warnVerificationDisabled sync.Once

func verificationOptions(cfg *config.ManifestConfig) fetch.Options {
	return fetch.Options{VerifyContent: cfg == nil || cfg.Manifest.VerifyContent == nil || *cfg.Manifest.VerifyContent}
}

// CustomerKeyFunc returns the process-fixed key resolver used by manifest
// fetch and ingest operations. It is nil when no manifest config was supplied.
func (s *ProcessStorage) CustomerKeyFunc() ingest.CustomerKeyFunc {
	if s == nil {
		return nil
	}
	return s.keyFn
}

// Fetcher returns the process-owned lazy manifest fetcher. It is nil when no
// manifest config was supplied.
func (s *ProcessStorage) Fetcher() fetch.Fetcher {
	if s == nil || s.fetcher == nil {
		return nil
	}
	return s.fetcher
}

// LocalCodec returns the codec selected by crypto.local.
func (s *ProcessStorage) LocalCodec() tarstream.Codec {
	if s == nil {
		return nil
	}
	return s.localCodec
}

// LocalRequired reports whether plaintext local artifacts are forbidden.
func (s *ProcessStorage) LocalRequired() bool {
	return s != nil && s.localRequired
}

// Close releases the process-owned cache or store client, if one was opened.
func (s *ProcessStorage) Close() error {
	if s == nil || s.fetcher == nil {
		return nil
	}
	return s.fetcher.Close()
}

type onDemandManifestFetcher struct {
	mu          sync.Mutex
	cfg         *config.ManifestConfig
	keyFn       ingest.CustomerKeyFunc
	initialized bool
	closed      bool
	inner       fetch.Fetcher
	closer      io.Closer
	err         error
}

func (f *onDemandManifestFetcher) OpenManifest(ctx context.Context, key store.ContentKey) (fetch.Stream, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, fmt.Errorf("manifest fetcher is closed")
	}
	if !f.initialized {
		f.inner, f.closer, f.err = newManifestFetcher(f.cfg, f.keyFn)
		f.initialized = true
	}
	inner, err := f.inner, f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return inner.OpenManifest(ctx, key)
}

func (f *onDemandManifestFetcher) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	closer := f.closer
	f.closer = nil
	f.mu.Unlock()
	if closer == nil {
		return nil
	}
	return closer.Close()
}

func newManifestFetcher(cfg *config.ManifestConfig, keyFn ingest.CustomerKeyFunc) (fetch.Fetcher, io.Closer, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("manifest config is required")
	}
	if keyFn == nil {
		return nil, nil, fmt.Errorf("customer key resolver is required")
	}
	customerKey, err := keyFn()
	if err != nil {
		return nil, nil, err
	}
	defer clear(customerKey[:])
	_, decryptor, err := crypto.New(cfg.Crypto)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Cache.Endpoint != "" {
		timeout, err := optionalDuration(cfg.Cache.Timeout, "cache.timeout")
		if err != nil {
			return nil, nil, err
		}
		client, err := cacheclient.NewGetter(cfg.Cache.Endpoint, cacheclient.Options{Pool: cfg.Cache.Pool, Timeout: timeout})
		if err != nil {
			return nil, nil, fmt.Errorf("manifest: dial cache: %w", err)
		}
		return fetch.NewFetcherWithOptions(customerKey, client, decryptor, verificationOptions(cfg)), client, nil
	}
	if cfg.Store.Endpoint == "" {
		return nil, nil, fmt.Errorf("manifest: cache.endpoint or store.endpoint required for fetch")
	}
	timeout, err := optionalDuration(cfg.Store.Timeout, "store.timeout")
	if err != nil {
		return nil, nil, err
	}
	client, err := storeclient.New(cfg.Store.Endpoint, cfg.Store.Pool, timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("manifest: dial store: %w", err)
	}
	return fetch.NewFetcherWithOptions(customerKey, cache.NewStoreOrigin(client), decryptor, verificationOptions(cfg)), client, nil
}

func optionalDuration(raw, field string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("manifest: %s: %w", field, err)
	}
	return value, nil
}
