package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestOpenDiskStreamValidatesLocatedContentName(t *testing.T) {
	dir := t.TempDir()
	tmp, err := os.CreateTemp(dir, "disk-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := tarstream.WriteTo(context.Background(), tmp, "image", sparse.Dense(bytes.NewReader([]byte("image")), 5))
	if err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	goodName := hexDigest + ".image"
	if err := os.Rename(tmp.Name(), filepath.Join(dir, goodName)); err != nil {
		t.Fatal(err)
	}
	locations := config.RefLocations{"shared": dir}
	stream, _, err := OpenDiskStream(context.Background(), "file://"+goodName+"@location:shared", nil, locations)
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()

	body, err := os.ReadFile(filepath.Join(dir, goodName))
	if err != nil {
		t.Fatal(err)
	}
	badName := strings.Repeat("0", 64) + ".image"
	if err := os.WriteFile(filepath.Join(dir, badName), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenDiskStream(context.Background(), "file://"+badName+"@location:shared", nil, locations); err == nil {
		t.Fatal("located ref with a mismatched content name succeeded")
	}
}

func TestOpenDiskStreamRejectsLocatedSymlink(t *testing.T) {
	locationDir := t.TempDir()
	outsideDir := t.TempDir()
	tmp, err := os.CreateTemp(outsideDir, "disk-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := tarstream.WriteTo(context.Background(), tmp, "image", sparse.Dense(bytes.NewReader([]byte("image")), 5))
	if err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(digest, "sha256:") + ".image"
	target := filepath.Join(outsideDir, name)
	if err := os.Rename(tmp.Name(), target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(locationDir, name)); err != nil {
		t.Fatal(err)
	}

	_, _, err = OpenDiskStream(context.Background(), "file://"+name+"@location:shared", nil, config.RefLocations{"shared": locationDir})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("located symlink error = %v", err)
	}
}
