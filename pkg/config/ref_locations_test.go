package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
)

func TestRefLocationsSetAndResolve(t *testing.T) {
	locations := RefLocations{}
	if err := locations.Set("shared=file:///mnt/shared/checkpoints"); err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.ParseRef("file://root.snapshot@location:shared")
	if err != nil {
		t.Fatal(err)
	}
	got, err := locations.ResolveFile(ref, "/ignored")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/mnt/shared/checkpoints/root.snapshot"; got != want {
		t.Fatalf("resolved path = %q, want %q", got, want)
	}

	local, err := manifest.ParseRef("file://parent.overlay")
	if err != nil {
		t.Fatal(err)
	}
	got, err = locations.ResolveFile(local, "/snapshots")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/snapshots", "parent.overlay"); got != want {
		t.Fatalf("local path = %q, want %q", got, want)
	}
}

func TestRefLocationsRejectInvalid(t *testing.T) {
	for _, spec := range []string{
		"",
		".bad=file:///tmp/x",
		"x=/tmp/x",
		"x=file://host/tmp/x",
		"x=file://relative",
		"x=file:///tmp/x?q=1",
	} {
		t.Run(spec, func(t *testing.T) {
			if err := (RefLocations{}).Set(spec); err == nil {
				t.Fatalf("Set(%q) succeeded", spec)
			}
		})
	}

	missing, err := manifest.ParseRef("file://x.snapshot@location:missing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (RefLocations{}).ResolveFile(missing, ""); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing location error = %v", err)
	}
}
