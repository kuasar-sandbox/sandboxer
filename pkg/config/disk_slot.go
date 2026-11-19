package config

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/kuasar-sandbox/sandboxer/pkg/util"
	"gopkg.in/yaml.v3"
)

// DiskSlotConfig reserves one virtio block device; nil Size adopts the bound disk's geometry.
type DiskSlotConfig struct {
	Size *string `yaml:"size,omitempty" json:"size,omitempty"`
}

// The decoder callback preserves the enclosing reader's unknown-field policy.
// Keep the size node to distinguish an omitted capacity from an explicit null.
func (s *DiskSlotConfig) UnmarshalYAML(decode func(any) error) error {
	var fields struct {
		Size yaml.Node `yaml:"size"`
	}
	if err := decode(&fields); err != nil {
		return err
	}
	if fields.Size.IsZero() {
		s.Size = nil
		return nil
	}
	if fields.Size.ShortTag() == "!!null" {
		return errors.New("slot.size cannot be null")
	}
	var size string
	if err := fields.Size.Decode(&size); err != nil {
		return err
	}
	s.Size = &size
	_, err := s.Capacity()
	return err
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
		normalized, err := d.Disk.NormalizeCreate()
		if err != nil {
			return err
		}
		*d.Disk = normalized
		return nil
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

// ValidateMountProjections checks new declarations without a boot disk table.
// With no declared slots, ordinary mount ordering applies.
func ValidateMountProjections(mounts []MountConfig) error {
	if err := ValidateMountPaths(mounts); err != nil {
		return err
	}
	return validateMountProjections(mounts, nil)
}

// ValidateMountPaths checks the original spelling of new mount declarations
// before they are combined with existing portable mounts or image volumes.
func ValidateMountPaths(mounts []MountConfig) error {
	for i, m := range mounts {
		if hasDotComponent(m.Target) {
			return fmt.Errorf("mounts[%d].target %q contains a . or .. component", i, m.Target)
		}
		if hasDotComponent(m.Subpath) {
			return fmt.Errorf("mounts[%d].subpath %q contains a . or .. component", i, m.Subpath)
		}
	}
	return nil
}

// ValidateMountTopology checks effective mounts, including inherited image volumes.
func (c *SandboxConfig) ValidateMountTopology(mounts []MountConfig) error {
	slots := make(map[string]bool)
	for _, d := range c.Boot.Disks {
		if d.Slot != nil {
			slots[d.Name] = true
		}
	}
	return validateMountProjections(mounts, slots)
}

func (p *PortableSandboxConfig) slotNames() map[string]bool {
	slots := make(map[string]bool)
	for _, d := range p.Boot.Disks {
		if d.Slot != nil {
			slots[d.Name] = true
		}
	}
	return slots
}

func hasParentComponent(name string) bool {
	return strings.Contains("/"+name+"/", "/../")
}

func hasDotComponent(name string) bool {
	for component := range strings.SplitSeq(name, "/") {
		if component == "." || component == ".." {
			return true
		}
	}
	return false
}

func validateMountProjections(mounts []MountConfig, slots map[string]bool) error {
	type projection struct {
		target, source, subpath string
		slot, disk              bool
	}
	previous := make([]projection, 0, len(mounts))
	for i, m := range mounts {
		disk := m.Type == "disk"
		if !disk && m.Subpath != "" {
			return fmt.Errorf("mounts[%d].subpath requires type=disk", i)
		}
		if disk && (!path.IsAbs(m.Target) || path.IsAbs(m.Subpath) || strings.Contains("/"+m.Subpath+"/", "/../") || strings.IndexByte(m.Subpath, 0) >= 0) {
			return fmt.Errorf("mounts[%d]: invalid disk target %q or subpath %q", i, m.Target, m.Subpath)
		}
		cur := projection{target: path.Clean(m.Target), source: m.Source, subpath: path.Clean(m.Subpath), slot: disk && slots[m.Source], disk: disk}
		for j, p := range previous {
			if cur.disk && p.disk && !cur.slot && cur.source == p.source && cur.subpath == "." && p.subpath == "." {
				return fmt.Errorf("mounts[%d]: disk %q root already mounted at mounts[%d]", i, m.Source, j)
			}
			if cur.slot && p.slot && cur.source == p.source && pathsOverlap(path.Join("/", cur.subpath), path.Join("/", p.subpath)) {
				return fmt.Errorf("mounts[%d]: slot %q subpath %q overlaps mounts[%d] (%q)", i, m.Source, m.Subpath, j, p.subpath)
			}
			// A saved target or an OCI volume may contain a parent component.
			// Its meaning depends on image symlinks. Guest checks the actual pair.
			if (cur.slot || p.slot) && !hasParentComponent(m.Target) && !hasParentComponent(mounts[j].Target) && pathsOverlap(cur.target, p.target) {
				return fmt.Errorf("mounts[%d] target %q overlaps mounts[%d] target %q", i, m.Target, j, mounts[j].Target)
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
		if d.PureBlock && d.Overlay != nil {
			return errors.New("pure block disk cannot be an overlay")
		}
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
		if !d.PureBlock || b.ID == "" || b.SourceID == "" || b.SizeBytes <= 0 || b.SizeBytes > 1<<40 || b.SizeBytes%4096 != 0 || n != 0 && n != b.SizeBytes {
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
