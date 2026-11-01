package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestCurrentDisksSDKUpdatesReusedDiffRunAndSurvivesClose(t *testing.T) {
	shape, launch, paths, _ := coldCheckpointHandoffFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	storage, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	previous := artifact.CurrentRun{SandboxID: shape.SandboxID, InstanceID: "instance", RunID: "first"}
	shape.CurrentRun = &previous
	r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	current := previous
	current.RunID = "second"
	shape.CurrentRun = &current
	r, err = Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range r.disks {
		if r.spec.checkpointPlan.Disks[i].DiffID+".diff" != filepath.Base(paths[i]) {
			t.Fatalf("expected reused diff %s", paths[i])
		}
		if _, err := d.cow.WriteAt(bytes.Repeat([]byte{byte(40 + i)}, 512), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(filepath.Dir(paths[0]))
	if _, err := storage.ResetCurrentDisks(ctx, dir, previous, "stale"); err == nil {
		t.Fatal("accepted previous run")
	}
	if _, err := storage.ResetCurrentDisks(ctx, dir, current, "reset"); err != nil {
		t.Fatal(err)
	}
	for i, path := range paths {
		v, err := diffstore.OpenExport(ctx, path, "reset", nil)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 512)
		_, err = v.ReadAt(ctx, got, 0)
		v.Close()
		if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{byte(40 + i)}, 512)) {
			t.Fatal("lost latest disk content", i, err)
		}
	}
}

func TestCurrentDisksRetainsLocalLowersAfterMaterializationRemoval(t *testing.T) {
	ctx := context.Background()
	temporary, output := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(temporary, "source", nil, false, nil)
	lower, _, err := sink.AbsorbOverlaySource(ctx, sparse.Dense(bytes.NewReader(bytes.Repeat([]byte{0x11}, 8192)), 8192))
	if err != nil {
		t.Fatal(err)
	}
	p := snapshotTestPortable(t)
	p.Boot.Root = config.PortableRootConfig{Base: "self", BaseFromRefs: []string{lower}}
	raw, err := config.MarshalPortableSandboxConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	src, err := sandboxfile.BuildSourceContext(ctx, sparse.Dense(bytes.NewReader(bytes.Repeat([]byte{0x22}, 8192)), 8192), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	parent, parentPath, err := sink.AbsorbSandbox(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	bases, err := p.Checkpointed(parent)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.SandboxConfig{Boot: config.BootConfig{Root: config.RootConfig{Base: parent, BaseFromRefs: []string{lower}}}}
	owner, err := diffstore.LockDirectory(output)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	plan, err := PrepareCheckpointDiskPlan(cfg, nil, output, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Bind(bases); err != nil {
		t.Fatal(err)
	}
	disk := plan.Disks[0]
	st, err := diffstore.Create(filepath.Join(output, "diffs", disk.DiffID+".diff"), diffstore.Options{DiffID: disk.DiffID, BaseID: disk.BaseID, LogicalSize: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	storage, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	run := artifact.CurrentRun{SandboxID: "parent", InstanceID: "instance", RunID: "run"}
	err = RetainCurrentDiskPlan(ctx, storage, owner, &run, cfg, p, parent, plan, []int64{8192}, nil, RunOptions{SourceBinding: &RunSourceBinding{SandboxRef: parent, RuntimeRef: "file://" + parentPath, RelativeDir: temporary}})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(temporary); err != nil {
		t.Fatal(err)
	}
	ref, err := storage.ResetCurrentDisks(ctx, output, run, "reset")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	f, err := storage.OpenFile(ctx, filepath.Join(output, parsed.Path), parsed)
	if err != nil {
		t.Fatal(err)
	}
	e, err := sandboxfile.Open(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for i, ref := range append([]string{bases.Boot.Root.Base}, bases.Boot.Root.BaseFromRefs...) {
		parsed, _ := manifest.ParseRef(ref)
		f, err := storage.OpenFile(ctx, filepath.Join(output, parsed.Path), parsed)
		if err != nil {
			t.Fatal(err)
		}
		payload, _, err := sandboxfile.PayloadIfSandbox(ctx, f, false)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 8192)
		_, err = payload.ReadAt(ctx, got, 0)
		payload.Close()
		want := byte(0x22)
		if i > 0 {
			want = 0x11
		}
		if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{want}, 8192)) {
			t.Fatal("retained lower content changed", err)
		}
	}
}
