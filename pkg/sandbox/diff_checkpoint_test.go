package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
)

func TestFreshCheckpointDeviceBindingAndImmediateCapture(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "encrypted"}[encrypted], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "disk.diff")
			options := []diffstore.DiskOption{diffstore.WithCheckpoint("bound-disk", "base")}
			if encrypted {
				options = append(options, diffstore.WithDiffEncryption([32]byte{7}, true))
			}
			cow, err := diffstore.OpenDisk(path, nil, diffstore.DiffInit{CreateSize: 32 << 30}, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer cow.Close()
			// Use the actual cold/restore binding path immediately after logical
			// open. It must use validated immutable identity, while the exported
			// callbacks keep readiness/publication ownership with this Disk.
			refs := bindCheckpointDisks([]SnapDiskRef{{}}, []DiskBackend{{Cow: cow}})
			if refs[0].DiffID != "bound-disk" || refs[0].Checkpoint == nil || refs[0].WaitMaterialized == nil || refs[0].Export == nil {
				t.Fatal("incomplete live device binding", refs[0])
			}
			if _, err := cow.WriteAt([]byte{0x51}, 0); err != nil {
				t.Fatal(err)
			}
			if err := refs[0].Checkpoint(context.Background(), "immediate"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("capture bound an unpublished file", err)
			}
			view, err := refs[0].Export(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer view.Close()
		})
	}
}

func TestCheckpointMaintenanceCanonicalOwner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	owner, err := diffstore.LockDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, dir)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err = os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"absolute": dir, "relative": relative, "symlink": alias} {
		t.Run(name, func(t *testing.T) {
			h := &SnapshotHandler{CheckpointDir: path}
			if err := h.startCheckpointMaintenance(context.Background(), owner); err != nil {
				t.Fatal(err)
			}
			if err := h.checkpointMaintenance.Close(); err != nil {
				t.Fatal(err)
			}
			if err := owner.Validate(); err != nil {
				t.Fatal("maintenance released borrowed owner", err)
			}
		})
	}
	other, err := diffstore.LockDirectory(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	h := &SnapshotHandler{CheckpointDir: relative}
	if err := h.startCheckpointMaintenance(context.Background(), other); err == nil {
		t.Fatal("accepted unrelated directory owner")
	}
}

