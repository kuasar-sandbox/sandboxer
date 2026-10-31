package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestDeriveRuntimeSpecWithOptionsBindsSourceOnClone(t *testing.T) {
	ctx := context.Background()
	dir, dataDir := t.TempDir(), t.TempDir()
	lowerPath, scheme, digest := writeDiskArtifact(t, dir, "overlay", make([]byte, 8192), nil)
	lower := fileRef(filepath.Base(lowerPath), scheme, digest)
	dataPath, scheme, digest := writeDiskArtifact(t, dataDir, "overlay", make([]byte, 12288), nil)
	dataRef := manifest.Ref{Scheme: manifest.RefSchemeFile, Location: "data", Path: filepath.Base(dataPath), DigestScheme: scheme, Digest: digest}
	portable := snapshotTestLivePortable(t)
	portable.Boot.Root.BaseFromRefs = []string{lower}
	portable.Boot.Disks = []config.PortableDiskConfig{{Name: "data", PortableRootConfig: config.PortableRootConfig{Base: dataRef.String()}}}
	portable.Mounts = []config.MountConfig{{Type: "disk", Source: "data", Target: "/data"}}
	source := runtimeShapeSandboxSource(t, portable, 8192)
	ref, path, err := snapshot.NewFileSink(dir, "shape", nil, false, nil).AbsorbSandbox(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.SandboxConfig{Boot: config.BootConfig{
		Kernel: "file:///host/kernel", Runtime: "file:///host/runtime", Cmdline: "console=hvc0",
		Root:  config.RootConfig{Base: "self", BaseFromRefs: []string{lower}},
		Disks: []config.DiskConfig{{Name: "data", RootConfig: config.RootConfig{Base: dataRef.String()}}},
	}, Launch: config.LaunchConfig{Exec: "/bin/unchanged", EphemeralEnv: map[string]string{"INSTANCE": "value"}}}
	cfg.Resources.Capacity = config.CapacityConfig{CPU: 2, Memory: "128MiB"}
	storage, err := artifact.NewProcessStorageWithCustomerKey(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	launch := LaunchSpecFromConfig(cfg)
	launch.Storage, launch.PortableConfig = storage, portable
	launch.RefLocations = config.RefLocations{"data": dataDir}
	launch.SourceBinding = &RunSourceBinding{SandboxRef: ref, RuntimeRef: "file://" + path, RelativeDir: dir}
	before := runtimeShapeJSON(t, cfg)
	portableBefore := runtimeShapeJSON(t, portable)
	target := filepath.Join(t.TempDir(), "not-created")
	opts := DeriveRuntimeOptions{Launch: launch, BaseRoot: target, SandboxID: "logical", PathID: "task-path"}
	got, err := DeriveRuntimeSpecWithOptions(ctx, cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []RuntimeDiskSpec{{Name: "root", WritableCapacity: 8192}, {Name: "data", WritableCapacity: 12288}}
	if !reflect.DeepEqual(got.Disks, want) || got.Kernel != cfg.Boot.Kernel || got.Cmdline != cfg.Boot.Cmdline || got.Resources.Capacity != cfg.Resources.Capacity {
		t.Fatalf("derived shape = %+v", got)
	}
	if got.SandboxID != opts.SandboxID || got.PathID != opts.PathID || got.BaseRoot != target {
		t.Fatalf("shape lost runtime paths: %+v", got)
	}
	if !bytes.Equal(before, runtimeShapeJSON(t, cfg)) || !bytes.Equal(portableBefore, runtimeShapeJSON(t, portable)) || launch.Root.Base != "self" || launch.Root.BaseFromRefs[0] != lower {
		t.Fatal("shape derivation mutated the graph needed by Launch")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("derivation created target directory: %v", err)
	}

	// Run reaches the shared core after binding. Retained source provenance
	// must not cause the already-bound graph to require another self value.
	bound, err := cloneSDKConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindRuntimeDiskGraph(bound, portable, launch.SourceBinding); err != nil {
		t.Fatal(err)
	}
	fromRun, err := deriveBoundRuntimeSpec(ctx, bound, opts, nil)
	if err != nil || !reflect.DeepEqual(fromRun.Disks, want) {
		t.Fatalf("bound Run shape = %+v, %v", fromRun.Disks, err)
	}
}

func TestDeriveRuntimeSpecWithOptionsPreservesScopedBundleAccess(t *testing.T) {
	ctx := context.Background()
	key := [32]byte{0x61}
	keyFn := func() ([32]byte, error) { return key, nil }
	manifestCfg := &config.ManifestConfig{
		Manifest: manifest.ManifestSubConfig{WriteGeneration: "shape"},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes", Local: manifestcrypto.LocalOff},
	}
	dir := t.TempDir()
	sink, err := snapshot.NewBundleSink(ctx, dir, "shape", manifestCfg, keyFn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	lower, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(make([]byte, 8192)), nil)
	if err != nil {
		t.Fatal(err)
	}
	portable := snapshotTestLivePortable(t)
	portable.Boot.Root.BaseFromRefs = []string{lower}
	rootRef, _, err := sink.AbsorbSandbox(ctx, runtimeShapeSandboxSource(t, portable, 8192))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSandbox(ctx, rootRef, ""); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	rootKey, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, manifest.HexKey(rootKey)+".bundle")
	selector := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path, DigestScheme: "manifest", Digest: manifest.HexKey(rootKey)}
	remote := &runtimeShapeRejectFetcher{}
	storage, err := artifact.NewProcessStorageWithLocalAccess(manifestCfg, keyFn, nil, false, remote)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	opened, err := storage.OpenFile(ctx, path, selector)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	cfg := &config.SandboxConfig{Boot: config.BootConfig{Root: config.RootConfig{Base: "self", BaseFromRefs: []string{lower}}}}
	launch := LaunchSpec{Storage: storage, Fetcher: opened.ScopedFetcher(), ManifestConfig: manifestCfg,
		BundleReader: opened.BundleReader(), BundleFetcher: opened.ManifestFetcher(), PortableConfig: portable,
		SourceBinding: &RunSourceBinding{SandboxRef: rootRef, RuntimeRef: rootRef, RelativeDir: dir,
			BundleSource: &BundleSourceBinding{RootPath: path, Reader: opened.BundleReader(), Fetcher: opened.ManifestFetcher()}},
	}
	got, err := DeriveRuntimeSpecWithOptions(ctx, cfg, DeriveRuntimeOptions{Launch: launch})
	if err != nil || got.Disks[0].WritableCapacity != 8192 {
		t.Fatalf("scoped Bundle shape = %+v, %v", got.Disks, err)
	}
	launch.ManifestConfig = nil
	launch.SourceBinding.RuntimeRef = selector.String()
	got, err = DeriveRuntimeSpecWithOptions(ctx, cfg, DeriveRuntimeOptions{Launch: launch})
	if err != nil || got.Disks[0].WritableCapacity != 8192 {
		t.Fatalf("scoped file Bundle with storage-owned configuration = %+v, %v", got.Disks, err)
	}
	// A file selector must also retain the opaque storage's manifest crypto
	// policy; callers need not copy that policy into a second public field.
	bound := &config.SandboxConfig{Boot: config.BootConfig{Root: config.RootConfig{Base: selector.String()}}}
	if _, err := DeriveRuntimeSpec(ctx, bound, storage, nil); err != nil {
		t.Fatalf("opaque storage Bundle access: %v", err)
	}
	if remote.calls != 0 {
		t.Fatalf("source-scoped Bundle access fell back to remote %d times", remote.calls)
	}
	if _, err := opened.ReadAt(ctx, make([]byte, 1), 0); err != nil {
		t.Fatalf("derivation closed the borrowed Bundle: %v", err)
	}
}

type runtimeShapeRejectFetcher struct{ calls int }

func (f *runtimeShapeRejectFetcher) OpenManifest(context.Context, store.ContentKey) (fetch.Stream, error) {
	f.calls++
	return nil, readerr.Mark(errors.New("unexpected remote fetch"), false)
}

func TestDeriveRuntimeSpecWithOptionsUsesActualDefaultDiffPaths(t *testing.T) {
	base := t.TempDir()
	pathDir := filepath.Join(base, "task-path")
	if err := os.Mkdir(pathDir, 0700); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(pathDir, "logical.overlay.diff")
	dataPath := filepath.Join(pathDir, "logical.disk0.diff")
	writeTestDiff(t, rootPath, make([]byte, 20480))
	writeTestDiff(t, dataPath, make([]byte, 12288))
	template := filepath.Join(base, "template.diff")
	writeTestDiff(t, template, make([]byte, 4096))
	imagePath, scheme, digest := writeDiskArtifact(t, base, "image", lifecycleEROFSFixture(), nil)
	cfg := &config.SandboxConfig{Boot: config.BootConfig{
		Root: config.RootConfig{DiffTemplate: "file://" + template},
		Disks: []config.DiskConfig{{Name: "data", RootConfig: config.RootConfig{Base: fileRef(imagePath, scheme, digest),
			Overlay: &config.OverlayConfig{DiffTemplate: "file://" + template}}}},
	}}
	opts := DeriveRuntimeOptions{BaseRoot: base, SandboxID: "logical", PathID: "task-path"}
	got, err := DeriveRuntimeSpecWithOptions(context.Background(), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []RuntimeDiskSpec{{Name: "root", WritableCapacity: 20480}, {Name: "data", Overlay: true, BaseCapacity: 4096, WritableCapacity: 12288}}
	if !reflect.DeepEqual(got.Disks, want) {
		t.Fatalf("existing default DIFFs lost to templates: %+v", got.Disks)
	}
	// A PathID without a SandboxID does not license a guessed filename.
	opts.SandboxID = ""
	got, err = DeriveRuntimeSpecWithOptions(context.Background(), cfg, opts)
	if err != nil || got.SandboxID != "" || got.PathID != "task-path" || got.Disks[0].WritableCapacity != 4096 || got.Disks[1].WritableCapacity != 4096 {
		t.Fatalf("unknown identity shape = %+v, %v", got, err)
	}
	// An omitted PathID uses the logical SandboxID, just like StartRuntime.
	defaultDir := filepath.Join(base, "logical")
	if err := os.Mkdir(defaultDir, 0700); err != nil {
		t.Fatal(err)
	}
	writeTestDiff(t, filepath.Join(defaultDir, "logical.overlay.diff"), make([]byte, 8192))
	got, err = DeriveRuntimeSpecWithOptions(context.Background(), cfg, DeriveRuntimeOptions{BaseRoot: base, SandboxID: "logical"})
	if err != nil || got.PathID != "logical" || got.Disks[0].WritableCapacity != 8192 {
		t.Fatalf("default PathID shape = %+v, %v", got, err)
	}
	if err := os.WriteFile(rootPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := DeriveRuntimeSpecWithOptions(context.Background(), cfg, DeriveRuntimeOptions{BaseRoot: base, SandboxID: "logical", PathID: "task-path"}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty existing default DIFF incorrectly fell back to template: %v", err)
	}
}

func TestDeriveRuntimeSpecWithOptionsLocalCrypto(t *testing.T) {
	key := [32]byte{0x35}
	codec, err := manifestcrypto.NewTarStreamCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"encrypted-target", "plaintext-target", "plaintext-template", "wrong-key"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path, scheme, digest := writeDiskArtifact(t, dir, "overlay", make([]byte, 8192), codec)
			root := config.RootConfig{Base: fileRef(path, scheme, digest)}
			diffPath := filepath.Join(dir, "active.diff")
			if mode == "encrypted-target" {
				disk, err := diffstore.OpenDisk(diffPath, nil, diffstore.DiffInit{CreateSize: 16384}, diffstore.WithDiffEncryption(key, true))
				if err != nil {
					t.Fatal(err)
				}
				if err := disk.Close(); err != nil {
					t.Fatal(err)
				}
				root.Diff = "file://" + diffPath
			} else if mode == "plaintext-target" {
				writeTestDiff(t, diffPath, make([]byte, 16384))
				root.Diff = "file://" + diffPath
			} else if mode == "plaintext-template" {
				template := filepath.Join(dir, "template.diff")
				writeTestDiff(t, template, make([]byte, 12288))
				root.Diff, root.DiffTemplate = "file://"+diffPath, "file://"+template
			}
			calls := 0
			credentials := &ArtifactCredentials{Config: &config.ManifestConfig{Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalRequired}},
				CustomerKey: func() ([32]byte, error) {
					calls++
					if mode == "wrong-key" {
						return [32]byte{0xff}, nil
					}
					return key, nil
				},
			}
			cfg := &config.SandboxConfig{Boot: config.BootConfig{Root: root}}
			got, err := DeriveRuntimeSpecWithOptions(context.Background(), cfg, DeriveRuntimeOptions{Launch: LaunchSpec{Credentials: credentials}})
			switch mode {
			case "wrong-key":
				if !errors.Is(err, tarstream.ErrAuthentication) {
					t.Fatalf("wrong-key error = %v", err)
				}
			case "plaintext-target":
				if !errors.Is(err, diffstore.ErrDiffPlaintextForbidden) {
					t.Fatalf("plaintext target error = %v", err)
				}
			default:
				want := int64(16384)
				if mode == "plaintext-template" {
					want = 12288
					if _, statErr := os.Stat(diffPath); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("derivation created a target DIFF: %v", statErr)
					}
				}
				if err != nil || got.Disks[0].WritableCapacity != want {
					t.Fatalf("logical capacity = %+v, %v", got.Disks, err)
				}
			}
			if calls != 1 {
				t.Fatalf("temporary credential storage resolved key %d times", calls)
			}
		})
	}
}

