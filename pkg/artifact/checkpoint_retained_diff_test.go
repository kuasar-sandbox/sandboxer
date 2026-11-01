package artifact

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestCleanupCheckpointRetainsIndependentDiskCaptures(t *testing.T) {
	ctx := context.Background()
	f := newDiffCheckpointFixture(t)
	for _, store := range f.stores {
		if _, err := store.WriteAt(bytes.Repeat([]byte{9}, 4096), 0); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"current", "obsolete"} {
			if err := store.Checkpoint(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	cp := *f.cp
	cp.CheckpointID = "current"
	source, err := sandboxfile.BuildCheckpointSource(ctx, f.cfg, &cp)
	if err != nil {
		t.Fatal(err)
	}
	sink := snapshot.NewFileSink(f.dir, "fixture", nil, false, nil)
	ref, path, err := sink.AbsorbSandbox(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSandbox(ctx, ref, path); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	cleanup := Checkpoint{Directory: f.dir, SandboxID: "fixture", SandboxRef: ref, Retain: []CheckpointSource{{SandboxRef: f.ref}}}
	if err := storage.CleanupCheckpoint(ctx, cleanup); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"root-1", "data-1", "data-2"} {
		disk := filepath.Join(f.dir, "diffs", id+".diff")
		store, err := diffstore.OpenMetadata(disk)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := store.ListCheckpoints(ctx)
		store.Close()
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(ids)
		if !slices.Equal(ids, []string{"cp-001", "current"}) {
			t.Fatalf("disk %s retained %v", id, ids)
		}
		view, err := diffstore.OpenExport(ctx, disk, "cp-001", nil)
		if err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 4096)
		_, err = view.ReadAt(ctx, data, 0)
		view.Close()
		if err != nil || !bytes.Equal(data, bytes.Repeat([]byte{byte(i + 1)}, 4096)) {
			t.Fatalf("old capture changed on %s: %v", id, err)
		}
	}
	cleanup.Retain = nil
	if err := storage.CleanupCheckpoint(ctx, cleanup); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.path); !os.IsNotExist(err) {
		t.Fatalf("consumed root remains: %v", err)
	}
	for _, id := range []string{"root-1", "data-1", "data-2"} {
		store, err := diffstore.OpenMetadata(filepath.Join(f.dir, "diffs", id+".diff"))
		if err != nil {
			t.Fatal(err)
		}
		ids, err := store.ListCheckpoints(ctx)
		store.Close()
		if err != nil || !slices.Equal(ids, []string{"current"}) {
			t.Fatalf("disk %s retained %v: %v", id, ids, err)
		}
	}
}
