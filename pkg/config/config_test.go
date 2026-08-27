package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
	"gopkg.in/yaml.v3"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sandbox.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalCold = `
resources:
  capacity:
    cpu: 2
    memory: 2GiB
  allocatable:
    cpu: 2
    memory: 1GiB
network:
  tap: tap0
boot:
  kernel: file:///opt/sandbox/vmlinux
  runtime: file:///opt/sandbox/sandbox-runtime.bundle
  cmdline: "console=hvc0 ip=169.254.1.1::169.254.1.0:255.255.255.254:test:eth0:off"
  root:
    base: file:///container.erofs
    overlay:
      diff: file:///run/sb/diff.ext4
      size: 1GiB
launch:
  exec: /usr/bin/echo
  args: ["hello", "world"]
`

func TestLoad_ValidCold(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalCold))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.Capacity.CPU != 2 {
		t.Errorf("Capacity.CPU = %d", cfg.Resources.Capacity.CPU)
	}
	if cfg.Launch.Exec != "/usr/bin/echo" {
		t.Errorf("Launch.Exec = %q", cfg.Launch.Exec)
	}
	if len(cfg.Launch.Args) != 2 || cfg.Launch.Args[0] != "hello" {
		t.Errorf("Launch.Args = %v", cfg.Launch.Args)
	}
	if err := cfg.ValidateCold(); err != nil {
		t.Errorf("ValidateCold: %v", err)
	}
}

func TestLaunchCgroupControlYAMLRoundTripAndDefault(t *testing.T) {
	defaultCfg, err := Load(writeYAML(t, minimalCold))
	if err != nil {
		t.Fatal(err)
	}
	if defaultCfg.Launch.CgroupControl {
		t.Fatal("launch.cgroup_control default = true, want false")
	}
	defaultBody, err := yaml.Marshal(defaultCfg)
	if err != nil {
		t.Fatal(err)
	}
	var defaultDecoded SandboxConfig
	if err := yaml.Unmarshal(defaultBody, &defaultDecoded); err != nil {
		t.Fatal(err)
	}
	if defaultDecoded.Launch.CgroupControl {
		t.Fatalf("default YAML round-trip changed launch.cgroup_control:\n%s", defaultBody)
	}

	enabledDoc := strings.Replace(minimalCold, "launch:\n", "launch:\n  cgroup_control: true\n", 1)
	enabled, err := Load(writeYAML(t, enabledDoc))
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.Launch.CgroupControl {
		t.Fatal("launch.cgroup_control did not parse as true")
	}
	if err := enabled.ValidateCold(); err != nil {
		t.Fatalf("ValidateCold rejected launch.cgroup_control=true: %v", err)
	}
	body, err := yaml.Marshal(enabled)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SandboxConfig
	if err := yaml.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Launch.CgroupControl {
		t.Fatalf("YAML round-trip lost launch.cgroup_control:\n%s", body)
	}

	invalidDoc := strings.Replace(minimalCold, "launch:\n", "launch:\n  cgroup_control: not-a-bool\n", 1)
	if _, err := Load(writeYAML(t, invalidDoc)); err == nil {
		t.Fatal("launch.cgroup_control accepted a non-boolean value")
	}
}

func TestValidateCold_InheritedCgroupFDIsAuthoritative(t *testing.T) {
	target, err := os.Open("/sys/fs/cgroup")
	if err != nil {
		t.Skipf("open cgroup v2 root: %v", err)
	}
	defer target.Close()

	cfg, err := Load(writeYAML(t, minimalCold))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Resources.Control.CgroupPath = "/resolved/cgroup/path/no-longer-visible"
	cfg.Resources.Control.CgroupFD = int(target.Fd())
	if err := cfg.ValidateCold(); err != nil {
		t.Fatalf("ValidateCold rejected valid inherited cgroup: %v", err)
	}

	nonCgroup, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer nonCgroup.Close()
	cfg.Resources.Control.CgroupFD = int(nonCgroup.Fd())
	if err := cfg.ValidateCold(); err == nil || !strings.Contains(err.Error(), "not on cgroup v2") {
		t.Fatalf("ValidateCold non-cgroup fd error = %v", err)
	}
}

