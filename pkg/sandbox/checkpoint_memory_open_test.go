package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func TestPrepareSnapshotDependencyPlanMaterializesLocatedMemoryCheckpoint(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	sink := snapshot.NewFileSink(directory, "sid", nil, false, nil)
	defer sink.Close()
	const size = 3 * 4096
	view := &memory.CaptureView{Size: size, Materialized: []byte{3}, Dirty: []byte{2}, Released: []byte{4}}
	b, err := memorycheckpoint.EncodeMap(view)
	if err != nil {
		t.Fatal(err)
	}
	mapSource, err := sparse.NewSource(bytes.NewReader(b), uint64(len(b)), nil)
	if err != nil {
		t.Fatal(err)
	}
	mapRef, _, err := sink.AbsorbImageSource(ctx, mapSource)
	if err != nil {
		t.Fatal(err)
	}
	data := append(bytes.Repeat([]byte{'a'}, 4096), bytes.Repeat([]byte{'b'}, 8192)...)
	top, _, err := snapshot.CaptureMemory(bytes.NewReader(data), view, true)
	if err != nil {
		t.Fatal(err)
	}
	payloadRef, _, err := sink.AbsorbImageSource(ctx, top)
	if err != nil {
		t.Fatal(err)
	}
	eRef := "manifest://" + strings.Repeat("a", 64)
	cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: eRef})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := memorycheckpoint.Descriptor{Version: 1, MemoryID: "memory", CheckpointID: "checkpoint", Size: size, MapRef: mapRef, WorkingSet: true}
	source, err := snapshotfile.BuildLocalSource([]byte("{}"), []byte("{}"), cfg, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := sink.AbsorbSnapshot(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	start, err := memorycheckpoint.ProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := memorycheckpoint.CreateRecord(directory, memorycheckpoint.CaptureRecord{
		Version: 1, Producer: memorycheckpoint.RunIdentity{SandboxID: "sid"}, CheckpointID: descriptor.CheckpointID,
		MemoryID: descriptor.MemoryID, Epoch: 1, SnapshotRef: raw, SandboxRef: eRef, PayloadRef: payloadRef,
		Committed: true, RuntimeRetired: true, FileReady: true, RAMReleased: true,
		Holder: memorycheckpoint.HolderIdentity{PID: os.Getpid(), StartTime: start, SocketPath: filepath.Join(directory, "unused.socket")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		t.Fatal(err)
	}
	ref.Location = "checkpoint-source"
	raw = ref.String()
	output := t.TempDir()
	plan, err := prepareSnapshotDependencyPlan(ctx, RunOptions{
		PortableConfig: snapshotTestLivePortable(t), RefLocations: config.RefLocations{ref.Location: directory},
	}, []string{raw}, []bool{true}, output, store.WriteAdmission{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	target := snapshot.NewFileSink(output, "new-snapshot", nil, false, nil)
	defer target.Close()
	replacements, err := plan.Emit(ctx, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if replacements[raw] == "" || replacements[raw] == raw {
		t.Fatal("located memory checkpoint was retained as a portable dependency")
	}
	portableRef, err := manifest.ParseRef(replacements[raw])
	if err != nil {
		t.Fatal(err)
	}
	stream, err := artifact.OpenFile(ctx, filepath.Join(output, portableRef.Path), portableRef, nil, nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	root, err := snapshotfile.Open(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if len(root.MemoryDirty) != snapshotfile.DirtyHeaderSize+1 || root.MemoryDirty[snapshotfile.DirtyHeaderSize] != 6 {
		t.Fatalf("import changed clean/dirty/zero semantics: %x", root.MemoryDirty)
	}
	got := make([]byte, size)
	if _, err := root.Memory.ReadAt(ctx, got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:8192], data[:8192]) || !bytes.Equal(got[8192:], make([]byte, 4096)) {
		t.Fatal("import changed memory values")
	}
}
