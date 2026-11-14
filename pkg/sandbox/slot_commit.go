package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
)

// slotBindPlan stages the durable and live descriptions while the owner holds the capture gate.
type slotBindPlan struct {
	DiffPath, DiffID, BaseID string
	Commit                   func(context.Context, *diffstore.Disk) error
	Abort                    func()
}

func (r *Runtime) planSlotBind(index int, source config.DiskV1, id string, size int64, loc config.RefLocations) (*slotBindPlan, error) {
	h := r.diskSnapshot
	if h == nil || h.Cfg == nil || h.PortableConfig == nil || index < 0 || index >= len(h.Cfg.Boot.Disks) || index >= len(h.PortableConfig.Boot.Disks) {
		return nil, errors.New("slot has no current capture topology")
	}
	cfg, err := cloneSDKConfig(h.Cfg)
	if err != nil {
		return nil, err
	}
	c0, err := h.PortableConfig.Clone()
	if err != nil {
		return nil, err
	}
	d := &cfg.Boot.Disks[index]
	if !d.Unbound() {
		return nil, errors.New("slot already contains a disk")
	}
	d.Binding = &config.SlotBinding{ID: id, SourceID: source.Identity(), SizeBytes: size}
	d.PureBlock = true
	d.Base, d.BaseFromRefs = source.Base, append([]string(nil), source.BaseFromRefs...)
	graph, err := config.ProjectDiskGraph(d.RootConfig)
	if err != nil {
		return nil, err
	}
	out := &c0.Boot.Disks[index]
	out.PortableRootConfig, out.Binding, out.PureBlock = graph, d.Binding, true
	if err = c0.Validate(); err != nil {
		return nil, err
	}
	plan := &slotBindPlan{Abort: func() {}}
	diffID := diffstore.NewDiffID()
	var candidate *CheckpointDiskPlan
	if r.spec.checkpointPlan != nil {
		original := r.spec.checkpointPlan
		copyPlan := *original
		copyPlan.Disks = append([]CheckpointDiskBinding(nil), original.Disks...)
		candidate = &copyPlan
		plan.DiffID, plan.BaseID = diffID, artifact.DiffCheckpointBaseID(graph)
		plan.DiffPath = filepath.Join(candidate.Directory, "diffs", diffID+".diff")
		candidate.Disks[index+1] = CheckpointDiskBinding{DiffID: diffID, BaseID: plan.BaseID, Path: plan.DiffPath}
		if m := h.checkpointMaintenance; m != nil {
			if err = m.ReserveActive(diffID); err != nil {
				return nil, err
			}
			plan.Abort = func() { m.ReleaseActive(diffID) }
		}
	} else {
		plan.DiffPath = filepath.Join(r.baseDir, fmt.Sprintf("slot-%d-%s.diff", index, diffID))
	}
	d.Diff = "file://" + plan.DiffPath
	plan.Commit = func(ctx context.Context, cow *diffstore.Disk) error {
		if cow == nil || cow.Size() != size {
			return errors.New("prepared slot writer capacity mismatch")
		}
		if r.spec.CurrentRun != nil {
			if err := cow.WaitMaterialized(ctx); err != nil {
				return err
			}
			f, err := os.OpenFile(plan.DiffPath, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			if err = errors.Join(f.Sync(), f.Close()); err != nil {
				return err
			}
			if err = syncDiskDirectory(filepath.Dir(plan.DiffPath)); err != nil {
				return err
			}
			// Rename publishes the metadata before its directory sync. Preserve
			// this writer on either outcome; Bind retries the sync before ACK.
			if err = r.retainSlotTopology(ctx, cfg, c0, candidate, index, size, loc); err != nil && !errors.Is(err, artifact.ErrCurrentMetadataPublished) {
				return err
			}
		}
		h.Cfg, h.PortableConfig = cfg, c0
		r.spec.checkpointPlan = candidate
		plan.Abort = func() {}
		return nil
	}
	return plan, nil
}

func (r *Runtime) retainSlotLocations(ctx context.Context, locations config.RefLocations) error {
	if r.spec.CurrentRun == nil {
		return nil
	}
	h := r.diskSnapshot
	return r.retainSlotTopology(ctx, h.Cfg, h.PortableConfig, r.spec.checkpointPlan, -1, 0, locations)
}

func (r *Runtime) retainSlotTopology(ctx context.Context, cfg *config.SandboxConfig, c0 *config.PortableSandboxConfig, plan *CheckpointDiskPlan, index int, size int64, locations config.RefLocations) error {
	h := r.diskSnapshot
	caps := make([]int64, len(h.Disks))
	for i, d := range h.Disks {
		caps[i] = d.Size
	}
	if index >= 0 {
		caps[index+1] = size
	}
	parent := ""
	if h.SourceBinding != nil {
		parent = h.SourceBinding.SandboxRef
	}
	return RetainCurrentDiskPlan(ctx, r.diskStorage, r.checkpointOwner, r.spec.CurrentRun, cfg, c0, parent, plan, caps, locations, RunOptions{
		SourceBinding: h.SourceBinding, BundleReader: h.BundleReader, BundleFetcher: h.BundleFetcher, Fetcher: h.Fetcher,
	})
}

func syncDiskDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
