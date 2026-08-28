package restore

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func TestSnapshotCfgReaderDerivesCompatibilityProjectionFromSandbox(t *testing.T) {
	const (
		sha    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		shaAlt = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	)
	ctx := context.Background()
	dir := t.TempDir()
	baseRef := "file://root.erofs@sha256:" + shaAlt
	lowerRef := "manifest://" + strings.Repeat("b", 64)
	portable := &config.PortableSandboxConfig{
		Version: config.PortableSandboxConfigVersion,
		Resources: config.PortableResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 2, Memory: "1GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1.5, Memory: "768MiB"},
		},
		Boot: config.PortableBootConfig{
			Kernel:  "file://vmlinux@sha256:" + sha,
			Runtime: "file://sandbox-runtime.bundle@sha256:" + shaAlt,
			Root: config.PortableRootConfig{
				Base: baseRef,
				Overlay: &config.PortableOverlayConfig{
					Base: "self", BaseFromRefs: []string{lowerRef},
				},
			},
		},
		Launch:   config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never", CgroupControl: true},
		Metadata: map[string]string{"e2b.start_cmd": "node server.js"},
	}
	portableRaw, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	eLogical, err := sandboxfile.BuildSource(
		sparse.Dense(bytes.NewReader(bytes.Repeat([]byte{0x41}, 4096)), 4096), nil, portableRaw,
	)
	if err != nil {
		t.Fatal(err)
	}
	eSink := snapshot.NewFileSink(dir, "reader-e", nil, false, nil)
	eRef, ePath, err := eSink.AbsorbSandbox(ctx, eLogical)
	if err != nil {
		t.Fatal(err)
	}
	if err := eSink.CommitSandbox(ctx, eRef, ePath); err != nil {
		t.Fatal(err)
	}

	memoryParent := "manifest://" + strings.Repeat("c", 64)
	sCfgRaw, err := snapshot.MarshalConfig(&snapshot.Config{
		Version: snapshot.SnapshotConfigVersion, SandboxRef: eRef, FromRefs: []string{memoryParent},
	})
	if err != nil {
		t.Fatal(err)
	}
	sLogical, err := snapshotfile.BuildSource(
		sparse.Dense(bytes.NewReader(bytes.Repeat([]byte{0x51}, 4096)), 4096),
		[]byte("{}"), []byte("{}"), sCfgRaw,
	)
	if err != nil {
		t.Fatal(err)
	}
	sSink := snapshot.NewFileSink(dir, "reader-s", nil, false, nil)
	_, sPath, err := sSink.AbsorbSnapshot(ctx, sLogical)
	if err != nil {
		t.Fatal(err)
	}

	reader, err := NewSnapshotCfgReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	document, err := reader.Read(ctx, sPath, SnapshotCfgReadOptions{RelativeDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(document.Raw, sCfgRaw) {
		t.Fatalf("raw snapshot.cfg changed:\n%s\nwant:\n%s", document.Raw, sCfgRaw)
	}
	cfg := document.Config
	if cfg.Version != snapshot.SnapshotConfigVersion || cfg.SandboxRef != eRef {
		t.Fatalf("snapshot identity = version %d sandbox_ref %q", cfg.Version, cfg.SandboxRef)
	}
	if cfg.Resources.Capacity.CPU != 2 || cfg.Resources.Capacity.Memory != "1GiB" {
		t.Fatalf("capacity = %d/%s", cfg.Resources.Capacity.CPU, cfg.Resources.Capacity.Memory)
	}
	if cfg.Boot.RuntimeRef != portable.Boot.Runtime || cfg.Boot.Root.BaseRef != baseRef {
		t.Fatalf("derived boot = runtime %q base %q", cfg.Boot.RuntimeRef, cfg.Boot.Root.BaseRef)
	}
	if cfg.Boot.Root.Overlay == nil || cfg.Boot.Root.Overlay.Base != eRef ||
		strings.Join(cfg.Boot.Root.Overlay.BaseFromRefs, ",") != lowerRef {
		t.Fatalf("derived root overlay = %+v", cfg.Boot.Root.Overlay)
	}
	if strings.Join(cfg.FromRefs, ",") != memoryParent || cfg.Metadata["e2b.start_cmd"] != "node server.js" || !cfg.Launch.CgroupControl {
		t.Fatalf("derived memory/metadata/launch = %+v", cfg)
	}
	wantArtifacts := strings.Join([]string{baseRef, eRef, lowerRef}, ",")
	if got := strings.Join(cfg.ArtifactRefs(), ","); got != wantArtifacts {
		t.Fatalf("ArtifactRefs = %q, want %q", got, wantArtifacts)
	}
	raw, err := reader.ReadRaw(ctx, "file://"+filepath.Base(sPath), SnapshotCfgReadOptions{RelativeDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, sCfgRaw) {
		t.Fatal("ReadRaw changed snapshot.cfg")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotCfgReaderRejectsLegacyDiskSchema(t *testing.T) {
	legacy := []byte("version: 1\nmemory: false\nboot:\n  root:\n    base: file://legacy.overlay\n")
	logical, err := snapshotfile.BuildSource(
		sparse.Dense(bytes.NewReader(bytes.Repeat([]byte{0x61}, 4096)), 4096),
		[]byte("{}"), []byte("{}"), legacy,
	)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	_, path, err := snapshot.NewFileSink(dir, "legacy-reader", nil, false, nil).AbsorbSnapshot(context.Background(), logical)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewSnapshotCfgReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.Read(context.Background(), path, SnapshotCfgReadOptions{}); err == nil || !strings.Contains(err.Error(), "unsupported snapshot format/version") {
		t.Fatalf("legacy reader error = %v", err)
	}
}
