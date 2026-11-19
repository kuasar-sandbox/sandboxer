package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

func TestExistingDiskSkipsEveryTemplateAndRetainsSizeAssertion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retained.diff")
	init, err := PrepareDiffWithStorage(ctx, path, "mkfs://ext4?size=1MiB", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := diffstore.OpenDisk(path, nil, init)
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	keyCalls := 0
	storage, err := artifact.NewProcessStorageWithCustomerKey(&manifest.Config{Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalOff}}, func() ([32]byte, error) {
		keyCalls++
		return [32]byte{}, errors.New("template key must not be resolved")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	id := strings.Repeat("a", 64)
	for _, ref := range []string{"file:///unavailable", "file:///unavailable@digest:" + id, "file:///unavailable@hmac:" + id, "file:///unavailable@manifest:" + id, "manifest://" + id, "mkfs://ext4?size=1", "mkfs://ext4?size=2TiB"} {
		size := int64(1 << 20)
		spec := config.DiskV1{Kind: "disk", Version: 1, Size: &size, DiffTemplate: ref}
		definition := config.DiskConfig{Disk: &spec}
		base, actual, template, err := openColdDiskSource(ctx, definition, "file://"+path, storage, nil)
		if err != nil || base != nil || actual != size || template != "" {
			t.Fatalf("%s: %v %d %s %v", ref, base, actual, template, err)
		}
		if keyCalls != 0 {
			t.Fatal("ignored template resolved its key")
		}
		size *= 2
		if _, _, _, err := openColdDiskSource(ctx, definition, "file://"+path, storage, nil); !errors.Is(err, ctl.ErrDiskCapacity) {
			t.Fatalf("ignored capacity assertion: %v", err)
		}
	}
}

func TestPreflightWritableExt4RejectsDanglingBeforeTemplateStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dangling.diff")
	if err := os.Symlink(path+".missing", path); err != nil {
		t.Fatal(err)
	}
	fetcher := &runVerifiedTemplateFetcher{}
	storage, err := artifact.NewProcessStorageWithLocalAccess(nil, nil, nil, false, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	root := &config.RootConfig{Diff: "file://" + path, DiffTemplate: "manifest://" + strings.Repeat("a", 64)}
	if err := preflightExt4WithBase(context.Background(), nil, root, "root", "", nil, [32]byte{}, false, storage, nil); err == nil {
		t.Fatal("dangling existing disk selected template")
	}
	if fetcher.verified != 0 || fetcher.calls != 0 {
		t.Fatal("existing bad disk accessed template Store")
	}
}

func TestSelectedMkfsRejectsBaseAndInvalidCapacityWithoutPublishing(t *testing.T) {
	ctx := context.Background()
	base := capacityBase{bytes.NewReader(make([]byte, 1<<20))}
	for _, tc := range []struct {
		ref  string
		base diffstore.BlockReader
	}{{"mkfs://ext4?size=1MiB", base}, {"mkfs://ext4?size=1", nil}, {"mkfs://ext4?size=2TiB", nil}} {
		path := filepath.Join(t.TempDir(), "new.diff")
		init, err := PrepareDiffWithStorage(ctx, path, tc.ref, tc.base, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := diffstore.OpenDisk(path, tc.base, init); err == nil {
			t.Fatal("invalid mkfs creation accepted")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed creation published target: %v", err)
		}
	}
}

func TestHostSlotSizeOnlyCreatesExt4AndRetriesExplicitMkfs(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		name := "dynamic"
		if fixed {
			name = "fixed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			capacity := int64(1 << 20)
			slot := &config.DiskSlotConfig{}
			initial := int64(0)
			if fixed {
				size := "1MiB"
				slot.Size = &size
				initial = capacity
			}
			cfg := &config.SandboxConfig{Boot: config.BootConfig{Disks: []config.DiskConfig{{Name: "data", Slot: slot}}}}
			portable := testRunFromPortable(t)
			portable.Version = 2
			portable.Boot.Disks = []config.PortableDiskConfig{{Name: "data", Slot: slot}}
			portable.Mounts = []config.MountConfig{{Type: "disk", Source: "data", Target: "/data"}}
			storage, err := artifact.NewProcessStorage(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			cache, err := diffstore.NewCache(64<<10, 32<<10)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			set, err := vhost.NewBackendSet([]vhost.BlockDeviceSpec{{Slot: true, Capacity: initial}})
			if err != nil {
				t.Fatal(err)
			}
			if err = set.Bind([]vhost.Backend{nil}); err != nil {
				t.Fatal(err)
			}
			server := vhost.NewServer("", set.DeviceBackends()[0], func(string, ...any) {})
			handler := &SnapshotHandler{Cfg: cfg, PortableConfig: portable, Disks: []SnapDiskRef{{Size: 8192}, {Slot: true}}, Servers: []*vhost.Server{server}}
			rt := &Runtime{state: runtimeRunning, lifeCtx: ctx, baseDir: t.TempDir(), diskStorage: storage, diskCache: cache, diskBackends: set, diskSnapshot: handler, diskInventory: []diskEntry{{reserved: true, dynamic: !fixed}}}
			defer rt.closeBoundDisks()
			rt.diskInventory[0].Name = "data"
			rt.diskInventory[0].State = diskUnbound
			if fixed {
				rt.diskInventory[0].SizeBytes = &initial
			}
			req := BindDiskRequest{SlotName: "data", Disk: config.DiskV1{Kind: "disk", Version: 1, Size: &capacity}}
			if err := rt.BindDisk(ctx, req); err != nil {
				t.Fatal(err)
			}
			if err := rt.diskOwned[0].cow.WaitMaterialReady(ctx); err != nil {
				t.Fatal(err)
			}
			var magic [2]byte
			if _, err := set.DeviceBackends()[0].ReadAt(magic[:], 1024+0x38); err != nil || magic != [2]byte{0x53, 0xef} {
				t.Fatalf("generated ext4: %x %v", magic, err)
			}
			if _, err := set.DeviceBackends()[0].WriteAt([]byte("preserved"), capacity-4096); err != nil {
				t.Fatal(err)
			}
			binding := handler.PortableConfig.Boot.Disks[0].Binding.ID
			req.Disk.DiffTemplate = "mkfs://ext4?size=1024KiB"
			if err := rt.BindDisk(ctx, req); err != nil {
				t.Fatal(err)
			}
			if len(rt.diskOwned) != 1 || handler.PortableConfig.Boot.Disks[0].Binding.ID != binding {
				t.Fatal("retry recreated writer")
			}
			got := make([]byte, 9)
			if _, err := set.DeviceBackends()[0].ReadAt(got, capacity-4096); err != nil || string(got) != "preserved" {
				t.Fatalf("retry lost write: %q %v", got, err)
			}
		})
	}
}