func TestDeriveRuntimeSpecWithOptionsPinsCheckpointVersions(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plaintext", true: "encrypted"}[encrypted], func(t *testing.T) {
			cfg, launch, paths := runtimeShapeCheckpointFixture(t, encrypted)
			before := runtimeShapeJSON(t, cfg)
			target := filepath.Join(t.TempDir(), "not-created")
			got, err := DeriveRuntimeSpecWithOptions(context.Background(), cfg, DeriveRuntimeOptions{Launch: launch, BaseRoot: target, SandboxID: "new-run"})
			want := []RuntimeDiskSpec{{Name: "root", Overlay: true, BaseCapacity: 4096, WritableCapacity: 8192}, {Name: "data", WritableCapacity: 12288}}
			if err != nil || !reflect.DeepEqual(got.Disks, want) {
				t.Fatalf("checkpoint capacities = %+v, %v", got.Disks, err)
			}
			if !bytes.Equal(before, runtimeShapeJSON(t, cfg)) {
				t.Fatal("checkpoint derivation bound the caller's graph")
			}
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("checkpoint derivation materialized target directory: %v", err)
			}
			for _, path := range paths {
				if checkpointHasReaders(t, path) {
					t.Fatalf("source pin survived successful derivation: %s", path)
				}
				metadata, err := diffstore.OpenMetadata(path)
				if err != nil {
					t.Fatalf("derivation retained a writer: %v", err)
				}
				if err := metadata.Close(); err != nil {
					t.Fatal(err)
				}
			}
			// The borrowed storage remains usable after derivation; independent
			// exports retain their own reader lifetime.
			pins, err := launch.Storage.PinDiffCheckpoints(context.Background(), launch.SourceBinding.RelativeDir, launch.SourceBinding.Checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			if err := pins.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeriveRuntimeSpecWithOptionsReleasesCheckpointPinsOnFailure(t *testing.T) {
	for _, mode := range []string{"missing-version", "base-open-failure", "base-identity-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			cfg, launch, paths := runtimeShapeCheckpointFixture(t, false)
			switch mode {
			case "missing-version":
				metadata, err := diffstore.OpenMetadata(paths[1])
				if err != nil {
					t.Fatal(err)
				}
				err = errors.Join(metadata.DeleteCheckpoint(context.Background(), "source"), metadata.Close())
				if err != nil {
					t.Fatal(err)
				}
			case "base-open-failure":
				cfg.Boot.Root.Base = "file://missing.image@digest:" + strings.Repeat("1", 64)
			case "base-identity-mismatch":
				launch.PortableConfig.Boot.Root.Base = "file://other.image@digest:" + strings.Repeat("2", 64)
			}
			_, err := DeriveRuntimeSpecWithOptions(context.Background(), cfg, DeriveRuntimeOptions{Launch: launch})
			if err == nil {
				t.Fatal("invalid checkpoint source accepted")
			}
			if mode == "missing-version" && !errors.Is(err, diffstore.ErrNotExist) {
				t.Fatalf("missing named checkpoint fell back to active DIFF: %v", err)
			}
			for _, path := range paths {
				if checkpointHasReaders(t, path) {
					t.Fatalf("source pin survived failed derivation: %s", path)
				}
			}
		})
	}
}

