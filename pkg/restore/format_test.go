package restore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

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
