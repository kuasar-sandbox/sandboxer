package restore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
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
