package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/image"
)

// TestLoadImageConfig_RoundTrip writes a fake "erofs prefix + appended
// config.json ZIP" file using image.AppendConfigZip, then reads it
// back via sandbox.LoadImageConfig. Validates the host-side reader
// pairs correctly with the writer side.
func TestLoadImageConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rootfs.erofs")

	// Simulate an EROFS image with arbitrary prefix bytes.
	// archive/zip locates EOCD by scanning from EOF; the prefix is
	// invisible to the ZIP reader.
	prefix := []byte("FAKE EROFS BLOB BYTES — ignored by zip reader")
	if err := os.WriteFile(path, prefix, 0o644); err != nil {
		t.Fatal(err)
	}

	rc := &image.RuntimeConfig{
		Architecture: "amd64",
		Os:           "linux",
		Env:          []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"},
		Cmd:          []string{"python3"},
		WorkingDir:   "/",
	}
	if err := image.AppendConfigZip(path, rc); err != nil {
		t.Fatalf("AppendConfigZip: %v", err)
	}

	got, err := LoadImageConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Cmd, rc.Cmd) {
		t.Errorf("Cmd mismatch: got %v want %v", got.Cmd, rc.Cmd)
	}
	if !reflect.DeepEqual(got.Env, rc.Env) {
		t.Errorf("Env mismatch:\n got  %v\n want %v", got.Env, rc.Env)
	}
	if got.WorkingDir != rc.WorkingDir {
		t.Errorf("WorkingDir = %q want %q", got.WorkingDir, rc.WorkingDir)
	}
}

// TestLoadImageConfig_NoZip — bare file with no ZIP trailer must
// soft-return an empty config, not an error. This lets sandbox-ctl
// always run MergeLaunch unconditionally.
func TestLoadImageConfig_NoZip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rootfs.erofs")
	if err := os.WriteFile(path, []byte("just bytes, no zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadImageConfig(path)
	if err != nil {
		t.Fatalf("expected soft-return on missing ZIP, got err: %v", err)
	}
	if len(got.Cmd) != 0 || len(got.Entrypoint) != 0 || len(got.Env) != 0 || got.WorkingDir != "" {
		t.Errorf("expected empty config, got %+v", got)
	}
}

// TestLoadImageConfig_FileMissing — file not present is a hard error.
func TestLoadImageConfig_FileMissing(t *testing.T) {
	_, err := LoadImageConfig("/nonexistent/path.erofs")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
