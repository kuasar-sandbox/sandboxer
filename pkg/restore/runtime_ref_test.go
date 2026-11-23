package restore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
)

func TestAnnotatedSnapshotUsesBaseCarrier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.bundle")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	base := "file://snapshot.bundle@manifest:" + strings.Repeat("a", 64) + "@location:capture"
	annotated := base + "@runtime:runtime.bundle:digest:" + strings.Repeat("b", 64)
	want, wantPath, err := fileBundleSource(path, base)
	if err != nil {
		t.Fatal(err)
	}
	got, gotPath, err := fileBundleSource(path, annotated)
	if err != nil || got != want || gotPath != wantPath || strings.Contains(got, "@runtime") || strings.Contains(got, "@manifest") {
		t.Fatalf("physical carrier inherited snapshot metadata: %q, %q, %v", got, gotPath, err)
	}
	key, _ := manifest.ParseHexKey(strings.Repeat("a", 64))
	self, err := fileBundleRef(path, annotated, key)
	if err != nil || self != annotated {
		t.Fatalf("snapshot self reference lost its declaration: %s, %v", self, err)
	}
}