func TestValidateCold_TapFDSocket(t *testing.T) {
	cfg, err := Load(writeYAML(t, strings.Replace(minimalCold, "  tap: tap0", `  tapfd:
    socket: /run/kuasar/connector/sw0/tapfd.sock
    request: "VSWITCH=sw0 PORT=1"
    timeout: 500ms`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateCold(); err != nil {
		t.Fatalf("ValidateCold: %v", err)
	}
	if got := cfg.Network.TapFD.Socket; got != "/run/kuasar/connector/sw0/tapfd.sock" {
		t.Fatalf("tapfd.socket = %q", got)
	}
	if got := cfg.Network.TapFD.Request; got != "VSWITCH=sw0 PORT=1" {
		t.Fatalf("tapfd.request = %q", got)
	}
	if got := cfg.Network.TapFD.TimeoutDuration(); got != 500*time.Millisecond {
		t.Fatalf("tapfd timeout = %s", got)
	}
}

func TestValidate_NoNetwork(t *testing.T) {
	withoutNetwork := strings.Replace(minimalCold, "network:\n  tap: tap0\n", "", 1)
	cases := map[string]string{
		"omitted": withoutNetwork,
		"empty":   strings.Replace(minimalCold, "network:\n  tap: tap0", "network: {}", 1),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(writeYAML(t, doc))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Network.Interface != "" {
				t.Fatalf("network.interface = %q, want empty without a network source", cfg.Network.Interface)
			}
			if err := cfg.ValidateCold(); err != nil {
				t.Fatalf("ValidateCold: %v", err)
			}
			if err := cfg.ValidateRestoreHostConfig(); err != nil {
				t.Fatalf("ValidateRestoreHostConfig: %v", err)
			}
		})
	}
}

func TestApplyDefaults_NetworkInterface(t *testing.T) {
	cases := []struct {
		name string
		net  NetworkConfig
		want string
	}{
		{name: "no source"},
		{name: "tap", net: NetworkConfig{TAP: "tap0"}, want: "eth0"},
		{name: "tapfd", net: NetworkConfig{TapFD: &TapFDConfig{Exec: []string{"helper"}}}, want: "eth0"},
		{name: "explicit", net: NetworkConfig{TAP: "tap0", Interface: "ens3"}, want: "ens3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &SandboxConfig{Network: tc.net}
			cfg.ApplyDefaults()
			if cfg.Network.Interface != tc.want {
				t.Fatalf("network.interface = %q, want %q", cfg.Network.Interface, tc.want)
			}
		})
	}
}

func TestValidate_NetworkFieldsRequireSource(t *testing.T) {
	cases := []struct {
		name string
		net  NetworkConfig
	}{
		{name: "mac", net: NetworkConfig{MAC: "02:00:00:00:00:01"}},
		{name: "ip", net: NetworkConfig{IP: "169.254.1.1/31"}},
		{name: "mtu", net: NetworkConfig{MTU: 1500}},
		{name: "nexthop", net: NetworkConfig{Nexthop: "169.254.1.0"}},
		{name: "hostname", net: NetworkConfig{Hostname: "sandbox"}},
		{name: "interface", net: NetworkConfig{Interface: "eth0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeYAML(t, minimalCold))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Network = tc.net
			for mode, validate := range map[string]func() error{
				"cold":    cfg.ValidateCold,
				"restore": cfg.ValidateRestoreHostConfig,
			} {
				err := validate()
				want := "network." + tc.name + " requires network.tap or network.tapfd"
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("%s validation error = %v, want %q", mode, err, want)
				}
			}
		})
	}
}

