package sandbox

import (
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"gopkg.in/yaml.v3"
)

// TestBuildSnapshotCfg_SingleDisk verifies a single-disk snapshot.cfg records
// the captured diff at root.base (with the cold-start CoW lower chained into
// base_from_refs) and emits neither a base_ref nor an overlay node.
func TestBuildSnapshotCfg_SingleDisk(t *testing.T) {
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity.CPU = 2
	cfg.Resources.Capacity.Memory = "2GiB"
	cfg.SnapshotRefs.RuntimeRef = "file://rt@sha256:aa"
	cfg.SnapshotRefs.BaseRef = "file://img@sha256:bb" // overlay-only; must be omitted here
	cfg.Boot.Root.DiffTemplate = "file:///root.ext4"  // Overlay nil ⇒ single-disk
	cfg.Boot.Root.Base = "manifest://coldbase"        // CoW lower → chained on cold start

	body, err := buildSnapshotCfg(cfg, []string{"manifest://captured"}, nil, []bool{false})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{"base: manifest://captured", "base_from_refs:", "manifest://coldbase"} {
		if !strings.Contains(s, want) {
			t.Errorf("single-disk snapshot.cfg missing %q\n%s", want, s)
		}
	}
	for _, banned := range []string{"overlay:", "base_ref:"} {
		if strings.Contains(s, banned) {
			t.Errorf("single-disk snapshot.cfg must not contain %q\n%s", banned, s)
		}
	}
}

// TestBuildSnapshotCfg_Overlay verifies overlay mode still records base_ref +
// the overlay node (the captured diff at overlay.base).
func TestBuildSnapshotCfg_Overlay(t *testing.T) {
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity.CPU = 2
	cfg.Resources.Capacity.Memory = "2GiB"
	cfg.SnapshotRefs.RuntimeRef = "file://rt@sha256:aa"
	cfg.SnapshotRefs.BaseRef = "file://img@sha256:bb"
	cfg.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///d.ext4"} // overlay mode

	body, err := buildSnapshotCfg(cfg, []string{"manifest://captured"}, nil, []bool{false})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{"base_ref: file://img@sha256:bb", "overlay:", "base: manifest://captured"} {
		if !strings.Contains(s, want) {
			t.Errorf("overlay snapshot.cfg missing %q\n%s", want, s)
		}
	}
}

func TestBuildSnapshotCfg_OmitsRestorePolicy(t *testing.T) {
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity.CPU = 2
	cfg.Resources.Capacity.Memory = "2GiB"
	cfg.SnapshotRefs.RuntimeRef = "file://rt@sha256:aa"
	cfg.SnapshotRefs.BaseRef = "file://img@sha256:bb"
	cfg.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///d.ext4"}
	cfg.Restore.Prefetch = "memory"

	body, err := buildSnapshotCfg(cfg, []string{"manifest://captured"}, nil, []bool{false})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "prefetch:") || strings.Contains(string(body), "restore:") {
		t.Fatalf("snapshot.cfg must not persist host restore policy:\n%s", body)
	}
}

func TestBuildSnapshotCfg_PersistsCgroupControl(t *testing.T) {
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity.CPU = 2
	cfg.Resources.Capacity.Memory = "2GiB"
	cfg.Launch.CgroupControl = true
	cfg.SnapshotRefs.RuntimeRef = "file://rt@sha256:aa"
	cfg.Boot.Root.DiffTemplate = "file:///root.ext4"

	body, err := buildSnapshotCfg(cfg, []string{"manifest://captured"}, nil, []bool{false})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Launch struct {
			CgroupControl bool `yaml:"cgroup_control"`
		} `yaml:"launch"`
	}
	if err := yaml.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Launch.CgroupControl {
		t.Fatalf("snapshot.cfg lost launch.cgroup_control:\n%s", body)
	}
}

