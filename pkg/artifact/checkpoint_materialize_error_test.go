package artifact

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestCheckpointMaterializationFailurePreservesErrorAndCleansScratch(t *testing.T) {
	ctx := context.Background()
	f := newDiffCheckpointFixture(t)
	scratch, output := t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", scratch)
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	pins, err := storage.PinDiffCheckpoints(ctx, f.dir, f.cp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pins.Close() })
	// Root export follows the data disks, so this read failure also exercises
	// removal of ordinary data artifacts already written to scratch.
	if err := pins.Root.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := storage.MaterializePinnedCheckpoint(ctx, f.path, nil, output, pins)
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("materialization error = %v, want %v", err, os.ErrClosed)
	}
	if result.Ref != "" || result.SandboxRef != "" {
		t.Fatalf("failed materialization returned a published reference: %+v", result)
	}
	for _, directory := range []string{scratch, output} {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("failed materialization left files in %s: %v", directory, entries)
		}
	}
}
