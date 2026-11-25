package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestExportDiskCLIProducesBindableJSON(t *testing.T) {
	ctx := context.Background()
	t.Setenv(config.ManifestConfigEnv, "")
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
	namedDir := filepath.Join(t.TempDir(), "named")
	namedArgs := []string{"--source-ref", e, "--source-dir", dir, "--disk", "work", "--upload", "--to-ref-location", "named=file://" + namedDir}
	rc, stdout, stderr = captureInfoOutput(t, func() int { return exportCmd(namedArgs) })
	if rc != 0 {
		t.Fatalf("named export rc %d stderr %s", rc, stderr)
	}
	var namedSpec artifact.DiskV1
	if err = json.Unmarshal([]byte(stdout), &namedSpec); err != nil {
		t.Fatal(err)
	}
	namedRef, err := manifest.ParseRef(namedSpec.Base)
	if err != nil || namedRef.Location != "named" || namedRef.DigestScheme != "manifest" || namedRef.Scheme != manifest.RefSchemeFile || filepath.IsAbs(namedRef.Path) || !strings.HasSuffix(namedRef.Path, ".bundle") {
		t.Fatalf("named output %q: %v", stdout, err)
	}
	if _, err := os.Stat(filepath.Join(namedDir, namedRef.Path)); err != nil {
		t.Fatalf("named output file missing: %v", err)
	}
	rc, stdout, _ = captureInfoOutput(t, func() int { return exportCmd(append(args, "--resume")) })
	if rc != 2 || stdout != "" {
		t.Fatalf("accepted live option: rc=%d stdout=%q", rc, stdout)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	storage, err := newCLIStorage(&config.ManifestConfig{Crypto: manifestcrypto.Config{Chunk: "aes", Manifest: "aes"}})
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	locations := config.RefLocations{"named": namedDir}
	path, err = locations.ResolveFile(namedRef, "")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := storage.OpenFileWithLocations(ctx, path, namedRef, locations)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := make([]byte, len(body))
	n, err := stream.ReadAt(ctx, got, 0)
	if n != len(body) || err != nil && !errors.Is(err, io.EOF) || !bytes.Equal(got, body) {
		t.Fatalf("named export lost data after source removal: read=%d err=%v", n, err)
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
			args = append(args, "--upload", "--to-ref-location", "target=file://"+output)
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

func TestExportDiskCLIRejectsInvalidNamedDestinationBeforeIO(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	output := filepath.Join(dir, "output")
	input := filepath.Join(dir, "input")
	offline := []string{"--source-ref", "file:///missing.sandbox", "--disk", "work", "--manifest-config", filepath.Join(dir, "missing.yaml")}
	for _, tc := range []struct {
		name   string
		args   []string
		target string
		want   string
	}{
		{"without-upload", append(slices.Clone(offline), "--output", output), "result=file://" + target, "requires --upload"},
		{"with-output", append(slices.Clone(offline), "--output", output, "--upload"), "result=file://" + target, "one of --output DIR or --upload"},
		{"live", []string{"--sandbox-id", "live", "--upload"}, "result=file://" + target, "only for offline disk export"},
		{"checkpoint", []string{"--sandbox-id", "live", "--checkpoint", "saved"}, "result=file://" + target, "only for offline disk export"},
		{"image-assembly", []string{"--from", "file:///missing.erofs", "--config", "missing.yaml", "--upload"}, "result=file://" + target, "only for offline disk export"},
		{"offline-checkpoint", append(slices.Clone(offline), "--upload", "--checkpoint", "saved"), "result=file://" + target, "incompatible option: checkpoint"},
		{"offline-mode", append(slices.Clone(offline), "--upload", "--mode", "bundle"), "result=file://" + target, "incompatible option: mode"},
		{"offline-resume", append(slices.Clone(offline), "--upload", "--resume"), "result=file://" + target, "incompatible option: resume"},
		{"empty", append(slices.Clone(offline), "--upload"), "", "want name=file:///absolute/path"},
		{"invalid-name", append(slices.Clone(offline), "--upload"), "bad/name=file://" + target, "ref location"},
		{"relative-path", append(slices.Clone(offline), "--upload"), "result=file:relative", "URI must be file:///absolute/path"},
		{"remote-host", append(slices.Clone(offline), "--upload"), "result=file://host/dir", "URI must be file:///absolute/path"},
		{"input-conflict", append(slices.Clone(offline), "--upload", "--ref-location", "result=file://"+input), "result=file://" + target, "conflicts with input ref location"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(slices.Clone(tc.args), "--to-ref-location", tc.target)
			rc, stdout, stderr := captureInfoOutput(t, func() int { return exportCmd(args) })
			if rc != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("export rc=%d stdout=%q stderr=%q, want argument error %q", rc, stdout, stderr, tc.want)
			}
			for _, path := range []string{target, output, input} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("invalid export touched %q: %v", path, err)
				}
			}
		})
	}
}
