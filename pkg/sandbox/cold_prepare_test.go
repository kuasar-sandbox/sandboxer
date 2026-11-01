package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
)

type coldPreparationFetcher struct {
	body          []byte
	opens, closes atomic.Int32
}

func (f *coldPreparationFetcher) OpenManifest(ctx context.Context, _ store.ContentKey) (fetch.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.opens.Add(1)
	source, err := sparse.NewSource(bytes.NewReader(f.body), uint64(len(f.body)), nil)
	if err != nil {
		return nil, err
	}
	return &coldPreparationStream{Source: source, closes: &f.closes}, nil
}

type coldPreparationStream struct {
	sparse.Source
	closes *atomic.Int32
}

func (s *coldPreparationStream) Close() error { s.closes.Add(1); return nil }

func coldPreparationFixture(t *testing.T) (RuntimeSpec, LaunchSpec, *coldPreparationFetcher) {
	t.Helper()
	shape, launch := sdkFixture(t)
	launch.Root = config.RootConfig{Base: "manifest://" + strings.Repeat("b", 64),
		Overlay: &config.OverlayConfig{DiffTemplate: launch.Root.DiffTemplate}}
	shape.Disks[0].Overlay, shape.Disks[0].BaseCapacity = true, 4096
	fetcher := &coldPreparationFetcher{body: lifecycleEROFSFixture()}
	launch.Fetcher = fetcher
	return shape, launch, fetcher
}

func TestRunReusesColdPreflight(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		name := "ordinary"
		if checkpoint {
			name = "checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			shape, launch, fetcher := coldPreparationFixture(t)
			cfg, err := launch.config(shape.config())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ready := make(chan struct{}, 1)
			_, runErr := Run(ctx, RunOptions{Cfg: cfg, Fetcher: fetcher, EnableCheckpoint: checkpoint,
				SandboxID: shape.SandboxID, CHBinary: shape.CHBinary, RuntimeRoot: shape.RuntimeRoot, BaseRoot: shape.BaseRoot,
				NotifyReadiness: func(event ReadinessEvent) {
					if event == ReadinessReady {
						ready <- struct{}{}
						cancel()
					}
				}})
			select {
			case <-ready:
			default:
				t.Fatalf("Run did not reach ready: %v", runErr)
			}
			// Early preflight, runtime capacity derivation, and the actual live
			// backend each open the root once. Launch must not add a fourth open.
			if opens, closes := fetcher.opens.Load(), fetcher.closes.Load(); opens != 3 || closes != opens {
				t.Fatalf("root Manifest opens/closes = %d/%d, want 3/3", opens, closes)
			}
		})
	}
}

func TestRawLaunchStillPreflights(t *testing.T) {
	shape, launch, fetcher := coldPreparationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if opens, closes := fetcher.opens.Load(), fetcher.closes.Load(); opens != 2 || closes != opens {
		t.Fatalf("raw Launch root opens/closes = %d/%d, want preflight plus live backend", opens, closes)
	}
}

