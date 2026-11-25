package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

func TestExportDiskCLIRetainsPublishedGraphWithoutManifestConfig(t *testing.T) {
	t.Setenv(config.ManifestConfigEnv, "")
	t.Setenv("MANIFEST_KEY", "")
	dir := t.TempDir()
	sink := snapshot.NewFileSink(dir, "source", nil, false, nil)
	defer sink.Close()
	c := testRunFromPortable(t)
	c.Version = 2
	c.Boot.Root = config.PortableRootConfig{Base: "self"}
	c.Boot.DiskCapabilities = &config.DiskCapabilities{SlotV1: true}
	base := "manifest://" + strings.Repeat("a", 64)
	parents := []string{"file://parent.bundle@manifest:" + strings.Repeat("b", 64) + "@location:offline"}
	c.Boot.Disks = []config.PortableDiskConfig{{
		Name: "work", PureBlock: true, Slot: &config.DiskSlotConfig{},
		Binding:            &config.SlotBinding{ID: "binding", SourceID: "source", SizeBytes: 4096},
		PortableRootConfig: config.PortableRootConfig{Base: base, BaseFromRefs: parents},
	}}
	c.Mounts = []config.MountConfig{{Type: "disk", Source: "work", Target: "/data"}}
	ctx := context.Background()
	ref, path, err := sink.AbsorbSandbox(ctx, runFromLogical(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if err = sink.CommitSandbox(ctx, ref, path); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"local", "store", "named"} {
		args := []string{"--source-ref", ref, "--source-dir", dir, "--disk", "work"}
		output := filepath.Join(t.TempDir(), "unused")
		switch mode {
		case "store":
			args = append(args, "--upload")
		case "named":
			args = append(args, "--upload", "--to-ref-location", "target=file://"+filepath.Join(t.TempDir(), "published"))
		default:
			args = append(args, "--output", output)
		}
		rc, stdout, stderr := captureInfoOutput(t, func() int { return exportCmd(args) })
		if rc != 0 {
			t.Fatalf("mode=%s: export rc %d stderr %s", mode, rc, stderr)
		}
		var spec artifact.DiskV1
		if err = json.Unmarshal([]byte(stdout), &spec); err != nil {
			t.Fatal(err)
		}
		if spec.Base != base || !slices.Equal(spec.BaseFromRefs, parents) || spec.Size == nil || *spec.Size != 4096 {
			t.Fatalf("published graph changed: %s", stdout)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("all-retained graph created output: %v", err)
		}
	}
}
