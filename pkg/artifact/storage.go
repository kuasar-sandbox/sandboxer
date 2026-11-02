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
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
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
	cfg             *config.ManifestConfig
	keyFn           ingest.CustomerKeyFunc
	localCodec      tarstream.Codec
	localRequired   bool
	ref             *refTarstreams
	fetcher         *onDemandManifestFetcher
	borrowedFetcher fetch.Fetcher
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
// ingest, local-codec and named-location operation owned by the returned storage.
func NewProcessStorageWithCustomerKey(cfg *config.ManifestConfig, customerKey ingest.CustomerKeyFunc) (*ProcessStorage, error) {
	s, err := newProcessStorage(cfg, customerKey)
	if err != nil || cfg == nil {
		return s, err
	}
	policy, err := cfg.Crypto.LocalPolicy()
	if err != nil {
		return nil, err
	}
	if policy != crypto.LocalOff {
		key, err := s.keyFn()
		if err != nil {
			return nil, err
		}
		s.localCodec, err = crypto.NewTarStreamCodec(key)
		clear(key[:])
		if err != nil {
			return nil, err
		}
		s.localRequired = policy == crypto.LocalRequired
	}
	return s, nil
}

// NewProcessStorageWithLocalAccess preserves an embedding caller's explicit
// local codec and plaintext policy, including when no manifest config exists.
// The optional Fetcher is borrowed and remains owned by the caller. The customer
// key is fixed on first use, just as for manifest-backed storage.
func NewProcessStorageWithLocalAccess(cfg *config.ManifestConfig, customerKey ingest.CustomerKeyFunc, codec tarstream.Codec, required bool, borrowed ...fetch.Fetcher) (*ProcessStorage, error) {
	if len(borrowed) > 1 {
		return nil, fmt.Errorf("artifact storage: at most one borrowed fetcher is allowed")
	}
	if required && codec == nil {
		return nil, fmt.Errorf("artifact storage: local required policy requires a codec")
	}
	if codec != nil && customerKey == nil {
		return nil, fmt.Errorf("artifact storage: local codec requires a customer key resolver")
	}
	s, err := newProcessStorage(cfg, customerKey)
	if err != nil {
		return nil, err
	}
	s.localCodec, s.localRequired = codec, required
	if len(borrowed) == 1 {
		s.borrowedFetcher = borrowed[0]
	}
	return s, nil
}

// Borrow preserves the complete artifact-access capability without transferring
// ownership. Closing the result never closes the original storage or its client.
// The caller must keep the original storage open until the borrower is released.
func (s *ProcessStorage) Borrow() *ProcessStorage {
	return s.BorrowWithFetcher(s.Fetcher())
}

// BorrowWithFetcher keeps both tarstream policies and the fixed key while
// selecting the caller's Manifest source (for example an opened Bundle).
// Closing this view never closes either the storage or the selected fetcher.
func (s *ProcessStorage) BorrowWithFetcher(fetcher fetch.Fetcher) *ProcessStorage {
	if s == nil {
		return nil
	}
	return &ProcessStorage{cfg: s.cfg, keyFn: s.keyFn, localCodec: s.localCodec, ref: s.ref,
		localRequired: s.localRequired, borrowedFetcher: fetcher}
}

func newProcessStorage(cfg *config.ManifestConfig, customerKey ingest.CustomerKeyFunc) (*ProcessStorage, error) {
	s := &ProcessStorage{cfg: cfg}
	if customerKey != nil {
		s.keyFn = sync.OnceValues(customerKey)
	} else if cfg != nil {
		return nil, fmt.Errorf("artifact storage: customer key resolver is required")
	}
	ref, err := newRefTarstreams(cfg, s.keyFn)
	if err != nil {
		return nil, err
	}
	s.ref = ref
	if cfg != nil {
		if _, err := cfg.Crypto.LocalPolicy(); err != nil {
			return nil, err
		}
		s.fetcher = &onDemandManifestFetcher{cfg: cfg, keyFn: s.keyFn}
		if !verificationOptions(cfg).VerifyContent {
			warnVerificationDisabled.Do(func() {
				log.Printf("WARNING: manifest.verify_content=false; ordinary remote and Bundle Manifest/Chunk SHA-256 verification is disabled")
			})
		}
	}
	return s, nil
}

var warnVerificationDisabled sync.Once

// refTarstreams shares both successful and failed initialization across reads,
// writes and borrowers. Plaintext reads in auto mode never resolve the key.
type refTarstreams struct {
	policy crypto.LocalPolicy
	keyFn  ingest.CustomerKeyFunc
	once   sync.Once
	codec  tarstream.Codec
	err    error
}

func newRefTarstreams(cfg *config.ManifestConfig, keyFn ingest.CustomerKeyFunc) (*refTarstreams, error) {
	var settings crypto.Config
	if cfg != nil {
		settings = cfg.Crypto
	}
	policy, err := settings.RefPolicy()
	if err != nil {
		return nil, err
	}
	return &refTarstreams{policy: policy, keyFn: keyFn}, nil
}

