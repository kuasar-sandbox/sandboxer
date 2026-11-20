package restore

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func TestSnapshotCfgReaderLocalCheckpointNeedsNoMemoryService(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: "manifest://" + strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := memorycheckpoint.Descriptor{Version: memorycheckpoint.Version, MemoryID: "memory", CheckpointID: "checkpoint",
		Size: 4096, MapRef: "file://missing.image@digest:" + strings.Repeat("a", 64)}
	source, err := snapshotfile.BuildLocalSource([]byte("{}"), []byte("{}"), cfg, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	sink := snapshot.NewFileSink(directory, "sid", nil, false, nil)
	_, path, err := sink.AbsorbSnapshot(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewSnapshotCfgReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := reader.ReadRaw(ctx, path, SnapshotCfgReadOptions{})
	if err != nil || !bytes.Equal(got, cfg) {
		t.Fatalf("snapshot.cfg metadata requires unavailable local memory: %q, %v", got, err)
	}
}
