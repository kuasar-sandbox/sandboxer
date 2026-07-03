package sandbox

import (
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"testing"

	"golang.org/x/sys/unix"
)

func TestParseStopSignal(t *testing.T) {
	cases := []struct {
		in   string
		want int
		err  bool
	}{
		{"SIGTERM", int(unix.SIGTERM), false},
		{"TERM", int(unix.SIGTERM), false},
		{"sigkill", int(unix.SIGKILL), false},
		{"15", 15, false},
		{"9", 9, false},
		{"SIGNOPE", 0, true},
		{"0", 0, true},
		{"-3", 0, true},
	}
	for _, c := range cases {
		got, err := config.ParseStopSignal(c.in)
		if c.err {
			if err == nil {
				t.Errorf("config.ParseStopSignal(%q): want error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("config.ParseStopSignal(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("config.ParseStopSignal(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestMergeLaunch_UserAndStopSignal(t *testing.T) {
	// override wins over image.
	img := &ImageConfig{Cmd: []string{"/bin/sh"}, User: "1000:1000", StopSignal: "SIGQUIT"}
	spec, err := MergeLaunch(img, config.LaunchConfig{User: "app:app", StopSignal: "SIGUSR1"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.User != "app:app" {
		t.Errorf("User = %q, want app:app", spec.User)
	}
	if spec.StopSignal != int(unix.SIGUSR1) {
		t.Errorf("StopSignal = %d, want %d", spec.StopSignal, int(unix.SIGUSR1))
	}

	// fall back to image when override empty.
	spec, err = MergeLaunch(img, config.LaunchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if spec.User != "1000:1000" {
		t.Errorf("User = %q, want image 1000:1000", spec.User)
	}
	if spec.StopSignal != int(unix.SIGQUIT) {
		t.Errorf("StopSignal = %d, want image SIGQUIT", spec.StopSignal)
	}

	// neither set → zero (guest defaults to SIGTERM).
	spec, _ = MergeLaunch(&ImageConfig{Cmd: []string{"/bin/sh"}}, config.LaunchConfig{})
	if spec.StopSignal != 0 || spec.User != "" {
		t.Errorf("unset: User=%q StopSignal=%d, want empty/0", spec.User, spec.StopSignal)
	}
}

func TestMergeLaunch_BadStopSignal(t *testing.T) {
	_, err := MergeLaunch(&ImageConfig{Cmd: []string{"/bin/sh"}}, config.LaunchConfig{StopSignal: "NOPE"})
	if err == nil {
		t.Fatal("want error on bad stop_signal")
	}
}

func TestEffectiveMounts_VolumesMerge(t *testing.T) {
	mounts := []config.MountConfig{
		{Target: "/tmp", Type: "tmpfs", Options: "mode=1777"},
		{Target: "/data", Type: ""}, // default → empty
	}
	vols := map[string]struct{}{"/var/log": {}, "/data": {}} // /data dup with explicit
	got := effectiveMounts(mounts, vols)

	byTarget := map[string]MountSpecView{}
	for _, m := range got {
		byTarget[m.Target] = MountSpecView{m.Type, m.Options}
	}
	if len(got) != 3 {
		t.Fatalf("want 3 mounts (/tmp,/data,/var/log), got %d: %+v", len(got), got)
	}
	if byTarget["/data"].Type != "empty" || byTarget["/data"].Options != "" {
		t.Errorf("/data should stay the explicit empty mount, got %+v", byTarget["/data"])
	}
	if byTarget["/var/log"].Type != "empty" {
		t.Errorf("/var/log volume should be empty, got %+v", byTarget["/var/log"])
	}
	if byTarget["/tmp"].Type != "tmpfs" {
		t.Errorf("/tmp should be tmpfs, got %+v", byTarget["/tmp"])
	}
}

// MountSpecView is a tiny test helper to compare resolved mount fields.
type MountSpecView struct{ Type, Options string }

func TestStopGraceAndStartTimeoutAccessors(t *testing.T) {
	c := &config.SandboxConfig{}
	if c.StopGraceSeconds() != 10 {
		t.Errorf("default StopGraceSeconds = %d, want 10", c.StopGraceSeconds())
	}
	if c.StartTimeoutDuration() != 0 {
		t.Errorf("default StartTimeoutDuration = %v, want 0", c.StartTimeoutDuration())
	}
	c.Launch.StopGracePeriod = "30s"
	c.Launch.StartTimeout = "2m"
	if c.StopGraceSeconds() != 30 {
		t.Errorf("StopGraceSeconds = %d, want 30", c.StopGraceSeconds())
	}
	if c.StartTimeoutDuration().Seconds() != 120 {
		t.Errorf("StartTimeoutDuration = %v, want 2m", c.StartTimeoutDuration())
	}
}

func TestValidateCold_MountsFilesInit(t *testing.T) {
	load := func(t *testing.T) *config.SandboxConfig {
		cfg, err := config.Load(writeYAML(t, minimalCold))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("valid", func(t *testing.T) {
		cfg := load(t)
		cfg.Mounts = []config.MountConfig{{Target: "/tmp", Type: "tmpfs"}, {Target: "/v"}}
		cfg.Files = []config.FileConfig{{Path: "/etc/resolv.conf", Mode: "0644"}}
		cfg.Init = []config.InitConfig{{Exec: "/bin/sh", Args: []string{"-c", "true"}}}
		cfg.Launch.StopSignal = "SIGTERM"
		cfg.Launch.StopGracePeriod = "5s"
		cfg.ApplyDefaults()
		if err := cfg.ValidateCold(); err != nil {
			t.Fatalf("ValidateCold: %v", err)
		}
		if cfg.Mounts[1].Type != "empty" {
			t.Errorf("default mount type = %q, want empty", cfg.Mounts[1].Type)
		}
	})

	bad := []struct {
		name   string
		mutate func(*config.SandboxConfig)
		substr string
	}{
		{"rel mount target", func(c *config.SandboxConfig) { c.Mounts = []config.MountConfig{{Target: "tmp", Type: "tmpfs"}} }, "absolute"},
		{"nfs mount", func(c *config.SandboxConfig) { c.Mounts = []config.MountConfig{{Target: "/d", Type: "nfs"}} }, "not yet implemented"},
		{"unknown mount", func(c *config.SandboxConfig) { c.Mounts = []config.MountConfig{{Target: "/d", Type: "bind"}} }, "unknown"},
		{"rel file path", func(c *config.SandboxConfig) { c.Files = []config.FileConfig{{Path: "etc/x"}} }, "absolute"},
		{"bad file mode", func(c *config.SandboxConfig) { c.Files = []config.FileConfig{{Path: "/etc/x", Mode: "99x"}} }, "octal"},
		{"empty init exec", func(c *config.SandboxConfig) { c.Init = []config.InitConfig{{Exec: ""}} }, "init[0].exec"},
		{"bad signal", func(c *config.SandboxConfig) { c.Launch.StopSignal = "NOPE" }, "stop_signal"},
		{"bad grace", func(c *config.SandboxConfig) { c.Launch.StopGracePeriod = "abc" }, "stop_grace_period"},
		{"bad start_timeout", func(c *config.SandboxConfig) { c.Launch.StartTimeout = "abc" }, "start_timeout"},
	}
	for _, b := range bad {
		t.Run(b.name, func(t *testing.T) {
			cfg := load(t)
			b.mutate(cfg)
			err := cfg.ValidateCold()
			if err == nil {
				t.Fatalf("want error containing %q, got nil", b.substr)
			}
			if !contains(err.Error(), b.substr) {
				t.Errorf("error %q does not contain %q", err.Error(), b.substr)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
