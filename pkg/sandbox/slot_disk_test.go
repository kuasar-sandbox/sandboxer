package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

func TestHostSlotBindCommitsCaptureAndPreservesPrivateWriter(t *testing.T) {
	size := "8KiB"
	t.Run("fixed", func(t *testing.T) { testHostSlotBind(t, &size, 8192) })
	t.Run("dynamic without capacity notifications", func(t *testing.T) { testHostSlotBind(t, nil, 0) })
}

func testHostSlotBind(t *testing.T, size *string, initialCapacity int64) {
	ctx := context.Background()
	root := t.TempDir()
	slot := &config.DiskSlotConfig{Size: size}
	cfg := &config.SandboxConfig{Boot: config.BootConfig{Disks: []config.DiskConfig{{Name: "data", Slot: slot}}}}
	c0 := testRunFromPortable(t)
	c0.Version = 2
	c0.Boot.Disks = []config.PortableDiskConfig{{Name: "data", Slot: slot}}
	c0.Mounts = []config.MountConfig{{Type: "disk", Source: "data", Target: "/data"}}
	if err := c0.Validate(); err != nil {
		t.Fatal(err)
	}
	store, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cache, err := diffstore.NewCache(64<<10, 32<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	set, err := vhost.NewBackendSet([]vhost.BlockDeviceSpec{{Slot: true, Capacity: initialCapacity}})
	if err != nil {
		t.Fatal(err)
	}
	if err = set.Bind([]vhost.Backend{nil}); err != nil {
		t.Fatal(err)
	}
	srv := vhost.NewServer("", set.DeviceBackends()[0], func(string, ...any) {})
	snapshot := &SnapshotHandler{Cfg: cfg, PortableConfig: c0, Disks: []SnapDiskRef{{Size: 8192}, {Slot: true}}, Servers: []*vhost.Server{srv}}
	rt := &Runtime{state: runtimeRunning, lifeCtx: ctx, baseDir: root, diskStorage: store, diskCache: cache, diskBackends: set, diskSnapshot: snapshot, diskInventory: []diskEntry{{reserved: true, dynamic: size == nil}}}
	defer rt.closeBoundDisks()
	entry := &rt.diskInventory[0]
	entry.Name = "data"
	entry.State = diskUnbound
	if size != nil {
		entry.SizeBytes = &initialCapacity
	}
	seed := bytes.Repeat([]byte{'A'}, 8192)
	if err := os.WriteFile(filepath.Join(root, "seed"), seed, 0600); err != nil {
		t.Fatal(err)
	}
	layer, _, _ := writeDiskArtifact(t, root, "immutable", seed, nil)
	req := BindDiskRequest{SlotName: "data", Disk: config.DiskV1{Kind: "disk", Version: 1, Base: "file://" + layer, DiffTemplate: "file://seed@location:seeds"}, RefLocations: config.RefLocations{"seeds": root}}
	// Hold the same gate as a real capture. A competing bind must leave the
	// saved device unbound and must not allocate a writer.
	if err := snapshot.capture.begin(); err != nil {
		t.Fatal(err)
	}
	if err := rt.BindDisk(ctx, req); !errors.Is(err, ctl.ErrDiskBusy) || len(rt.diskOwned) != 0 {
		t.Fatalf("bind during capture: %v, writers=%d", err, len(rt.diskOwned))
	}
	snapshot.capture.finish(false)
	if err := rt.BindDisk(ctx, req); err != nil || len(rt.diskOwned) != 1 {
		t.Fatalf("host bind: %v, writers=%d", err, len(rt.diskOwned))
	}
	state, err := rt.ListDataDisks(ctx)
	if err != nil || state.Disks[0].State != diskReady || state.Disks[0].SizeBytes == nil || *state.Disks[0].SizeBytes != 8192 {
		t.Fatalf("published disk inventory: %#v %v", state, err)
	}
	if err := snapshot.capture.begin(); err != nil {
		t.Fatal(err)
	}
	saved := snapshot.PortableConfig.Boot.Disks[0]
	if saved.Binding == nil || saved.Binding.ID == "" || saved.Binding.SizeBytes != 8192 || snapshot.Disks[1].Size != 8192 {
		t.Fatalf("incomplete capture topology after bind: %+v disk=%+v", saved, snapshot.Disks[1])
	}
	snapshot.capture.finish(false)
	if _, err = set.DeviceBackends()[0].WriteAt([]byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	// Repeating the call after a lost response must keep the same writer.
	if err = rt.BindDisk(ctx, req); err != nil || len(rt.diskOwned) != 1 || snapshot.PortableConfig.Boot.Disks[0].Binding.ID != saved.Binding.ID {
		t.Fatalf("retry changed committed binding: %v", err)
	}
	var b [4]byte
	if _, err = set.DeviceBackends()[0].ReadAt(b[:], 0); err != nil || string(b[:]) != "newA" {
		t.Fatalf("%q %v", b[:], err)
	}
	conflict := req
	conflict.Disk.DiffTemplate = "file://other@location:seeds"
	if err = rt.BindDisk(ctx, conflict); err == nil {
		t.Fatal("accepted changed disk")
	}
	renamed := filepath.Join(root, "renamed.overlay")
	if err := os.Rename(layer, renamed); err != nil {
		t.Fatal(err)
	}
	req.Disk.Base = "file://" + renamed
	if err = rt.BindDisk(ctx, req); err != nil {
		t.Fatalf("retry after carrier rename: %v", err)
	}
	replacement, _, _ := writeDiskArtifact(t, root, "immutable", bytes.Repeat([]byte{'B'}, 8192), nil)
	if err = os.Rename(replacement, layer); err != nil {
		t.Fatal(err)
	}
	conflict = req
	conflict.Disk.Base = "file://" + layer
	if err = rt.BindDisk(ctx, conflict); err == nil {
		t.Fatal("replacement at original path treated as the same disk")
	}
	req.RefLocations["cache"] = root
	if err = rt.BindDisk(ctx, req); err != nil || rt.diskSnapshot.RefLocations["cache"] != root {
		t.Fatalf("merge on retry: %v", err)
	}
}

func TestColdLaunchOnlyInstallsAutofsForUnboundSlot(t *testing.T) {
	disks := []config.DiskConfig{
		{Name: "restored", Slot: &config.DiskSlotConfig{}, Binding: &config.SlotBinding{ID: "saved", SizeBytes: 8192}},
		{Name: "unbound", Slot: &config.DiskSlotConfig{}},
		{Name: "ordinary"},
	}
	mounts := []proto.MountSpec{{Type: "disk", Source: "restored", Subpath: "data"}, {Type: "disk", Source: "unbound", Subpath: "output"}, {Type: "disk", Source: "ordinary"}}
	resolveDiskMounts(mounts, disks, true)
	for i, m := range mounts {
		if m.DiskSlotDeclared != (i < 2) || m.DiskSlot != (i == 1) || m.DiskIndex != i || len(m.DiskDevs) != 1 || m.DiskDevs[0] != guestDevPath(i+1) {
			t.Fatalf("mount %d: %+v", i, m)
		}
	}
}

func TestSnapshotPureBlockDependenciesKeepEveryByte(t *testing.T) {
	p := testRunFromPortable(t)
	p.Version = 2
	p.Boot.Disks = []config.PortableDiskConfig{{Name: "work", PureBlock: true, PortableRootConfig: config.PortableRootConfig{Base: "manifest://" + strings.Repeat("a", 64)}}}
	refs, err := snapshotLayerDependencies(RunOptions{PortableConfig: p}, nil, []bool{false, false})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range refs {
		if ref.raw == p.Boot.Disks[0].Base {
			found = true
			if ref.role != dependencyPureBlock {
				t.Fatal("pure disk marked for format probing")
			}
		}
	}
	if !found {
		t.Fatal("missing immutable disk dependency")
	}
	// A pure block stream may end in arbitrary bytes. Publication must never
	// probe, strip, or even read its tail while classifying its declared type.
	in := &checkpointSourceStream{}
	out, transformed, err := prepareSnapshotDependencyStream(context.Background(), in, dependencyPureBlock)
	if err != nil || transformed || out != in {
		t.Fatalf("pure block dependency was interpreted: %v", err)
	}
}