func TestLoad_DefaultsAllocFromCapacity(t *testing.T) {
	cfg, err := Load(writeYAML(t, `
resources:
  capacity:
    cpu: 2
    memory: 4GiB
network:
  tap: tap0
boot:
  kernel: file:///vmlinux
  runtime: file:///runtime.erofs
  root:
    base: file:///c.erofs
    overlay:
      diff: file:///d.ext4
launch:
  exec: /bin/true
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.Allocatable.CPU != 2 {
		t.Errorf("Allocatable.CPU should default to capacity, got %v", cfg.Resources.Allocatable.CPU)
	}
	if cfg.Resources.Allocatable.Memory != "4GiB" {
		t.Errorf("Allocatable.Memory should default to capacity, got %q", cfg.Resources.Allocatable.Memory)
	}
}

func TestMemoryParsing(t *testing.T) {
	cfg, _ := Load(writeYAML(t, minimalCold))
	got, err := cfg.CapacityMemoryBytes()
	if err != nil {
		t.Fatal(err)
	}
	if got != 2<<30 {
		t.Errorf("CapacityMemoryBytes = %d, want %d", got, 2<<30)
	}
	got, err = cfg.AllocatableMemoryBytes()
	if err != nil {
		t.Fatal(err)
	}
	if got != 1<<30 {
		t.Errorf("AllocatableMemoryBytes = %d, want %d", got, 1<<30)
	}
}

func TestLoadRejectsRemovedWatermarkMemory(t *testing.T) {
	_, err := LoadConfigBytes([]byte(`
resources:
  capacity: {cpu: 1, memory: 1GiB}
  allocatable: {cpu: 1, memory: 256MiB}
  watermark_high:
    memory: 224MiB
`))
	if err == nil || !strings.Contains(err.Error(), "field memory not found") {
		t.Fatalf("LoadConfigBytes removed watermark_high.memory error=%v", err)
	}
}

func TestLoadWatermarkRatio(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`
resources:
  capacity: {cpu: 1, memory: 1GiB}
  allocatable: {cpu: 1, memory: 256MiB}
  watermark_high:
    ratio: 0.875
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.WatermarkHighRatio()
	if err != nil {
		t.Fatal(err)
	}
	if got != resource.DefaultWatermarkHighRatio {
		t.Fatalf("WatermarkHighRatio()=%d, want %d", got, resource.DefaultWatermarkHighRatio)
	}
}

func TestLoadMerged(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	over := filepath.Join(dir, "over.yaml")
	if err := os.WriteFile(base, []byte(`
resources: { capacity: { cpu: 2, memory: 8GiB }, allocatable: { cpu: 2, memory: 8GiB } }
network: { tap: tap0, ip: 169.254.1.1/31, hostname: h1 }
boot:
  kernel: file:///opt/vmlinux
  runtime: file:///opt/rt.erofs
  root: { base: file:///opt/app.erofs, overlay: { diff_template: file:///opt/t.ext4 } }
launch: { exec: /bin/app, args: ["a", "b"], env: { K1: v1, K2: v2 }, restart: never }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(over, []byte(`
resources: { capacity: { cpu: 4 }, allocatable: { cpu: 4 } }
network: { ip: 10.0.0.5/24, hostname: h2 }
launch: { args: ["c"], env: { K2: v2x, K3: v3 } }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadMerged([]string{base, over})
	if err != nil {
		t.Fatal(err)
	}
	// scalar override (later wins) + sibling kept (deep merge)
	if cfg.Resources.Capacity.CPU != 4 {
		t.Errorf("capacity.cpu = %d, want 4 (override)", cfg.Resources.Capacity.CPU)
	}
	if cfg.Resources.Capacity.Memory != "8GiB" {
		t.Errorf("capacity.memory = %q, want 8GiB (kept from base)", cfg.Resources.Capacity.Memory)
	}
	if cfg.Network.IP != "10.0.0.5/24" || cfg.Network.Hostname != "h2" {
		t.Errorf("network override mismatch: ip=%q host=%q", cfg.Network.IP, cfg.Network.Hostname)
	}
	if cfg.Network.TAP != "tap0" {
		t.Errorf("network.tap = %q, want tap0 (kept from base)", cfg.Network.TAP)
	}
	if cfg.Boot.Kernel != "file:///opt/vmlinux" {
		t.Errorf("boot.kernel lost: %q", cfg.Boot.Kernel)
	}
	// list replaced wholesale
	if len(cfg.Launch.Args) != 1 || cfg.Launch.Args[0] != "c" {
		t.Errorf("launch.args = %v, want [c] (list replaced)", cfg.Launch.Args)
	}
	// map merged (K1 kept, K2 overridden, K3 added)
	if cfg.Launch.Env["K1"] != "v1" || cfg.Launch.Env["K2"] != "v2x" || cfg.Launch.Env["K3"] != "v3" {
		t.Errorf("launch.env merge mismatch: %v", cfg.Launch.Env)
	}
}

func TestDiffSize_Defaults(t *testing.T) {
	cfg, _ := Load(writeYAML(t, `
resources:
  capacity: { cpu: 1, memory: 256MiB }
network: { tap: tap0 }
boot:
  kernel: file:///k
  runtime: file:///r
  root:
    base: file:///b
    overlay:
      diff: file:///d
launch: { exec: /bin/true }
`))
	got, err := cfg.DiffSizeBytes()
	if err != nil {
		t.Fatal(err)
	}
	if got != 1<<30 {
		t.Errorf("default diff size = %d, want 1GiB", got)
	}
}

func TestTimeouts_Resolution(t *testing.T) {
	// Empty / unset → 0 = no forced timeout for the guest/remote-coupled fields.
	var zero SandboxConfig
	for _, got := range []time.Duration{
		zero.RestoreDeadline(), zero.APIReadyDeadline(), zero.VAReportDeadline(),
		zero.PingDeadline(), zero.AppNotifyDeadline(),
	} {
		if got != 0 {
			t.Errorf("unset timeout resolved to %v, want 0 (no forced timeout)", got)
		}
	}
	// ch_api is the exception: unset → DefaultCHApiDeadline (local fast call).
	if zero.CHApiDeadline() != DefaultCHApiDeadline {
		t.Errorf("unset ch_api = %v, want %v (default safety net)", zero.CHApiDeadline(), DefaultCHApiDeadline)
	}

	// Explicit values parse; "0" and garbage both fall back to 0.
	c := SandboxConfig{Timeouts: TimeoutsConfig{
		Restore: "90s", CHApi: "0", APIReady: "", VAReport: "bogus",
		Ping: "2s", AppNotify: "200ms",
	}}
	if c.RestoreDeadline() != 90*time.Second {
		t.Errorf("restore = %v, want 90s", c.RestoreDeadline())
	}
	if c.CHApiDeadline() != 0 {
		t.Errorf(`ch_api "0" = %v, want 0`, c.CHApiDeadline())
	}
	if c.VAReportDeadline() != 0 {
		t.Errorf("va_report bogus = %v, want 0 (resolver tolerates; validate rejects)", c.VAReportDeadline())
	}
	if c.PingDeadline() != 2*time.Second {
		t.Errorf("ping = %v, want 2s", c.PingDeadline())
	}
	if c.AppNotifyDeadline() != 200*time.Millisecond {
		t.Errorf("app_notify = %v, want 200ms", c.AppNotifyDeadline())
	}
}

func TestTimeouts_Validate(t *testing.T) {
	if err := (TimeoutsConfig{Restore: "60s", CHApi: "", VAReport: "15s"}).validate(); err != nil {
		t.Errorf("valid timeouts rejected: %v", err)
	}
	err := (TimeoutsConfig{Restore: "nope"}).validate()
	if err == nil || !strings.Contains(err.Error(), "timeouts.restore") {
		t.Errorf("malformed timeouts.restore: got err=%v, want mention of timeouts.restore", err)
	}
}

func TestParsePrefetchMode(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  PrefetchMode
	}{
		{"", PrefetchOff},
		{"off", PrefetchOff},
		{"memory", PrefetchMemory},
	} {
		got, err := ParsePrefetchMode(tc.input)
		if err != nil {
			t.Errorf("ParsePrefetchMode(%q): %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParsePrefetchMode(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	if _, err := ParsePrefetchMode("disk"); err == nil || !strings.Contains(err.Error(), "restore.prefetch") {
		t.Fatalf("ParsePrefetchMode(disk) error = %v, want restore.prefetch validation error", err)
	}
	if _, err := ParsePrefetchMode(" memory "); err == nil {
		t.Fatal("ParsePrefetchMode must accept only the literal memory value")
	}
}

func TestRestorePrefetchValidation(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalCold+"\nrestore:\n  prefetch: memory\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Restore.Prefetch != "memory" {
		t.Fatalf("restore.prefetch = %q, want memory", cfg.Restore.Prefetch)
	}
	if err := cfg.ValidateCold(); err != nil {
		t.Fatalf("ValidateCold(memory): %v", err)
	}
	if err := cfg.ValidateRestoreHostConfig(); err != nil {
		t.Fatalf("ValidateRestoreHostConfig(memory): %v", err)
	}

	cfg.Restore.Prefetch = "disk"
	for name, validate := range map[string]func() error{
		"cold":    cfg.ValidateCold,
		"restore": cfg.ValidateRestoreHostConfig,
	} {
		if err := validate(); err == nil || !strings.Contains(err.Error(), "restore.prefetch") {
			t.Errorf("%s validation error = %v, want restore.prefetch error", name, err)
		}
	}
}

func TestRestorePrefetchMergedOverride(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	over := filepath.Join(dir, "over.yaml")
	if err := os.WriteFile(base, []byte(minimalCold+"\nrestore:\n  prefetch: memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(over, []byte("restore:\n  prefetch: off\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadMerged([]string{base, over})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Restore.Prefetch != "off" {
		t.Fatalf("restore.prefetch = %q, want later file to override with off", cfg.Restore.Prefetch)
	}
	if err := cfg.ValidateRestoreHostConfig(); err != nil {
		t.Fatalf("ValidateRestoreHostConfig(off): %v", err)
	}
}

func TestValidateRestoreHostRejectsLocatedRuntime(t *testing.T) {
	cfg := &SandboxConfig{}
	cfg.Boot.Runtime = "file://" + strings.Repeat("a", 64) + ".bundle@location:platform"
	if err := cfg.ValidateRestoreHostConfig(); err == nil || !strings.Contains(err.Error(), "does not support named ref locations") {
		t.Fatalf("ValidateRestoreHostConfig located runtime error = %v", err)
	}
}

func TestValidateCold_MissingFields(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(c *SandboxConfig)
		wantSubst string
	}{
		{"no kernel", func(c *SandboxConfig) { c.Boot.Kernel = "" }, "boot.kernel"},
		{"manifest kernel", func(c *SandboxConfig) { c.Boot.Kernel = "manifest://abc" }, "must be file"},
		{"no runtime", func(c *SandboxConfig) { c.Boot.Runtime = "" }, "boot.runtime"},
		// no exec is now allowed (image config can supply it; MergeLaunch fails late if neither does)
		{"no root.base", func(c *SandboxConfig) { c.Boot.Root.Base = "" }, "boot.root.base"},
		{"no overlay.diff", func(c *SandboxConfig) { c.Boot.Root.Overlay.Diff = "" }, "boot.root.overlay.diff"},
		{"manifest overlay.diff", func(c *SandboxConfig) { c.Boot.Root.Overlay.Diff = "manifest://abc" }, "must be file"},
		{"both tap and tapfd", func(c *SandboxConfig) {
			c.Network.TapFD = &TapFDConfig{Exec: []string{"helper"}}
		}, "mutually exclusive"},
		{"tapfd without transport", func(c *SandboxConfig) {
			c.Network.TAP = ""
			c.Network.TapFD = &TapFDConfig{}
		}, "exactly one of exec or socket"},
		{"tapfd socket without request", func(c *SandboxConfig) {
			c.Network.TAP = ""
			c.Network.TapFD = &TapFDConfig{Socket: "/run/kuasar/connector/sw0/tapfd.sock"}
		}, "request is required"},
		{"tapfd socket relative path", func(c *SandboxConfig) {
			c.Network.TAP = ""
			c.Network.TapFD = &TapFDConfig{Socket: "tapfd.sock", Request: "VSWITCH=sw0 PORT=1"}
		}, "socket must be absolute"},
		{"tapfd exec and socket", func(c *SandboxConfig) {
			c.Network.TAP = ""
			c.Network.TapFD = &TapFDConfig{Exec: []string{"helper"}, Socket: "/run/kuasar/connector/sw0/tapfd.sock", Request: "VSWITCH=sw0 PORT=1"}
		}, "exactly one of exec or socket"},
		{"tapfd exec with request", func(c *SandboxConfig) {
			c.Network.TAP = ""
			c.Network.TapFD = &TapFDConfig{Exec: []string{"helper"}, Request: "VSWITCH=sw0 PORT=1"}
		}, "request requires socket"},
		{"alloc mem > capacity", func(c *SandboxConfig) {
			c.Resources.Allocatable.Memory = "16GiB"
		}, "allocatable.memory must be ≤"},
		{"alloc cpu > capacity", func(c *SandboxConfig) {
			c.Resources.Allocatable.CPU = 99
		}, "allocatable.cpu must be ≤"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeYAML(t, minimalCold))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(cfg)
			err = cfg.ValidateCold()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSubst)
			}
			if !strings.Contains(err.Error(), tc.wantSubst) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSubst)
			}
		})
	}
}