func TestDeriveRuntimeSpecWithOptionsCheckpointLowerCapacity(t *testing.T) {
	for _, size := range []int{4096, 8192, 12288} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			ctx := context.Background()
			cfg, launch, paths := runtimeShapeCheckpointFixture(t, false)
			dir := launch.SourceBinding.RelativeDir
			path, scheme, digest := writeDiskArtifact(t, dir, "overlay", make([]byte, size), nil)
			ref := fileRef(filepath.Base(path), scheme, digest)
			cfg.Boot.Root.Overlay.Base = ref
			launch.PortableConfig.Boot.Root.Overlay.Base = ref
			if err := os.Remove(paths[0]); err != nil {
				t.Fatal(err)
			}
			disk, err := diffstore.Create(paths[0], diffstore.Options{DiffID: "root", BaseID: artifact.DiffCheckpointBaseID(launch.PortableConfig.Boot.Root), LogicalSize: 8192})
			if err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(disk.Checkpoint(ctx, "source"), disk.Close()); err != nil {
				t.Fatal(err)
			}
			logical, err := sandboxfile.BuildCheckpointSource(ctx, launch.PortableConfig, launch.SourceBinding.Checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			sourceRef, sourcePath, err := snapshot.NewFileSink(dir, "with-lower", nil, false, nil).AbsorbSandbox(ctx, logical)
			if err != nil {
				t.Fatal(err)
			}
			launch.SourceBinding.SandboxRef, launch.SourceBinding.RuntimeRef = sourceRef, "file://"+sourcePath
			got, err := DeriveRuntimeSpecWithOptions(ctx, cfg, DeriveRuntimeOptions{Launch: launch})
			if size > 8192 {
				if err == nil || !strings.Contains(err.Error(), "smaller than immutable base") {
					t.Fatalf("larger immutable lower accepted: %+v, %v", got.Disks, err)
				}
			} else if err != nil || got.Disks[0].WritableCapacity != 8192 {
				t.Fatalf("supported lower/checkpoint capacities rejected: %+v, %v", got.Disks, err)
			}
			for _, path := range paths {
				if checkpointHasReaders(t, path) {
					t.Fatalf("source pin survived derivation: %s", path)
				}
			}
		})
	}
}

