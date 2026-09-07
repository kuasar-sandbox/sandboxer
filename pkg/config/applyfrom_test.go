package config

import (
	"fmt"
	"reflect"
	"slices"
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

func TestApplyFromRulesAppliesCHExtraArgs(t *testing.T) {
	host, presence, err := LoadConfigBytesWithPresence([]byte(`
ch:
  extra_args: ["-vv", "--log-file", "/tmp/ch.log"]
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
	runtime, c0, err := ApplyFromRules(portableFromFixture(), host, presence, ApplyFromOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(runtime.CH.ExtraArgs, []string{"-vv", "--log-file", "/tmp/ch.log"}) {
		t.Fatalf("run --from dropped host ch.extra_args: %v", runtime.CH.ExtraArgs)
	}
	raw, err := MarshalPortableSandboxConfig(c0)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"extra_args", "-vv", "/tmp/ch.log"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("C0 leaked %q into the portable artifact:\n%s", forbidden, raw)
		}
	}

	// The merged runtime must still see the section, so reserved-flag
	// validation cannot be silently bypassed by routing through --from:
	// CH.validate runs near the top of validateCold, so a reserved flag
	// surfaces even if later host-fs checks would also fail.
	host.CH.ExtraArgs = []string{"--api-socket", "/tmp/hijack.sock"}
	runtime, _, err = ApplyFromRules(portableFromFixture(), host, presence, ApplyFromOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ValidateCold(); err == nil || !strings.Contains(err.Error(), "ch.extra_args[0]") {
		t.Fatalf("ValidateCold after --from merge = %v, want ch.extra_args[0] reserved-flag error", err)
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
	runtime, c0, err := ApplyFromRules(portableFromFixture(), host, presence, ApplyFromOptions{})
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
			_, _, err = ApplyFromRules(portableFromFixture(), host, presence, ApplyFromOptions{})
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
			_, _, err = ApplyFromRules(artifact, host, presence, ApplyFromOptions{})
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
			_, _, err = ApplyFromRules(artifact, host, presence, ApplyFromOptions{})
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
			_, c0, err := ApplyFromRules(artifact, host, presence, ApplyFromOptions{})
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

func TestApplyFromRulesReplacesCompleteBootWithoutSourceProvenance(t *testing.T) {
	artifact := portableFromFixture()
	artifact.Resources.Allocatable.CPU = float64(artifact.Resources.Capacity.CPU)
	artifact.Resources.Allocatable.Memory = artifact.Resources.Capacity.Memory
	artifact.Boot.Root = PortableRootConfig{
		Base: "file://old-root.erofs@digest:" + testSHA,
		Overlay: &PortableOverlayConfig{
			Base: "self",
		},
	}
	artifact.Boot.Disks = []PortableDiskConfig{{
		Name: "old-data",
		PortableRootConfig: PortableRootConfig{
			Base: "file://old-data.ext4@digest:" + testSHA2,
		},
	}}
	artifact.Mounts = []MountConfig{{Target: "/old", Type: "disk", Source: "old-data"}}
	artifact.Init = []InitConfig{{Exec: "/artifact/init"}}
	artifact.Metadata = map[string]string{"source": "inherited"}
	if err := artifact.Validate(); err != nil {
		t.Fatal(err)
	}

	host, presence, err := LoadConfigBytesWithPresence([]byte(`
resources:
  capacity: {cpu: 2, memory: 1GiB}
  allocatable: {cpu: 2, memory: 1GiB}
network: {tap: tap-replacement, interface: eth0, ip: 192.0.2.2/24}
boot:
  kernel: file:///host/new-vmlinux
  runtime: file:///host/new-runtime.bundle
  cmdline: console=hvc0 replacement=true
  root:
    base: manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    overlay:
      base: file:///host/new-root-parent.ext4@digest:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
      diff_template: file:///host/new-root-diff.ext4
  disks:
    - name: cache
      base: manifest://cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
      diff_template: file:///host/new-cache-diff.ext4
mounts:
  - {target: /cache, type: disk, source: cache}
launch:
  env: {REPLACED: "true"}
  ephemeral_env: {TOKEN: current-only}
files:
  - {path: /etc/replacement, content: persistent}
ephemeral_files:
  - {path: /run/token, content: current-only}
`))
	if err != nil {
		t.Fatal(err)
	}
	wantBoot := cloneBootConfig(host.Boot)
	runtime, c0, err := ApplyFromRules(artifact, host, presence, ApplyFromOptions{ReplaceBoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if c0 != nil {
		t.Fatalf("replacement returned source-derived C0: %#v", c0)
	}
	if !reflect.DeepEqual(runtime.Boot, wantBoot) {
		t.Fatalf("replacement boot = %#v, want %#v", runtime.Boot, wantBoot)
	}
	if runtime.Metadata["source"] != "inherited" {
		t.Fatalf("source non-boot metadata was not inherited: %#v", runtime.Metadata)
	}
	if runtime.Resources.Capacity.CPU != 2 || runtime.Resources.Allocatable.Memory != "1GiB" {
		t.Fatalf("replacement resource override = %#v", runtime.Resources)
	}
	if runtime.Network.TAP != "tap-replacement" || runtime.Network.Interface != "eth0" {
		t.Fatalf("replacement network binding/topology = %#v", runtime.Network)
	}
	if len(runtime.Mounts) != 1 || runtime.Mounts[0].Source != "cache" {
		t.Fatalf("replacement mounts = %#v", runtime.Mounts)
	}
	if len(runtime.Files) != 1 || runtime.Files[0].Path != "/etc/replacement" || len(runtime.Init) != 1 || runtime.Init[0].Exec != "/artifact/init" {
		t.Fatalf("replacement files/init = %#v / %#v", runtime.Files, runtime.Init)
	}
	if runtime.Launch.Env["REPLACED"] != "true" || runtime.Launch.EphemeralEnv["TOKEN"] != "current-only" {
		t.Fatalf("replacement launch = %#v", runtime.Launch)
	}
	if len(runtime.EphemeralFiles) != 1 || runtime.EphemeralFiles[0].Path != "/run/token" {
		t.Fatalf("replacement ephemeral files = %#v", runtime.EphemeralFiles)
	}

	projected, err := ProjectPortableCold(runtime, PortableProjection{
		KernelRef:  "file://new-vmlinux@digest:" + testSHA,
		RuntimeRef: "file://new-runtime.bundle@digest:" + testSHA2,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalPortableSandboxConfig(projected)
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"old-root", "old-data"} {
		if strings.Contains(string(raw), old) {
			t.Fatalf("replacement C0 retained source boot dependency %q:\n%s", old, raw)
		}
	}
	for _, replacement := range []string{"manifest://aaaaaaaa", "new-root-parent.ext4", "name: cache", "manifest://cccccccc"} {
		if !strings.Contains(string(raw), replacement) {
			t.Fatalf("replacement C0 omitted %q:\n%s", replacement, raw)
		}
	}

	// The result owns its boot value; later caller changes cannot turn the
	// replacement into a field-by-field alias of the host document.
	host.Boot.Root.Base = "manifest://dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	host.Boot.Disks[0].Name = "mutated"
	if runtime.Boot.Root.Base != wantBoot.Root.Base || runtime.Boot.Disks[0].Name != "cache" {
		t.Fatalf("replacement boot aliases host input: %#v", runtime.Boot)
	}
}

func TestApplyFromRulesReplaceBootOmittedDisksRemovesSourceDisks(t *testing.T) {
	artifact := portableFromFixture()
	artifact.Resources.Allocatable.CPU = float64(artifact.Resources.Capacity.CPU)
	artifact.Resources.Allocatable.Memory = artifact.Resources.Capacity.Memory
	artifact.Network = PortableNetworkConfig{}
	artifact.Boot.Disks = []PortableDiskConfig{{
		Name:               "data",
		PortableRootConfig: PortableRootConfig{Base: "file://old-data.ext4@digest:" + testSHA},
	}}
	artifact.Mounts = []MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
	if err := artifact.Validate(); err != nil {
		t.Fatal(err)
	}
	host, presence, err := LoadConfigBytesWithPresence([]byte(`
boot:
  kernel: file:///host/vmlinux
  runtime: file:///host/runtime.bundle
  root:
    base: manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    overlay: {diff_template: file:///host/root.ext4}
mounts: []
`))
	if err != nil {
		t.Fatal(err)
	}
	runtime, c0, err := ApplyFromRules(artifact, host, presence, ApplyFromOptions{ReplaceBoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if c0 != nil || len(runtime.Boot.Disks) != 0 || len(runtime.Mounts) != 0 {
		t.Fatalf("omitted replacement disks inherited source topology: C0=%#v boot=%#v mounts=%#v", c0, runtime.Boot, runtime.Mounts)
	}
}

func TestApplyFromRulesReplaceBootRejectsInconsistentInheritedMount(t *testing.T) {
	artifact := portableFromFixture()
	artifact.Resources.Allocatable.CPU = float64(artifact.Resources.Capacity.CPU)
	artifact.Resources.Allocatable.Memory = artifact.Resources.Capacity.Memory
	artifact.Network = PortableNetworkConfig{}
	artifact.Boot.Disks = []PortableDiskConfig{{
		Name:               "data",
		PortableRootConfig: PortableRootConfig{Base: "file://old-data.ext4@digest:" + testSHA},
	}}
	artifact.Mounts = []MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
	if err := artifact.Validate(); err != nil {
		t.Fatal(err)
	}
	host, presence, err := LoadConfigBytesWithPresence([]byte(`
boot:
  kernel: file:///host/vmlinux
  runtime: file:///host/runtime.bundle
  root:
    base: manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    overlay: {diff_template: file:///host/root.ext4}
`))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ApplyFromRules(artifact, host, presence, ApplyFromOptions{ReplaceBoot: true})
	if err == nil || !strings.Contains(err.Error(), "names no boot.disks") {
		t.Fatalf("inconsistent inherited mount error = %v", err)
	}
}

func TestApplyFromRulesReplaceBootDoesNotFillMissingBootFromSource(t *testing.T) {
	artifact := portableFromFixture()
	artifact.Resources.Allocatable.CPU = float64(artifact.Resources.Capacity.CPU)
	artifact.Resources.Allocatable.Memory = artifact.Resources.Capacity.Memory
	artifact.Network = PortableNetworkConfig{}
	host, presence, err := LoadConfigBytesWithPresence([]byte(`
boot:
  root:
    base: manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    overlay: {diff_template: file:///host/root.ext4}
`))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ApplyFromRules(artifact, host, presence, ApplyFromOptions{ReplaceBoot: true})
	if err == nil || !strings.Contains(err.Error(), "host boot.kernel binding is required") {
		t.Fatalf("incomplete replacement boot error = %v", err)
	}
}

func TestLoadConfigBytesWithPresenceRejectsMultipleDocuments(t *testing.T) {
	_, _, err := LoadConfigBytesWithPresence([]byte("boot: {}\n---\nboot: {}\n"))
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("multiple document error = %v", err)
	}
}
