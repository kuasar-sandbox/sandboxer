package artifact

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func TestPublishManifestRootsToLocatedTarstream(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	remote, err := NewManifestPublisher(storage, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	runtime, _ := publishPortable(t, "")
	payload := bytes.Repeat([]byte{0x37}, 8192)
	e, err := sandboxfile.BuildSource(publishSource(t, payload), nil, runtime)
	if err != nil {
		t.Fatal(err)
	}
	remoteE, err := remote.PublishSource(ctx, RoleSandbox, e)
	if err != nil {
		t.Fatal(err)
	}
	scfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: remoteE.Ref})
	if err != nil {
		t.Fatal(err)
	}
	memory := bytes.Repeat([]byte{0x62}, 16384)
	s, err := snapshotfile.BuildSource(publishSource(t, memory), []byte("{}"), []byte("{}"), scfg)
	if err != nil {
		t.Fatal(err)
	}
	remoteS, err := remote.PublishSource(ctx, RoleSnapshot, s)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	locations := config.RefLocations{"output": directory}
	for _, want := range []PublishResult{remoteE, remoteS} {
		target, err := NewLocationPublisher(storage, "output", directory, locations, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := target.Publish(ctx, want.Ref)
		closeErr := target.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("Manifest root publish: %v %v", err, closeErr)
		}
		if got.Role != want.Role {
			t.Fatalf("role %s want %s", got.Role, want.Role)
		}
		ref, err := manifest.ParseRef(got.Ref)
		if err != nil || ref.Location != "output" {
			t.Fatalf("ref=%s err=%v", got.Ref, err)
		}
		info, err := storage.Inspect(ctx, got.Ref, locations)
		if err != nil || info.Role != want.Role {
			t.Fatalf("inspect=%v err=%v", info, err)
		}
		opened, err := storage.OpenFileWithLocations(ctx, filepath.Join(directory, ref.Path), ref, locations)
		if err != nil {
			t.Fatal(err)
		}
		if want.Role == RoleSandbox {
			root, err := sandboxfile.Open(ctx, opened)
			if err != nil {
				t.Fatal(err)
			}
			body, err := readPublishSource(ctx, root.Payload)
			root.Close()
			if err != nil || !bytes.Equal(body, payload) {
				t.Fatalf("disk content changed: %v", err)
			}
		} else {
			root, err := snapshotfile.Open(ctx, opened)
			if err != nil {
				t.Fatal(err)
			}
			body, err := readPublishSource(ctx, root.Memory)
			root.Close()
			if err != nil || !bytes.Equal(body, memory) {
				t.Fatalf("memory content changed: %v", err)
			}
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 2 {
		t.Fatalf("unexpected publication files: %v %v", entries, err)
	}
}

func TestPublishSnapshotSourceToSingleRootBundle(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	key, err := cfg.CustomerKey()
	if err != nil {
		t.Fatal(err)
	}
	runtime, _ := publishPortable(t, "")
	source, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x20}, 4096)), nil, runtime)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := NewManifestPublisher(storage, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, err := remote.PublishSource(ctx, RoleSandbox, source)
	remote.Close()
	if err != nil {
		t.Fatal(err)
	}
	scfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: e.Ref})
	if err != nil {
		t.Fatal(err)
	}
	memory := publishSource(t, bytes.Repeat([]byte{0x40}, 8192))
	s, err := snapshotfile.BuildSource(memory, []byte("{}"), []byte("{}"), scfg)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	target := newSourceBundlePublisher(t, cfg, key, directory)
	result, err := target.PublishSource(ctx, RoleSnapshot, s)
	target.Close()
	if err != nil {
		t.Fatal(err)
	}
	locations := config.RefLocations{"release": directory}
	info, err := storage.Inspect(ctx, result.Ref, locations)
	if err != nil || info.Role != RoleSnapshot {
		t.Fatalf("snapshot Bundle: %v %v", info, err)
	}
}

func TestNewSourceLocatedIdentityCreatesOnlyFinal(t *testing.T) {
	target := locationTestTarget(t.TempDir(), nil, false)
	fs := newTrackingLocationFileSystem()
	target.fs = fs
	body := bytes.Repeat([]byte{0x17}, 8192)
	raw, err := sparse.NewSource(bytes.NewReader(body), uint64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := target.Put(context.Background(), RoleOverlay, raw)
	if err != nil {
		t.Fatal(err)
	}
	if result == "" || fs.creates.Load() != 1 {
		t.Fatalf("result %q created=%d", result, fs.creates.Load())
	}
}