func TestDeriveRuntimeSpecWithOptionsCheckpointWriterSelection(t *testing.T) {
	for _, mode := range []string{"new-source-capacity", "new-template", "current-reopen", "current-pathid-only", "historical-template", "cold-template"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			target := filepath.Join(base, "task", "checkpoint")
			sourceDir := t.TempDir()
			current := strings.HasPrefix(mode, "current-")
			if current || mode == "historical-template" {
				sourceDir = target
			}
			cfg, launch, paths := runtimeShapeCheckpointFixtureIn(t, false, sourceDir, current)
			if err := os.MkdirAll(filepath.Join(base, "task"), 0700); err != nil {
				t.Fatal(err)
			}
			// Checkpoint writers do not select the ordinary default DIFFs.
			writeTestDiff(t, filepath.Join(base, "task", "run.overlay.diff"), make([]byte, 16384))
			writeTestDiff(t, filepath.Join(base, "task", "run.disk0.diff"), make([]byte, 20480))
			wantRoot, wantData := int64(8192), int64(12288)
			if mode != "new-source-capacity" {
				templateDir := t.TempDir()
				rootTemplate, dataTemplate := filepath.Join(templateDir, "root.diff"), filepath.Join(templateDir, "data.diff")
				writeTestDiff(t, rootTemplate, make([]byte, 24576))
				writeTestDiff(t, dataTemplate, make([]byte, 28672))
				cfg.Boot.Root.Overlay.DiffTemplate, cfg.Boot.Disks[0].DiffTemplate = "file://"+rootTemplate, "file://"+dataTemplate
				if !current {
					wantRoot, wantData = 24576, 28672
				}
			}
			if mode == "cold-template" {
				if err := bindRuntimeDiskGraph(cfg, launch.PortableConfig, launch.SourceBinding); err != nil {
					t.Fatal(err)
				}
				launch.PortableConfig, launch.SourceBinding = nil, nil
			}
			before := runtimeShapeJSON(t, cfg)
			opts := DeriveRuntimeOptions{Launch: launch, EnableCheckpoint: true, BaseRoot: base, SandboxID: "run", PathID: "task"}
			if mode == "current-pathid-only" {
				opts.SandboxID = ""
			}
			got, err := DeriveRuntimeSpecWithOptions(ctx, cfg, opts)
			if err != nil || got.Disks[0].WritableCapacity != wantRoot || got.Disks[1].WritableCapacity != wantData || !got.EnableCheckpoint {
				t.Fatalf("checkpoint writer shape = %+v, enabled=%v, err=%v; want %d/%d", got.Disks, got.EnableCheckpoint, err, wantRoot, wantData)
			}
			if !bytes.Equal(before, runtimeShapeJSON(t, cfg)) {
				t.Fatal("checkpoint selection mutated caller config")
			}
			if sourceDir != target {
				if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("derivation created checkpoint target: %v", err)
				}
			}
			for _, path := range paths {
				if checkpointHasReaders(t, path) {
					t.Fatalf("source pin survived derivation: %s", path)
				}
			}
			// Compare against the real preparation policy on an independent clone.
			// This fixture writes local checkpoint metadata; it starts no VM.
			clone, err := cloneSDKConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			run, release, err := prepareColdCheckpoint(ctx, RunOptions{Cfg: clone, EnableCheckpoint: true, PortableConfig: launch.PortableConfig, SourceBinding: launch.SourceBinding, checkpointStorage: launch.Storage, BaseRoot: base, SandboxID: opts.SandboxID, PathID: "task"})
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if run.SourceBinding != nil {
				if err := bindRuntimeDiskGraph(run.Cfg, run.PortableConfig, run.SourceBinding); err != nil {
					t.Fatal(err)
				}
			}
			actualLaunch := LaunchSpecFromConfig(run.Cfg)
			actualLaunch.Storage, actualLaunch.legacyAccess = launch.Storage, &run
			actual, err := deriveBoundRuntimeSpec(ctx, run.Cfg, DeriveRuntimeOptions{Launch: actualLaunch, EnableCheckpoint: true, BaseRoot: base, SandboxID: opts.SandboxID, PathID: "task"}, nil)
			if err != nil || !reflect.DeepEqual(actual.Disks, got.Disks) {
				t.Fatalf("read-only shape=%+v; prepared shape=%+v, err=%v", got.Disks, actual.Disks, err)
			}
		})
	}
}

