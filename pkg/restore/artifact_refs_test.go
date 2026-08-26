package restore

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFileBundleSourceCanonicalizesLocatedAlias(t *testing.T) {
	dir := t.TempDir()
	key := strings.Repeat("a", 64)
	bundleName := key + ".bundle"
	bundlePath := filepath.Join(dir, bundleName)
	if err := os.WriteFile(bundlePath, []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(dir, "root.snapshot")
	if err := os.Symlink(bundleName, aliasPath); err != nil {
		t.Fatal(err)
	}

	got, real, err := fileBundleSource(aliasPath,
		"file://root.snapshot@manifest:"+key+"@location:A")
	if err != nil {
		t.Fatal(err)
	}
	if want := "file://" + bundleName + "@location:A"; got != want {
		t.Fatalf("source = %q, want %q", got, want)
	}
	if real != bundlePath {
		t.Fatalf("real path = %q, want %q", real, bundlePath)
	}

	noncanonicalPath := filepath.Join(dir, "regular.snapshot")
	if err := os.WriteFile(noncanonicalPath, []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fileBundleSource(noncanonicalPath,
		"file://regular.snapshot@manifest:"+key+"@location:A"); err == nil ||
		!strings.Contains(err.Error(), "canonical Bundle source") {
		t.Fatalf("noncanonical located source error = %v", err)
	}

	otherDir := t.TempDir()
	outsidePath := filepath.Join(otherDir, bundleName)
	if err := os.WriteFile(outsidePath, []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideAlias := filepath.Join(dir, "outside.snapshot")
	if err := os.Symlink(outsidePath, outsideAlias); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fileBundleSource(outsideAlias,
		"file://outside.snapshot@manifest:"+key+"@location:A"); err == nil ||
		!strings.Contains(err.Error(), "same location directory") {
		t.Fatalf("outside located source error = %v", err)
	}
}

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