func (s *ProcessStorage) refAccess() (*refTarstreams, error) {
	if s.ref != nil {
		return s.ref, nil
	}
	return newRefTarstreams(s.cfg, s.keyFn)
}

func (s *ProcessStorage) readCodec(located bool) (tarstream.Codec, bool, error) {
	if !located {
		return s.localCodec, s.localRequired, nil
	}
	access, err := s.refAccess()
	if err != nil {
		return nil, false, err
	}
	codec, required := access.reader()
	return codec, required, nil
}

func (r *refTarstreams) resolve() (tarstream.Codec, error) {
	if r.policy == crypto.LocalOff {
		return nil, nil
	}
	r.once.Do(func() {
		if r.keyFn == nil {
			r.err = fmt.Errorf("ref tarstream: customer key resolver required: %w", tarstream.ErrCodecRequired)
			return
		}
		key, err := r.keyFn()
		if err != nil {
			r.err = readerr.Mark(fmt.Errorf("ref tarstream customer key: %w", err), false)
			return
		}
		r.codec, r.err = crypto.NewTarStreamCodec(key)
		clear(key[:])
	})
	return r.codec, r.err
}

func (r *refTarstreams) reader() (tarstream.Codec, bool) {
	if r.policy == crypto.LocalOff {
		return nil, false
	}
	return refReaderCodec{r}, r.policy == crypto.LocalRequired
}

// SourceAt/SourceFrom bind before asking for a keyed digest. Writers resolve
// the real codec instead: CarrierDigest may need a digest without any bind.
type refReaderCodec struct{ ref *refTarstreams }

func (c refReaderCodec) BindArtifact(salt [32]byte) (tarstream.RecordCodec, error) {
	codec, err := c.ref.resolve()
	if err != nil {
		return nil, err
	}
	return codec.BindArtifact(salt)
}

func (c refReaderCodec) KeyedDigest(plain [32]byte) [32]byte {
	return c.ref.codec.KeyedDigest(plain)
}

func verificationOptions(cfg *config.ManifestConfig) fetch.Options {
	return fetch.Options{VerifyContent: cfg == nil || cfg.Manifest.VerifyContent == nil || *cfg.Manifest.VerifyContent}
}

// ManifestConfig returns the immutable manifest configuration captured by this
// storage. Callers must treat the returned pointer as read-only. It lets live
// capture reuse the same write policy/endpoint without duplicating credentials
// in a LaunchSpec.
func (s *ProcessStorage) ManifestConfig() *config.ManifestConfig {
	if s == nil {
		return nil
	}
	return s.cfg
}

// CustomerKeyFunc returns the process-fixed key resolver used by manifest
// fetch, ingest, and local DIFF operations. It is nil when no resolver was supplied.
func (s *ProcessStorage) CustomerKeyFunc() ingest.CustomerKeyFunc {
	if s == nil {
		return nil
	}
	return s.keyFn
}

// Fetcher returns the borrowed access capability or the process-owned lazy
// manifest fetcher. It is nil when neither was supplied.
func (s *ProcessStorage) Fetcher() fetch.Fetcher {
	if s == nil {
		return nil
	}
	if s.borrowedFetcher != nil {
		return s.borrowedFetcher
	}
	if s.fetcher == nil {
		return nil
	}
	return s.fetcher
}

// LocalCodec returns the configured or explicitly supplied local codec.
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
	mu     sync.Mutex
	cfg    *config.ManifestConfig
	keyFn  ingest.CustomerKeyFunc
	closed bool
	inner  fetch.Fetcher
	closer io.Closer
}

func (f *onDemandManifestFetcher) OpenManifest(ctx context.Context, key store.ContentKey) (fetch.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, readerr.Mark(fmt.Errorf("manifest fetcher is closed"), false)
	}
	if f.inner == nil {
		inner, closer, err := newManifestFetcher(f.cfg, f.keyFn)
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			if closer != nil {
				_ = closer.Close()
			}
			f.mu.Unlock()
			return nil, err
		}
		f.inner, f.closer = inner, closer
	}
	inner := f.inner
	f.mu.Unlock()
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
		return nil, nil, readerr.Mark(fmt.Errorf("manifest config is required"), false)
	}
	if keyFn == nil {
		return nil, nil, readerr.Mark(fmt.Errorf("customer key resolver is required"), false)
	}
	customerKey, err := keyFn()
	if err != nil {
		return nil, nil, readerr.Mark(err, false)
	}
	defer clear(customerKey[:])
	_, decryptor, err := crypto.New(cfg.Crypto)
	if err != nil {
		return nil, nil, readerr.Mark(err, false)
	}
	if cfg.Cache.Endpoint != "" {
		log.Printf("manifest fetcher: store=%s cache=%s", cfg.Store.Endpoint, cfg.Cache.Endpoint)
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
		return nil, nil, readerr.Mark(fmt.Errorf("manifest: cache.endpoint or store.endpoint required for fetch"), false)
	}
	log.Printf("manifest fetcher: store=%s", cfg.Store.Endpoint)
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
		return 0, readerr.Mark(fmt.Errorf("manifest: %s: %w", field, err), false)
	}
	return value, nil
}
