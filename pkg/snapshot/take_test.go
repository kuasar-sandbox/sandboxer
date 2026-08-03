package snapshot

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

func TestAbsorbOverlayUsesSnapshotView(t *testing.T) {
	const size = 3 * 4096
	logical := make([]byte, size)
	copy(logical[4096:8192], bytes.Repeat([]byte{0x6d}, 4096))
	holes := []sparse.Extent{
		{Offset: 0, Size: 4096},
		{Offset: 8192, Size: 4096},
	}
	called := false
	diff := DiskDiff{
		Path: filepath.Join(t.TempDir(), "must-not-be-opened.diff"),
		SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			called = true
			return bytes.NewReader(logical), holes, nil
		},
	}
	sink := NewFileSink(t.TempDir(), "sid", nil, false, nil)
	ref, path, err := absorbOverlay(context.Background(), sink, diff, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("snapshot view provider was not called")
	}
	if ref == "" || path == "" {
		t.Fatalf("absorbOverlay returned ref=%q path=%q", ref, path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	view, err := tarstream.ReadSeekFrom(bytes.NewReader(raw), "overlay")
	if err != nil {
		t.Fatal(err)
	}
	if got := view.Holes(); len(got) != len(holes) || got[0] != holes[0] || got[1] != holes[1] {
		t.Fatalf("artifact holes=%v want %v", got, holes)
	}
	got, err := io.ReadAll(view)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, logical) {
		t.Fatal("artifact content differs from snapshot view")
	}
}

func TestAbsorbOverlayRequiresSnapshotView(t *testing.T) {
	sink := NewFileSink(t.TempDir(), "sid", nil, false, nil)
	if _, _, err := absorbOverlay(context.Background(), sink, DiskDiff{Path: "raw.diff"}, false, nil, false); err == nil {
		t.Fatal("absorbOverlay accepted a raw-path-only diff")
	}
}
