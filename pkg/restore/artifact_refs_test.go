package restore

import (
	"reflect"
	"testing"
)

func TestSnapshotCfgArtifactRefsSingleDisk(t *testing.T) {
	cfg := &SnapshotCfg{}
	cfg.Boot.RuntimeRef = "file://platform-runtime"
	cfg.FromRefs = []string{"manifest://memory-parent"}
	cfg.Boot.Root.BaseRef = "file://root-base-ref"
	cfg.Boot.Root.Base = "manifest://root-base"
	cfg.Boot.Root.BaseFromRefs = []string{"manifest://root-oldest", "", "manifest://root-newest"}

	want := []string{
		"file://root-base-ref",
		"manifest://root-base",
		"manifest://root-oldest",
		"manifest://root-newest",
	}
	if got := cfg.ArtifactRefs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ArtifactRefs() = %#v, want %#v", got, want)
	}
}

func TestSnapshotCfgArtifactRefsOverlayAndDataDisks(t *testing.T) {
	cfg := &SnapshotCfg{}
	cfg.Boot.Root.BaseRef = "root-ref"
	cfg.Boot.Root.Base = "root-base"
	cfg.Boot.Root.BaseFromRefs = []string{"root-base-parent"}
	cfg.Boot.Root.Overlay = &SnapOverlayCfg{
		Base:         "root-overlay",
		BaseFromRefs: []string{"root-overlay-parent"},
	}
	cfg.Boot.Disks = []SnapDiskNode{
		{
			BaseRef:      "data0-ref",
			Base:         "data0-base",
			BaseFromRefs: []string{"data0-base-parent"},
			Overlay: &SnapOverlayCfg{
				Base:         "data0-overlay",
				BaseFromRefs: []string{"data0-overlay-parent-a", "data0-overlay-parent-b"},
			},
		},
		{Base: "data1-base"},
	}

	want := []string{
		"root-ref", "root-base", "root-base-parent", "root-overlay", "root-overlay-parent",
		"data0-ref", "data0-base", "data0-base-parent", "data0-overlay", "data0-overlay-parent-a", "data0-overlay-parent-b",
		"data1-base",
	}
	if got := cfg.ArtifactRefs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ArtifactRefs() = %#v, want %#v", got, want)
	}
}

func TestSnapshotCfgArtifactRefsEmptyAndNil(t *testing.T) {
	var nilCfg *SnapshotCfg
	if got := nilCfg.ArtifactRefs(); got != nil {
		t.Fatalf("nil ArtifactRefs() = %#v, want nil", got)
	}
	if got := (&SnapshotCfg{}).ArtifactRefs(); len(got) != 0 {
		t.Fatalf("empty ArtifactRefs() = %#v, want empty", got)
	}
}
