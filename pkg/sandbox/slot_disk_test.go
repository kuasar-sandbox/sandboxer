package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

func TestSlotOwnerAckLossPreservesWriterAndProtectsCapture(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	size := "8KiB"
	slot := &config.DiskSlotConfig{Size: &size}
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
	set, err := vhost.NewBackendSet([]vhost.BlockDeviceSpec{{Slot: true, Capacity: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	if err = set.Bind([]vhost.Backend{nil}); err != nil {
		t.Fatal(err)
	}
	srv := vhost.NewServer("", set.DeviceBackends()[0], func(string, ...any) {})
	snapshot := &SnapshotHandler{Cfg: cfg, PortableConfig: c0, Disks: []SnapDiskRef{{Size: 8192}, {Slot: true}}, Servers: []*vhost.Server{srv}}
	rt := &Runtime{state: runtimeRunning, lifeCtx: ctx, baseDir: root, diskStorage: store, diskCache: cache, diskBackends: set, diskSnapshot: snapshot, diskRunID: "first", diskInventory: []diskEntry{{reserved: true}}}
	defer rt.closeBoundDisks()
	entry := &rt.diskInventory[0]
	entry.Name = "data"
	entry.State = diskUnbound
	geometry := int64(8192)
	entry.SizeBytes = &geometry
	seed := bytes.Repeat([]byte{'A'}, 8192)
	if err := os.WriteFile(filepath.Join(root, "seed"), seed, 0600); err != nil {
		t.Fatal(err)
	}
	req := BindDiskRequest{SlotName: "data", Disk: config.DiskV1{Kind: "disk", Version: 1, DiffTemplate: "file://seed@location:seeds"}, RefLocations: config.RefLocations{"seeds": root}, ExpectedRun: "other"}
	rt.diskBindGuest = func(context.Context, proto.DiskBinding) error { return errors.New("response lost") }
	if err = rt.BindDisk(ctx, req); err == nil || len(rt.diskOwned) != 0 {
		t.Fatal("mismatching generation published disk")
	}
	req.ExpectedRun = "first"
	if err = rt.BindDisk(ctx, req); err == nil || len(rt.diskOwned) != 1 {
		t.Fatalf("first bind: %v, writers=%d", err, len(rt.diskOwned))
	}
	state, err := rt.ListDataDisks(ctx)
	if err != nil || state.Disks[0].State != diskPending {
		t.Fatalf("list pending: %#v %v", state, err)
	}
	if err = snapshot.beginCapture(); err == nil {
		t.Fatal("capture crossed pending ACK")
	}
	if _, err = set.DeviceBackends()[0].WriteAt([]byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	id := rt.diskInventory[0].bindingID
	rt.diskBindGuest = func(_ context.Context, b proto.DiskBinding) error {
		if b.BindingID != id || b.SizeBytes != 8192 {
			t.Fatalf("mismatching guest request: %#v", b)
		}
		return nil
	}
	if err = rt.BindDisk(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err = rt.BindDisk(ctx, req); err != nil || len(rt.diskOwned) != 1 {
		t.Fatal(err)
	}
	if err = snapshot.beginCapture(); err != nil {
		t.Fatal(err)
	}
	snapshot.capture.finish(false)
	var b [4]byte
	if _, err = set.DeviceBackends()[0].ReadAt(b[:], 0); err != nil || string(b[:]) != "newA" {
		t.Fatalf("%q %v", b[:], err)
	}
	conflict := req
	conflict.Disk.DiffTemplate = "file://other@location:seeds"
	if err = rt.BindDisk(ctx, conflict); err == nil {
		t.Fatal("accepted changed disk")
	}
	req.RefLocations["cache"] = root
	if err = rt.BindDisk(ctx, req); err != nil || rt.diskSnapshot.RefLocations["cache"] != root {
		t.Fatalf("merge on retry: %v", err)
	}
}