const minimalSingle = `
resources:
  capacity:
    cpu: 2
    memory: 2GiB
network:
  tap: tap0
boot:
  kernel: file:///opt/sandbox/vmlinux
  runtime: file:///opt/sandbox/sandbox-runtime.bundle
  root:
    diff_template: file:///opt/root.ext4
launch:
  exec: /usr/bin/echo
`

func TestSingleDiskMode(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalSingle))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SingleDisk() {
		t.Fatal("SingleDisk() = false, want true when boot.root.overlay is omitted")
	}
	if cfg.Boot.Root.DiffTemplate != "file:///opt/root.ext4" {
		t.Errorf("DiffTemplate = %q", cfg.Boot.Root.DiffTemplate)
	}
	if err := cfg.ValidateCold(); err != nil {
		t.Errorf("single-disk ValidateCold: %v", err)
	}
	// Overlay mode for comparison: minimalCold has an overlay node.
	ov, err := Load(writeYAML(t, minimalCold))
	if err != nil {
		t.Fatal(err)
	}
	if ov.SingleDisk() {
		t.Error("SingleDisk() = true, want false when boot.root.overlay is present")
	}
}

func TestValidateCold_SingleDisk(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(c *SandboxConfig)
		wantSubst string
	}{
		{"no exec (no image config)", func(c *SandboxConfig) { c.Launch.Exec = "" }, "launch.exec"},
		{"placeholder relaxes exec requirement", func(c *SandboxConfig) {
			c.Launch.Exec = ""
			c.Launch.Placeholder = true
		}, ""}, // single-disk + placeholder needs no exec

		{"no ext4 source", func(c *SandboxConfig) { c.Boot.Root.DiffTemplate = "" }, "ext4 source"},
		{"manifest diff", func(c *SandboxConfig) { c.Boot.Root.Diff = "manifest://abc" }, "must be file"},
		{"base ok (ext4 cow lower)", func(c *SandboxConfig) {
			c.Boot.Root.DiffTemplate = ""
			c.Boot.Root.Base = "file:///opt/base.ext4"
		}, ""}, // valid: base is a mountable ext4 source
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeYAML(t, minimalSingle))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(cfg)
			err = cfg.ValidateCold()
			if tc.wantSubst == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestValidateCold_DiskModeMutualExclusion(t *testing.T) {
	// overlay present + a root-level single-disk field → error.
	cfg, err := Load(writeYAML(t, minimalCold)) // has boot.root.overlay
	if err != nil {
		t.Fatal(err)
	}
	cfg.Boot.Root.DiffTemplate = "file:///opt/x.ext4"
	if err := cfg.ValidateCold(); err == nil || !strings.Contains(err.Error(), "single-disk only") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

func TestValidateCold_Disks(t *testing.T) {
	// A valid single data disk + its 1:1 mount (atop the overlay-root fixture).
	valid := func() *SandboxConfig {
		cfg, err := Load(writeYAML(t, minimalCold))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Boot.Disks = []DiskConfig{{Name: "data", RootConfig: RootConfig{DiffTemplate: "file:///opt/d.ext4"}}}
		cfg.Mounts = []MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
		return cfg
	}
	if err := valid().ValidateCold(); err != nil {
		t.Fatalf("valid single data disk: %v", err)
	}
	// Overlay-mode data disk is also valid.
	ov := valid()
	ov.Boot.Disks[0].RootConfig = RootConfig{Base: "file:///opt/ds.erofs", Overlay: &OverlayConfig{DiffTemplate: "file:///opt/u.ext4"}}
	if err := ov.ValidateCold(); err != nil {
		t.Fatalf("valid overlay data disk: %v", err)
	}

	cases := []struct {
		name      string
		mutate    func(*SandboxConfig)
		wantSubst string
	}{
		{"too many disks", func(c *SandboxConfig) {
			c.Boot.Disks = make([]DiskConfig, MaxDataDisks+1)
			c.Mounts = nil
		}, "at most"},
		{"missing name", func(c *SandboxConfig) { c.Boot.Disks[0].Name = "" }, "name is required"},
		{"duplicate name", func(c *SandboxConfig) {
			c.Boot.Disks = append(c.Boot.Disks, c.Boot.Disks[0])
			c.Mounts = append(c.Mounts, MountConfig{Target: "/d2", Type: "disk", Source: "data"})
		}, "duplicated"},
		{"mount names unknown disk", func(c *SandboxConfig) { c.Mounts[0].Source = "nope" }, "names no boot.disks"},
		{"disk not mounted", func(c *SandboxConfig) { c.Mounts = nil }, "not mounted"},
		{"disk mounted twice", func(c *SandboxConfig) {
			c.Mounts = append(c.Mounts, MountConfig{Target: "/d2", Type: "disk", Source: "data"})
		}, "already mounted"},
		{"single data disk no ext4 source", func(c *SandboxConfig) { c.Boot.Disks[0].DiffTemplate = "" }, "ext4 source"},
		{"overlay data disk needs base", func(c *SandboxConfig) {
			c.Boot.Disks[0].RootConfig = RootConfig{Overlay: &OverlayConfig{DiffTemplate: "file:///opt/u.ext4"}}
		}, "base is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)
			err := cfg.ValidateCold()
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestValidateCold_LaunchExtras(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(c *SandboxConfig)
		wantSubst string // "" → expect success
	}{
		{"bad pid_namespace", func(c *SandboxConfig) { c.Launch.PIDNamespace = "weird" }, "pid_namespace"},
		{"shared pid_namespace ok", func(c *SandboxConfig) { c.Launch.PIDNamespace = "shared" }, ""},
		{"bad launch.restart", func(c *SandboxConfig) { c.Launch.Restart = "sometimes" }, "launch.restart"},
		{"launch.restart always ok", func(c *SandboxConfig) { c.Launch.Restart = "always" }, ""},
		{"plugin needs exec", func(c *SandboxConfig) {
			c.Launch.Plugin = []PluginConfig{{Restart: "always"}}
		}, "plugin[0].exec"},
		{"bad plugin restart", func(c *SandboxConfig) {
			c.Launch.Plugin = []PluginConfig{{Exec: "/x", Restart: "foo"}}
		}, "plugin[0].restart"},
		{"plugin ok", func(c *SandboxConfig) {
			c.Launch.Plugin = []PluginConfig{{Exec: "/sidecar", Restart: "always"}}
		}, ""},
		{"bad init timeout", func(c *SandboxConfig) {
			c.Init = []InitConfig{{Exec: "/x", Timeout: "nope"}}
		}, "init[0].timeout"},
		{"init timeout ok", func(c *SandboxConfig) {
			c.Init = []InitConfig{{Exec: "/x", Timeout: "30s"}}
		}, ""},
		{"placeholder ok (no exec)", func(c *SandboxConfig) {
			c.Launch.Exec = ""
			c.Launch.Placeholder = true
		}, ""},
		{"placeholder + exec mutually exclusive", func(c *SandboxConfig) {
			c.Launch.Placeholder = true // minimalCold still sets launch.exec
		}, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeYAML(t, minimalCold))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(cfg)
			err = cfg.ValidateCold()
			if tc.wantSubst == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSubst) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSubst)
			}
		})
	}
}

