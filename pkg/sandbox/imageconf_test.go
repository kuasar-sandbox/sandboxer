package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/image"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
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

func TestMaterializeImageDefaultsMakesPortableWorkloadSelfContained(t *testing.T) {
	cfg := &config.SandboxConfig{
		Launch: config.LaunchConfig{
			Args: []string{"override"}, Env: map[string]string{"B": "host"},
			EphemeralEnv: map[string]string{"SECRET": "once"},
		},
		Mounts: []config.MountConfig{{Target: "/explicit", Type: "tmpfs"}},
	}
	imageCfg := &ImageConfig{
		Entrypoint: []string{"/entry", "fixed"}, Cmd: []string{"default"},
		Env: []string{"A=image", "B=image"}, WorkingDir: "/work",
		User: "1000:1000", StopSignal: "SIGQUIT",
		Volumes: map[string]struct{}{"/volume": {}, "/explicit": {}},
	}
	if err := MaterializeImageDefaults(cfg, imageCfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Launch.Exec != "/entry" || !reflect.DeepEqual(cfg.Launch.Args, []string{"fixed", "override"}) {
		t.Fatalf("materialized argv = %q %v", cfg.Launch.Exec, cfg.Launch.Args)
	}
	if !reflect.DeepEqual(cfg.Launch.Env, map[string]string{"A": "image", "B": "host"}) {
		t.Fatalf("materialized env = %#v", cfg.Launch.Env)
	}
	if !reflect.DeepEqual(cfg.Launch.EphemeralEnv, map[string]string{"SECRET": "once"}) {
		t.Fatalf("ephemeral env changed: %#v", cfg.Launch.EphemeralEnv)
	}
	if cfg.Launch.Workdir != "/work" || cfg.Launch.User != "1000:1000" || cfg.Launch.StopSignal != "3" {
		t.Fatalf("materialized launch = %#v", cfg.Launch)
	}
	wantMounts := []config.MountConfig{
		{Target: "/explicit", Type: "tmpfs"},
		{Target: "/volume", Type: "empty"},
	}
	if !reflect.DeepEqual(cfg.Mounts, wantMounts) {
		t.Fatalf("materialized mounts = %#v, want %#v", cfg.Mounts, wantMounts)
	}

	// Re-reading the same image during run --from must not change C0's
	// persistent launch/mount projection.
	firstLaunch, firstMounts := cfg.Launch, append([]config.MountConfig(nil), cfg.Mounts...)
	if err := MaterializeImageDefaults(cfg, imageCfg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Launch, firstLaunch) || !reflect.DeepEqual(cfg.Mounts, firstMounts) {
		t.Fatalf("materialization is not idempotent: launch=%#v mounts=%#v", cfg.Launch, cfg.Mounts)
	}
}