func TestDeriveRuntimeSpecWithOptionsCheckpointExplicitTarget(t *testing.T) {
	for _, mode := range []string{"new-target", "existing-target", "outside", "unknown-directory"} {
		t.Run(mode, func(t *testing.T) {
			cfg, launch, _ := runtimeShapeCheckpointFixture(t, false)
			base := t.TempDir()
			path := filepath.Join(base, "task", "checkpoint", "diffs", "requested.diff")
			opts := DeriveRuntimeOptions{Launch: launch, EnableCheckpoint: true, BaseRoot: base, SandboxID: "run", PathID: "task"}
			switch mode {
			case "existing-target":
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				writeTestDiff(t, path, make([]byte, 16384))
			case "outside":
				path = filepath.Join(base, "requested.diff")
			case "unknown-directory":
				opts.SandboxID, opts.PathID = "", ""
			}
			cfg.Boot.Root.Overlay.Diff = "file://" + path
			got, err := DeriveRuntimeSpecWithOptions(context.Background(), cfg, opts)
			if mode == "new-target" {
				if err != nil || got.Disks[0].WritableCapacity != 8192 {
					t.Fatalf("valid new target = %+v, %v", got.Disks, err)
				}
				if _, err := os.Stat(filepath.Join(base, "task")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("derivation created target directory: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid explicit checkpoint target accepted")
			}
		})
	}
}

func TestDeriveRuntimeSpecCompatibilityAndCanceledAccess(t *testing.T) {
	if _, err := DeriveRuntimeSpec(context.Background(), &config.SandboxConfig{}, nil, nil); err == nil {
		t.Fatal("legacy helper accepted nil storage")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	keyCalled := false
	_, err := DeriveRuntimeSpecWithOptions(ctx, &config.SandboxConfig{}, DeriveRuntimeOptions{Launch: LaunchSpec{Credentials: &ArtifactCredentials{
		Config: &config.ManifestConfig{Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalRequired}},
		CustomerKey: func() ([32]byte, error) {
			keyCalled = true
			return [32]byte{}, nil
		},
	}}})
	if !errors.Is(err, context.Canceled) || keyCalled {
		t.Fatalf("canceled derivation opened credentials: %v, called=%v", err, keyCalled)
	}
}

func runtimeShapeCheckpointFixture(t *testing.T, encrypted bool) (*config.SandboxConfig, LaunchSpec, []string) {
	t.Helper()
	return runtimeShapeCheckpointFixtureIn(t, encrypted, t.TempDir(), false)
}

func runtimeShapeCheckpointFixtureIn(t *testing.T, encrypted bool, dir string, current bool) (*config.SandboxConfig, LaunchSpec, []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "diffs"), 0700); err != nil {
		t.Fatal(err)
	}
	key := [32]byte{0x29}
	var codec tarstream.Codec
	var storeKey *[32]byte
	if encrypted {
		var err error
		codec, err = manifestcrypto.NewTarStreamCodec(key)
		if err != nil {
			t.Fatal(err)
		}
		storeKey = &key
	}
	keyFn := func() ([32]byte, error) { return key, nil }
	imagePath, scheme, digest := writeDiskArtifact(t, dir, "image", lifecycleEROFSFixture(), codec)
	imageRef := fileRef(filepath.Base(imagePath), scheme, digest)
	portable := snapshotTestPortable(t)
	portable.Boot.Root.Base = imageRef
	portable.Boot.Disks = []config.PortableDiskConfig{{Name: "data"}}
	portable.Mounts = []config.MountConfig{{Type: "disk", Source: "data", Target: "/data"}}
	portable, err := portable.Checkpointed("")
	if err != nil {
		t.Fatal(err)
	}
	cp := &sandboxfile.Checkpoint{Version: 1, CheckpointID: "source", Root: sandboxfile.CheckpointDisk{DiffID: "root"}, Disks: []sandboxfile.CheckpointNamedDisk{{Name: "data", DiffID: "data"}}}
	var paths []string
	for i, id := range []string{"root", "data"} {
		base := portable.Boot.Root
		if i == 1 {
			base = portable.Boot.Disks[0].PortableRootConfig
		}
		path := filepath.Join(dir, "diffs", id+".diff")
		disk, err := diffstore.Create(path, diffstore.Options{DiffID: id, BaseID: artifact.DiffCheckpointBaseID(base), LogicalSize: int64(8192 + i*4096), Key: storeKey, RequireEncryption: encrypted})
		if err != nil {
			t.Fatal(err)
		}
		if err := disk.Checkpoint(context.Background(), cp.CheckpointID); err != nil {
			t.Fatal(err)
		}
		if !current {
			if err := disk.Checkpoint(context.Background(), "later"); err != nil {
				t.Fatal(err)
			}
		}
		if err := disk.Close(); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	source, err := sandboxfile.BuildCheckpointSource(context.Background(), portable, cp)
	if err != nil {
		t.Fatal(err)
	}
	ref, path, err := snapshot.NewFileSink(dir, "shape", codec, encrypted, nil).AbsorbSandbox(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := artifact.NewProcessStorageWithLocalAccess(nil, keyFn, codec, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	cfg := &config.SandboxConfig{Boot: config.BootConfig{Root: config.RootConfig{Base: imageRef, Overlay: &config.OverlayConfig{}}, Disks: []config.DiskConfig{{Name: "data"}}}}
	launch := LaunchSpec{Storage: storage, PortableConfig: portable, SourceBinding: &RunSourceBinding{SandboxRef: ref, RuntimeRef: "file://" + path, RelativeDir: dir, Checkpoint: cp}}
	return cfg, launch, paths
}

func runtimeShapeSandboxSource(t *testing.T, portable *config.PortableSandboxConfig, size int) sparse.Source {
	t.Helper()
	raw, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	source, err := sandboxfile.BuildSourceContext(context.Background(), sparse.Dense(bytes.NewReader(make([]byte, size)), uint64(size)), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func runtimeShapeJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
