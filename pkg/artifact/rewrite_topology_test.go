package artifact

import (
	"bytes"
	"context"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"os"
	"path/filepath"
	"testing"
)

func TestRewriteSandboxRefRejectsMovingDataToAnotherDevice(t *testing.T) {
	ctx := context.Background()
	input, output := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	publisher := rewriteTarget(t, storage, output)
	diskRef, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{0x51}, 4096)), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, cfg := publishPortable(t, "")
	cfg.Boot.Disks = []config.PortableDiskConfig{
		{Name: "a", PortableRootConfig: config.PortableRootConfig{Base: diskRef}},
		{Name: "b"},
	}
	cfg.Mounts = []config.MountConfig{{Target: "/a", Type: "disk", Source: "a"}, {Target: "/b", Type: "disk", Source: "b"}}
	payload := bytes.Repeat([]byte{0x21}, 4096)
	oldE := rewriteSaveE(t, sink, cfg, rewriteSparse(t, payload))
	updated, err := cfg.Clone()
	if err != nil {
		t.Fatal(err)
	}
	updated.Boot.Disks[0].Base = ""
	updated.Boot.Disks[1].Base = diskRef
	newE := rewriteSaveE(t, sink, updated, rewriteSparse(t, payload))
	root := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: oldE}, rewriteSparse(t, payload))
	parsed, err := manifest.ParseRef(root)
	if err != nil {
		t.Fatal(err)
	}
	options := RewriteOptions{Replacements: []RefReplacement{{Old: oldE, New: newE}}}
	result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, parsed.Path), options)
	if err == nil || result.Ref != "" {
		t.Fatalf("device relocation accepted as equivalent: %+v %v", result, err)
	}
	entries, readErr := os.ReadDir(output)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("device mismatch created %d outputs", len(entries))
	}
}
