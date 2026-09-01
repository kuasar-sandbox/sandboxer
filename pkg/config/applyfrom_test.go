package config

import (
	"fmt"
	"strings"
	"testing"
)

func portableFromFixture() *PortableSandboxConfig {
	return &PortableSandboxConfig{
		Version: PortableSandboxConfigVersion,
		Resources: PortableResourcesConfig{
			Capacity:    CapacityConfig{CPU: 4, Memory: "2GiB"},
			Allocatable: AllocatableConfig{CPU: 2, Memory: "1GiB"},
		},
		Network: PortableNetworkConfig{Enabled: true, Interface: "eth0"},
		Boot: PortableBootConfig{
			Kernel:  "file://vmlinux@digest:" + testSHA,
			Runtime: "file://sandbox-runtime.bundle@digest:" + testSHA2,
			Root:    PortableRootConfig{Base: "self", Overlay: &PortableOverlayConfig{}},
		},
		Launch: PortableLaunchConfig{
			Exec: "/artifact/app", Env: map[string]string{"ARTIFACT": "yes"},
			Workdir: "/artifact", Restart: "never", CgroupControl: true,
		},
		Files:    []FileConfig{{Path: "/etc/artifact", Content: "artifact"}},
		Metadata: map[string]string{"source": "artifact"},
	}
}

func TestApplyFromRulesUsesPresenceAndSeparatesEphemeralInput(t *testing.T) {
	host, presence, err := LoadConfigBytesWithPresence([]byte(`
resources:
  allocatable:
    cpu: 3
launch:
  cgroup_control: false
  env:
    HOST: persistent
  ephemeral_env:
    TOKEN: current-only
  start_timeout: 30s
files:
  - path: /etc/host
    content: persistent-host
ephemeral_files:
  - path: /etc/token
    content: current-only
network:
  tap: tap0
boot:
  kernel: file:///node/vmlinux
  runtime: file:///node/sandbox-runtime.bundle
  root:
    overlay:
      diff_template: file:///node/upper.ext4
`))
	if err != nil {
		t.Fatal(err)
	}
	runtime, c0, err := ApplyFromRules(portableFromFixture(), host, presence)
	if err != nil {
		t.Fatal(err)
	}
	if c0.Resources.Capacity.CPU != 4 {
		t.Fatalf("absent capacity default overrode artifact: %d", c0.Resources.Capacity.CPU)
	}
	if c0.Resources.Allocatable.CPU != 3 {
		t.Fatalf("explicit allocatable override = %v", c0.Resources.Allocatable.CPU)
	}
	if c0.Launch.CgroupControl {
		t.Fatal("explicit false launch override was lost")
	}
	if c0.Launch.Env["HOST"] != "persistent" || len(c0.Launch.Env) != 1 {
		t.Fatalf("C0 persistent env = %#v", c0.Launch.Env)
	}
	if _, leaked := c0.Launch.Env["TOKEN"]; leaked {
		t.Fatal("ephemeral env leaked into C0")
	}
	if len(c0.Files) != 1 || c0.Files[0].Path != "/etc/host" {
		t.Fatalf("C0 files = %#v", c0.Files)
	}
	if runtime.Launch.EphemeralEnv["TOKEN"] != "current-only" || len(runtime.EphemeralFiles) != 1 {
		t.Fatalf("runtime ephemeral input missing: env=%v files=%v", runtime.Launch.EphemeralEnv, runtime.EphemeralFiles)
	}
	if runtime.Launch.StartTimeout != "30s" {
		t.Fatalf("host-only start timeout = %q", runtime.Launch.StartTimeout)
	}
	if runtime.Boot.Root.Overlay == nil || runtime.Boot.Root.Overlay.DiffTemplate != "file:///node/upper.ext4" {
		t.Fatalf("active upper binding = %#v", runtime.Boot.Root.Overlay)
	}
	if runtime.Network.TAP != "tap0" || runtime.Network.Interface != "eth0" {
		t.Fatalf("network binding = %#v", runtime.Network)
	}
	raw, err := MarshalPortableSandboxConfig(c0)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"TOKEN", "current-only", "start_timeout", "/node/", "tap0"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("C0 leaked %q:\n%s", forbidden, raw)
		}
	}
}

