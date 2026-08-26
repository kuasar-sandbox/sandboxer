package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

type cachedBundleSource struct {
	source bundle.ManifestSource
	err    error
}

// bundleSourceResolver owns the flat, lazy, race-safe Reader cache for one
// current root Bundle. Referenced Readers' own refs are never traversed.
type bundleSourceResolver struct {
	mu sync.Mutex

	rootDir     string
	locations   config.RefLocations
	customerKey [32]byte
	decryptor   manifestcrypto.Decryptor
	options     fetch.Options
	cache       map[string]cachedBundleSource
	closed      bool
}

func newBundleSourceResolver(rootDir string, locations config.RefLocations, customerKey [32]byte, decryptor manifestcrypto.Decryptor, options fetch.Options) *bundleSourceResolver {
	return &bundleSourceResolver{
		rootDir: filepath.Clean(rootDir), locations: locations,
		customerKey: customerKey, decryptor: decryptor, options: options,
		cache: make(map[string]cachedBundleSource),
	}
}

func (r *bundleSourceResolver) ResolveBundle(ctx context.Context, raw string) (bundle.ManifestSource, error) {
	if err := ctx.Err(); err != nil {
		return bundle.ManifestSource{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return bundle.ManifestSource{}, fmt.Errorf("artifact: Bundle resolver is closed")
	}
	if cached, ok := r.cache[raw]; ok {
		return cached.source, cached.err
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return bundle.ManifestSource{}, fmt.Errorf("artifact: parse Bundle ref %q: %w", raw, err)
	}
	path, err := r.locations.ResolveFile(ref, r.rootDir)
	if err != nil {
		err = fmt.Errorf("%w: %s: %v", bundle.ErrSourceUnavailable, raw, err)
		r.cache[raw] = cachedBundleSource{err: err}
		return bundle.ManifestSource{}, err
	}
	reader, err := bundle.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			err = fmt.Errorf("%w: %s: %v", bundle.ErrSourceUnavailable, raw, err)
		}
		r.cache[raw] = cachedBundleSource{err: err}
		return bundle.ManifestSource{}, err
	}
	source := bundle.ManifestSource{
		Reader:  reader,
		Fetcher: fetch.NewFetcherWithOptions(r.customerKey, reader.Getter(), r.decryptor, r.options),
		Ref:     raw,
	}
	r.cache[raw] = cachedBundleSource{source: source}
	return source, nil
}

func (r *bundleSourceResolver) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	var closeErr error
	for raw, cached := range r.cache {
		if cached.source.Reader != nil {
			closeErr = errors.Join(closeErr, cached.source.Reader.Close())
		}
		delete(r.cache, raw)
	}
	clear(r.customerKey[:])
	r.mu.Unlock()
	return closeErr
}

var _ bundle.SourceResolver = (*bundleSourceResolver)(nil)
