package sandbox

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

type BindDiskRequest = ctl.BindDiskRequest
type DiskStateReport = ctl.DiskStateReport

const (
	diskUnbound = "unbound"
	diskPending = "published_pending_guest"
	diskReady   = "ready"
)

type diskEntry struct {
	ctl.DiskState
	deviceIndex         int
	reserved            bool
	dynamic             bool
	sourceID, bindingID string
}

func appendDiskBackend(out []vhost.Backend, d DiskBackend) []vhost.Backend {
	if d.Overlay {
		out = append(out, &vhost.ReadOnlyBackend{R: d.Reader})
	}
	if d.Cow == nil {
		return append(out, nil)
	}
	return append(out, &vhost.CowBackend{C: d.Cow})
}

func snapshotDiskRef(d DiskBackend) SnapDiskRef {
	if d.Cow == nil {
		return SnapDiskRef{Slot: d.Slot}
	}
	return SnapDiskRef{Slot: d.Slot, DiffPath: d.DiffPath, OwnedDiff: d.OwnedDiff, Size: d.Cow.Size(), SnapshotView: d.Cow.SnapshotView, WaitReady: d.Cow.WaitMaterialReady, CheckError: d.Cow.Err}
}

// Restored devices already have their real geometry before the VMM resumes.
// Keep one table for both ordinary and empty devices so a zero-capacity slot
// cannot shift the ordinal of another disk or its restore socket.
func restoredDiskBackends(disks []DiskBackend, cfg *config.SandboxConfig) (*vhost.BackendSet, []RuntimeDiskSpec, error) {
	if len(disks) == 0 {
		return nil, nil, nil
	}
	shapes := make([]RuntimeDiskSpec, len(disks))
	var backends []vhost.Backend
	for i, d := range disks {
		var shape RuntimeDiskSpec
		shape.Overlay = d.Overlay
		if i > 0 && cfg != nil && i <= len(cfg.Boot.Disks) {
			shape.Name = cfg.Boot.Disks[i-1].Name
		}
		shape.Slot = d.Slot
		if d.Slot {
			if i == 0 || cfg == nil || i > len(cfg.Boot.Disks) || cfg.Boot.Disks[i-1].Slot == nil {
				return nil, nil, errors.New("restore slot has no declaration")
			}
			capacity, err := cfg.Boot.Disks[i-1].Slot.Capacity()
			if err != nil {
				return nil, nil, err
			}
			shape.WritableCapacity = capacity
			if d.Cow != nil {
				binding := cfg.Boot.Disks[i-1].Binding
				if binding == nil || binding.SizeBytes != d.Cow.Size() || (capacity != 0 && capacity != binding.SizeBytes) {
					return nil, nil, errors.New("restore slot writer differs from saved capacity")
				}
				shape.WritableCapacity = binding.SizeBytes
			} else if cfg.Boot.Disks[i-1].Binding != nil {
				return nil, nil, errors.New("restore bound slot has no writer")
			}
		} else if d.Cow != nil {
			shape.WritableCapacity = d.Cow.Size()
		} else {
			return nil, nil, errors.New("restore data disk has no writer")
		}
		if d.Overlay {
			if d.Reader == nil {
				return nil, nil, errors.New("restore overlay without lower")
			}
			shape.BaseCapacity = d.Reader.Size()
		}
		shapes[i] = shape
		backends = appendDiskBackend(backends, d)
	}
	specs, err := runtimeDeviceSpecs(shapes)
	if err != nil {
		return nil, nil, err
	}
	set, err := vhost.NewBackendSet(specs)
	if err != nil {
		return nil, nil, err
	}
	if err = set.Bind(backends); err != nil {
		return nil, nil, err
	}
	return set, shapes, nil
}

func (r *Runtime) initDiskInventory(cfg *config.SandboxConfig, disks []DiskBackend) {
	r.diskStateMu.Lock()
	defer r.diskStateMu.Unlock()
	r.diskRunID = rand.Text()
	if r.spec.CurrentRun != nil {
		r.diskRunID = r.spec.CurrentRun.RunID
	}
	if cfg == nil {
		return
	}
	index := 1
	if disks[0].Overlay {
		index++
	}
	r.diskInventory = make([]diskEntry, len(cfg.Boot.Disks))
	for i, definition := range cfg.Boot.Disks {
		d := disks[i+1]
		entry := diskEntry{DiskState: ctl.DiskState{Name: definition.Name, Ordinal: i, State: diskReady}, deviceIndex: index, reserved: definition.Slot != nil}
		if d.Cow != nil {
			size := d.Cow.Size()
			entry.SizeBytes = &size
		}
		if definition.Slot != nil {
			capacity, _ := definition.Slot.Capacity() // runtime shape already validated
			entry.dynamic = capacity == 0
			if definition.Binding == nil {
				entry.State = diskUnbound
				if capacity != 0 {
					entry.SizeBytes = &capacity
				}
			} else {
				entry.bindingID = definition.Binding.ID
				entry.sourceID = definition.Binding.SourceID
			}
		}
		r.diskInventory[i] = entry
		index++
		if d.Overlay {
			index++
		}
	}
}

