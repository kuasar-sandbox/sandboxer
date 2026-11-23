package sandbox

import (
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"testing"
)

func TestSystemdPathMaterializationAndClone(t *testing.T) {
	image := &ImageConfig{Cmd: []string{"/bin/true"}, Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin", "A=image"}}
	cfg := &config.SandboxConfig{Launch: config.LaunchConfig{Systemd: "auto", Env: map[string]string{"B": "request"}}}
	for i := 0; i < 2; i++ {
		if err := MaterializeImageDefaults(cfg, image); err != nil {
			t.Fatal(err)
		}
		if cfg.Launch.Env["PATH"] != "/usr/local/bin:/usr/bin:/bin" || !cfg.Launch.PersistentSystemdPathDefault() {
			t.Fatalf("materialize %d lost origin: %+v", i, cfg.Launch)
		}
	}
	clone, err := cloneSDKConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !clone.Launch.SystemdPathDefault() {
		t.Fatal("SDK JSON clone lost internal origin")
	}
	clone.Launch.EphemeralEnv = map[string]string{"PATH": ""}
	spec, err := MergeLaunch(image, clone.Launch)
	if err != nil {
		t.Fatal(err)
	}
	if spec.SystemdPathDefault || spec.Env["PATH"] != "" {
		t.Fatal("ephemeral explicit empty PATH not respected")
	}
	if !clone.Launch.PersistentSystemdPathDefault() {
		t.Fatal("ephemeral PATH changed artifact provenance")
	}
	clone.Launch.EphemeralEnv = nil
	clone.Launch.Env["PATH"] = "/sdk-explicit"
	if clone.Launch.SystemdPathDefault() {
		t.Fatal("SDK map mutation retained stale default marker")
	}
	if err := MaterializeImageDefaults(clone, image); err != nil {
		t.Fatal(err)
	}
	if clone.Launch.SystemdPathDefault() {
		t.Fatal("materialize guessed explicit SDK PATH")
	}
}

func TestSystemdWireKeepsRawPolicyAndExplicitPath(t *testing.T) {
	for _, mode := range []string{"", "off", "auto", "runtime", "provided"} {
		for _, control := range []bool{false, true} {
			override := config.LaunchConfig{Systemd: mode, Exec: "/bin/true", CgroupControl: control, Env: map[string]string{"PATH": "/explicit"}}
			spec, err := MergeLaunch(nil, override)
			if err != nil {
				t.Fatal(err)
			}
			expected := mode
			if expected == "" {
				expected = "off"
			}
			if spec.Systemd != expected || spec.CgroupControl != control || spec.SystemdPathDefault || spec.Env["PATH"] != "/explicit" {
				t.Fatalf("wire changed policy: %+v", spec)
			}
		}
	}
}
