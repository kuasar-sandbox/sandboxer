package sandbox

import (
	"context"
	"errors"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
)

// RetainCurrentDiskPlan is shared by cold Launch and memory Restore. The plan,
// portable bases and effective host graph must describe the same ordered disks.
// It records even a not-yet-initialized set; only the offline opener decides if
// a terminated run actually left complete, readable disks.
func RetainCurrentDiskPlan(ctx context.Context, storage *artifact.ProcessStorage, owner *diffstore.DirectoryOwner, run *artifact.CurrentRun, cfg *config.SandboxConfig, portable *config.PortableSandboxConfig, parent string, plan *CheckpointDiskPlan, capacities []int64, locations config.RefLocations, provenance ...RunOptions) error {
	if run == nil {
		return nil
	}
	if err := run.Validate(); err != nil {
		return err
	}
	if cfg == nil || portable == nil || owner == nil || plan == nil || len(plan.Disks) != len(capacities) || len(plan.Disks) != len(cfg.Boot.Disks)+1 {
		return errors.New("retained current disks require a complete checkpoint plan")
	}
	bases, err := portable.Checkpointed(parent)
	if err != nil {
		return err
	}
	sources := map[string]string{}
	add := func(p, actual string) error {
		if p == "" {
			return nil
		}
		if old, ok := sources[p]; ok && old != actual {
			return errors.New("ambiguous current disk dependency binding")
		}
		sources[p] = actual
		return nil
	}
	bind := func(p config.PortableRootConfig, actual config.RootConfig) error {
		if err := add(p.Base, actual.Base); err != nil {
			return err
		}
		if len(p.BaseFromRefs) != len(actual.BaseFromRefs) {
			return errors.New("current lower topology mismatch")
		}
		for i, raw := range p.BaseFromRefs {
			if err := add(raw, actual.BaseFromRefs[i]); err != nil {
				return err
			}
		}
		if (p.Overlay == nil) != (actual.Overlay == nil) {
			return errors.New("current overlay topology mismatch")
		}
		if p.Overlay != nil {
			if err := add(p.Overlay.Base, actual.Overlay.Base); err != nil {
				return err
			}
			if len(p.Overlay.BaseFromRefs) != len(actual.Overlay.BaseFromRefs) {
				return errors.New("current overlay lowers mismatch")
			}
			for i, raw := range p.Overlay.BaseFromRefs {
				if err := add(raw, actual.Overlay.BaseFromRefs[i]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	disks := make([]artifact.CurrentDisk, len(plan.Disks))
	for i, d := range plan.Disks {
		p, actual, name := bases.Boot.Root, cfg.Boot.Root, ""
		if i > 0 {
			p, actual, name = bases.Boot.Disks[i-1].PortableRootConfig, cfg.Boot.Disks[i-1].RootConfig, cfg.Boot.Disks[i-1].Name
		}
		if i > 0 && cfg.Boot.Disks[i-1].Unbound() {
			size, err := cfg.Boot.Disks[i-1].Slot.Capacity()
			if err != nil {
				return err
			}
			disks[i] = artifact.CurrentDisk{Name: name, Capacity: size}
			continue
		}
		if err := bind(p, actual); err != nil {
			return err
		}
		disks[i] = artifact.CurrentDisk{Name: name, DiffID: d.DiffID, BaseID: d.BaseID, Capacity: capacities[i]}
	}
	// Reuse named capture's physical-source resolution, including local Bundle
	// selectors. Carrier/location rewriting preserves DiffCheckpointBaseID.
	var opts RunOptions
	if len(provenance) > 0 {
		opts = provenance[0]
	}
	opts.Cfg, opts.PortableConfig, opts.SandboxID, opts.RefLocations = cfg, bases, run.SandboxID, locations
	known := bundleKnownRefPaths(opts)
	dependencies, err := snapshotLayerDependencies(opts, nil, make([]bool, len(disks)))
	if err != nil {
		return err
	}
	replacements := map[string]string{}
	for _, dep := range dependencies {
		retained, err := preserveNamedCheckpointDependency(ctx, dep, opts, known, owner.Directory)
		if err != nil {
			return err
		}
		replacements[dep.raw] = retained
	}
	bases, err = bases.RewriteDiskArtifactRefs(replacements)
	if err != nil {
		return err
	}
	// Every unlocated dependency now resolves in owner.Directory; remote and
	// located dependencies retain their explicit access contract.
	return storage.RetainCurrentDisks(ctx, owner, *run, bases, disks, locations)
}
