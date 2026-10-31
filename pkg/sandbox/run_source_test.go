package sandbox

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	storefs "github.com/kuasar-sandbox/accelerator/pkg/store/fs"
	"github.com/kuasar-sandbox/accelerator/pkg/store/pb"
	storeserver "github.com/kuasar-sandbox/accelerator/pkg/store/server"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestOpenRunSourceLocalTarstream(t *testing.T) {
	dir := t.TempDir()
	portable := testRunFromPortable(t)
	runtimeBytes, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := sparse.NewSource(bytes.NewReader(bytes.Repeat([]byte{0x5a}, 4096)), 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := sandboxfile.BuildSource(payload, nil, runtimeBytes)
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := os.CreateTemp(dir, "sandbox-*.partial")
	if err != nil {
		t.Fatal(err)
	}
	scheme, digest, err := tarstream.WriteTo(context.Background(), tmp, "sandbox", logical)
	if err != nil {
		_ = tmp.Close()
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "alias.sandbox")
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}
	storage, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	source, err := OpenRunSource(context.Background(), path, storage, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if source.runtimeRef != "file://"+path+"@"+scheme+":"+digest {
		t.Fatalf("runtime ref = %q", source.runtimeRef)
	}
	wantPortable := "file://" + digest + ".sandbox@" + scheme + ":" + digest
	if source.portableRef != wantPortable {
		t.Fatalf("portable ref = %q, want %q", source.portableRef, wantPortable)
	}
	if source.relativeDir != dir {
		t.Fatalf("relative dir = %q, want %q", source.relativeDir, dir)
	}
	if !bytes.Equal(source.root.RuntimeConfig, runtimeBytes) {
		t.Fatal("runtime config changed")
	}
}

func TestOpenRunSourceEncryptedTarstream(t *testing.T) {
	ctx := context.Background()
	key := [32]byte{0x31, 0x32, 0x33}
	cfg := runFromManifestConfig(key, "")
	cfg.Crypto.Local = manifestcrypto.LocalRequired
	storage, err := artifact.NewProcessStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	logical := runFromLogical(t, runFromDirectPortable(t))
	_, path, err := snapshot.NewFileSink(t.TempDir(), "encrypted", storage.LocalCodec(), storage.LocalRequired(), nil).
		AbsorbSandbox(ctx, logical)
	if err != nil {
		t.Fatal(err)
	}
	source, err := OpenRunSource(ctx, path, storage, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if source.root.Portable.Boot.Root.Base != "self" || !strings.Contains(source.portableRef, "@hmac:") {
		t.Fatalf("encrypted Sandbox source = ref:%q config:%+v", source.portableRef, source.root.Portable.Boot.Root)
	}
}

func TestOpenRunSourceManifestBundleDefaultAndExplicitRoot(t *testing.T) {
	ctx := context.Background()
	key := [32]byte{0x41, 0x42, 0x43}
	cfg := runFromManifestConfig(key, "")
	storage, err := artifact.NewProcessStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	dir := t.TempDir()
	sink, err := snapshot.NewBundleSink(ctx, dir, "bundle", cfg, storage.CustomerKeyFunc(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rootRef, _, err := sink.AbsorbSandbox(ctx, runFromLogical(t, runFromDirectPortable(t)))
	if err == nil {
		err = sink.CommitSandbox(ctx, rootRef, "")
	}
	closeErr := sink.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("build Bundle err=%v close=%v", err, closeErr)
	}
	rootKey := strings.TrimPrefix(rootRef, "manifest://")
	alias := filepath.Join(dir, "bundle.sandbox")
	for _, input := range []string{
		alias,
		"file://" + filepath.Clean(mustEvalSymlinks(t, alias)) + "@manifest:" + rootKey,
		"file://bundle.sandbox@location:source",
	} {
		t.Run(filepath.Base(input), func(t *testing.T) {
			source, err := OpenRunSource(ctx, input, storage, config.RefLocations{"source": dir})
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			if source.bundleReader == nil || source.bundleFetcher == nil || source.bundleSource == nil {
				t.Fatalf("Bundle source bindings are incomplete: %+v", source)
			}
			if source.bundleSource.Reader != source.bundleReader || source.bundleSource.Fetcher != source.bundleFetcher {
				t.Fatal("Bundle source provenance is not paired with its reader/fetcher")
			}
			if !strings.Contains(source.portableRef, "@manifest:"+rootKey) {
				t.Fatalf("portable Bundle ref = %q", source.portableRef)
			}
			_, launch, err := source.Apply(&config.SandboxConfig{}, config.FieldPresence{}, config.ApplyFromOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if launch.SourceBinding.BundleSource.Reader != launch.BundleReader || launch.SourceBinding.BundleSource.Fetcher != launch.BundleFetcher || launch.Fetcher == nil {
				t.Fatal("Apply did not preserve paired Bundle access")
			}
			key, err := manifest.ParseKeyRef(rootKey)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := launch.Fetcher.OpenManifest(ctx, key)
			if err != nil {
				t.Fatalf("source-scoped root is unavailable: %v", err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			host := &config.SandboxConfig{Boot: config.BootConfig{
				Kernel: "file:///replacement/kernel", Runtime: "file:///replacement/runtime",
				Root: config.RootConfig{DiffTemplate: "file:///replacement/root.ext4"},
			}}
			host.ApplyDefaults()
			replacement, next, err := source.Apply(host, config.FieldPresence{}, config.ApplyFromOptions{ReplaceBoot: true})
			if err != nil {
				t.Fatal(err)
			}
			if next.SourceBinding != nil || next.PortableConfig != nil || next.BundleReader != nil || next.BundleFetcher != nil || next.Fetcher != nil || next.Storage != storage {
				t.Fatal("ReplaceBoot retained source provenance or source-scoped access")
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			if err := replacement.ValidateCold(); err != nil || next.Root.DiffTemplate != "file:///replacement/root.ext4" {
				t.Fatalf("replacement depends on the closed source: cfg=%+v err=%v", replacement.Boot, err)
			}
		})
	}
}

func TestOpenRunSourceManifestStore(t *testing.T) {
	ctx := context.Background()
	cfg, storage := runFromManifestStore(t)
	ingester, err := cfg.NewIngester(storage.CustomerKeyFunc(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, absorbErr := snapshot.NewIngestSink(ingester, nil).
		AbsorbSandbox(ctx, runFromLogical(t, runFromDirectPortable(t)))
	closeErr := ingester.Close()
	if absorbErr != nil || closeErr != nil {
		t.Fatalf("ingest Sandbox err=%v close=%v", absorbErr, closeErr)
	}
	source, err := OpenRunSource(ctx, ref, storage, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if source.portableRef != ref || source.runtimeRef != ref || source.root.Portable.Boot.Root.Base != "self" {
		t.Fatalf("Manifest Sandbox source = %+v", source)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	// Source closure must not close the caller's process-owned manifest client.
	again, err := OpenRunSource(ctx, ref, storage, nil)
	if err != nil {
		t.Fatalf("borrowed storage was closed with source: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunSourceApplyPreservesHostInputs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	storage, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	_, path, err := snapshot.NewFileSink(dir, "source", nil, false, nil).AbsorbSandbox(ctx, runFromLogical(t, runFromDirectPortable(t)))
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "source.sandbox")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	locations := config.RefLocations{"other": dir}
	source, err := OpenRunSource(ctx, alias, storage, locations)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	locations["other"] = "/changed-after-open"
	host, presence, err := config.LoadConfigBytesWithPresence([]byte(`
launch:
  env: {PERSISTENT: override}
  ephemeral_env: {TOKEN: current}
ephemeral_files:
  - path: /run/token
    content: current
`))
	if err != nil {
		t.Fatal(err)
	}
	host.Resources.Control.CgroupFD = 47
	cfg, launch, err := source.Apply(host, presence, config.ApplyFromOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.Control.CgroupFD != 47 || host.Resources.Control.CgroupFD != 47 {
		t.Fatal("Apply lost the borrowed cgroup descriptor")
	}
	if cfg.Boot.Kernel != "file://"+filepath.Join(dir, "vmlinux") || cfg.Boot.Runtime != "file://"+filepath.Join(dir, "runtime.bundle") || launch.SourceBinding.RelativeDir != dir {
		t.Fatalf("boot bindings did not follow the source target: cfg=%+v source=%+v", cfg.Boot, launch.SourceBinding)
	}
	if host.Boot.Kernel != "" || host.Boot.Runtime != "" || launch.Root.Base != "self" {
		t.Fatal("Apply mutated host bindings or prematurely bound the source graph")
	}
	if launch.Storage != storage || launch.RefLocations["other"] != dir || launch.Process.Env["PERSISTENT"] != "override" || launch.Process.EphemeralEnv["TOKEN"] != "current" || len(launch.EphemeralFiles) != 1 {
		t.Fatal("Apply lost storage, overrides or ephemeral input")
	}
	if _, leaked := launch.PortableConfig.Launch.Env["TOKEN"]; leaked {
		t.Fatal("ephemeral input leaked into portable C0")
	}
	cfg.Launch.EphemeralEnv["TOKEN"] = "changed"
	launch.PortableConfig.Launch.Exec = "/changed"
	launch.RefLocations["other"] = "/changed-after-apply"
	if host.Launch.EphemeralEnv["TOKEN"] != "current" {
		t.Fatal("returned config aliases host maps")
	}
	_, again, err := source.Apply(host, presence, config.ApplyFromOptions{})
	if err != nil || again.PortableConfig.Launch.Exec != "/bin/true" || again.RefLocations["other"] != dir {
		t.Fatalf("Apply reused caller-mutated source metadata: %+v / %v", again, err)
	}
	if _, _, err := source.Apply(host, presence, config.ApplyFromOptions{ReplaceBoot: true}); err == nil {
		t.Fatal("ReplaceBoot filled missing host boot from source")
	}
}

func TestOpenRunSourceLocatedAliasStaysWithinLocation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	storage, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	_, target, err := snapshot.NewFileSink(dir, "source", nil, false, nil).AbsorbSandbox(ctx, runFromLogical(t, runFromDirectPortable(t)))
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.sandbox")
	if err := os.Symlink(filepath.Base(target), alias); err != nil {
		t.Fatal(err)
	}
	ref := "file://alias.sandbox@location:source"
	source, err := OpenRunSource(ctx, ref, storage, config.RefLocations{"source": dir})
	if err != nil {
		t.Fatalf("same-directory located alias rejected: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	escapeDir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(escapeDir, "alias.sandbox")); err != nil {
		t.Fatal(err)
	}
	if source, err := OpenRunSource(ctx, ref, storage, config.RefLocations{"source": escapeDir}); source != nil || err == nil || !strings.Contains(err.Error(), "escapes ref-location") {
		t.Fatalf("cross-directory located alias accepted: source=%v err=%v", source, err)
	}
}

func TestRunSourceApplyCheckpointProvenance(t *testing.T) {
	ctx := context.Background()
	portable, err := runFromDirectPortable(t).Checkpointed("")
	if err != nil {
		t.Fatal(err)
	}
	portable.Boot.Disks = []config.PortableDiskConfig{{Name: "data"}}
	portable.Mounts = []config.MountConfig{{Type: "disk", Source: "data", Target: "/data"}}
	checkpoint := &sandboxfile.Checkpoint{
		Version: 1, CheckpointID: "source", Root: sandboxfile.CheckpointDisk{DiffID: "root"},
		Disks: []sandboxfile.CheckpointNamedDisk{{Name: "data", DiffID: "data"}},
	}
	logical, err := sandboxfile.BuildCheckpointSource(ctx, portable, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	_, path, err := snapshot.NewFileSink(t.TempDir(), "checkpoint", nil, false, nil).AbsorbSandbox(ctx, logical)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	source, err := OpenRunSource(ctx, path, storage, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	_, launch, err := source.Apply(&config.SandboxConfig{}, config.FieldPresence{}, config.ApplyFromOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if launch.SourceBinding.Checkpoint == nil || launch.SourceBinding.Checkpoint.CheckpointID != "source" || !launch.PortableConfig.IsCheckpoint() {
		t.Fatal("checkpoint provenance missing from Apply")
	}
	launch.SourceBinding.Checkpoint.Root.DiffID = "changed"
	launch.SourceBinding.Checkpoint.Disks[0].DiffID = "changed"
	_, again, err := source.Apply(&config.SandboxConfig{}, config.FieldPresence{}, config.ApplyFromOptions{})
	if err != nil || again.SourceBinding.Checkpoint.Root.DiffID != "root" || again.SourceBinding.Checkpoint.Disks[0].DiffID != "data" {
		t.Fatalf("Apply reused caller-mutated checkpoint metadata: %+v / %v", again.SourceBinding, err)
	}
}

type runSourceCountingStream struct {
	sparse.Source
	closes int
	err    error
}

func (s *runSourceCountingStream) Close() error { s.closes++; return s.err }

type runSourceFetcher struct{ stream fetch.Stream }

func (f runSourceFetcher) OpenManifest(context.Context, store.ContentKey) (fetch.Stream, error) {
	return f.stream, nil
}

func TestRunSourceOwnsOnlyRootAndRejectsApplyAfterClose(t *testing.T) {
	closeErr := errors.New("source close error")
	for _, valid := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid", true: "valid"}[valid], func(t *testing.T) {
			stream := &runSourceCountingStream{Source: sparse.Dense(bytes.NewReader([]byte("invalid")), 7), err: closeErr}
			if valid {
				stream.Source = runFromLogical(t, runFromDirectPortable(t))
			}
			storage, err := artifact.NewProcessStorageWithLocalAccess(nil, nil, nil, false, runSourceFetcher{stream})
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			source, err := OpenRunSource(context.Background(), "manifest://"+strings.Repeat("a", 64), storage, nil)
			if !valid {
				if source != nil || !errors.Is(err, closeErr) || stream.closes != 1 {
					t.Fatalf("failed open leaked or lost cleanup error: source=%v err=%v closes=%d", source, err, stream.closes)
				}
				return
			}
			if err != nil || stream.closes != 0 {
				t.Fatalf("open prematurely closed source: err=%v closes=%d", err, stream.closes)
			}
			for i := 0; i < 2; i++ {
				if err := source.Close(); !errors.Is(err, closeErr) || stream.closes != 1 {
					t.Fatalf("close is not retained/idempotent: err=%v closes=%d", err, stream.closes)
				}
			}
			if _, _, err := source.Apply(&config.SandboxConfig{}, config.FieldPresence{}, config.ApplyFromOptions{}); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("Apply accepted closed source: %v", err)
			}
		})
	}
}

func TestOpenRunSourceManifestReadRecovery(t *testing.T) {
	for _, kind := range []string{"recover", "cancel", "permanent"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(context.Canceled)
			timer := time.AfterFunc(5*time.Second, func() { cancel(context.DeadlineExceeded) })
			defer timer.Stop()
			stop := errors.New("run source operation stopped")
			var active atomic.Bool
			var calls atomic.Int32
			cfg, storage := runFromManifestStore(t, grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
				if active.Load() && strings.HasSuffix(info.FullMethod, "/Get") {
					n := calls.Add(1)
					switch kind {
					case "recover":
						if n <= 2 {
							return status.Error([]codes.Code{codes.Unavailable, codes.DeadlineExceeded}[n-1], "injected source attempt")
						}
					case "cancel":
						cancel(stop)
						return status.Error(codes.Canceled, "operation canceled")
					case "permanent":
						return status.Error(codes.InvalidArgument, "invalid source request")
					}
				}
				return handler(srv, stream)
			}))
			ingester, err := cfg.NewIngester(storage.CustomerKeyFunc(), nil)
			if err != nil {
				t.Fatal(err)
			}
			ref, _, err := snapshot.NewIngestSink(ingester, nil).AbsorbSandbox(ctx, runFromLogical(t, runFromDirectPortable(t)))
			closeErr := ingester.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("seed Sandbox: %v / %v", err, closeErr)
			}
			active.Store(true)
			source, err := OpenRunSource(ctx, ref, storage, nil)
			if kind == "recover" {
				if err != nil {
					t.Fatal(err)
				}
				defer source.Close()
				if calls.Load() < 3 || source.portableRef != ref || source.runtimeRef != ref || source.root.Portable.Boot.Root.Base != "self" {
					t.Fatalf("source did not recover intact: calls=%d source=%+v", calls.Load(), source)
				}
			} else if calls.Load() != 1 || source != nil || err == nil || kind == "cancel" && !errors.Is(err, stop) || kind == "permanent" && !readerr.IsPermanent(err) {
				t.Fatalf("source terminal changed: calls=%d source=%v err=%v", calls.Load(), source, err)
			}
		})
	}
}

func testRunFromPortable(t *testing.T) *config.PortableSandboxConfig {
	t.Helper()
	const a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const b = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cfg := &config.PortableSandboxConfig{
		Version: config.PortableSandboxConfigVersion,
		Resources: config.PortableResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 1, Memory: "1GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1, Memory: "1GiB"},
		},
		Boot: config.PortableBootConfig{
			Kernel: "file://vmlinux@digest:" + a, Runtime: "file://runtime.bundle@digest:" + b,
			Root: config.PortableRootConfig{Base: "file://base.erofs@digest:" + a, Overlay: &config.PortableOverlayConfig{Base: "self"}},
		},
		Launch: config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func runFromDirectPortable(t *testing.T) *config.PortableSandboxConfig {
	t.Helper()
	cfg := testRunFromPortable(t)
	cfg.Boot.Root = config.PortableRootConfig{Base: "self"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func runFromLogical(t *testing.T, portable *config.PortableSandboxConfig) sparse.Source {
	t.Helper()
	runtimeBytes, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := sparse.NewSource(bytes.NewReader(bytes.Repeat([]byte{0x6d}, 4096)), 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := sandboxfile.BuildSource(payload, nil, runtimeBytes)
	if err != nil {
		t.Fatal(err)
	}
	return logical
}

func runFromManifestConfig(key [32]byte, endpoint string) *config.ManifestConfig {
	return &config.ManifestConfig{
		Manifest: manifest.ManifestSubConfig{Key: hex.EncodeToString(key[:]), WriteGeneration: "G1"},
		Store:    manifest.StoreConfig{Endpoint: endpoint, Pool: 2, Timeout: "5s"},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes", Local: manifestcrypto.LocalOff},
	}
}

func runFromManifestStore(t *testing.T, options ...grpc.ServerOption) (*config.ManifestConfig, *artifact.ProcessStorage) {
	t.Helper()
	backend, err := storefs.New(storefs.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	server, err := storeserver.New(storeserver.Options{
		Backend: backend, VerifyKey: true,
		Generations: func() []store.Generation { return []store.Generation{"G1"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer(options...)
	pb.RegisterStoreServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})
	cfg := runFromManifestConfig([32]byte{0x51, 0x52, 0x53}, listener.Addr().String())
	storage, err := artifact.NewProcessStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	return cfg, storage
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return realPath
}

// Rotate a published semantic alias after the Bundle reader is open but before
// OpenRunSource attaches physical provenance. The opened source must stay fixed.
func TestRunSourceAliasRotationKeepsOpenedProvenance(t *testing.T) {
	ctx := context.Background()
	key := [32]byte{0x41, 0x42, 0x43}
	cfg := runFromManifestConfig(key, "")
	writerStorage, err := artifact.NewProcessStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer writerStorage.Close()
	dir := t.TempDir()
	build := func(sid, exec string) (string, string) {
		t.Helper()
		sink, err := snapshot.NewBundleSink(ctx, dir, sid, cfg, writerStorage.CustomerKeyFunc(), nil)
		if err != nil {
			t.Fatal(err)
		}
		portable := runFromDirectPortable(t)
		portable.Launch.Exec = exec
		root, _, err := sink.AbsorbSandbox(ctx, runFromLogical(t, portable))
		if err == nil {
			err = sink.CommitSandbox(ctx, root, "")
		}
		closeErr := sink.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("build: %v, close: %v", err, closeErr)
		}
		return mustEvalSymlinks(t, filepath.Join(dir, sid+".sandbox")), root
	}
	first, firstRef := build("before", "/bin/true")
	second, _ := build("after", "/bin/false")
	for _, located := range []bool{false, true} {
		name := "plain"
		if located {
			name = "located"
		}
		t.Run(name, func(t *testing.T) {
			alias := filepath.Join(dir, name+".sandbox")
			if err := os.Symlink(filepath.Base(first), alias); err != nil {
				t.Fatal(err)
			}
			rotate := func() ([32]byte, error) {
				next := alias + ".next"
				if err := os.Symlink(filepath.Base(second), next); err != nil {
					return [32]byte{}, err
				}
				if err := os.Rename(next, alias); err != nil {
					return [32]byte{}, err
				}
				return key, nil
			}
			storage, err := artifact.NewProcessStorageWithCustomerKey(cfg, rotate)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			raw := alias
			if located {
				raw = "file://" + filepath.Base(alias) + "@location:source"
			}
			source, err := OpenRunSource(ctx, raw, storage, config.RefLocations{"source": dir})
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			_, launch, err := source.Apply(&config.SandboxConfig{}, config.FieldPresence{}, config.ApplyFromOptions{})
			if err != nil {
				t.Fatal(err)
			}
			runtimeRef, err := manifest.ParseRef(launch.SourceBinding.RuntimeRef)
			if err != nil {
				t.Fatal(err)
			}
			runtimePath, err := launch.RefLocations.ResolveFile(runtimeRef, "")
			if err != nil || runtimePath != first {
				t.Fatalf("runtime binding followed rotated alias: %q, %v", runtimePath, err)
			}
			reopened, err := storage.OpenFileWithLocations(ctx, runtimePath, runtimeRef, launch.RefLocations)
			if err != nil {
				t.Fatalf("runtime binding cannot reopen selected source: %v", err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			rootKey, err := manifest.ParseKeyRef(strings.TrimPrefix(firstRef, "manifest://"))
			if err != nil {
				t.Fatal(err)
			}
			gotRef, gotPath, found, err := bundleSourceForManifest(ctx, rootKey, RunOptions{
				SourceBinding: launch.SourceBinding, BundleReader: launch.BundleReader, BundleFetcher: launch.BundleFetcher,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("root A not selected from Bundle")
			}
			t.Logf("opened root=%s; portable.exec=%s; returned provenance=%s (%s)", firstRef, launch.PortableConfig.Launch.Exec, gotPath, gotRef)
			if gotPath != first {
				t.Fatalf("opened Bundle A but provenance selected alias's later Bundle B: got %s; want %s", gotPath, first)
			}
		})
	}
}
