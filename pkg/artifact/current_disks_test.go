package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"golang.org/x/sys/unix"
)

func TestRetainedDiskListIgnoresInitializerClaims(t *testing.T) {
	f, s, _ := retainedCurrentFixture(t)
	path := filepath.Join(f.dir, "diffs", ".claim-root-1.diff")
	claim, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close()
	for _, held := range []bool{true, false} {
		op := unix.LOCK_UN
		if held {
			op = unix.LOCK_EX | unix.LOCK_NB
		}
		if err := unix.Flock(int(claim.Fd()), op); err != nil {
			t.Fatal(err)
		}
		refs, err := s.ListDiffCheckpoints(context.Background(), f.dir)
		if err != nil || len(refs) != 3 {
			t.Fatalf("claim held=%v list=%v: %v", held, refs, err)
		}
		for _, ref := range refs {
			if ref.ID != "cp-001" {
				t.Fatal("historical checkpoint changed", ref)
			}
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal("enumeration removed initializer claim", err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.dir, "diffs", "broken.diff"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListDiffCheckpoints(context.Background(), f.dir); err == nil {
		t.Fatal("a corrupt real DIFF was silently ignored")
	}
}

func retainedCurrentFixture(t *testing.T) (*diffCheckpointFixture, *ProcessStorage, CurrentRun) {
	t.Helper()
	return retainedCurrentFixtureWithSlot(t, false)
}

func retainedCurrentFixtureWithSlot(t *testing.T, slot bool) (*diffCheckpointFixture, *ProcessStorage, CurrentRun) {
	t.Helper()
	f := newDiffCheckpointFixture(t)
	s, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	run := CurrentRun{SandboxID: "fixture", InstanceID: "instance-1", RunID: "run-1"}
	disks := make([]CurrentDisk, len(f.stores))
	for i, st := range f.stores {
		info := st.Info()
		name := ""
		if i > 0 {
			name = f.cfg.Boot.Disks[i-1].Name
		}
		disks[i] = CurrentDisk{Name: name, DiffID: info.DiffID, BaseID: info.BaseID, Capacity: info.LogicalSize}
		if _, err := st.WriteAt(bytes.Repeat([]byte{byte(10 + i)}, 4096), 0); err != nil {
			t.Fatal(err)
		}
	}
	if slot {
		f.cfg.Version = 2
		f.cfg.Boot.Disks = append(f.cfg.Boot.Disks[:1:1], append([]config.PortableDiskConfig{{Name: "unused", Slot: &config.DiskSlotConfig{}}}, f.cfg.Boot.Disks[1:]...)...)
		f.cfg.Mounts = append(f.cfg.Mounts, config.MountConfig{Type: "disk", Source: "unused", Target: "/unused"})
		disks = append(disks[:2:2], append([]CurrentDisk{{Name: "unused"}}, disks[2:]...)...)
	}
	if err := s.RetainCurrentDisks(context.Background(), f.owner, run, f.cfg, disks, nil); err != nil {
		t.Fatal(err)
	}
	for _, st := range f.stores {
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	return f, s, run
}

func assertResetPayload(t *testing.T, f *diffCheckpointFixture, s *ProcessStorage, ref, id string) {
	t.Helper()
	ctx := context.Background()
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := s.OpenFile(ctx, filepath.Join(f.dir, parsed.Path), parsed)
	if err != nil {
		t.Fatal(err)
	}
	e, err := sandboxfile.Open(ctx, opened)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Checkpoint == nil || e.Checkpoint.CheckpointID != id {
		t.Fatalf("invalid Reset E: %+v", e.Checkpoint)
	}
	ids := []string{e.Checkpoint.Root.DiffID}
	for _, d := range e.Checkpoint.Disks {
		ids = append(ids, d.DiffID)
	}
	for i, disk := range ids {
		for version, value := range map[string]byte{id: byte(10 + i), "cp-001": byte(1 + i)} {
			v, err := diffstore.OpenExport(ctx, filepath.Join(f.dir, "diffs", disk+".diff"), version, nil)
			if err != nil {
				t.Fatal(err)
			}
			b := make([]byte, 4096)
			_, err = v.ReadAt(ctx, b, 0)
			err = errors.Join(err, v.Close())
			if err != nil || !bytes.Equal(b, bytes.Repeat([]byte{value}, 4096)) {
				t.Fatalf("disk %s version %s data changed: %v", disk, version, err)
			}
		}
	}
}

func TestResetCurrentDisksPreservesLatestAndHistoricalPayload(t *testing.T) {
	f, s, run := retainedCurrentFixture(t)
	ref, err := s.ResetCurrentDisks(context.Background(), f.dir, run, "reset-1")
	if err != nil {
		t.Fatal(err)
	}
	assertResetPayload(t, f, s, ref, "reset-1")
	again, err := s.ResetCurrentDisks(context.Background(), f.dir, run, "reset-1")
	if err != nil || again != ref {
		t.Fatalf("replay: %s, %v", again, err)
	}
}

func TestResetCurrentCheckpointVersionsProtectBothSources(t *testing.T) {
	f, s, run := retainedCurrentFixture(t)
	ctx := context.Background()
	ref, err := s.ResetCurrentDisks(ctx, f.dir, run, "reset-versions")
	if err != nil {
		t.Fatal(err)
	}
	c := Checkpoint{Directory: f.dir, SandboxID: run.SandboxID, SandboxRef: ref, Retain: []CheckpointSource{{SandboxRef: f.ref}}}
	refs, err := s.CheckpointVersions(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 6 {
		t.Fatalf("protected versions: %+v", refs)
	}
	for _, ref := range refs {
		if ref.ID != "cp-001" && ref.ID != "reset-versions" {
			t.Fatalf("unexpected version: %+v", ref)
		}
	}
	// Missing retained metadata fails the entire plan instead of returning a
	// partial keep set that an online collector could accidentally delete.
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if refs, err := s.CheckpointVersions(ctx, c); err == nil || refs != nil {
		t.Fatalf("partial keep plan: %+v, %v", refs, err)
	}
}

func TestResetCurrentDisksRecoversEveryPersistedBoundary(t *testing.T) {
	for _, stage := range []string{"accepted", "disk:0", "disk:1", "disk:2", "sandbox"} {
		t.Run(stage, func(t *testing.T) {
			f, s, run := retainedCurrentFixture(t)
			interrupted := errors.New("conductor interrupted")
			_, err := s.resetCurrentDisks(context.Background(), f.dir, run, "reset-interrupted", func(at string) error {
				if at == stage {
					return interrupted
				}
				return nil
			})
			if !errors.Is(err, interrupted) {
				t.Fatalf("boundary not reached: %v", err)
			}
			ref, err := s.ResetCurrentDisks(context.Background(), f.dir, run, "reset-interrupted")
			if err != nil {
				t.Fatal(err)
			}
			assertResetPayload(t, f, s, ref, "reset-interrupted")
		})
	}
}

func TestResetCurrentDisksRejectsWrongRunAndIncompleteDisks(t *testing.T) {
	for _, scenario := range []string{"previous-run", "wrong-instance", "missing", "uninitialized", "capacity", "identity"} {
		t.Run(scenario, func(t *testing.T) {
			f, s, run := retainedCurrentFixture(t)
			path := filepath.Join(f.dir, "diffs", "data-2.diff")
			switch scenario {
			case "previous-run":
				run.RunID = "run-2"
			case "wrong-instance":
				run.InstanceID = "instance-2"
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "uninitialized":
				if err := os.Truncate(path, 4096); err != nil {
					t.Fatal(err)
				}
			case "capacity", "identity":
				var set CurrentDiskSet
				if _, err := readCurrentJSON(filepath.Join(f.dir, currentDisksName), &set); err != nil {
					t.Fatal(err)
				}
				if scenario == "capacity" {
					set.Disks[2].Capacity *= 2
				} else {
					set.Disks[2].DiffID = "other"
				}
				if err := writeCurrentJSON(filepath.Join(f.dir, currentDisksName), &set); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ResetCurrentDisks(context.Background(), f.dir, run, "must-fail"); err == nil {
				t.Fatal("invalid source accepted")
			}
			// Full-set validation precedes the first version mutation.
			st, err := diffstore.OpenMetadata(filepath.Join(f.dir, "diffs", "root-1.diff"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ids, err := st.ListCheckpoints(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range ids {
				if id == "must-fail" {
					t.Fatal("partial checkpoint before complete validation")
				}
			}
		})
	}
}

func TestResetCurrentDisksRejectsUnownedVersionAndChangedActive(t *testing.T) {
	for _, scenario := range []string{"unowned", "modified", "description"} {
		t.Run(scenario, func(t *testing.T) {
			f, s, run := retainedCurrentFixture(t)
			ctx := context.Background()
			if scenario != "unowned" {
				_, err := s.resetCurrentDisks(ctx, f.dir, run, "reset-test", func(at string) error {
					if at == "disk:0" {
						return errors.New("stop")
					}
					return nil
				})
				if err == nil {
					t.Fatal("expected interruption")
				}
			}
			st, err := diffstore.Open(filepath.Join(f.dir, "diffs", "root-1.diff"), diffstore.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "unowned" {
				err = st.Checkpoint(ctx, "reset-test")
			} else if scenario == "modified" {
				_, err = st.WriteAt(bytes.Repeat([]byte{99}, 4096), 0)
			}
			err = errors.Join(err, st.Close())
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "description" {
				var set CurrentDiskSet
				if _, err := readCurrentJSON(filepath.Join(f.dir, currentDisksName), &set); err != nil {
					t.Fatal(err)
				}
				set.Run.RunID = "next-run"
				if err := writeCurrentJSON(filepath.Join(f.dir, currentDisksName), &set); err != nil {
					t.Fatal(err)
				}
				run = set.Run
			}
			if _, err := s.ResetCurrentDisks(ctx, f.dir, run, "reset-test"); err == nil {
				t.Fatal("accepted foreign/changed version")
			}
		})
	}
}

func TestResetCurrentDisksRequiresExclusiveDirectory(t *testing.T) {
	f, s, run := retainedCurrentFixture(t)
	owner, err := diffstore.LockDirectory(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := s.ResetCurrentDisks(context.Background(), f.dir, run, "busy"); !errors.Is(err, diffstore.ErrBusy) {
		t.Fatalf("owner not fenced: %v", err)
	}
}

func TestCurrentDisksRejectsReleasedOwner(t *testing.T) {
	f, s, run := retainedCurrentFixture(t)
	var set CurrentDiskSet
	before, err := readCurrentJSON(filepath.Join(f.dir, currentDisksName), &set)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := diffstore.LockDirectory(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	run.RunID = "stale-owner"
	if err := s.RetainCurrentDisks(context.Background(), f.owner, run, f.cfg, set.Disks, nil); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("released owner accepted: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(f.dir, currentDisksName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("stale owner overwrote current run", err)
	}
}

func TestCurrentDisksCleanupStillValidatesActiveIdentityAndVersions(t *testing.T) {
	for _, scenario := range []string{"identity", "missing-version"} {
		t.Run(scenario, func(t *testing.T) {
			f, _, _ := retainedCurrentFixture(t)
			path := filepath.Join(f.dir, "diffs", "root-1.diff")
			if scenario == "identity" {
				if err := os.Rename(path, filepath.Join(f.dir, "diffs", "wrong.diff")); err != nil {
					t.Fatal(err)
				}
			}
			keep := map[string]map[string]bool{"root-1": {"missing": true}}
			active := map[string]bool{"root-1": true, "wrong": true}
			if err := cleanupDiffVersionSet(context.Background(), f.dir, keep, active); err == nil {
				t.Fatal("cleanup accepted invalid active source")
			}
			// No mutation is allowed before validating the entire keep set.
			st, err := diffstore.OpenMetadata(filepath.Join(f.dir, "diffs", "data-1.diff"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ids, err := st.ListCheckpoints(context.Background())
			if err != nil || len(ids) != 1 || ids[0] != "cp-001" {
				t.Fatal("cleanup mutated an unvalidated set", ids, err)
			}
		})
	}
}

func TestResetCurrentDisksPreservesMiddleUnboundSlot(t *testing.T) {
	f, s, run := retainedCurrentFixtureWithSlot(t, true)
	ctx := context.Background()
	ref, err := s.ResetCurrentDisks(ctx, f.dir, run, "middle-slot")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := s.OpenFile(ctx, filepath.Join(f.dir, parsed.Path), parsed)
	if err != nil {
		t.Fatal(err)
	}
	e, err := sandboxfile.Open(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	d := e.Checkpoint.Disks
	if e.Checkpoint.Version != 2 || len(d) != 3 || d[0].DiffID != "data-1" || d[1].Name != "unused" || d[1].DiffID != "" || d[2].DiffID != "data-2" {
		t.Fatalf("reset topology: %+v", d)
	}
	versions, err := s.CheckpointVersions(ctx, Checkpoint{Directory: f.dir, SandboxID: run.SandboxID, SandboxRef: ref})
	if err != nil || len(versions) != 3 {
		t.Fatalf("pinned versions: %v %v", versions, err)
	}
	replay, err := s.ResetCurrentDisks(ctx, f.dir, run, "middle-slot")
	if err != nil || replay != ref {
		t.Fatalf("reset retry %q, %v", replay, err)
	}
}