func TestApplyFromRulesRejectsProtectedDiskGraph(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "root base", yaml: `
network: {tap: tap0}
boot:
  kernel: file:///node/vmlinux
  runtime: file:///node/runtime
  root:
    base: file:///override.sandbox
    overlay: {diff_template: file:///upper.ext4}
`, want: "boot.root immutable graph"},
		{name: "root topology", yaml: `
network: {tap: tap0}
boot:
  kernel: file:///node/vmlinux
  runtime: file:///node/runtime
  root:
    diff_template: file:///single.ext4
`, want: "active diff fields under boot.root.overlay"},
		{name: "cmdline", yaml: `
network: {tap: tap0}
boot:
  kernel: file:///node/vmlinux
  runtime: file:///node/runtime
  cmdline: changed=true
  root:
    overlay: {diff_template: file:///upper.ext4}
`, want: "boot.cmdline is owned"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, presence, err := LoadConfigBytesWithPresence([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = ApplyFromRules(portableFromFixture(), host, presence)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ApplyFromRules error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestApplyFromRulesRequiresMatchingNetworkProvider(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		yaml    string
	}{
		{name: "missing", enabled: true, yaml: `boot: {kernel: file:///vmlinux, runtime: file:///runtime, root: {overlay: {diff_template: file:///upper}}}`},
		{name: "unexpected", enabled: false, yaml: `network: {tap: tap0}
boot: {kernel: file:///vmlinux, runtime: file:///runtime, root: {overlay: {diff_template: file:///upper}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := portableFromFixture()
			artifact.Network.Enabled = tc.enabled
			if !tc.enabled {
				artifact.Network.Interface = ""
			}
			host, presence, err := LoadConfigBytesWithPresence([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = ApplyFromRules(artifact, host, presence)
			if err == nil || !strings.Contains(err.Error(), "provider presence") {
				t.Fatalf("network mismatch error = %v", err)
			}
		})
	}
}

func TestApplyFromRulesRejectsDataDiskCountAndNameChanges(t *testing.T) {
	artifact := portableFromFixture()
	artifact.Boot.Disks = []PortableDiskConfig{{
		Name:               "data",
		PortableRootConfig: PortableRootConfig{Base: "file://data.overlay@digest:" + testSHA},
	}}
	artifact.Mounts = []MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
	if err := artifact.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		disks string
		want  string
	}{
		{name: "explicit empty", disks: "[]", want: "count"},
		{name: "name", disks: "[{name: other, diff_template: file:///data.ext4}]", want: "name"},
		{name: "too many", disks: "[{name: data, diff_template: file:///data.ext4}, {name: extra, diff_template: file:///extra.ext4}]", want: "count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "network: {tap: tap0}\nboot:\n  kernel: file:///vmlinux\n  runtime: file:///runtime\n  root:\n    overlay: {diff_template: file:///upper.ext4}\n  disks: " + tc.disks + "\n"
			host, presence, err := LoadConfigBytesWithPresence([]byte(yaml))
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = ApplyFromRules(artifact, host, presence)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("data topology error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestApplyFromRulesPreservesDataDiskMountTopology(t *testing.T) {
	artifact := portableFromFixture()
	artifact.Boot.Disks = []PortableDiskConfig{{
		Name:               "data",
		PortableRootConfig: PortableRootConfig{Base: "file://data.overlay@digest:" + testSHA},
	}}
	artifact.Mounts = []MountConfig{
		{Target: "/data", Type: "disk", Source: "data", Options: "ro"},
		{Target: "/cache", Type: "tmpfs"},
	}
	if err := artifact.Validate(); err != nil {
		t.Fatal(err)
	}
	base := `
network: {tap: tap0}
boot:
  kernel: file:///vmlinux
  runtime: file:///runtime
  root:
    overlay: {diff_template: file:///upper.ext4}
mounts:
  %s
  - {target: /scratch, type: tmpfs}
`
	for _, tc := range []struct {
		name       string
		diskMounts string
		wantOK     bool
	}{
		{name: "same disk topology", diskMounts: "- {target: /data, type: disk, source: data, options: rw}", wantOK: true},
		{name: "changed disk target", diskMounts: "- {target: /other, type: disk, source: data, options: rw}"},
		{name: "duplicate disk source", diskMounts: "- {target: /other, type: disk, source: data}\n  - {target: /data, type: disk, source: data}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, presence, err := LoadConfigBytesWithPresence([]byte(fmt.Sprintf(base, tc.diskMounts)))
			if err != nil {
				t.Fatal(err)
			}
			_, c0, err := ApplyFromRules(artifact, host, presence)
			if !tc.wantOK {
				if err == nil || !strings.Contains(err.Error(), "artifact-owned") {
					t.Fatalf("ApplyFromRules error = %v, want mount topology rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(c0.Mounts) != 2 || c0.Mounts[0].Target != "/data" || c0.Mounts[0].Options != "rw" || c0.Mounts[1].Target != "/scratch" {
				t.Fatalf("persistent mount override = %#v", c0.Mounts)
			}
		})
	}
}

func TestLoadConfigBytesWithPresenceRejectsMultipleDocuments(t *testing.T) {
	_, _, err := LoadConfigBytesWithPresence([]byte("boot: {}\n---\nboot: {}\n"))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("multiple document error = %v", err)
	}
}
