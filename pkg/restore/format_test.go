package restore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

type restoreCloseTrackingStream struct {
	sparse.Source
	closes int
}

func (s *restoreCloseTrackingStream) Close() error {
	s.closes++
	return nil
}

func TestRunRejectsLegacySnapshotDiskSchemaBeforeRunDirectory(t *testing.T) {
	legacy := []byte(`version: 1
memory: false
boot:
  root:
    base: file://legacy.overlay
`)
	logical, err := snapshotfile.BuildSource(
		sparse.Dense(bytes.NewReader(bytes.Repeat([]byte{0x31}, 4096)), 4096),
		[]byte("{}"), []byte("{}"), legacy,
	)
	if err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	_, path, err := snapshot.NewFileSink(output, "legacy", nil, false, nil).AbsorbSnapshot(context.Background(), logical)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(t.TempDir(), "run")
	_, err = Run(context.Background(), Options{
		SnapshotPath: path,
		HostCfg:      &config.SandboxConfig{},
		SandboxID:    "legacy",
		RuntimeRoot:  runtimeRoot,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported snapshot format/version") {
		t.Fatalf("restore error = %v", err)
	}
	if _, statErr := os.Stat(runtimeRoot); !os.IsNotExist(statErr) {
		t.Fatalf("legacy snapshot created runtime state: %v", statErr)
	}
}

func TestPreflightRestoreDiskGraphRejectsUnformattedActiveDiff(t *testing.T) {
	dir := t.TempDir()
	ext4 := make([]byte, 4096)
	ext4[1024+0x38], ext4[1024+0x38+1] = 0x53, 0xef
	baseRef, _, err := snapshot.NewFileSink(dir, "restore-base", nil, false, nil).
		AbsorbOverlaySource(context.Background(), sparse.Dense(bytes.NewReader(ext4), uint64(len(ext4))))
	if err != nil {
		t.Fatal(err)
	}
	diffPath := filepath.Join(dir, "active.diff")
	if err := os.WriteFile(diffPath, make([]byte, len(ext4)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.SandboxConfig{Boot: config.BootConfig{Root: config.RootConfig{
		Base: baseRef,
		Diff: "file://" + diffPath,
	}}}
	err = preflightRestoreDiskGraph(context.Background(), cfg, Options{
		SandboxID: "restore-preflight",
		BaseRoot:  filepath.Join(dir, "base"),
	}, dir, [32]byte{})
	if err == nil || !strings.Contains(err.Error(), "formatted ext4") {
		t.Fatalf("restore active diff preflight error = %v", err)
	}
}

func TestOpenMemoryParentLayersValidatesCapacityBeforeUse(t *testing.T) {
	dir := t.TempDir()
	memory := bytes.Repeat([]byte{0x42}, 4096)
	logical, err := snapshotfile.BuildSource(
		sparse.Dense(bytes.NewReader(memory), uint64(len(memory))),
		[]byte("{}"), []byte("{}"),
		[]byte("version: 1\nsandbox_ref: manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := snapshot.NewFileSink(dir, "parent", nil, false, nil).AbsorbSnapshot(context.Background(), logical)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{SnapshotPath: filepath.Join(dir, "current.snapshot")}
	layers, err := openMemoryParentLayers(context.Background(), []string{ref}, opts, uint64(len(memory)))
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 || layers[0].Size() != uint64(len(memory)) {
		t.Fatalf("parent layers = %d size=%d", len(layers), layers[0].Size())
	}
	if err := layers[0].Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openMemoryParentLayers(context.Background(), []string{ref}, opts, uint64(len(memory))+4096); err == nil || !strings.Contains(err.Error(), "does not match capacity") {
		t.Fatalf("capacity mismatch error = %v", err)
	}
}

func TestSeparateSandboxBundleRetainsSnapshotBundleMemoryScope(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ctx := context.Background()
	dir := t.TempDir()
	customerKey := [32]byte{0x41, 0x42, 0x43}
	keyFn := func() ([32]byte, error) { return customerKey, nil }
	manifestCfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}

	portable := &config.PortableSandboxConfig{
		Version: config.PortableSandboxConfigVersion,
		Resources: config.PortableResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 1, Memory: "4KiB"},
			Allocatable: config.AllocatableConfig{CPU: 1, Memory: "4KiB"},
		},
		Boot: config.PortableBootConfig{
			Kernel:  "file://vmlinux@digest:" + sha,
			Runtime: "file://sandbox-runtime.bundle@digest:" + sha,
			Root:    config.PortableRootConfig{Base: "self"},
		},
		Launch: config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"},
	}
	portableRaw, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	eLogical, err := sandboxfile.BuildSource(
		sparse.Dense(bytes.NewReader(bytes.Repeat([]byte{0x51}, 4096)), 4096), nil, portableRaw,
	)
	if err != nil {
		t.Fatal(err)
	}
	eSink, err := snapshot.NewBundleSink(ctx, dir, "separate-e", manifestCfg, keyFn, nil)
	if err != nil {
		t.Fatal(err)
	}
	eRef, _, err := eSink.AbsorbSandbox(ctx, eLogical)
	if err != nil {
		t.Fatal(err)
	}
	if err := eSink.CommitSandbox(ctx, eRef, ""); err != nil {
		t.Fatal(err)
	}
	if err := eSink.Close(); err != nil {
		t.Fatal(err)
	}
	eKey, err := manifest.ParseKeyRef(eRef)
	if err != nil {
		t.Fatal(err)
	}
	eBundleRef := "file://" + manifest.HexKey(eKey) + ".bundle@manifest:" + manifest.HexKey(eKey)

	sSink, err := snapshot.NewBundleSink(ctx, dir, "separate-s", manifestCfg, keyFn, nil)
	if err != nil {
		t.Fatal(err)
	}
	parentConfig, err := snapshot.MarshalConfig(&snapshot.Config{
		Version: snapshot.SnapshotConfigVersion, SandboxRef: eBundleRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	parentLogical, err := snapshotfile.BuildSource(
		sparse.Dense(bytes.NewReader(bytes.Repeat([]byte{0x61}, 4096)), 4096),
		[]byte("{}"), []byte("{}"), parentConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	parentRef, _, err := sSink.AbsorbSnapshot(ctx, parentLogical)
	if err != nil {
		t.Fatal(err)
	}
	rootConfig, err := snapshot.MarshalConfig(&snapshot.Config{
		Version: snapshot.SnapshotConfigVersion, SandboxRef: eBundleRef, FromRefs: []string{parentRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	rootLogical, err := snapshotfile.BuildSource(
		sparse.Dense(bytes.NewReader(bytes.Repeat([]byte{0x71}, 4096)), 4096),
		[]byte("{}"), []byte("{}"), rootConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	sRef, _, err := sSink.AbsorbSnapshot(ctx, rootLogical)
	if err != nil {
		t.Fatal(err)
	}
	if err := sSink.CommitSnapshot(ctx, sRef, ""); err != nil {
		t.Fatal(err)
	}
	if err := sSink.Close(); err != nil {
		t.Fatal(err)
	}
	sKey, err := manifest.ParseKeyRef(sRef)
	if err != nil {
		t.Fatal(err)
	}
	sPath := filepath.Join(dir, manifest.HexKey(sKey)+".bundle")

	root, err := openRootSnapshot(ctx, Options{
		SnapshotPath: sPath, SnapshotRef: "file://" + filepath.Base(sPath) + "@manifest:" + manifest.HexKey(sKey),
		ManifestCfg: manifestCfg, CustomerKeyFn: keyFn,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer root.stream.Close()
	sandboxSource, err := openReferencedSandbox(ctx, eBundleRef, root.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer sandboxSource.Root.Close()
	if sandboxSource.bundleSource == nil || sandboxSource.bundleSource.Reader != sandboxSource.bundleReader ||
		sandboxSource.bundleSource.Fetcher != sandboxSource.bundleFetcher {
		t.Fatal("Sandbox E Bundle provenance is not paired with its reader/fetcher")
	}

	// openReferencedSandbox deliberately nests its scoped fetcher over the
	// Snapshot Bundle's fetcher. A miss in E therefore falls through to S,
	// retaining parent memory Manifests that exist only in S.
	layers, err := openMemoryParentLayers(ctx, []string{parentRef}, sandboxSource.opts, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 {
		t.Fatalf("memory parent layers = %d, want 1", len(layers))
	}
	if err := layers[0].Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenLayeredDiskBaseClosesEachStreamExactlyOnce(t *testing.T) {
	newStream := func(size int) *restoreCloseTrackingStream {
		return &restoreCloseTrackingStream{Source: sparse.Dense(bytes.NewReader(make([]byte, size)), uint64(size))}
	}
	t.Run("success", func(t *testing.T) {
		top, lower := newStream(4096), newStream(4096)
		byRef := map[string]fetch.Stream{"top": top, "lower": lower}
		reader, err := openLayeredDiskBase(context.Background(), []string{"top", "lower"}, func(_ context.Context, raw string) (fetch.Stream, error) {
			return byRef[raw], nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if top.closes != 1 || lower.closes != 1 {
			t.Fatalf("close counts = top:%d lower:%d, want 1/1", top.closes, lower.closes)
		}
	})
	t.Run("partial failure", func(t *testing.T) {
		top := newStream(4096)
		_, err := openLayeredDiskBase(context.Background(), []string{"top", "missing"}, func(_ context.Context, raw string) (fetch.Stream, error) {
			if raw == "top" {
				return top, nil
			}
			return nil, errors.New("missing lower")
		})
		if err == nil || !strings.Contains(err.Error(), "missing lower") {
			t.Fatalf("open error = %v", err)
		}
		if top.closes != 1 {
			t.Fatalf("top close count = %d, want 1", top.closes)
		}
	})
}

func TestValidateRestoreSandboxIDRejectsPathComponents(t *testing.T) {
	for _, sandboxID := range []string{"", ".", "..", "../escape", "nested/id", `nested\id`} {
		if err := validateRestoreSandboxID(sandboxID); err == nil {
			t.Fatalf("validateRestoreSandboxID(%q) succeeded", sandboxID)
		}
	}
	if err := validateRestoreSandboxID("sandbox-123"); err != nil {
		t.Fatalf("validateRestoreSandboxID(valid): %v", err)
	}
}
