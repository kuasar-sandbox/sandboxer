package artifact

import (
	"bufio"
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/cache/wire"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestProcessFetcherFirstDialFailureAndCancellationRecover(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "dial-failure", true: "canceled"}[cancelFirst], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cache.sock")
			cfg := &config.ManifestConfig{Cache: manifest.CacheConfig{Endpoint: path, Pool: 1}, Crypto: manifestcrypto.Config{Chunk: "aes", Manifest: "aes", Local: manifestcrypto.LocalOff}}
			var keys atomic.Int32
			storage, err := NewProcessStorageWithCustomerKey(cfg, func() ([32]byte, error) { keys.Add(1); return [32]byte{1}, nil })
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			ctx, cancel := context.WithCancel(context.Background())
			if cancelFirst {
				cancel()
			}
			defer cancel()
			if _, err = storage.Fetcher().OpenManifest(ctx, store.ContentKey{}); err == nil {
				t.Fatal("first unavailable open succeeded")
			}
			if storage.fetcher.inner != nil {
				t.Fatal("failed initialization cached")
			}
			l, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var conns []net.Conn
			var wg sync.WaitGroup
			accepted := make(chan struct{})
			go func() {
				defer close(accepted)
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					mu.Lock()
					conns = append(conns, c)
					mu.Unlock()
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer c.Close()
						r := bufio.NewReader(c)
						for {
							req, err := wire.ReadRequest(r)
							if err != nil {
								return
							}
							req.Release()
							if wire.WriteResponse(c, &wire.Response{Status: wire.StatusMiss}) != nil {
								return
							}
						}
					}()
				}
			}()
			defer func() {
				storage.Close()
				l.Close()
				<-accepted
				mu.Lock()
				for _, c := range conns {
					c.Close()
				}
				mu.Unlock()
				wg.Wait()
			}()
			// A confirmed miss proves the new client reached the peer; only transport
			// initialization is cached, while immutable customer-key resolution stays fixed.
			for range 2 {
				_, err = storage.Fetcher().OpenManifest(context.Background(), store.ContentKey{})
				if !errors.Is(err, store.ErrNotFound) || !readerr.IsPermanent(err) {
					t.Fatalf("recovered client did not reach confirmed miss: %v", err)
				}
			}
			if storage.fetcher.inner == nil || keys.Load() != 1 {
				t.Fatalf("inner=%v key resolutions=%d", storage.fetcher.inner, keys.Load())
			}
			storage.Close()
			_, err = storage.Fetcher().OpenManifest(context.Background(), store.ContentKey{})
			if !readerr.IsPermanent(err) {
				t.Fatalf("closed fetcher resurrected: %v", err)
			}
		})
	}
}

func TestExactBundleSelectionRetriesBeforeBindingSource(t *testing.T) {
	cfg, storage := manifestPublisherFixture(t)
	defer storage.Close()
	extRef, extPath := overlayBundleFixture(t, cfg, storage, t.TempDir())
	rootRef, rootPath := bundlePublishFixtureWithRefs(t, cfg, storage, []string{"file://external.bundle"})
	ctx := context.Background()
	open := func(path, raw string) *OpenedFile {
		key, err := manifest.ParseKeyRef(raw)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := manifest.ParseRef("file://" + path + "@manifest:" + manifest.HexKey(key))
		if err != nil {
			t.Fatal(err)
		}
		f, err := storage.OpenFile(ctx, path, ref)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	ext, root := open(extPath, extRef), open(rootPath, rootRef)
	key, _ := manifest.ParseKeyRef(extRef)
	rootKey, _ := manifest.ParseKeyRef(rootRef)
	var attempts int
	root.manifestSet = bundle.NewManifestFetcherWithResolver(root.BundleReader(), root.ScopedFetcher(), bundle.SourceResolverFunc(func(context.Context, string) (bundle.ManifestSource, error) {
		attempts++
		if attempts == 1 {
			return bundle.ManifestSource{}, errors.New("temporary index read")
		}
		return bundle.ManifestSource{Reader: ext.BundleReader(), Fetcher: ext.ScopedFetcher()}, nil
	}), nil)
	_, dependencies, _, _, _, err := selectExactBundleSources(ctx, root, rootKey, []store.ContentKey{key})
	if err != nil || attempts != 2 || len(dependencies) != 1 || dependencies[0].Reader != ext.BundleReader() {
		t.Fatalf("selected source changed or retry missing: attempts=%d deps=%v err=%v", attempts, dependencies, err)
	}
}