func TestRunColdPreflightStillFailsBeforeRuntime(t *testing.T) {
	shape, launch, fetcher := coldPreparationFixture(t)
	fetcher.body = make([]byte, 4096)
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	var events atomic.Int32
	_, err = Run(context.Background(), RunOptions{Cfg: cfg, Fetcher: fetcher,
		SandboxID: shape.SandboxID, CHBinary: "/nonexistent/cloud-hypervisor", RuntimeRoot: shape.RuntimeRoot, BaseRoot: shape.BaseRoot,
		NotifyReadiness: func(ReadinessEvent) { events.Add(1) }})
	if err == nil || !strings.Contains(err.Error(), "preflight boot.root.base") {
		t.Fatalf("invalid source did not fail in early preflight: %v", err)
	}
	if events.Load() != 0 {
		t.Fatal("invalid source reached runtime readiness")
	}
	if _, err := os.Stat(filepath.Join(shape.RuntimeRoot, shape.SandboxID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid source created a runtime directory: %v", err)
	}
	if opens, closes := fetcher.opens.Load(), fetcher.closes.Load(); opens != 1 || closes != opens {
		t.Fatalf("invalid source opens/closes = %d/%d, want 1/1", opens, closes)
	}
}

func TestRunReusedPreflightChecksOpenedCOW(t *testing.T) {
	shape, launch, fetcher := coldPreparationFixture(t)
	diffPath := filepath.Join(shape.BaseRoot, "retained.diff")
	payload := make([]byte, 4096)
	payload[ext4SuperblockMagicOffset], payload[ext4SuperblockMagicOffset+1] = 0x53, 0xef
	writeTestDiff(t, diffPath, payload)
	launch.Root.Overlay = &config.OverlayConfig{Diff: "file://" + diffPath}
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(shape.BaseRoot, "replacement.diff")
	writeTestDiff(t, badPath, make([]byte, 4096))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	changed := make(chan error, 1)
	var ready atomic.Bool
	_, err = Run(ctx, RunOptions{Cfg: cfg, Fetcher: fetcher,
		SandboxID: shape.SandboxID, CHBinary: shape.CHBinary, RuntimeRoot: shape.RuntimeRoot, BaseRoot: shape.BaseRoot,
		NotifyReadiness: func(event ReadinessEvent) {
			if event == ReadinessRuntimeReady {
				changed <- os.Rename(badPath, diffPath)
			}
			if event == ReadinessReady {
				ready.Store(true)
				cancel()
			}
		}})
	select {
	case changeErr := <-changed:
		if changeErr != nil {
			t.Fatal(changeErr)
		}
	default:
		t.Fatalf("did not reach the replacement boundary: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "launch disk 0 writable filesystem") || ready.Load() {
		t.Fatalf("replaced COW passed live validation: ready=%t, error=%v", ready.Load(), err)
	}
	if opens, closes := fetcher.opens.Load(), fetcher.closes.Load(); opens != 3 || closes != opens {
		t.Fatalf("failure leaked/double-closed root readers: %d/%d", opens, closes)
	}
	// A caller-owned active diff survives failure and its writer lock is gone.
	cow, err := diffstore.OpenDisk(diffPath, nil, diffstore.DiffInit{Existing: true})
	if err != nil {
		t.Fatalf("failed live validation retained the COW writer: %v", err)
	}
	if err := cow.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestColdPreparationRequiresMatchingInputs(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Runtime, *LaunchSpec, *config.SandboxConfig)
	}{
		{"unchanged-once", nil},
		{"config", func(_ *Runtime, _ *LaunchSpec, cfg *config.SandboxConfig) { cfg.Launch.Exec = "/different" }},
		{"cgroup-capability", func(_ *Runtime, _ *LaunchSpec, cfg *config.SandboxConfig) { cfg.Resources.Control.CgroupFD = 99 }},
		{"portable", func(_ *Runtime, s *LaunchSpec, _ *config.SandboxConfig) { s.PortableConfig.Metadata["owner"] = "other" }},
		{"source", func(_ *Runtime, s *LaunchSpec, _ *config.SandboxConfig) { s.SourceBinding.RelativeDir += "/other" }},
		{"source-bundle", func(_ *Runtime, s *LaunchSpec, _ *config.SandboxConfig) {
			s.SourceBinding.BundleSource.Refs[0] = "different"
		}},
		{"source-selector", func(_ *Runtime, s *LaunchSpec, _ *config.SandboxConfig) {
			s.SourceBinding.BundleSource.Reader = new(manifestbundle.Reader)
		}},
		{"locations", func(_ *Runtime, s *LaunchSpec, _ *config.SandboxConfig) { s.RefLocations["images"] = "/different" }},
		{"manifest-access", func(_ *Runtime, s *LaunchSpec, _ *config.SandboxConfig) {
			s.ManifestConfig.Store.Endpoint = "different"
		}},
		{"fetcher", func(_ *Runtime, s *LaunchSpec, _ *config.SandboxConfig) { s.Fetcher = &coldPreparationFetcher{} }},
		{"storage", func(_ *Runtime, s *LaunchSpec, _ *config.SandboxConfig) { s.Storage = &artifact.ProcessStorage{} }},
		{"access-capsule", func(_ *Runtime, s *LaunchSpec, _ *config.SandboxConfig) {
			copy := *s.legacyAccess
			s.legacyAccess = &copy
		}},
		{"local-policy", func(_ *Runtime, s *LaunchSpec, _ *config.SandboxConfig) { s.legacyAccess.LocalRequired = true }},
		{"sandbox-id", func(r *Runtime, _ *LaunchSpec, _ *config.SandboxConfig) { r.sandboxID = "other" }},
		{"path-id", func(r *Runtime, _ *LaunchSpec, _ *config.SandboxConfig) { r.pathID = "other" }},
		{"base-dir", func(r *Runtime, _ *LaunchSpec, _ *config.SandboxConfig) { r.baseDir += "/other" }},
		{"run-root", func(r *Runtime, _ *LaunchSpec, _ *config.SandboxConfig) { r.spec.RuntimeRoot += "/other" }},
		{"disk-shape", func(r *Runtime, _ *LaunchSpec, _ *config.SandboxConfig) { r.spec.Disks[0].WritableCapacity *= 2 }},
		{"checkpoint-base", func(r *Runtime, _ *LaunchSpec, _ *config.SandboxConfig) {
			r.spec.checkpointPlan.Disks[0].BaseID = "other"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			shape, launch := sdkFixture(t)
			storage, err := artifact.NewProcessStorageWithCustomerKey(nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			launch.Storage = storage
			launch.legacyAccess = &RunOptions{}
			launch.RefLocations = config.RefLocations{"images": "/images"}
			launch.ManifestConfig = &config.ManifestConfig{}
			launch.PortableConfig = &config.PortableSandboxConfig{Metadata: map[string]string{"owner": "original"}}
			launch.SourceBinding = &RunSourceBinding{RelativeDir: "/sources", BundleSource: &BundleSourceBinding{Refs: []string{"original"}}}
			shape.checkpointPlan = &CheckpointDiskPlan{Disks: []CheckpointDiskBinding{{BaseID: "original"}}}
			prepared := &coldPreparation{imageDefaults: &ImageConfig{}}
			if err := prepared.seal(shape, &launch); err != nil {
				t.Fatal(err)
			}
			cfg, err := launch.config(shape.config())
			if err != nil {
				t.Fatal(err)
			}
			r := &Runtime{spec: shape, sandboxID: shape.SandboxID, pathID: shape.SandboxID, baseDir: DefaultBaseDir(shape.BaseRoot, shape.SandboxID)}
			if test.change != nil {
				test.change(r, &launch, cfg)
			}
			if got, want := prepared.reuse(r, launch, cfg), test.change == nil; got != want {
				t.Fatalf("preparation reuse = %t, want %t", got, want)
			}
			if test.change == nil && prepared.reuse(r, launch, cfg) {
				t.Fatal("preparation was consumed more than once")
			}
		})
	}
}