// TestBuildSnapshotCfg_OverlayColdBase verifies that an overlay-mode COLD start
// with an inherited overlay.base (e.g. a fromTemplate build) chains that
// read-only lower into the snapshot's overlay.base_from_refs — else a restore of
// the new snapshot would lose the inherited layer's filesystem.
func TestBuildSnapshotCfg_OverlayColdBase(t *testing.T) {
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity.CPU = 2
	cfg.Resources.Capacity.Memory = "2GiB"
	cfg.SnapshotRefs.RuntimeRef = "file://rt@sha256:aa"
	cfg.SnapshotRefs.BaseRef = "file://img@sha256:bb"
	cfg.Boot.Root.Overlay = &config.OverlayConfig{
		Base: "manifest://templatelower", // inherited ro lower (fromTemplate)
		Diff: "file:///d.ext4",
	}

	body, err := buildSnapshotCfg(cfg, []string{"manifest://captured"}, nil, []bool{false})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"base_ref: file://img@sha256:bb",
		"base: manifest://captured", // captured top diff at overlay.base
		"base_from_refs:",           // inherited lower chained below it
		"manifest://templatelower",  // ... the inherited template overlay
	} {
		if !strings.Contains(s, want) {
			t.Errorf("overlay cold-base snapshot.cfg missing %q\n%s", want, s)
		}
	}
}

// TestBuildSnapshotCfg_DataDisks verifies boot.disks[] is rendered, one node per
// data disk (single → base; overlay → base_ref + overlay.base), with the
// captured overlay refs taken from the per-disk overlayRefs (root is [0]).
func TestBuildSnapshotCfg_DataDisks(t *testing.T) {
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity.CPU = 2
	cfg.Resources.Capacity.Memory = "2GiB"
	cfg.SnapshotRefs.RuntimeRef = "file://rt@sha256:aa"
	cfg.SnapshotRefs.BaseRef = "file://img@sha256:bb"
	cfg.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///d.ext4"} // overlay root
	cfg.Boot.Disks = []config.DiskConfig{
		{Name: "scratch", RootConfig: config.RootConfig{DiffTemplate: "file:///s.ext4"}},
		{Name: "dataset", RootConfig: config.RootConfig{Base: "file:///ds.erofs", Overlay: &config.OverlayConfig{Diff: "file:///u.ext4"}}},
	}
	cfg.SnapshotRefs.DiskBaseRefs = []string{"", "file://ds@sha256:cc"}

	body, err := buildSnapshotCfg(cfg, []string{"manifest://root", "manifest://scratch", "manifest://dataset"},
		nil, []bool{false, false, false})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"disks:",
		"base: manifest://scratch",      // single data disk: captured diff at base
		"base_ref: file://ds@sha256:cc", // overlay data disk: erofs base ref
		"base: manifest://dataset",      // overlay data disk: captured upper at overlay.base
	} {
		if !strings.Contains(s, want) {
			t.Errorf("data-disk snapshot.cfg missing %q\n%s", want, s)
		}
	}
}

func TestBuildSnapshotCfg_MixedLocalMemoryManifestDiskKeepsDiskParent(t *testing.T) {
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity.CPU = 2
	cfg.Resources.Capacity.Memory = "2GiB"
	cfg.SnapshotRefs.RuntimeRef = "file://rt@sha256:aa"
	cfg.SnapshotRefs.BaseRef = "file://root@sha256:bb"
	cfg.SnapshotRefs.DiskBaseRefs = []string{"file://data@sha256:cc"}
	cfg.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///root.diff"}
	cfg.Boot.Disks = []config.DiskConfig{{
		Name: "data",
		RootConfig: config.RootConfig{
			Base:    "file:///data.erofs",
			Overlay: &config.OverlayConfig{Diff: "file:///data.diff"},
		},
	}}
	cfg.SnapshotProvenance = config.SnapshotProvenance{
		ParentSnapshotRef:  "file://parent.snapshot",
		ParentSnapshotPath: "/snapshots/parent.snapshot",
		ParentOverlayBase:  "file://parent-root.overlay",
		ParentOverlayPath:  "/snapshots/parent-root.overlay",
		ParentBaseFromRefs: []string{"manifest://root-lower"},
		ParentDisks: []config.DiskProvenance{{
			OverlayBase:  "manifest://data-parent",
			BaseFromRefs: []string{"manifest://data-lower"},
		}},
	}

	body, err := buildSnapshotCfg(cfg,
		[]string{"manifest://new-root", "manifest://new-data"},
		[]string{"file://parent.snapshot"}, []bool{true, false})
	if err != nil {
		t.Fatal(err)
	}
	var doc snapshotCfgYAML
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	rootChain := doc.Boot.Root.Overlay.BaseFromRefs
	if len(rootChain) != 1 || rootChain[0] != "manifest://root-lower" {
		t.Fatalf("root chain = %v, want merged parent lower only", rootChain)
	}
	dataChain := doc.Boot.Disks[0].Overlay.BaseFromRefs
	if len(dataChain) != 2 || dataChain[0] != "manifest://data-parent" || dataChain[1] != "manifest://data-lower" {
		t.Fatalf("data chain = %v, want portable parent retained", dataChain)
	}
}

