package restore

import (
	"bytes"
	"context"
	"errors"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"strings"
	"testing"
	"time"
)

type selectedRetryFetcher func(context.Context, store.ContentKey) (fetch.Stream, error)

func (f selectedRetryFetcher) OpenManifest(ctx context.Context, key store.ContentKey) (fetch.Stream, error) {
	return f(ctx, key)
}

func TestSelectedSandboxRemoteOpenRetainsRetryAndOwnership(t *testing.T) {
	identity := strings.Repeat("a", 64)
	cfg := &config.PortableSandboxConfig{Version: config.PortableSandboxConfigVersion,
		Resources: config.PortableResourcesConfig{Capacity: config.CapacityConfig{CPU: 1, Memory: "4KiB"}, Allocatable: config.AllocatableConfig{CPU: 1, Memory: "4KiB"}},
		Boot:      config.PortableBootConfig{Kernel: "file://vmlinux@digest:" + identity, Runtime: "file://runtime.bundle@digest:" + identity, Root: config.PortableRootConfig{Base: "self"}},
		Launch:    config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"}}
	raw, err := config.MarshalPortableSandboxConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"transient", "permanent", "cancel-on-open"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			source, err := sandboxfile.BuildSource(sparse.Dense(bytes.NewReader(make([]byte, 4096)), 4096), nil, raw)
			if err != nil {
				t.Fatal(err)
			}
			stream := &restoreCloseTrackingStream{Source: source}
			cause := errors.New("injected selected-source failure")
			attempts := 0
			remote := selectedRetryFetcher(func(context.Context, store.ContentKey) (fetch.Stream, error) {
				attempts++
				if mode == "permanent" {
					return nil, readerr.Mark(cause, false)
				}
				if mode == "transient" && attempts == 1 {
					return nil, cause
				}
				if mode == "cancel-on-open" {
					cancel()
				}
				return stream, nil
			})
			scoped := bundle.NewManifestFetcher(nil, nil, remote)
			result, err := openReferencedSandbox(ctx, "manifest://"+identity, Options{Fetcher: scoped})
			switch mode {
			case "transient":
				if err != nil || attempts != 2 {
					t.Fatalf("transient open attempts=%d err=%v", attempts, err)
				}
				if result.SelectedRef != "manifest://"+identity {
					t.Fatalf("binding changed: %q", result.SelectedRef)
				}
				if err = result.Root.Close(); err != nil {
					t.Fatal(err)
				}
				if stream.closes != 1 {
					t.Fatalf("source closes=%d", stream.closes)
				}
			case "permanent":
				if !errors.Is(err, cause) || attempts != 1 || result != nil {
					t.Fatalf("permanent attempts=%d result=%v err=%v", attempts, result, err)
				}
			case "cancel-on-open":
				if !errors.Is(err, context.Canceled) || attempts != 1 || stream.closes != 1 || result != nil {
					t.Fatalf("cancel attempts=%d closes=%d result=%v err=%v", attempts, stream.closes, result, err)
				}
			}
		})
	}
}
