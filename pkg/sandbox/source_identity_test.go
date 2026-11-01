package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"golang.org/x/sys/unix"
)

// These fixtures open a real Sandbox E and use the fake CH child from sdkFixture.
// They exercise the public source/derive/start boundary without a VM or KVM.
func sourceIdentityFixture(t *testing.T) (RuntimeSpec, *config.SandboxConfig, *RunSource) {
	t.Helper()
	shape, launch := sdkFixture(t)
	host, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	identities, err := ResolvePortableProjection(host)
	if err != nil {
		t.Fatal(err)
	}
	portable, err := config.ProjectPortableCold(host, identities)
	if err != nil {
		t.Fatal(err)
	}
	portable.Boot.Root.Base = "self"
	raw, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 4096)
	payload[1024+56], payload[1024+57] = 0x53, 0xef
	logical, err := sandboxfile.BuildSource(sparse.Dense(bytes.NewReader(payload), uint64(len(payload))), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	_, path, err := snapshot.NewFileSink(shape.RuntimeRoot, "source", nil, false, nil).AbsorbSandbox(context.Background(), logical)
	if err != nil {
		t.Fatal(err)
	}
	return shape, host, openIdentitySource(t, path)
}

func openIdentitySource(t *testing.T, path string) *RunSource {
	t.Helper()
	storage, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	source, err := OpenRunSource(context.Background(), path, storage, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source
}

func applyIdentitySource(t *testing.T, source *RunSource, host *config.SandboxConfig, replace bool) (*config.SandboxConfig, LaunchSpec) {
	t.Helper()
	cfg, launch, err := source.Apply(host, config.FieldPresence{}, config.ApplyFromOptions{ReplaceBoot: replace})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, launch
}

func deriveIdentityShape(t *testing.T, template RuntimeSpec, cfg *config.SandboxConfig, launch LaunchSpec) RuntimeSpec {
	t.Helper()
	shape, err := DeriveRuntimeSpecWithOptions(context.Background(), cfg, DeriveRuntimeOptions{
		Launch: launch, EnableCheckpoint: template.EnableCheckpoint,
		BaseRoot: template.BaseRoot, SandboxID: template.SandboxID, PathID: template.PathID,
	})
	if err != nil {
		t.Fatal(err)
	}
	shape.CHBinary, shape.RuntimeRoot = template.CHBinary, template.RuntimeRoot
	return shape
}

func readSourceIdentityRuntimeConfig(shape RuntimeSpec, checkpoint bool) (*config.PortableSandboxConfig, error) {
	pathID, err := ResolvePathID(shape.SandboxID, shape.PathID)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(shape.RuntimeRoot, pathID, config.SandboxRuntimeConfigName))
	if err != nil {
		return nil, err
	}
	if checkpoint {
		return config.ParseCheckpointSandboxConfig(raw)
	}
	return config.ParsePortableSandboxConfig(raw)
}