func TestBuildSnapshotCfg_LocalWorkingSetKeepsMemoryParentButMergesDisks(t *testing.T) {
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity.Memory = "2GiB"
	cfg.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///root.diff"}
	cfg.Boot.Disks = []config.DiskConfig{
		{Name: "data", RootConfig: config.RootConfig{DiffTemplate: "file:///data.diff"}},
	}
	cfg.SnapshotProvenance = config.SnapshotProvenance{
		ParentSnapshotRef:  "file://base.snapshot",
		ParentFromRefs:     []string{"manifest://memory-lower"},
		ParentOverlayBase:  "file://base-root.overlay",
		ParentBaseFromRefs: []string{"manifest://root-lower"},
		ParentDisks: []config.DiskProvenance{{
			OverlayBase:  "file://base-data.overlay",
			BaseFromRefs: []string{"manifest://data-lower"},
		}},
	}

	body, err := buildSnapshotCfg(cfg,
		[]string{"file://working-root.overlay", "file://working-data.overlay"},
		[]string{"file://base.snapshot", "manifest://memory-lower"}, []bool{true, true})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	var doc snapshotCfgYAML
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(doc.FromRefs, ","); got != "file://base.snapshot,manifest://memory-lower" {
		t.Errorf("memory from_refs = %v", doc.FromRefs)
	}
	if got := strings.Join(doc.Boot.Root.Overlay.BaseFromRefs, ","); got != "manifest://root-lower" {
		t.Errorf("root base_from_refs = %v", doc.Boot.Root.Overlay.BaseFromRefs)
	}
	if got := strings.Join(doc.Boot.Disks[0].BaseFromRefs, ","); got != "manifest://data-lower" {
		t.Errorf("data base_from_refs = %v", doc.Boot.Disks[0].BaseFromRefs)
	}
	for _, banned := range []string{"file://base-root.overlay", "file://base-data.overlay"} {
		if strings.Contains(s, banned) {
			t.Errorf("merged disk parent %q must not remain in snapshot.cfg\n%s", banned, s)
		}
	}
}

func TestBuildSnapshotCfg_DefaultLocalMergeDropsMemoryParent(t *testing.T) {
	cfg := &config.SandboxConfig{}
	cfg.Resources.Capacity.Memory = "2GiB"
	cfg.Boot.Root.DiffTemplate = "file:///root.diff"
	cfg.SnapshotProvenance = config.SnapshotProvenance{
		ParentSnapshotRef: "file://base.snapshot",
		ParentFromRefs:    []string{"manifest://memory-lower"},
	}

	body, err := buildSnapshotCfg(cfg, []string{"file://root.overlay"},
		[]string{"manifest://memory-lower"}, []bool{true})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if strings.Contains(s, "file://base.snapshot") {
		t.Fatalf("merged local memory parent must be dropped:\n%s", s)
	}
	if !strings.Contains(s, "manifest://memory-lower") {
		t.Fatalf("merged local memory must inherit lower chain:\n%s", s)
	}
}
