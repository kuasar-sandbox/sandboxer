package config

import (
	"errors"
	"fmt"
)

// IsCheckpoint reports the layout selected by the enclosing Sandbox reader.
// The flag is intentionally not part of the portable runtime configuration.
func (p *PortableSandboxConfig) IsCheckpoint() bool { return p != nil && p.checkpointLayout }

// Checkpointed preserves the immutable disk graph and replaces the original
// root's self binding with its real immutable artifact identity. The caller
// obtains that identity from the running disk plan, never the new output.
func (p *PortableSandboxConfig) Checkpointed(rootBaseRef string) (*PortableSandboxConfig, error) {
	clone, err := p.Clone()
	if err != nil {
		return nil, err
	}
	if clone.checkpointLayout {
		return clone, nil
	}
	ref, err := portableArtifactRef(rootBaseRef)
	if err != nil || ref == "self" {
		return nil, errors.New("checkpoint: invalid immutable root base reference")
	}
	replace := func(base *string, lowers *[]string) {
		if *base != "self" {
			return
		}
		*base = ref
		if ref == "" && len(*lowers) != 0 {
			*base, *lowers = (*lowers)[0], (*lowers)[1:]
		}
	}
	replace(&clone.Boot.Root.Base, &clone.Boot.Root.BaseFromRefs)
	if clone.Boot.Root.Overlay != nil {
		replace(&clone.Boot.Root.Overlay.Base, &clone.Boot.Root.Overlay.BaseFromRefs)
	}
	clone.checkpointLayout = true
	if err := clone.Validate(); err != nil {
		return nil, err
	}
	return clone, nil
}

// MaterializedCheckpoint puts the exported DIFFs above their unchanged base
// graphs and restores the ordinary one-self payload layout.
func (p *PortableSandboxConfig) MaterializedCheckpoint(dataRefs []string) (*PortableSandboxConfig, error) {
	if p == nil || !p.checkpointLayout {
		return nil, errors.New("materialize: not a Checkpoint configuration")
	}
	if len(dataRefs) != len(p.Boot.Disks) {
		return nil, errors.New("materialize: data disk count mismatch")
	}
	clone, err := p.Clone()
	if err != nil {
		return nil, err
	}
	set := func(root *PortableRootConfig, top string) {
		if root.Overlay != nil {
			root.Overlay.BaseFromRefs = prependUniqueRef(root.Overlay.Base, root.Overlay.BaseFromRefs)
			root.Overlay.Base = top
		} else {
			root.BaseFromRefs = prependUniqueRef(root.Base, root.BaseFromRefs)
			root.Base = top
		}
	}
	set(&clone.Boot.Root, "self")
	for i, raw := range dataRefs {
		if clone.Boot.Disks[i].Unbound() {
			if raw != "" {
				return nil, errors.New("unbound slot has captured data")
			}
			continue
		}
		ref, err := portableArtifactRef(raw)
		if err != nil || ref == "" || ref == "self" {
			return nil, fmt.Errorf("materialize: invalid disk %d reference", i)
		}
		set(&clone.Boot.Disks[i].PortableRootConfig, ref)
	}
	clone.checkpointLayout = false
	if err := clone.Validate(); err != nil {
		return nil, err
	}
	return clone, nil
}

// WithMaterializedDisks inherits a materialized ordinary E's immutable graph
// while keeping this checkpoint launch's effective settings.
func (p *PortableSandboxConfig) WithMaterializedDisks(graph *PortableSandboxConfig) (*PortableSandboxConfig, error) {
	if !p.IsCheckpoint() || graph == nil || graph.IsCheckpoint() || len(graph.Boot.Disks) != len(p.Boot.Disks) {
		return nil, errors.New("invalid materialized checkpoint topology")
	}
	clone, err := p.Clone()
	if err != nil {
		return nil, err
	}
	disks, err := graph.Clone()
	if err != nil {
		return nil, err
	}
	for i, disk := range p.Boot.Disks {
		if disk.Name != disks.Boot.Disks[i].Name || disk.Unbound() != disks.Boot.Disks[i].Unbound() {
			return nil, errors.New("materialized checkpoint disk mismatch")
		}
	}
	clone.Boot.Root, clone.Boot.Disks, clone.checkpointLayout = disks.Boot.Root, disks.Boot.Disks, false
	return clone, clone.Validate()
}
