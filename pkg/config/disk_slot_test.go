package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSlotTopologyRoundTripAndBothCaptureLayouts(t *testing.T) {
	p := validPortableConfig()
	p.Version = 2
	fixed := "8GiB"
	p.Boot.Disks = []PortableDiskConfig{
		{Name: "first"},
		{Name: "future", Slot: &DiskSlotConfig{Size: &fixed}},
		{Name: "last", Slot: &DiskSlotConfig{}, Binding: &SlotBinding{ID: "binding", SourceID: "source", SizeBytes: 8192}, PureBlock: true},
	}
	for _, d := range p.Boot.Disks {
		p.Mounts = append(p.Mounts, MountConfig{Type: "disk", Source: d.Name, Target: "/" + d.Name})
	}
	p.Mounts[2].Subpath = "data"
	p.Mounts = append(p.Mounts, MountConfig{Type: "disk", Source: "last", Subpath: "out", Target: "/results"})
	buf, err := MarshalPortableSandboxConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	q, err := ParsePortableSandboxConfig(buf)
	if err != nil {
		t.Fatal(err)
	}
	q.Version = 1
	if q.Validate() == nil {
		t.Fatal("v1 accepted a bound slot")
	}
	top := []string{"manifest://" + testSHA, "", "manifest://" + testSHA2}
	saved, err := p.Exported("", top, []bool{false, false, false, false})
	if err != nil {
		t.Fatal(err)
	}
	cp, err := p.Checkpointed("")
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := cp.MaterializedCheckpoint(top)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []*PortableSandboxConfig{saved, materialized} {
		d := got.Boot.Disks
		if !d[1].Unbound() || d[1].Base != "" || d[0].Base != top[0] || d[2].Base != top[2] || d[2].Binding.ID != "binding" || got.Mounts[3].Subpath != "out" {
			t.Fatalf("changed topology: %+v", got)
		}
	}
	top[1] = top[0]
	if _, err := cp.MaterializedCheckpoint(top); err == nil {
		t.Fatal("unbound slot accepted a captured diff")
	}
}

func TestSlotTopologyRejectsReplacedOrOverlappingProjections(t *testing.T) {
	base := []MountConfig{{Type: "disk", Source: "work", Subpath: "a", Target: "/data"}, {Type: "disk", Source: "work", Subpath: "a.b", Target: "/data.b"}}
	if err := ValidateMountProjections(base); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []MountConfig{
		{Type: "disk", Source: "work", Subpath: "a/one", Target: "/extra"},
		{Type: "disk", Source: "other", Subpath: "ok", Target: "/data/shared"},
		{Type: "disk", Source: "work", Subpath: "x/../a.b", Target: "/data.b"},
	} {
		got := []MountConfig{base[0], bad}
		if err := ValidateMountProjections(got); err == nil {
			t.Fatal("accepted", bad)
		}
		if err := validateFromDiskMountTopology(base, got); err == nil {
			t.Fatal("restored", bad)
		}
	}
}

func TestSlotAndInlineGraphStrictInHostYAML(t *testing.T) {
	for _, input := range []string{
		"slot: {capacity: 8GiB}", "slot: {size: 0}", "slot: {size: null}",
		"disk: {kind: disk, version: 1, diff_template: file:///seed, siez: 4096}",
		"disk: {kind: disk, version: 1, diff_template: file:///seed, size: null}",
	} {
		var disk DiskConfig
		if err := yaml.Unmarshal([]byte(input), &disk); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