func TestValidateCold_ResourceControl(t *testing.T) {
	// Helper: write a minimal config and apply mutator before validating.
	run := func(t *testing.T, mutate func(c *SandboxConfig), wantErrSubstr string) {
		t.Helper()
		cfg, err := Load(writeYAML(t, minimalCold))
		if err != nil {
			t.Fatal(err)
		}
		mutate(cfg)
		err = cfg.ValidateCold()
		if wantErrSubstr == "" {
			if err != nil {
				t.Errorf("expected no error, got: %v", err)
			}
			return
		}
		if err == nil {
			t.Fatalf("expected error containing %q, got nil", wantErrSubstr)
		}
		if !strings.Contains(err.Error(), wantErrSubstr) {
			t.Errorf("error %q does not contain %q", err.Error(), wantErrSubstr)
		}
	}

	t.Run("controller without cgroup", func(t *testing.T) {
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.Controller = "/run/x.sock"
		}, "controller requires resources.control.cgroup_path")
	})

	t.Run("abstract controller socket", func(t *testing.T) {
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			c.Resources.Control.Controller = "@node-ctl"
		}, "filesystem Unix socket path")
	})

	t.Run("overhead without cgroup", func(t *testing.T) {
		run(t, func(c *SandboxConfig) {
			c.Resources.Overhead = &OverheadConfig{Memory: "32MiB"}
		}, "resources.overhead requires")
	})

	t.Run("watermark_high without cgroup", func(t *testing.T) {
		run(t, func(c *SandboxConfig) {
			c.Resources.WatermarkHigh = &WatermarkHighConfig{Ratio: 0.875}
		}, "resources.watermark_high requires")
	})

	t.Run("startup is valid in static mode", func(t *testing.T) {
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			c.Resources.Startup = &StartupConfig{Memory: "256MiB"}
		}, "")
	})

	t.Run("fractional cpu without cgroup", func(t *testing.T) {
		run(t, func(c *SandboxConfig) {
			c.Resources.Allocatable.CPU = 0.5 // < capacity.cpu (2)
		}, "fractional cpu requires cgroup_path")
	})

	t.Run("cgroup_path missing on disk", func(t *testing.T) {
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = "/nonexistent/cgroup/path/abc"
		}, "does not exist")
	})

	t.Run("cgroup_path file not dir", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "notadir")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = f.Name()
		}, "is not a directory")
	})

	t.Run("watermark_high ratio at one", func(t *testing.T) {
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			c.Resources.WatermarkHigh = &WatermarkHighConfig{Ratio: 1}
		}, "watermark_high.ratio")
	})

	t.Run("startup below allocatable is valid", func(t *testing.T) {
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			c.Resources.Control.Controller = "/run/x.sock"
			c.Resources.Startup = &StartupConfig{Memory: "256MiB"}
		}, "")
	})

	t.Run("startup zero", func(t *testing.T) {
		run(t, func(c *SandboxConfig) {
			c.Resources.Startup = &StartupConfig{Memory: "0"}
		}, "startup.memory must be > 0")
	})

	t.Run("startup above capacity", func(t *testing.T) {
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			c.Resources.Control.Controller = "/run/x.sock"
			// capacity=2GiB; startup above
			c.Resources.Startup = &StartupConfig{Memory: "4GiB"}
		}, "startup.memory")
	})

	t.Run("valid static-cgroup mode with fractional cpu", func(t *testing.T) {
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			c.Resources.Allocatable.CPU = 0.5
		}, "")
	})

	t.Run("valid dynamic mode with explicit startup", func(t *testing.T) {
		dir := t.TempDir()
		run(t, func(c *SandboxConfig) {
			c.Resources.Control.CgroupPath = dir
			c.Resources.Control.Controller = "/run/x.sock"
			c.Resources.Startup = &StartupConfig{Memory: "1500MiB"}
		}, "")
	})
}

