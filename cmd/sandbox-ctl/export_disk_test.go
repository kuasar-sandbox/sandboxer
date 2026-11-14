package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestExportDiskCLIProducesBindableJSON(t *testing.T) {
	ctx := context.Background()
	t.Setenv("MANIFEST_KEY", strings.Repeat("2d", 32))
	dir, out := t.TempDir(), t.TempDir()
	manifestFile := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(manifestFile, []byte("manifest: {write_generation: G1}\ncrypto: {chunk: aes, manifest: aes, local: off}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sink := snapshot.NewFileSink(dir, "A", nil, false, nil)
	defer sink.Close()
	body := bytes.Repeat([]byte("work"), 1024)
	layer, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := testRunFromPortable(t)
	c.Boot.Root = config.PortableRootConfig{Base: "self"}
	c.Boot.Disks = []config.PortableDiskConfig{{Name: "work", PortableRootConfig: config.PortableRootConfig{Base: layer}}}
	c.Mounts = []config.MountConfig{{Type: "disk", Source: "work", Target: "/data"}}
	e, path, err := sink.AbsorbSandbox(ctx, runFromLogical(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if err = sink.CommitSandbox(ctx, e, path); err != nil {
		t.Fatal(err)
	}
	args := []string{"--source-ref", e, "--source-dir", dir, "--disk", "work", "--output", out, "--manifest-config", manifestFile}
	rc, stdout, stderr := captureInfoOutput(t, func() int { return exportCmd(args) })
	if rc != 0 {
		t.Fatalf("export rc %d stderr %s", rc, stderr)
	}
	var spec artifact.DiskV1
	if err = json.Unmarshal([]byte(stdout), &spec); err != nil {
		t.Fatal(err)
	}
	r, err := manifest.ParseRef(spec.Base)
	if err != nil || !filepath.IsAbs(r.Path) || r.DigestScheme != "manifest" || *spec.Size != int64(len(body)) {
		t.Fatalf("output %q: %v", stdout, err)
	}
	rc, stdout, _ = captureInfoOutput(t, func() int { return exportCmd(append(args, "--resume")) })
	if rc != 2 || stdout != "" {
		t.Fatalf("accepted live option: rc=%d stdout=%q", rc, stdout)
	}
}