func sourceIdentityRuntimeConfig(t *testing.T, shape RuntimeSpec, checkpoint bool) *config.PortableSandboxConfig {
	t.Helper()
	portable, err := readSourceIdentityRuntimeConfig(shape, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	return portable
}

func assertNoIdentityStartup(t *testing.T, shape RuntimeSpec) {
	t.Helper()
	pathID, err := ResolvePathID(shape.SandboxID, shape.PathID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shape.RuntimeRoot, pathID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("identity error created runtime resources: %v", err)
	}
}

func TestRunSourceIdentitySharedByRunStartAndSplit(t *testing.T) {
	for _, entry := range []string{"run", "start", "split"} {
		for _, sidecar := range []bool{false, true} {
			t.Run(entry+"/"+fmtBool(sidecar), func(t *testing.T) {
				shape, host, source := sourceIdentityFixture(t)
				cfg, launch := applyIdentitySource(t, source, host, false)
				want := launch.PortableConfig.Boot
				kernel := strings.TrimPrefix(shape.Kernel, "file://")
				if err := os.WriteFile(kernel, []byte("different deployed bytes"), 0600); err != nil {
					t.Fatal(err)
				}
				if sidecar {
					digest := strings.Split(want.Kernel, "@digest:")[1]
					if err := os.WriteFile(kernel+".sha256", []byte(digest), 0600); err != nil {
						t.Fatal(err)
					}
				}
				watch := watchArtifactAccess(t, kernel)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var got *config.PortableSandboxConfig
				if entry == "run" {
					var readErr error
					_, err := Run(ctx, RunOptions{
						Cfg: cfg, PortableConfig: launch.PortableConfig, SourceBinding: launch.SourceBinding,
						Fetcher: launch.Fetcher, RefLocations: launch.RefLocations,
						SandboxID: shape.SandboxID, CHBinary: shape.CHBinary, RuntimeRoot: shape.RuntimeRoot, BaseRoot: shape.BaseRoot,
						NotifyReadiness: func(event ReadinessEvent) {
							if event == ReadinessReady {
								got, readErr = readSourceIdentityRuntimeConfig(shape, false)
								cancel()
							}
						},
					})
					if got == nil || readErr != nil {
						t.Fatalf("Run did not reach ready with valid C0: run=%v read=%v", err, readErr)
					}
				} else {
					shape = deriveIdentityShape(t, shape, cfg, launch)
					var r *Runtime
					var err error
					if entry == "split" {
						r, err = StartRuntime(ctx, shape)
						if err == nil {
							err = r.Launch(ctx, launch)
						}
					} else {
						r, err = Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
					}
					if r != nil {
						defer r.Close()
					}
					if err != nil {
						t.Fatal(err)
					}
					if r.unknownHostKernel == sidecar || (r.hostProjection.KernelRef == "") == sidecar {
						t.Fatalf("host identity incorrectly claimed or discarded: %+v unknown=%v", r.hostProjection, r.unknownHostKernel)
					}
					got = sourceIdentityRuntimeConfig(t, shape, false)
				}
				if got.Boot.Kernel != want.Kernel || got.Boot.Runtime != want.Runtime {
					t.Fatalf("source boot ancestry changed: %+v, want %+v", got.Boot, want)
				}
				if reads := watch.count(unix.IN_ACCESS); reads != 0 {
					t.Fatalf("portable %s read kernel payload %d times", entry, reads)
				}
			})
		}
	}
}

func TestRunSourceIdentityHostChecksAtStart(t *testing.T) {
	for _, mode := range []string{"missing-kernel", "missing-runtime", "bad-sidecar", "mismatched-sidecar", "changed-runtime"} {
		t.Run(mode, func(t *testing.T) {
			shape, host, source := sourceIdentityFixture(t)
			cfg, launch := applyIdentitySource(t, source, host, false)
			kernel, bundle := strings.TrimPrefix(shape.Kernel, "file://"), strings.TrimPrefix(shape.Bundle, "file://")
			if strings.HasPrefix(mode, "missing-") {
				path := kernel
				if mode == "missing-runtime" {
					path = bundle
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			// Shape inspection must work before host boot files are deployed.
			shape = deriveIdentityShape(t, shape, cfg, launch)
			if shape.preparedHost == nil || shape.preparedHost.projection != (config.PortableProjection{}) {
				t.Fatal("Derive inspected host identity instead of freezing bindings")
			}
			switch mode {
			case "bad-sidecar", "mismatched-sidecar":
				body := "invalid"
				if mode == "mismatched-sidecar" {
					body = strings.Repeat("f", 64)
				}
				// Added after Derive: StartRuntime must observe current metadata.
				if err := os.WriteFile(kernel+".sha256", []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			case "changed-runtime":
				raw, err := os.ReadFile(bundle)
				if err != nil {
					t.Fatal(err)
				}
				raw = bytes.ReplaceAll(raw, []byte(strings.Repeat("a", 64)), []byte(strings.Repeat("b", 64)))
				if err := os.WriteFile(bundle, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if r, err := StartRuntime(context.Background(), shape); err == nil || r != nil {
				if r != nil {
					r.Close()
				}
				t.Fatalf("StartRuntime accepted %s: %v", mode, err)
			}
			assertNoIdentityStartup(t, shape)
		})
	}
}

func TestRunSourceIdentityRejectsChangedProvenance(t *testing.T) {
	mutations := map[string]func(*LaunchSpec){
		"kernel": func(l *LaunchSpec) { l.PortableConfig.Boot.Kernel = testKernelRef("vmlinux", strings.Repeat("b", 64)) },
		"runtime": func(l *LaunchSpec) {
			l.PortableConfig.Boot.Runtime = testKernelRef("runtime.bundle", strings.Repeat("b", 64))
		},
		"sandbox-ref":      func(l *LaunchSpec) { l.SourceBinding.SandboxRef += "changed" },
		"runtime-ref":      func(l *LaunchSpec) { l.SourceBinding.RuntimeRef += "changed" },
		"source-directory": func(l *LaunchSpec) { l.SourceBinding.RelativeDir += "changed" },
		"locations":        func(l *LaunchSpec) { l.RefLocations = config.RefLocations{"source": "/changed"} },
		"checkpoint":       func(l *LaunchSpec) { l.SourceBinding.Checkpoint = &sandboxfile.Checkpoint{CheckpointID: "changed"} },
		"bundle":           func(l *LaunchSpec) { l.SourceBinding.BundleSource = &BundleSourceBinding{RootPath: "/changed"} },
	}
	for name, change := range mutations {
		t.Run(name, func(t *testing.T) {
			shape, host, source := sourceIdentityFixture(t)
			cfg, launch := applyIdentitySource(t, source, host, false)
			derived := deriveIdentityShape(t, shape, cfg, launch)
			change(&launch)
			if _, err := DeriveRuntimeSpecWithOptions(context.Background(), cfg, DeriveRuntimeOptions{Launch: launch}); err == nil {
				t.Fatal("Derive accepted changed source metadata")
			}
			if r, err := Start(context.Background(), SandboxSpec{Runtime: derived, Launch: launch}); err == nil || r != nil {
				if r != nil {
					r.Close()
				}
				t.Fatal("Start accepted changed source metadata", err)
			}
			assertNoIdentityStartup(t, derived)
		})
	}
	for _, binding := range []string{"kernel", "runtime"} {
		t.Run("host-"+binding, func(t *testing.T) {
			shape, host, source := sourceIdentityFixture(t)
			cfg, launch := applyIdentitySource(t, source, host, false)
			shape = deriveIdentityShape(t, shape, cfg, launch)
			if binding == "kernel" {
				shape.Kernel += ".changed"
			} else {
				shape.Bundle += ".changed"
			}
			if r, err := StartRuntime(context.Background(), shape); err == nil || !strings.Contains(err.Error(), "bindings differ") {
				if r != nil {
					r.Close()
				}
				t.Fatal("StartRuntime accepted changed host binding", err)
			}
			assertNoIdentityStartup(t, shape)
		})
	}
}

func TestRunSourceIdentityLaunchRequiresOriginalReceipt(t *testing.T) {
	shape, host, source := sourceIdentityFixture(t)
	cfg, launch := applyIdentitySource(t, source, host, false)
	shape = deriveIdentityShape(t, shape, cfg, launch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := StartRuntime(ctx, shape)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	manual := LaunchSpecFromConfig(cfg)
	manual.Storage, manual.SourceBinding, manual.PortableConfig = launch.Storage, launch.SourceBinding, launch.PortableConfig
	if err := r.Launch(ctx, manual); err == nil || !strings.Contains(err.Error(), "RunSource.Apply") || r.State() != "runtime_ready" {
		t.Fatalf("manual Launch bypassed receipt or consumed ready owner: %s / %v", r.State(), err)
	}
	originalKernel := launch.PortableConfig.Boot.Kernel
	launch.PortableConfig.Boot.Kernel = testKernelRef("vmlinux", strings.Repeat("b", 64))
	if err := r.Launch(ctx, launch); err == nil || r.State() != "runtime_ready" {
		t.Fatal("Launch accepted changed C0 or consumed ready owner", err)
	}
	launch.PortableConfig.Boot.Kernel = originalKernel
	if err := r.Launch(ctx, launch); err != nil {
		t.Fatal("valid source could not launch after rejected input", err)
	}
}

func TestRunSourceIdentityManualAndReplaceBootStayStrict(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual-source", true: "replace-boot"}[replace], func(t *testing.T) {
			shape, host, source := sourceIdentityFixture(t)
			cfg, launch := applyIdentitySource(t, source, host, replace)
			if !replace {
				manual := LaunchSpecFromConfig(cfg)
				manual.Storage, manual.SourceBinding, manual.PortableConfig = launch.Storage, launch.SourceBinding, launch.PortableConfig
				launch = manual
			} else if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			shape = deriveIdentityShape(t, shape, cfg, launch)
			if shape.preparedHost != nil || launch.sourceProof != nil {
				t.Fatal("manual source or ReplaceBoot acquired a portable identity exemption")
			}
			kernel := strings.TrimPrefix(shape.Kernel, "file://")
			if err := os.WriteFile(kernel, []byte("changed host kernel"), 0600); err != nil {
				t.Fatal(err)
			}
			watch := watchArtifactAccess(t, kernel)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
			if r != nil {
				defer r.Close()
			}
			if replace && err != nil || !replace && (err == nil || !strings.Contains(err.Error(), "kernel identity mismatch")) {
				t.Fatalf("strict Start(replace=%v) = %v", replace, err)
			}
			if reads := watch.count(unix.IN_ACCESS); reads != 1 {
				t.Fatalf("strict kernel payload reads=%d, want 1", reads)
			}
		})
	}
}

func TestRunSourceIdentityCheckpointMaterialization(t *testing.T) {
	for _, mode := range []string{"reopen", "materialize"} {
		t.Run(mode, func(t *testing.T) {
			shape, initial, paths, payloads := coldCheckpointHandoffFixture(t)
			host, err := initial.config(shape.config())
			if err != nil {
				t.Fatal(err)
			}
			source := openIdentitySource(t, initial.SourceBinding.RuntimeRef)
			cfg, launch := applyIdentitySource(t, source, host, false)
			if mode == "materialize" {
				shape.BaseRoot = filepath.Join(shape.BaseRoot, "new-location")
			}
			shape = deriveIdentityShape(t, shape, cfg, launch)
			kernel := strings.TrimPrefix(shape.Kernel, "file://")
			if err := os.WriteFile(kernel, []byte("different deployed bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			watch := watchArtifactAccess(t, kernel)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got := sourceIdentityRuntimeConfig(t, shape, mode == "reopen")
			if got.Boot.Kernel != initial.PortableConfig.Boot.Kernel || got.Boot.Runtime != initial.PortableConfig.Boot.Runtime {
				t.Fatal("checkpoint preparation changed boot ancestry")
			}
			if !launch.PortableConfig.IsCheckpoint() || launch.SourceBinding.Checkpoint == nil {
				t.Fatal("checkpoint preparation mutated caller source metadata")
			}
			for i, path := range paths {
				if checkpointHasReaders(t, path) {
					t.Fatal("source proof retained a checkpoint reader", path)
				}
				got := make([]byte, len(payloads[i]))
				if _, err := r.disks[i].cow.ReadAt(got, 0); err != nil || !bytes.Equal(got, payloads[i]) {
					t.Fatalf("checkpoint disk %d changed during %s: %v", i, mode, err)
				}
			}
			if reads := watch.count(unix.IN_ACCESS); reads != 0 {
				t.Fatalf("checkpoint source kernel reads=%d", reads)
			}
		})
	}
}
