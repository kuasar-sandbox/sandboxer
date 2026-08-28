package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBindPortableDiskGraph(t *testing.T) {
	dir := t.TempDir()
	cfg := &SandboxConfig{Boot: BootConfig{
		Root: RootConfig{Base: "file://image.erofs@sha256:" + strings.Repeat("a", 64), Overlay: &OverlayConfig{
			Base: "self", BaseFromRefs: []string{"manifest://" + strings.Repeat("1", 64)},
		}},
		Disks: []DiskConfig{{Name: "data", RootConfig: RootConfig{Base: "file://data.overlay@sha256:" + strings.Repeat("b", 64)}}},
	}}
	self := "file://" + filepath.Join(dir, "current.sandbox") + "@sha256:" + strings.Repeat("c", 64)
	if err := BindPortableDiskGraph(cfg, self, dir); err != nil {
		t.Fatal(err)
	}
	if cfg.Boot.Root.Overlay.Base != self {
		t.Fatalf("self = %q, want %q", cfg.Boot.Root.Overlay.Base, self)
	}
	if want := "file://" + filepath.Join(dir, "image.erofs") + "@sha256:" + strings.Repeat("a", 64); cfg.Boot.Root.Base != want {
		t.Fatalf("root base = %q, want %q", cfg.Boot.Root.Base, want)
	}
	if want := "file://" + filepath.Join(dir, "data.overlay") + "@sha256:" + strings.Repeat("b", 64); cfg.Boot.Disks[0].Base != want {
		t.Fatalf("data base = %q, want %q", cfg.Boot.Disks[0].Base, want)
	}
}

func TestBindPortableDiskGraphRejectsMissingDirectoryAndSelf(t *testing.T) {
	cfg := &SandboxConfig{Boot: BootConfig{Root: RootConfig{Base: "self", BaseFromRefs: []string{"file://lower@sha256:" + strings.Repeat("a", 64)}}}}
	if err := BindPortableDiskGraph(cfg, "manifest://"+strings.Repeat("1", 64), ""); err == nil || !strings.Contains(err.Error(), "trusted source directory") {
		t.Fatalf("error = %v", err)
	}
	cfg = &SandboxConfig{Boot: BootConfig{Root: RootConfig{Base: "manifest://" + strings.Repeat("2", 64)}}}
	if err := BindPortableDiskGraph(cfg, "manifest://"+strings.Repeat("1", 64), ""); err == nil || !strings.Contains(err.Error(), "expected self exactly once") {
		t.Fatalf("error = %v", err)
	}
}
