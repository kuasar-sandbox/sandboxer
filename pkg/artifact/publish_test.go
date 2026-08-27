package artifact

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

const publishTestSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func publishSource(t *testing.T, body []byte) sparse.Source {
	t.Helper()
	source, err := sparse.NewSource(bytes.NewReader(body), uint64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func publishPortable(t *testing.T, parent string) ([]byte, *config.PortableSandboxConfig) {
	t.Helper()
	cfg := &config.PortableSandboxConfig{
		Version: config.PortableSandboxConfigVersion,
		Resources: config.PortableResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 1, Memory: "1GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1, Memory: "1GiB"},
		},
		Boot: config.PortableBootConfig{
			Kernel:  "file://vmlinux@sha256:" + publishTestSHA,
			Runtime: "file://runtime.bundle@sha256:" + publishTestSHA,
			Root:    config.PortableRootConfig{Base: "self", BaseFromRefs: nil},
		},
		Launch: config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"},
	}
	if parent != "" {
		cfg.Boot.Root.BaseFromRefs = []string{parent}
	}
	raw, err := config.MarshalPortableSandboxConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return raw, cfg
}

func TestLocationPublisherRewritesSandboxAndSnapshotGraphs(t *testing.T) {
	ctx := context.Background()
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	inputSink := snapshot.NewFileSink(inputDir, "fixture", nil, false, nil)

	// The parent E intentionally names an unavailable lower. When it appears as
	// a disk ref, publication must consume only its payload and ignore this graph.
	parentRuntime, parentCfg := publishPortable(t, "file://unavailable.overlay@sha256:"+publishTestSHA)
	parentLogical, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x31}, 4096)), nil, parentRuntime)
	if err != nil {
		t.Fatal(err)
	}
	parentRef, _, err := inputSink.AbsorbSandbox(ctx, parentLogical)
	if err != nil {
		t.Fatal(err)
	}
	_ = parentCfg

	rootRuntime, _ := publishPortable(t, parentRef)
	rootLogical, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x42}, 4096)), nil, rootRuntime)
	if err != nil {
		t.Fatal(err)
	}
	rootRef, rootPath, err := inputSink.AbsorbSandbox(ctx, rootLogical)
	if err != nil {
		t.Fatal(err)
	}

	snapshotCfg, err := snapshot.MarshalConfig(&snapshot.Config{
		Version: snapshot.SnapshotConfigVersion, SandboxRef: rootRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshotLogical, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x55}, 8192)), []byte("{}"), []byte("{}"), snapshotCfg)
	if err != nil {
		t.Fatal(err)
	}
	_, snapshotPath, err := inputSink.AbsorbSnapshot(ctx, snapshotLogical)
	if err != nil {
		t.Fatal(err)
	}

	locations := config.RefLocations{"published": outputDir}
	publisher, err := NewLocationPublisher(storage, "published", outputDir, locations, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	eResult, err := publisher.Publish(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if eResult.Role != RoleSandbox {
		t.Fatalf("E role = %q", eResult.Role)
	}
	sResult, err := publisher.Publish(ctx, snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if sResult.Role != RoleSnapshot {
		t.Fatalf("S role = %q", sResult.Role)
	}

	sRef, err := manifest.ParseRef(sResult.Ref)
	if err != nil || sRef.Location != "published" {
		t.Fatalf("published S ref = %q, err=%v", sResult.Ref, err)
	}
	sPath, err := locations.ResolveFile(sRef, "")
	if err != nil {
		t.Fatal(err)
	}
	openedS, err := storage.OpenFileWithLocations(ctx, sPath, sRef, locations)
	if err != nil {
		t.Fatal(err)
	}
	strictS, err := snapshotfile.Open(ctx, openedS)
	if err != nil {
		t.Fatal(err)
	}
	newSnapshotCfg, err := snapshot.ParseConfig(strictS.SnapshotConfig)
	if err != nil {
		t.Fatal(err)
	}
	strictS.Close()
	newERef, err := manifest.ParseRef(newSnapshotCfg.SandboxRef)
	if err != nil || newERef.Location != "published" {
		t.Fatalf("rewritten sandbox_ref = %q, err=%v", newSnapshotCfg.SandboxRef, err)
	}
	ePath, err := locations.ResolveFile(newERef, "")
	if err != nil {
		t.Fatal(err)
	}
	openedE, err := storage.OpenFileWithLocations(ctx, ePath, newERef, locations)
	if err != nil {
		t.Fatal(err)
	}
	strictE, err := sandboxfile.Open(ctx, openedE)
	if err != nil {
		t.Fatal(err)
	}
	defer strictE.Close()
	if len(strictE.Portable.Boot.Root.BaseFromRefs) != 1 {
		t.Fatalf("rewritten E chain = %v", strictE.Portable.Boot.Root.BaseFromRefs)
	}
	parentOverlay, err := manifest.ParseRef(strictE.Portable.Boot.Root.BaseFromRefs[0])
	if err != nil {
		t.Fatal(err)
	}
	if parentOverlay.Location != "published" || filepath.Ext(parentOverlay.Path) != ".overlay" {
		t.Fatalf("parent .sandbox was not published as payload overlay: %s", parentOverlay.String())
	}
}
