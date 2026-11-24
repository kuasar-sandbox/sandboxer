package sandbox

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestRuntimeDeclarationDoesNotChangeKnownBundlePath(t *testing.T) {
	base := "manifest://" + strings.Repeat("a", 64)
	annotation := "@runtime:runtime.bundle:digest:" + strings.Repeat("b", 64)
	path := filepath.Join(t.TempDir(), "actual.bundle")
	opts := RunOptions{MemoryBinding: &MemorySourceBinding{SnapshotRef: base + annotation, RuntimeRef: "file://" + path + "@manifest:" + strings.Repeat("a", 64)}}
	known := bundleKnownRefPaths(opts)
	if len(known) != 1 || known[base] != path {
		t.Fatalf("expected base identity to locate carrier, got %v", known)
	}
	for _, raw := range []string{base, base + annotation} {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := resolveBundleArtifactPath(ref, raw, known, "", config.RefLocations{}, nil)
		if err != nil || got != path {
			t.Fatalf("resolved %q, %v; want %q", got, err, path)
		}
	}
}

func TestChildMemoryRefsExcludeRuntimeMetadata(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, base := range []string{
		"manifest://" + digest,
		"file://snapshot.bundle@manifest:" + digest + "@location:node",
		"file://snapshot.snapshot@digest:" + digest,
	} {
		binding := &MemorySourceBinding{SnapshotRef: base, FromRefs: []string{"manifest://" + strings.Repeat("c", 64)}}
		want, err := memoryRefsForSnapshot(binding)
		if err != nil {
			t.Fatal(err)
		}
		binding.SnapshotRef += "@runtime:runtime.bundle:digest:" + strings.Repeat("b", 64)
		got, err := memoryRefsForSnapshot(binding)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("snapshot declaration entered immutable parent graph: %v, want %v, error %v", got, want, err)
		}
	}
}

func TestNonSnapshotSourcesRejectRuntimeDeclaration(t *testing.T) {
	raw := "manifest://" + strings.Repeat("a", 64) + "@runtime:runtime.bundle:digest:" + strings.Repeat("b", 64)
	if _, _, err := resolveRunSourceRef(raw, nil); err == nil {
		t.Fatal("Sandbox source accepted a snapshot runtime declaration")
	}
	if _, _, err := resolveFlattenedImageRef(raw, nil); err == nil {
		t.Fatal("image source accepted a snapshot runtime declaration")
	}
}
