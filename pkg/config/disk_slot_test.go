package config

import (
	"bytes"
	"fmt"
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
	slots := map[string]bool{"work": true}
	if err := validateMountProjections(base, slots); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []MountConfig{
		{Type: "disk", Source: "work", Subpath: "a/one", Target: "/extra"},
		{Type: "disk", Source: "other", Subpath: "ok", Target: "/data/shared"},
		{Type: "disk", Source: "work", Subpath: "x/../a.b", Target: "/data.b"},
	} {
		got := []MountConfig{base[0], bad}
		if err := validateMountProjections(got, slots); err == nil {
			t.Fatal("accepted", bad)
		}
		if err := validateFromDiskMountTopology(base, got, slots); err == nil {
			t.Fatal("restored", bad)
		}
	}
}

func TestSlotFieldsFollowYAMLReaderPolicy(t *testing.T) {
	for _, tc := range []struct {
		fields    string
		wantBytes int64
	}{
		{"{szie: 8GiB}", 0},
		{"{szie: null, size: 8GiB}", 8 << 30},
	} {
		raw := []byte("boot:\n  disks:\n    - {name: data, slot: " + tc.fields + "}\n")
		host, err := LoadConfigBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		slot := host.Boot.Disks[0].Slot
		if size, err := slot.Capacity(); slot == nil || err != nil || size != tc.wantBytes {
			t.Fatalf("host slot %s: capacity %d, error %v", tc.fields, size, err)
		}
	}

	cfg := validPortableConfig()
	cfg.Version = 2
	cfg.Boot.Disks = []PortableDiskConfig{{Name: "data", Slot: &DiskSlotConfig{}}}
	cfg.Mounts = []MountConfig{{Type: "disk", Source: "data", Target: "/data"}}
	raw, err := MarshalPortableSandboxConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, fields := range []string{"{szie: 8GiB}", "{size: null}", "{size: 0}"} {
		patched := bytes.Replace(raw, []byte("slot: {}"), []byte("slot: "+fields), 1)
		if _, err := ParsePortableSandboxConfig(patched); err == nil {
			t.Errorf("portable slot accepted %s", fields)
		}
	}
}

func TestInvalidSlotSizeAndInlineGraphInHostYAML(t *testing.T) {
	for _, input := range []string{
		"slot: {size: 0}", "slot: {size: null}", "slot: {size: 1}", "slot: {size: 2TiB}",
		"disk: {kind: disk, version: 1, diff_template: file:///seed, siez: 4096}",
		"disk: {kind: disk, version: 1, diff_template: file:///seed, size: null}",
	} {
		var disk DiskConfig
		if err := yaml.Unmarshal([]byte(input), &disk); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestSlotMountScopeAndNewInput(t *testing.T) {
	slots := map[string]bool{"slot": true}
	mounts := []MountConfig{
		{Type: "disk", Source: "ordinary", Target: "/data", Subpath: "in"},
		{Type: "tmpfs", Target: "/data/cache"},
		{Type: "disk", Source: "ordinary", Target: "/other", Subpath: "in/child"},
		{Type: "disk", Source: "slot", Target: "/work", Subpath: ".git"},
	}
	if err := validateMountProjections(mounts, slots); err != nil {
		t.Fatal(err)
	}
	for _, m := range []MountConfig{
		{Type: "empty", Target: "/work/cache"},
		{Type: "disk", Source: "slot", Subpath: ".git/objects", Target: "/objects"},
		{Type: "disk", Source: "slot", Subpath: "other", Target: "/data"},
	} {
		if err := validateMountProjections(append(mounts, m), slots); err == nil {
			t.Errorf("accepted %+v", m)
		}
	}
	for _, pair := range [][2]string{{"/a/./dir", "ok"}, {"/a/../dir", "ok"}, {"/dir", "."}, {"/dir", "a/../b"}, {"/dir", "a/./b"}} {
		yml := fmt.Sprintf("mounts: [{type: disk, source: disk, target: %q, subpath: %q}]", pair[0], pair[1])
		if _, err := LoadConfigBytes([]byte(yml)); err == nil {
			t.Errorf("accepted %s", yml)
		}
		if _, _, err := LoadConfigBytesWithPresence([]byte(yml)); err == nil {
			t.Errorf("presence loader accepted %s", yml)
		}
	}
	if _, err := LoadConfigBytes([]byte("mounts: [{type: disk, source: disk, target: /dir//x/, subpath: .git}]")); err != nil {
		t.Fatal(err)
	}
}

func TestReadSavedDotSpellingAndCheckBoundSlotTopology(t *testing.T) {
	p := validPortableConfig()
	p.Version = 2
	p.Boot.Disks = []PortableDiskConfig{{Name: "slot", Slot: &DiskSlotConfig{}, Binding: &SlotBinding{ID: "id", SourceID: "source", SizeBytes: 4096}, PureBlock: true}}
	p.Mounts = []MountConfig{{Type: "disk", Source: "slot", Target: "/work/./repo", Subpath: "."}}
	raw, err := MarshalPortableSandboxConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParsePortableSandboxConfig(raw)
	if err != nil || got.Mounts[0] != p.Mounts[0] {
		t.Fatalf("legacy spelling lost: %v", err)
	}
	if err := validateFromDiskMountTopology(p.Mounts, p.Mounts, p.slotNames()); err == nil {
		t.Fatal("new override accepted legacy spelling")
	}
	p.Mounts = append(p.Mounts, MountConfig{Type: "empty", Target: "/work/repo/cache"})
	if err := p.Validate(); err == nil {
		t.Fatal("cold bound slot classified as ordinary")
	}
}