func (r *Runtime) ListDataDisks(ctx context.Context) (DiskStateReport, error) {
	return r.listDataDisks(ctx, "")
}
func (r *Runtime) listDataDisks(ctx context.Context, expected string) (DiskStateReport, error) {
	if err := r.beginOperation(); err != nil {
		return DiskStateReport{}, err
	}
	defer r.opsWG.Done()
	if err := ctx.Err(); err != nil {
		return DiskStateReport{}, err
	}
	r.diskStateMu.RLock()
	defer r.diskStateMu.RUnlock()
	if r.diskRunID == "" || expected != "" && expected != r.diskRunID {
		return DiskStateReport{}, errors.New("disk inventory owner mismatch")
	}
	out := DiskStateReport{RunID: r.diskRunID, Disks: make([]ctl.DiskState, len(r.diskInventory))}
	for i, entry := range r.diskInventory {
		out.Disks[i] = entry.DiskState
		if entry.SizeBytes != nil {
			size := *entry.SizeBytes
			out.Disks[i].SizeBytes = &size
		}
	}
	return out, nil
}

func mergeDiskLocations(current, extra config.RefLocations) (config.RefLocations, error) {
	out := maps.Clone(current)
	if out == nil {
		out = config.RefLocations{}
	}
	for name, path := range extra {
		add := config.RefLocations{}
		if err := add.Set(name + "=" + (&url.URL{Scheme: "file", Path: path}).String()); err != nil {
			return nil, err
		}
		clean := add[name]
		if existing, ok := out[name]; ok && filepath.Clean(existing) != clean {
			return nil, fmt.Errorf("disk location %q conflicts with current mapping", name)
		}
		out[name] = clean
	}
	return out, nil
}

func (r *Runtime) BindDisk(ctx context.Context, req BindDiskRequest) error {
	if _, err := req.MarshalBounded(); err != nil {
		return err
	}
	if err := r.beginOperation(); err != nil {
		return err
	}
	defer r.opsWG.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(r.lifeCtx, cancel)
	defer stop()
	if err := ctx.Err(); err != nil {
		return err
	}
	h := r.diskSnapshot
	if h == nil || r.diskBackends == nil || r.diskBindGuest == nil {
		return errors.New("owner does not support disk binding")
	}
	if err := h.capture.begin(); err != nil {
		return err
	}
	defer h.capture.finish(false)
	if req.ExpectedRun != "" && req.ExpectedRun != r.diskRunID {
		return errors.New("disk bind owner mismatch")
	}
	ordinal := slices.IndexFunc(r.diskInventory, func(d diskEntry) bool { return d.Name == req.SlotName })
	if ordinal < 0 {
		return fmt.Errorf("unknown disk %q", req.SlotName)
	}
	entry := r.diskInventory[ordinal]
	if !entry.reserved {
		return fmt.Errorf("disk %s is not a slot", entry.Name)
	}
	locations, err := mergeDiskLocations(r.diskLaunch.RefLocations, req.RefLocations)
	if err != nil {
		return err
	}
	sourceID := req.Disk.Identity()
	if entry.State == diskUnbound {
		entry, err = r.publishDiskSlot(ctx, entry, req.Disk, locations)
		if err != nil {
			return err
		}
	} else {
		if entry.sourceID != sourceID {
			return fmt.Errorf("disk %s is bound to a different source", entry.Name)
		}
		if req.Disk.Size != nil && *req.Disk.Size != *entry.SizeBytes {
			return errors.New("disk source capacity mismatch")
		}
		if !maps.Equal(r.diskLaunch.RefLocations, locations) {
			if err := r.retainSlotLocations(ctx, locations); err != nil {
				return err
			}
			r.diskLaunch.RefLocations = locations
			h.RefLocations = locations
		}
	}
	if entry.State == diskReady {
		return nil
	}
	if r.spec.CurrentRun != nil {
		if err := syncDiskDirectory(r.checkpointOwner.Directory); err != nil {
			return fmt.Errorf("disk published, current-run directory sync pending: %w", err)
		}
	}
	if entry.dynamic {
		if err := h.Servers[entry.deviceIndex].NotifyCapacity(); err != nil {
			return fmt.Errorf("disk published, capacity notification pending: %w", err)
		}
	}
	bind := proto.DiskBinding{DiskIndex: entry.Ordinal, BindingID: entry.bindingID, SizeBytes: uint64(*entry.SizeBytes)}
	if err := r.diskBindGuest(ctx, bind); err != nil {
		return fmt.Errorf("disk published, guest confirmation pending: %w", err)
	}
	r.diskStateMu.Lock()
	r.diskInventory[ordinal].State = diskReady
	r.diskStateMu.Unlock()
	h.slotConfirmations.Add(-1)
	return nil
}

