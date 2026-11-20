package artifact

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func TestMemoryCheckpointCleanupRetainsConcurrentFilePublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := newMemoryCheckpointFixture(t)
	mem, err := memory.Create("cleanup-publication", localMemorySize)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	copy(mem.Bytes(), checkpointPageBytes("uvxxxx"))
	fd, err := memorycheckpoint.ReadOnlyMemfd(mem.FD())
	if err != nil {
		t.Fatal(err)
	}
	descriptor := fixture.descriptor
	descriptor.CheckpointID, descriptor.MemoryID = "publishing", "memory-publishing"
	holder, err := memorycheckpoint.PrepareHolder(fixture.directory, t.TempDir(),
		memorycheckpoint.CaptureRecord{Version: memorycheckpoint.Version,
			Producer:     memorycheckpoint.RunIdentity{SandboxID: "sid"},
			CheckpointID: descriptor.CheckpointID, MemoryID: descriptor.MemoryID, Epoch: 1}, fd, localMemorySize, nil)
	if err != nil {
		t.Fatal(err)
	}
	begin := make(chan struct{})
	var release sync.Once
	defer func() {
		release.Do(func() { close(begin) })
		if err := holder.Wait(ctx); err != nil {
			t.Error(err)
		}
	}()
	sink := snapshot.NewFileSink(fixture.directory, "sid", nil, false, nil)
	defer sink.Close()
	cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: fixture.eRef, FromRefs: []string{fixture.external}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := snapshotfile.BuildLocalSource([]byte("{}"), []byte("{}"), cfg, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	fixture.ref, fixture.path, err = sink.AbsorbSnapshot(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	fixture.descriptor = descriptor
	if err := holder.Record().Update(func(r *memorycheckpoint.CaptureRecord) {
		r.Committed, r.RuntimeRetired = true, true
		r.SnapshotRef, r.SandboxRef = fixture.ref, fixture.eRef
	}); err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(); err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(func(saveCtx context.Context, frozen io.ReaderAt) (string, error) {
		select {
		case <-begin:
		case <-saveCtx.Done():
			return "", saveCtx.Err()
		}
		view := &memory.CaptureView{Size: localMemorySize, Materialized: []byte{0x2b}, Dirty: []byte{2}, Released: []byte{0x10}}
		top, _, err := snapshot.CaptureMemory(frozen, view, true)
		if err != nil {
			return "", err
		}
		ref, _, err := sink.AbsorbImageSource(saveCtx, top)
		return ref, err
	}, nil); err != nil {
		t.Fatal(err)
	}

	// Resolve the current disk root after GC has read the retained memory
	// record with no PayloadRef. The real holder then publishes its file and
	// releases its shared pin before GC takes the exclusive collection lock.
	// No sleeps or test-only product hooks are needed for this interleaving.
	disk, err := manifest.ParseRef(fixture.eRef)
	if err != nil {
		t.Fatal(err)
	}
	disk.Location = "after-memory"
	c := Checkpoint{Directory: fixture.directory, SandboxID: "sid", SandboxRef: disk.String(),
		Retain: []CheckpointSource{{SnapshotRef: fixture.ref, SandboxRef: fixture.eRef}},
		ResolveLocation: func(name string) (string, error) {
			if name != "after-memory" {
				t.Fatalf("unexpected location %q", name)
			}
			release.Do(func() { close(begin) })
			return fixture.directory, holder.Wait(ctx)
		}}
	if err := fixture.storage.CleanupCheckpoint(ctx, c); err != nil {
		t.Fatal(err)
	}
	record := holder.Record().Read()
	if !record.FileReady || !record.RAMReleased || record.Deleted {
		t.Fatalf("publication facts: ready=%v released=%v deleted=%v", record.FileReady, record.RAMReleased, record.Deleted)
	}
	payload, err := manifest.ParseRef(record.PayloadRef)
	if err != nil {
		t.Fatal(err)
	}
	path, err := c.RefLocations.ResolveFile(payload, fixture.directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("GC removed newly published retained payload: %v", err)
	}
	root := openLocalFixture(t, fixture)
	defer root.Close()
	assertMemoryPages(t, root.Memory, "DDZDZD", "uv\x00x\x00x")
}
