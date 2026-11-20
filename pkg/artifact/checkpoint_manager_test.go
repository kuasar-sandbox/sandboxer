package artifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
)

func TestCheckpointDeletionRequiresCompleteGraphAndExactIdentity(t *testing.T) {
	f := newMemoryCheckpointFixture(t)
	ctx := context.Background()
	ref := CheckpointRef{ID: f.descriptor.CheckpointID, MemoryID: f.descriptor.MemoryID}
	c := Checkpoint{Directory: f.directory, SandboxID: "sid"}
	if err := f.storage.DeleteCheckpoints(ctx, c, []CheckpointRef{ref}, nil); err == nil {
		t.Fatal("omitted registry treated as empty")
	}
	c.Retain = []CheckpointSource{{SnapshotRef: f.ref, SandboxRef: f.eRef}}
	if err := f.storage.DeleteCheckpoints(ctx, c, []CheckpointRef{ref}, nil); !errors.Is(err, ErrCheckpointReferenced) {
		t.Fatal("named root lost", err)
	}
	c.Retain = []CheckpointSource{}
	wrong := ref
	wrong.MemoryID = "another-memory"
	if err := f.storage.DeleteCheckpoints(ctx, c, []CheckpointRef{wrong}, nil); err == nil {
		t.Fatal("imprecise identity accepted")
	}
	view, err := f.storage.OpenCheckpoint(ctx, f.directory, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if err = f.storage.DeleteCheckpoints(ctx, c, []CheckpointRef{ref}, nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = f.storage.DeleteCheckpoints(ctx, c, []CheckpointRef{ref}, nil); err != nil {
			t.Fatal("idempotence", err)
		}
	}
	if _, err = f.storage.OpenCheckpoint(ctx, f.directory, ref); !errors.Is(err, memorycheckpoint.ErrDeleted) {
		t.Fatal("new open accepted", err)
	}
	assertMemoryPages(t, view.Memory.Memory, "DDZDZD", "bc\x00x\x00x")
	list, err := f.storage.ListCheckpoints(ctx, f.directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range list {
		if listed == ref {
			t.Fatal("revoked version remained in business list")
		}
	}
	record, err := memorycheckpoint.ReadRecord(f.directory, ref.ID, ref.MemoryID)
	if err != nil || !record.Deleted || !record.FileReady || !record.RAMReleased || record.Discarded {
		t.Fatalf("saved deleted lifecycle: %+v %v", record, err)
	}
	if err = f.storage.CleanupCheckpoint(ctx, c); !errors.Is(err, ErrCheckpointMemoryReadersBusy) {
		t.Fatal("live view physical barrier", err)
	}
	if _, err = os.Stat(f.path); err != nil {
		t.Fatal("pinned descriptor removed", err)
	}
	if err = view.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.storage.CleanupCheckpoint(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(f.path); !os.IsNotExist(err) {
		t.Fatal("unreachable descriptor retained", err)
	}
}

func TestCheckpointParentPublicationAndDeletionShareFence(t *testing.T) {
	f := newMemoryCheckpointFixture(t)
	ctx := context.Background()
	c := Checkpoint{Directory: f.directory, SandboxID: "sid", Retain: []CheckpointSource{}}
	ref := CheckpointRef{ID: f.descriptor.CheckpointID, MemoryID: f.descriptor.MemoryID}
	guard, err := f.storage.ProtectCheckpointParents(ctx, c, []string{f.ref})
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	if err = f.storage.DeleteCheckpoints(short, c, []CheckpointRef{ref}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("delete crossed parent publication fence", err)
	}
	cancel()
	child, _, _ := writeLocalMemory(t, f.directory, "child", f.eRef, []string{f.ref}, nil, "xxxxxq", 0x20, 0x20, 0, false)
	c.Retain = []CheckpointSource{{SnapshotRef: child, SandboxRef: f.eRef}}
	if err = guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.storage.DeleteCheckpoints(ctx, c, []CheckpointRef{ref}, nil); !errors.Is(err, ErrCheckpointReferenced) {
		t.Fatal("parent-first lost dependency", err)
	}
	view := openLocalFixture(t, f)
	defer view.Close()
	c.Retain = []CheckpointSource{}
	if err = f.storage.DeleteCheckpoints(ctx, c, []CheckpointRef{ref}, nil); err != nil {
		t.Fatal(err)
	}
	if next, err := f.storage.ProtectCheckpointParents(ctx, c, []string{f.ref}); err == nil {
		next.Close()
		t.Fatal("old reader authorized a new edge after deletion")
	}
	assertMemoryPages(t, view.Memory, "DDZDZD", "bc\x00x\x00x")
}

func TestCheckpointCurrentRootOptionalAndDoesNotPromoteRetainedAliases(t *testing.T) {
	for _, portable := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "portable"}[portable], func(t *testing.T) {
			f := newMemoryCheckpointFixture(t)
			c := Checkpoint{Directory: f.directory, SandboxID: "sid", Retain: []CheckpointSource{{SnapshotRef: f.ref, SandboxRef: f.eRef}}}
			if portable {
				c.SandboxRef = "manifest://" + strings.Repeat("a", 64)
			}
			if err := os.Remove(filepath.Join(f.directory, "current-memory.json")); err != nil {
				t.Fatal(err)
			}
			if err := f.storage.CleanupCheckpoint(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			for _, alias := range []string{"sid.snapshot", "sid.sandbox"} {
				if _, err := os.Lstat(filepath.Join(f.directory, alias)); !os.IsNotExist(err) {
					t.Fatal("retained source promoted to current alias", err)
				}
			}
			view := openLocalFixture(t, f)
			if err := view.Close(); err != nil {
				t.Fatal(err)
			}
			c.Retain = []CheckpointSource{}
			if err := f.storage.CleanupCheckpoint(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(f.path); !os.IsNotExist(err) {
				t.Fatal("latest hint or release evidence retained payload", err)
			}
		})
	}
}

func TestCheckpointLocatedParentFencesItsOwnDirectory(t *testing.T) {
	f := newMemoryCheckpointFixture(t)
	ctx := context.Background()
	childDirectory := t.TempDir()
	ref, err := manifest.ParseRef(f.ref)
	if err != nil {
		t.Fatal(err)
	}
	ref.Path, ref.Location = filepath.Base(ref.Path), "parent"
	c := Checkpoint{Directory: childDirectory, SandboxID: "child", RefLocations: config.RefLocations{"parent": f.directory}, Retain: []CheckpointSource{}}
	guard, err := f.storage.ProtectCheckpointParents(ctx, c, []string{ref.String()})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	parent := Checkpoint{Directory: f.directory, SandboxID: "sid", Retain: []CheckpointSource{}}
	version := CheckpointRef{ID: f.descriptor.CheckpointID, MemoryID: f.descriptor.MemoryID}
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err = f.storage.DeleteCheckpoints(short, parent, []CheckpointRef{version}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("located parent escaped fence: %v", err)
	}
	if err = guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.storage.DeleteCheckpoints(ctx, parent, []CheckpointRef{version}, nil); err != nil {
		t.Fatal(err)
	}
	if g, err := f.storage.ProtectCheckpointParents(ctx, c, []string{ref.String()}); err == nil {
		g.Close()
		t.Fatal("revoked located parent published")
	}
}

func TestRuntimeMemoryRetentionDoesNotInventSandboxPair(t *testing.T) {
	f := newMemoryCheckpointFixture(t)
	ctx := context.Background()
	_, _, other := writeLocalMemory(t, f.directory, "unrelated", f.eRef, nil, nil, "xxxxxx", 0x3f, 0x3f, 0, false)
	// The active disk has been materialized independently. Its current E no
	// longer equals S's original E, and the historical E is not a RAM edge.
	e, err := manifest.ParseRef(f.eRef)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(f.directory, e.Path)); err != nil {
		t.Fatal(err)
	}
	c := Checkpoint{Directory: f.directory, SandboxID: "sid", SandboxRef: "manifest://" + strings.Repeat("a", 64), Retain: []CheckpointSource{}, MemoryRefs: []string{f.ref}}
	ref := CheckpointRef{ID: f.descriptor.CheckpointID, MemoryID: f.descriptor.MemoryID}
	if err = f.storage.DeleteCheckpoints(ctx, c, []CheckpointRef{ref}, nil); !errors.Is(err, ErrCheckpointReferenced) {
		t.Fatalf("active memory dependency was not protected independently: %v", err)
	}
	if err = f.storage.DeleteCheckpoints(ctx, c, []CheckpointRef{{ID: other.CheckpointID, MemoryID: other.MemoryID}}, nil); err != nil {
		t.Fatalf("unrelated capture blocked by historical E: %v", err)
	}
	c.MemoryRefs = nil
	if err = f.storage.DeleteCheckpoints(ctx, c, []CheckpointRef{ref}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointListThenOpenRelativeDirectory(t *testing.T) {
	f := newMemoryCheckpointFixture(t)
	t.Chdir(filepath.Dir(f.directory))
	directory := "./" + filepath.Base(f.directory)
	refs, err := f.storage.ListCheckpoints(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if ref.MemoryID != f.descriptor.MemoryID {
			continue
		}
		view, err := f.storage.OpenCheckpoint(context.Background(), directory, ref)
		if err != nil {
			t.Fatal(err)
		}
		defer view.Close()
		if view.Memory == nil || view.Disk != nil {
			t.Fatal("wrong view type")
		}
		assertMemoryPages(t, view.Memory.Memory, "DDZDZD", "bc\x00x\x00x")
		return
	}
	t.Fatal("committed memory checkpoint absent from unified listing")
}
