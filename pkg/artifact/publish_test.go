package artifact

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

const publishTestSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func publishEROFSFixture() []byte {
	payload := make([]byte, 4096)
	binary.LittleEndian.PutUint32(payload[1024:1028], 0xE0F5E1E2)
	payload[1024+12] = 12
	binary.LittleEndian.PutUint32(payload[1024+36:1024+40], 1)
	return payload
}

func TestNewLocationPublisherRejectsSymlinkDirectory(t *testing.T) {
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	link := filepath.Join(t.TempDir(), "published")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	_, err = NewLocationPublisher(storage, "published", link, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("NewLocationPublisher error = %v, want symlink rejection", err)
	}
}

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
			Kernel:  "file://vmlinux@digest:" + publishTestSHA,
			Runtime: "file://runtime.bundle@digest:" + publishTestSHA,
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
	parentRuntime, parentCfg := publishPortable(t, "file://unavailable.overlay@digest:"+publishTestSHA)
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
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]int{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("named location created non-regular entry %q with mode %s", entry.Name(), info.Mode())
		}
		roles[filepath.Ext(entry.Name())]++
	}
	if len(entries) != 3 || roles[".overlay"] != 1 || roles[".sandbox"] != 1 || roles[".snapshot"] != 1 {
		t.Fatalf("named location entries = %v, roles = %v", entries, roles)
	}
	for _, alias := range []string{"publish.sandbox", "publish.snapshot", "fixture.sandbox", "fixture.snapshot"} {
		if _, err := os.Lstat(filepath.Join(outputDir, alias)); !os.IsNotExist(err) {
			t.Fatalf("named location created semantic alias %q: %v", alias, err)
		}
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

func TestLocationPublisherPublishesRootCarrierAsImage(t *testing.T) {
	ctx := context.Background()
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	inputSink := snapshot.NewFileSink(inputDir, "fixture", nil, false, nil)
	imageRef, _, err := inputSink.AbsorbImageSource(ctx, publishSource(t, publishEROFSFixture()))
	if err != nil {
		t.Fatal(err)
	}

	_, portable := publishPortable(t, "")
	portable.Boot.Root = config.PortableRootConfig{
		Base: imageRef, Overlay: &config.PortableOverlayConfig{Base: "self"},
	}
	runtimeConfig, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := sandboxfile.BuildSource(
		publishSource(t, bytes.Repeat([]byte{0x42}, 4096)), nil, runtimeConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, sandboxPath, err := inputSink.AbsorbSandbox(ctx, logical)
	if err != nil {
		t.Fatal(err)
	}

	locations := config.RefLocations{"published": outputDir}
	publisher, err := NewLocationPublisher(storage, "published", outputDir, locations, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	result, err := publisher.Publish(ctx, sandboxPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Role != RoleSandbox {
		t.Fatalf("published role = %q, want %q", result.Role, RoleSandbox)
	}

	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]int{}
	for _, entry := range entries {
		roles[filepath.Ext(entry.Name())]++
	}
	if len(entries) != 2 || roles[".image"] != 1 || roles[".sandbox"] != 1 || roles[".overlay"] != 0 {
		t.Fatalf("named root publication entries = %v, roles = %v", entries, roles)
	}

	rootRef, err := manifest.ParseRef(result.Ref)
	if err != nil {
		t.Fatal(err)
	}
	rootPath, err := locations.ResolveFile(rootRef, "")
	if err != nil {
		t.Fatal(err)
	}
	openedRoot, err := storage.OpenFileWithLocations(ctx, rootPath, rootRef, locations)
	if err != nil {
		t.Fatal(err)
	}
	root, err := sandboxfile.Open(ctx, openedRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	publishedImage, err := manifest.ParseRef(root.Portable.Boot.Root.Base)
	if err != nil {
		t.Fatal(err)
	}
	if publishedImage.Location != "published" || filepath.Ext(publishedImage.Path) != ".image" {
		t.Fatalf("published root image ref = %s", publishedImage.String())
	}
	imagePath, err := locations.ResolveFile(publishedImage, "")
	if err != nil {
		t.Fatal(err)
	}
	openedImage, err := storage.OpenFileWithLocations(ctx, imagePath, publishedImage, locations)
	if err != nil {
		t.Fatal(err)
	}
	imageRoot, err := sandboxfile.OpenEROFSArtifact(ctx, openedImage)
	if err != nil {
		t.Fatal(err)
	}
	if imageRoot.Payload.Size() != uint64(len(publishEROFSFixture())) {
		t.Fatalf("published root image size = %d", imageRoot.Payload.Size())
	}
	if err := imageRoot.Close(); err != nil {
		t.Fatal(err)
	}
}