func TestValidateRestoreHostConfigRejectsAbstractControllerSocket(t *testing.T) {
	cfg := &SandboxConfig{}
	cfg.Resources.Control.CgroupPath = "/sys/fs/cgroup/test"
	cfg.Resources.Control.Controller = "@node-ctl"
	if err := cfg.ValidateRestoreHostConfig(); err == nil || !strings.Contains(err.Error(), "filesystem Unix socket path") {
		t.Fatalf("ValidateRestoreHostConfig abstract controller error = %v", err)
	}
}

func TestValidateRestoreHostConfigRejectsControllerWithoutCgroup(t *testing.T) {
	cfg := &SandboxConfig{}
	cfg.Resources.Control.Controller = "/run/node-ctl.sock"
	if err := cfg.ValidateRestoreHostConfig(); err == nil || !strings.Contains(err.Error(), "requires resources.control.cgroup_path") {
		t.Fatalf("ValidateRestoreHostConfig controller-only error = %v", err)
	}
}

func TestValidateRestoreHostConfigRejectsRelativeCgroupPath(t *testing.T) {
	cfg := &SandboxConfig{}
	cfg.Resources.Control.CgroupPath = "slice/task"
	cfg.Resources.Control.Controller = "/run/node-ctl.sock"
	if err := cfg.ValidateRestoreHostConfig(); err == nil || !strings.Contains(err.Error(), "cgroup_path must be absolute") {
		t.Fatalf("ValidateRestoreHostConfig relative cgroup error = %v", err)
	}
}

