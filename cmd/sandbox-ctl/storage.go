package main

import (
	"context"
	"fmt"
	"io"
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

// storageOptions resolves crypto.local without creating a store or cache
// client. The returned key function memoizes both the key and any error, so a
// process never observes a later MANIFEST_KEY value. off deliberately leaves
// it uncalled until a real manifest fetch or ingest needs the key.
func storageOptions(cfg *config.ManifestConfig) (ingest.CustomerKeyFunc, tarstream.Codec, bool, error) {
	if cfg == nil {
		return nil, nil, false, nil
	}
	var (
		once   sync.Once
		key    [32]byte
		keyErr error
	)
	keyFn := func() ([32]byte, error) {
		once.Do(func() { key, keyErr = cfg.CustomerKey() })
		return key, keyErr
	}
	policy, err := cfg.Crypto.LocalPolicy()
	if err != nil {
		return nil, nil, false, err
	}
	if policy == crypto.LocalOff {
		return keyFn, nil, false, nil
	}
	key, err = keyFn()
	if err != nil {
		return nil, nil, false, err
	}
	codec, err := crypto.NewTarStreamCodec(key)
	if err != nil {
		return nil, nil, false, err
	}
	return keyFn, codec, policy == crypto.LocalRequired, nil
}

// onDemandManifestFetcher defers every network client allocation until a
// manifest is actually opened. It shares the process-fixed customer key with
// local tarstreams and snapshot ingestion.
type onDemandManifestFetcher struct {
	cfg   *config.ManifestConfig
	keyFn ingest.CustomerKeyFunc
	once  sync.Once
	inner fetch.Fetcher
	close io.Closer
	err   error
}

func (f *onDemandManifestFetcher) OpenManifest(ctx context.Context, key store.ContentKey) (fetch.Stream, error) {
	f.once.Do(func() { f.inner, f.close, f.err = newManifestFetcher(f.cfg, f.keyFn) })
	if f.err != nil {
		return nil, f.err
	}
	return f.inner.OpenManifest(ctx, key)
}

func (f *onDemandManifestFetcher) Close() error {
	if f.close == nil {
		return nil
	}
	return f.close.Close()
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
		return fetch.NewFetcher(customerKey, client, decryptor), client, nil
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
	return fetch.NewFetcher(customerKey, cache.NewStoreOrigin(client), decryptor), client, nil
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