func TestCheckpointDiskPlanFixesEveryPathAndRefusesTakeover(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "persistent-path", "checkpoint")
	cfg := &config.SandboxConfig{}
	cfg.Boot.Disks = make([]config.DiskConfig, 2)
	plan, err := PrepareCheckpointDiskPlan(cfg, nil, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Disks) != 3 {
		t.Fatalf("disks=%d", len(plan.Disks))
	}
	seen := map[string]bool{}
	for _, d := range plan.Disks {
		if seen[d.DiffID] || filepath.Dir(d.Path) != filepath.Join(dir, "diffs") {
			t.Fatalf("bad plan: %+v", plan)
		}
		seen[d.DiffID] = true
		if _, err := os.Stat(d.Path); !os.IsNotExist(err) {
			t.Fatalf("planning created/consulted disk: %v", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "diffs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan.Disks[0].Path, []byte("legacy raw bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareCheckpointDiskPlan(cfg, nil, dir, nil); err == nil {
		t.Fatal("implicitly adopted existing writable diff")
	}
	outside := &config.SandboxConfig{}
	outside.Boot.Root.Diff = "file://" + filepath.Join(t.TempDir(), "existing.diff")
	if _, err := PrepareCheckpointDiskPlan(outside, nil, dir, nil); err == nil {
		t.Fatal("accepted independent writable diff directory")
	}
	if _, err := PrepareCheckpointDiskPlan(&config.SandboxConfig{}, nil, dir, []string{"../escape"}); err == nil {
		t.Fatal("accepted escaped reopen identity")
	}
}

func TestExplicitCheckpointOutputAndUnsupportedArePreflight(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkpoint")
	opts := RunOptions{EnableCheckpoint: true, checkpointDir: dir}
	disks := []SnapDiskRef{{DiffID: "disk", Checkpoint: func(context.Context, string) error { t.Fatal("preflight registered a version"); return nil }}}
	req, err := prepareNamedCapture(ctl.Request{CheckpointID: "safe"}, opts, disks)
	if err != nil || req.OutDir != dir {
		t.Fatalf("derived output %q: %v", req.OutDir, err)
	}
	if _, err := prepareNamedCapture(ctl.Request{CheckpointID: "safe", OutDir: t.TempDir()}, opts, disks); err == nil {
		t.Fatal("cross-directory output accepted")
	}
	if _, err := prepareNamedCapture(ctl.Request{CheckpointID: "safe", Upload: true}, opts, disks); err == nil {
		t.Fatal("upload accepted")
	}
	opts.EnableCheckpoint = false
	if _, err := prepareNamedCapture(ctl.Request{CheckpointID: "safe"}, opts, disks); !errors.Is(err, ctl.ErrCheckpointUnsupported) {
		t.Fatalf("unsupported=%v", err)
	}
	if req, err := prepareNamedCapture(ctl.Request{OutDir: "ordinary"}, opts, disks); err != nil || req.CheckpointID != "" || req.OutDir != "ordinary" {
		t.Fatalf("default capture changed: %+v %v", req, err)
	}
}

func TestColdPreflightReadsReopenedV2ThroughTheSharedDiskPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.diff")
	binding := CheckpointDiskBinding{DiffID: "preflight-disk", BaseID: strings.Repeat("a", 64), Path: path}
	store, err := diffstore.Create(path, diffstore.Options{DiffID: binding.DiffID, BaseID: binding.BaseID, LogicalSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	page := make([]byte, 4096)
	page[ext4SuperblockMagicOffset] = 0x53
	page[ext4SuperblockMagicOffset+1] = 0xef
	if _, err := store.WriteAt(page, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	root := &config.RootConfig{Diff: "file://" + path}
	check := func(b *CheckpointDiskBinding) error {
		return preflightWritableExt4(context.Background(), root, "root", "", nil, nil, nil, [32]byte{}, false, nil, nil, b)
	}
	if err := check(&binding); err != nil {
		t.Fatalf("valid v2 plan rejected: %v", err)
	}
	wrong := binding
	wrong.BaseID = strings.Repeat("b", 64)
	if err := check(&wrong); err == nil {
		t.Fatal("preflight ignored planned immutable base identity")
	}
	if err := check(nil); err == nil {
		t.Fatal("legacy preflight implicitly accepted v2 writable diff")
	}
}

func TestOnlineCheckpointManagementSkipsRetiredAndMissingDiffs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "diffs"), 0700); err != nil {
		t.Fatal(err)
	}
	create := func(filename, identity string, retire bool) *diffstore.View {
		t.Helper()
		store, err := diffstore.Create(filepath.Join(dir, "diffs", filename+".diff"), diffstore.Options{DiffID: identity, LogicalSize: 4096})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Checkpoint(ctx, "cp"); err != nil {
			t.Fatal(err)
		}
		var view *diffstore.View
		if retire {
			view, err = store.Export(ctx, "cp")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = view.Close() })
			if err := store.Retire(ctx); !errors.Is(err, diffstore.ErrBusy) {
				t.Fatalf("pinned retirement: %v", err)
			}
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		return view
	}
	create("available", "available", false)
	pinned := create("retired", "retired", true)
	owner, err := diffstore.LockDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	maintenance, err := diffstore.NewDirectoryMaintenance(owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	handler := &SnapshotHandler{EnableCheckpoint: true, SandboxID: "sid", CheckpointDir: dir, checkpointMaintenance: maintenance}
	refs, err := handler.listCheckpoints(ctx)
	if err != nil || len(refs) != 1 || refs[0].DiffID != "available" || refs[0].ID != "cp" {
		t.Fatalf("retired file poisoned live list: %+v %v", refs, err)
	}
	for range 2 {
		err := handler.deleteCheckpoints(ctx, []ctl.DiffCheckpointRef{{DiffID: "retired", ID: "cp"}, {DiffID: "removed", ID: "cp"}}, &ctl.CheckpointSources{Retain: []ctl.CheckpointSource{}})
		if err != nil {
			t.Fatalf("exact cleanup was not idempotent: %v", err)
		}
	}
	if _, err := pinned.ReadAt(ctx, make([]byte, 4096), 0); err != nil {
		t.Fatalf("existing retired pin lost: %v", err)
	}
	create("wrong", "different", true)
	if _, err := handler.listCheckpoints(ctx); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("retired identity mismatch hidden by list: %v", err)
	}
	if err := handler.deleteCheckpoints(ctx, []ctl.DiffCheckpointRef{{DiffID: "wrong", ID: "cp"}}, &ctl.CheckpointSources{Retain: []ctl.CheckpointSource{}}); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("retired identity mismatch hidden by delete: %v", err)
	}
}