func (r *Runtime) publishDiskSlot(ctx context.Context, entry diskEntry, source config.DiskV1, locations config.RefLocations) (_ diskEntry, retErr error) {
	h := r.diskSnapshot
	if r.diskStorage == nil || r.diskCache == nil {
		return entry, errors.New("slot storage is unavailable")
	}
	if entry.dynamic && !h.Servers[entry.deviceIndex].CanNotifyCapacity() {
		return entry, errors.New("VMM did not negotiate dynamic disk capacity")
	}
	opts := []diffstore.DiskOption{diffstore.WithCache(r.diskCache)}
	if r.diskStorage.LocalCodec() != nil {
		keyFn := r.diskStorage.CustomerKeyFunc()
		if keyFn == nil {
			return entry, errors.New("encrypted disk source needs a key")
		}
		key, err := keyFn()
		if err != nil {
			return entry, err
		}
		defer clear(key[:])
		opts = append(opts, diffstore.WithDiffEncryption(key, r.diskStorage.LocalRequired()))
	}
	// The opened streams may be read after this operation. Scope their I/O to
	// the VM, releasing them on shutdown, rather than permanently borrowing a
	// caller's HTTP request context.
	readCtx, cancel := context.WithCancel(VMLifecycleContext(r.lifeCtx))
	stopCancel := context.AfterFunc(ctx, cancel)
	defer func() {
		stopCancel()
		if retErr != nil {
			cancel()
		}
	}()
	var resolved config.DiskV1
	base, size, template, err := openDiskV1Source(readCtx, source, r.diskStorage, locations, &resolved, opts...)
	if err != nil {
		return entry, err
	}
	defer func() {
		if retErr != nil && base != nil {
			_ = base.Close()
		}
	}()
	if entry.SizeBytes != nil && size != *entry.SizeBytes {
		return entry, fmt.Errorf("slot %s requires %d bytes, source has %d", entry.Name, *entry.SizeBytes, size)
	}
	id := rand.Text()
	plan, err := r.planSlotBind(entry.Ordinal, resolved, source.Identity(), id, size, locations)
	if err != nil {
		return entry, err
	}
	defer func() { plan.Abort() }()
	if err := os.MkdirAll(filepath.Dir(plan.DiffPath), 0755); err != nil {
		return entry, err
	}
	init, err := PrepareDiff(plan.DiffPath, template, size)
	if err != nil {
		return entry, err
	}
	if init.Existing {
		return entry, errors.New("new slot writer path already exists")
	}
	if plan.DiffID != "" {
		opts = append(opts, diffstore.WithCheckpoint(plan.DiffID, plan.BaseID))
	}
	owned := !r.spec.EnableCheckpoint
	if owned {
		opts = append(opts, diffstore.WithDiscardOnClose())
	}
	cow, err := diffstore.OpenDisk(plan.DiffPath, base, init, opts...)
	if err != nil {
		return entry, err
	}
	defer func() {
		if retErr != nil {
			_ = cow.Close()
			_ = os.Remove(plan.DiffPath)
		}
	}()
	if err = ctx.Err(); err != nil {
		return entry, err
	}

	// Detach the reader before the irreversible publication. A cancellation
	// that won the race must not leave a permanently canceled backend installed.
	if !stopCancel() {
		return entry, ctx.Err()
	}
	if cause := readCtx.Err(); cause != nil {
		return entry, cause
	}
	if err = cow.Err(); err != nil {
		return entry, err
	}
	d := DiskBackend{Slot: true, Cow: cow, DiffPath: plan.DiffPath, OwnedDiff: owned}
	ref := bindCheckpointDisks([]SnapDiskRef{snapshotDiskRef(d)}, []DiskBackend{d})[0]
	entry.State = diskPending
	entry.bindingID = id
	entry.sourceID = source.Identity()
	entry.SizeBytes = &size
	err = r.diskBackends.BindSlot(entry.deviceIndex, &vhost.CowBackend{C: cow}, func() error {
		if err := plan.Commit(ctx, cow); err != nil {
			return err
		}
		h.Servers[entry.deviceIndex].SetSlotOrigin(plan.DiffPath, size)
		return nil
	})
	if err != nil {
		return entry, err
	}
	r.diskStateMu.Lock()
	r.diskInventory[entry.Ordinal] = entry
	r.diskStateMu.Unlock()
	r.diskOwned = append(r.diskOwned, runtimeOwnedDisk{base: base, cow: cow, diffPath: plan.DiffPath, ownedDiff: owned})
	h.diskRefsMu.Lock()
	h.Disks[entry.Ordinal+1] = ref
	h.diskRefsMu.Unlock()
	h.RefLocations = locations
	r.diskLaunch.RefLocations = locations
	h.slotConfirmations.Add(1)
	if r.diskFatal != nil {
		cow.SetFatalHandler(r.diskFatal)
	}
	return entry, nil
}

func (r *Runtime) closeBoundDisks() error {
	var err error
	for _, d := range r.diskOwned {
		err = errors.Join(err, d.cow.Close())
		if d.base != nil {
			err = errors.Join(err, d.base.Close())
		}
		if d.ownedDiff {
			_ = os.Remove(d.diffPath)
		}
	}
	r.diskOwned = nil
	return err
}
