package config

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/kuasar-sandbox/sandboxer/pkg/util"
)

// DiskSlotConfig reserves one virtio block device; nil Size adopts the bound disk's geometry.
type DiskSlotConfig struct {
	Size *string `yaml:"size,omitempty" json:"size,omitempty"`
}

func (s *DiskSlotConfig) Capacity() (int64, error) {
	if s == nil || s.Size == nil {
		return 0, nil
	}
	n, err := util.ParseSize(*s.Size)
	if err != nil || n == 0 || n%4096 != 0 || n > 1<<40 {
		return 0, fmt.Errorf("slot.size must be a positive 4 KiB multiple up to 1 TiB: %q", *s.Size)
	}
	return int64(n), nil
}

// SlotBinding identifies the actual medium at the captured time.
type SlotBinding struct {
	ID        string `yaml:"id"`
	SizeBytes int64  `yaml:"size_bytes"`
	SourceID  string `yaml:"source_id"`
}

// SourceDefinition returns the block graph after local artifact bindings have been resolved.
func (d DiskConfig) SourceDefinition() DiskV1 {
	if d.Disk != nil {
		return *d.Disk
	}
	return DiskV1{Kind: "disk", Version: 1, Base: d.Base, BaseFromRefs: d.BaseFromRefs, DiffTemplate: d.DiffTemplate}
}
func (d DiskConfig) Unbound() bool         { return d.Slot != nil && d.Binding == nil }
func (d PortableDiskConfig) Unbound() bool { return d.Slot != nil && d.Binding == nil }

func (d *DiskConfig) validateSource(field string, cold bool) error {
	if d.Slot == nil && d.Disk == nil {
		return validateDiskSource(&d.RootConfig, field, cold)
	}
	if d.Disk != nil && d.Slot != nil || d.Overlay != nil {
		return fmt.Errorf("%s: slot, disk and overlay are mutually exclusive", field)
	}
	// A restored or internally prepared device carries separately resolved graph bindings.
	if !d.PureBlock && (d.Base != "" || len(d.BaseFromRefs) != 0 || d.Diff != "" || d.DiffTemplate != "") {
		return fmt.Errorf("%s: disk or slot cannot be combined with legacy disk sources", field)
	}
	if d.Disk != nil {
		return d.Disk.Validate()
	}
	_, err := d.Slot.Capacity()
	return err
}

// PrepareInlineDiskGraphs validates configuration before checkpoint planning
// assigns private writer paths, then exposes the effective immutable graph to
// capture and dependency retention. The DiskV1 still owns the template.
func (c *SandboxConfig) PrepareInlineDiskGraphs() error {
	for i := range c.Boot.Disks {
		d := &c.Boot.Disks[i]
		if d.Disk == nil {
			continue
		}
		if err := d.validateSource(fmt.Sprintf("boot.disks[%d]", i), true); err != nil {
			return err
		}
		d.Base, d.BaseFromRefs, d.PureBlock = d.Disk.Base, append([]string(nil), d.Disk.BaseFromRefs...), true
	}
	return nil
}

// ValidateMountProjections checks source overlap per disk and all targets that can mask a disk.
// Path comparisons are component based and independent of the host filesystem.
func ValidateMountProjections(mounts []MountConfig) error {
	type projection struct {
		target, source, subpath string
		disk                    bool
	}
	previous := make([]projection, 0, len(mounts))
	for i, m := range mounts {
		cur := projection{target: path.Clean(m.Target), source: m.Source, subpath: path.Clean(m.Subpath), disk: m.Type == "disk"}
		if !cur.disk && m.Subpath != "" {
			return fmt.Errorf("mounts[%d].subpath requires type=disk", i)
		}
		if cur.disk {
			if !path.IsAbs(m.Target) || path.IsAbs(m.Subpath) || strings.Contains("/"+m.Subpath+"/", "/../") || strings.IndexByte(m.Subpath, 0) >= 0 {
				return fmt.Errorf("mounts[%d]: invalid disk target %q or subpath %q", i, m.Target, m.Subpath)
			}
		}
		for j, p := range previous {
			if cur.disk && p.disk && cur.source == p.source && pathsOverlap(path.Join("/", cur.subpath), path.Join("/", p.subpath)) {
				return fmt.Errorf("mounts[%d]: disk %q subpath %q already mounted or overlaps mounts[%d] (%q)", i, m.Source, m.Subpath, j, p.subpath)
			}
			if (cur.disk || p.disk) && pathsOverlap(cur.target, p.target) {
				return fmt.Errorf("mounts[%d] target %q overlaps mounts[%d] target %q", i, cur.target, j, p.target)
			}
		}
		previous = append(previous, cur)
	}
	return nil
}

func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
}

func validateSlotArtifact(d PortableDiskConfig) error {
	if d.Slot == nil {
		if d.Binding != nil {
			return errors.New("disk binding requires a slot")
		}
		return nil
	}
	n, err := d.Slot.Capacity()
	if err != nil {
		return err
	}
	if d.Overlay != nil {
		return errors.New("slot cannot be an overlay")
	}
	if b := d.Binding; b != nil {
		if b.ID == "" || b.SourceID == "" || b.SizeBytes <= 0 || b.SizeBytes%4096 != 0 || n != 0 && n != b.SizeBytes {
			return errors.New("invalid saved slot identity or capacity")
		}
	} else if d.Base != "" || len(d.BaseFromRefs) != 0 || d.PureBlock {
		return errors.New("unbound slot has a disk source")
	}
	return nil
}

func checkHostDiskSelection(host *DiskConfig, saved *PortableDiskConfig) error {
	if saved.Slot != nil && (host.Diff != "" || host.DiffTemplate != "") {
		return fmt.Errorf("slot %q writer belongs to the captured artifact", host.Name)
	}
	if host.Disk != nil {
		return fmt.Errorf("immutable source of disk %q belongs to the captured artifact", host.Name)
	}
	if host.Slot == nil {
		return nil
	}
	a, err := host.Slot.Capacity()
	if err != nil {
		return err
	}
	b, err := saved.Slot.Capacity()
	if err != nil {
		return err
	}
	if saved.Slot == nil || a != b {
		return fmt.Errorf("slot %q conflicts with the captured device", host.Name)
	}
	return nil
}

// ProjectDiskGraph produces portable content references for a bound data disk.
func ProjectDiskGraph(root RootConfig) (PortableRootConfig, error) {
	return projectColdRoot(&root, false)
}
