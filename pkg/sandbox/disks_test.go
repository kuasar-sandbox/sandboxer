package sandbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/image"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestOpenDiskStreamValidatesLocatedContentName(t *testing.T) {
	dir := t.TempDir()
	tmp, err := os.CreateTemp(dir, "disk-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := tarstream.WriteTo(context.Background(), tmp, "image", sparse.Dense(bytes.NewReader([]byte("image")), 5))
	if err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	goodName := digest + ".image"
	if err := os.Rename(tmp.Name(), filepath.Join(dir, goodName)); err != nil {
		t.Fatal(err)
	}
	locations := config.RefLocations{"shared": dir}
	stream, _, err := OpenDiskStream(context.Background(), "file://"+goodName+"@location:shared", nil, locations, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()

	body, err := os.ReadFile(filepath.Join(dir, goodName))
	if err != nil {
		t.Fatal(err)
	}
	badName := strings.Repeat("0", 64) + ".image"
	if err := os.WriteFile(filepath.Join(dir, badName), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenDiskStream(context.Background(), "file://"+badName+"@location:shared", nil, locations, nil, false); err == nil {
		t.Fatal("located ref with a mismatched content name succeeded")
	}
}

func TestOpenDiskStreamFollowsLocatedSymlink(t *testing.T) {
	locationDir := t.TempDir()
	outsideDir := t.TempDir()
	tmp, err := os.CreateTemp(outsideDir, "disk-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := tarstream.WriteTo(context.Background(), tmp, "image", sparse.Dense(bytes.NewReader([]byte("image")), 5))
	if err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	name := digest + ".image"
	target := filepath.Join(outsideDir, name)
	if err := os.Rename(tmp.Name(), target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(locationDir, name)); err != nil {
		t.Fatal(err)
	}

	stream, _, err := OpenDiskStream(context.Background(), "file://"+name+"@location:shared", nil, config.RefLocations{"shared": locationDir}, nil, false)
	if err != nil {
		t.Fatalf("open located symlink: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRootImageBlockReaderExcludesFlattenedConfigTail(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "root.erofs")
	payload := make([]byte, 4096)
	binary.LittleEndian.PutUint32(payload[1024:1028], 0xE0F5E1E2)
	payload[1024+12] = 12
	binary.LittleEndian.PutUint32(payload[1024+36:1024+40], 1)
	if err := os.WriteFile(imagePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := image.AppendConfigZip(imagePath, &image.RuntimeConfig{Cmd: []string{"/app"}}); err != nil {
		t.Fatal(err)
	}
	flattened, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	ref, artifactPath, err := snapshot.NewFileSink(dir, "root", nil, false, nil).
		AbsorbOverlaySource(context.Background(), sparse.Dense(bytes.NewReader(flattened), uint64(len(flattened))))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = artifactPath
	reader, size, err := OpenRootImageBlockReaderWithOpener(context.Background(), parsed.String(), nil, nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if size != int64(len(payload)) || reader.Size() != int64(len(payload)) {
		t.Fatalf("root block size = %d/%d, want %d", size, reader.Size(), len(payload))
	}
	imageCfg, err := LoadImageConfigFrom(reader, reader.Size())
	if err != nil {
		t.Fatal(err)
	}
	if len(imageCfg.Cmd) != 1 || imageCfg.Cmd[0] != "/app" {
		t.Fatalf("root image config = %#v", imageCfg)
	}
	if _, err := reader.ReadAt(make([]byte, 1), int64(len(payload))); err == nil {
		t.Fatal("vhost reader exposed the flattened ZIP tail")
	}
}

func TestOpenRootImageBlockReaderUsesParentSandboxPayloadAndImageConfigOnly(t *testing.T) {
	dir := t.TempDir()
	payload := make([]byte, 4096)
	binary.LittleEndian.PutUint32(payload[1024:1028], 0xE0F5E1E2)
	payload[1024+12] = 12
	binary.LittleEndian.PutUint32(payload[1024+36:1024+40], 1)
	imageConfig, err := json.Marshal(&image.RuntimeConfig{Cmd: []string{"/from-image-config"}})
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	parentPortable := &config.PortableSandboxConfig{
		Version: config.PortableSandboxConfigVersion,
		Resources: config.PortableResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 1, Memory: "1GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1, Memory: "1GiB"},
		},
		Boot: config.PortableBootConfig{
			Kernel: "file://kernel@digest:" + key, Runtime: "file://runtime@digest:" + key,
			Root: config.PortableRootConfig{Base: "self", Overlay: &config.PortableOverlayConfig{}},
		},
		Launch: config.PortableLaunchConfig{Exec: "/must-not-be-adopted", Workdir: "/", Restart: "never"},
	}
	runtimeConfig, err := config.MarshalPortableSandboxConfig(parentPortable)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := sandboxfile.BuildSource(
		sparse.Dense(bytes.NewReader(payload), uint64(len(payload))), imageConfig, runtimeConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	ref, path, err := snapshot.NewFileSink(dir, "parent", nil, false, nil).AbsorbSandbox(context.Background(), logical)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = path
	reader, size, err := OpenRootImageBlockReaderWithOpener(context.Background(), parsed.String(), nil, nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if size != int64(len(payload)) || reader.Size() != int64(len(payload)) {
		t.Fatalf("parent Sandbox block size = %d/%d, want %d", size, reader.Size(), len(payload))
	}
	defaults, err := LoadImageConfigFrom(reader, reader.Size())
	if err != nil {
		t.Fatal(err)
	}
	if len(defaults.Cmd) != 1 || defaults.Cmd[0] != "/from-image-config" {
		t.Fatalf("parent Sandbox image config = %#v", defaults)
	}
	if _, err := reader.ReadAt(make([]byte, 1), int64(len(payload))); err == nil {
		t.Fatal("vhost reader exposed parent Sandbox ZIP tail")
	}
}

func TestOpenLayeredBlockReaderRejectsMismatchedLogicalSizes(t *testing.T) {
	dir := t.TempDir()
	sink := snapshot.NewFileSink(dir, "layers", nil, false, nil)
	refs := make([]string, 0, 2)
	for _, body := range [][]byte{make([]byte, 4096), make([]byte, 8192)} {
		ref, path, err := sink.AbsorbOverlaySource(context.Background(),
			sparse.Dense(bytes.NewReader(body), uint64(len(body))))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := manifest.ParseRef(ref)
		if err != nil {
			t.Fatal(err)
		}
		parsed.Path = path
		refs = append(refs, parsed.String())
	}
	if _, _, err := OpenLayeredBlockReader(context.Background(), refs, nil, nil, nil, false); err == nil ||
		!strings.Contains(err.Error(), "logical size") {
		t.Fatalf("mismatched layer error = %v", err)
	}
}