func TestValidateRestoreHostConfigRejectsColdOnlyInputBeforeArtifactOpen(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SandboxConfig)
		want   string
	}{
		{name: "files", mutate: func(cfg *SandboxConfig) {
			cfg.Files = []FileConfig{{Path: "/etc/value", Content: "cold"}}
		}, want: "files"},
		{name: "ephemeral env", mutate: func(cfg *SandboxConfig) {
			cfg.Launch.EphemeralEnv = map[string]string{"TOKEN": "secret"}
		}, want: "launch"},
		{name: "init", mutate: func(cfg *SandboxConfig) {
			cfg.Init = []InitConfig{{Exec: "/bin/true"}}
		}, want: "init"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &SandboxConfig{}
			cfg.ApplyDefaults()
			tc.mutate(cfg)
			presence := FieldPresence{}
			switch tc.name {
			case "files":
				presence.add("files")
			case "ephemeral env":
				presence.add("launch.ephemeral_env")
			case "init":
				presence.add("init")
			}
			err := cfg.ValidateRestoreHostConfigWithPresence(presence)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateRestoreHostConfig() error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateRestoreHostConfigMemoryPolicy(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*SandboxConfig)
		wantErr string
	}{
		{
			name: "overhead without cgroup",
			mutate: func(c *SandboxConfig) {
				c.Resources.Overhead = &OverheadConfig{Memory: "32MiB"}
			},
			wantErr: "resources.overhead requires",
		},
		{
			name: "watermark without cgroup",
			mutate: func(c *SandboxConfig) {
				c.Resources.WatermarkHigh = &WatermarkHighConfig{Ratio: 0.875}
			},
			wantErr: "resources.watermark_high requires",
		},
		{
			name: "invalid ratio",
			mutate: func(c *SandboxConfig) {
				c.Resources.Control.CgroupPath = "/sys/fs/cgroup/test"
				c.Resources.WatermarkHigh = &WatermarkHighConfig{Ratio: 1}
			},
			wantErr: "resources.watermark_high.ratio",
		},
		{
			name: "zero allocatable",
			mutate: func(c *SandboxConfig) {
				c.Resources.Allocatable.Memory = "0"
			},
			wantErr: "resources.allocatable.memory must be > 0",
		},
		{
			name: "allocatable above capacity",
			mutate: func(c *SandboxConfig) {
				c.Resources.Allocatable.Memory = "2GiB"
			},
			wantErr: "resources.allocatable.memory must be ≤ capacity.memory",
		},
		{
			name: "zero startup",
			mutate: func(c *SandboxConfig) {
				c.Resources.Startup = &StartupConfig{Memory: "0"}
			},
			wantErr: "resources.startup.memory must be > 0",
		},
		{
			name: "startup above capacity",
			mutate: func(c *SandboxConfig) {
				c.Resources.Startup = &StartupConfig{Memory: "2GiB"}
			},
			wantErr: "resources.startup.memory",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &SandboxConfig{}
			cfg.Resources.Capacity.CPU = 1
			cfg.Resources.Capacity.Memory = "1GiB"
			cfg.Resources.Allocatable.Memory = "256MiB"
			tc.mutate(cfg)
			err := cfg.ValidateRestoreHostConfig()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateRestoreHostConfig() error=%v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestResourceControlDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(writeYAML(t, `
resources:
  capacity: { cpu: 2, memory: 2GiB }
  allocatable: { cpu: 2, memory: 256MiB }
  control:
    cgroup_path: `+dir+`
    controller: /run/x.sock
network: { tap: tap0 }
boot:
  kernel: file:///k
  runtime: file:///r
  root:
    base: file:///b
    overlay: { diff: file:///d }
launch: { exec: /bin/true }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateCold(); err != nil {
		t.Fatalf("ValidateCold: %v", err)
	}

	// Overhead default = 32 MiB
	ovh, err := cfg.OverheadMemoryBytes()
	if err != nil {
		t.Fatal(err)
	}
	if ovh != 32<<20 {
		t.Errorf("default overhead = %d, want 32 MiB (%d)", ovh, 32<<20)
	}

	// WatermarkHigh default = fixed-point 0.875.
	wm, err := cfg.WatermarkHighRatio()
	if err != nil {
		t.Fatal(err)
	}
	want := uint64(875_000)
	if wm != want {
		t.Errorf("default watermark_high = %d, want %d", wm, want)
	}

	// Startup default = Capacity.
	sb, err := cfg.StartupBytes()
	if err != nil {
		t.Fatal(err)
	}
	if sb != 2<<30 {
		t.Errorf("default startup = %d, want Capacity %d", sb, uint64(2<<30))
	}

	// DeflateOnOOM default = true
	if !cfg.DeflateOnOOM() {
		t.Errorf("default DeflateOnOOM = false, want true")
	}
}

func TestDeflateOnOOM_Override(t *testing.T) {
	cfg, err := Load(writeYAML(t, `
resources:
  capacity: { cpu: 1, memory: 1GiB }
  allocatable: { cpu: 1, memory: 256MiB, deflate_on_oom: false }
network: { tap: tap0 }
boot:
  kernel: file:///k
  runtime: file:///r
  root:
    base: file:///b
    overlay: { diff: file:///d }
launch: { exec: /bin/true }
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeflateOnOOM() {
		t.Errorf("DeflateOnOOM = true, want false (explicit override)")
	}
}

func TestCPUWeight_Mapping(t *testing.T) {
	cases := []struct {
		alloc float64
		want  uint64
	}{
		{0.001, 1},     // clamp lower
		{0.1, 10},      // 0.1 core
		{1.0, 100},     // 1 core (kernel default)
		{2.0, 200},     // 2 cores
		{50.0, 5000},   // mid-range
		{200.0, 10000}, // clamp upper
	}
	for _, tc := range cases {
		cfg := &SandboxConfig{}
		cfg.Resources.Allocatable.CPU = tc.alloc
		got := cfg.CPUWeight()
		if got != tc.want {
			t.Errorf("CPUWeight(%g) = %d, want %d", tc.alloc, got, tc.want)
		}
	}
}

func TestSchemeAndPath(t *testing.T) {
	cases := []struct {
		uri    string
		scheme string
		val    string
		ok     bool
	}{
		{"file:///foo/bar", "file", "/foo/bar", true},
		{"manifest://abcd", "manifest", "abcd", true},
		{"https://example.com", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		s, v, ok := SchemeAndPath(c.uri)
		if s != c.scheme || v != c.val || ok != c.ok {
			t.Errorf("SchemeAndPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.uri, s, v, ok, c.scheme, c.val, c.ok)
		}
	}
}
