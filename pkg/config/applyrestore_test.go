package config

import (
	"bytes"
	"strings"
	"testing"
)

func restoreHostConfig() *SandboxConfig {
	return &SandboxConfig{
		Resources: ResourcesConfig{Control: ControlConfig{CgroupPath: "/sys/fs/cgroup/target"}},
		Boot: BootConfig{
			Kernel:  "file:///node/vmlinux",
			Runtime: "file:///node/sandbox-runtime.bundle",
			Root:    RootConfig{DiffTemplate: "file:///node/root.ext4"},
		},
		Timeouts: TimeoutsConfig{CHApi: "5s", Restore: "2m"},
		Restore:  RestoreConfig{Prefetch: "memory"},
	}
}

func restorePresence(paths ...string) FieldPresence {
	presence := FieldPresence{}
	for _, path := range paths {
		presence.add(path)
	}
	return presence
}

func TestApplyRestoreRulesUsesReferencedSandboxAsImmutableC0(t *testing.T) {
	artifact := validPortableConfig()
	artifact.Metadata = map[string]string{"owner": "artifact"}
	before, err := MarshalPortableSandboxConfig(artifact)
	if err != nil {
		t.Fatal(err)
	}
	host := restoreHostConfig()
	host.Resources.Startup = &StartupConfig{Memory: "256MiB"}
	runtime, c0, err := ApplyRestoreRules(artifact, host, FieldPresence{})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Boot.Kernel != host.Boot.Kernel || runtime.Boot.Runtime != host.Boot.Runtime {
		t.Fatalf("host bindings were not applied: %+v", runtime.Boot)
	}
	if runtime.Boot.Root.DiffTemplate != host.Boot.Root.DiffTemplate {
		t.Fatalf("active diff binding = %q", runtime.Boot.Root.DiffTemplate)
	}
	if runtime.Restore.Prefetch != "memory" || runtime.Timeouts.CHApi != "5s" {
		t.Fatalf("restore host policy was not applied: restore=%+v timeouts=%+v", runtime.Restore, runtime.Timeouts)
	}
	if runtime.Resources.Startup == nil || runtime.Resources.Startup.Memory != "256MiB" {
		t.Fatalf("restore node startup policy was not applied: %+v", runtime.Resources.Startup)
	}
	if runtime.Metadata["owner"] != "artifact" || c0.Metadata["owner"] != "artifact" {
		t.Fatalf("artifact workload was not retained: runtime=%v C0=%v", runtime.Metadata, c0.Metadata)
	}
	after, err := MarshalPortableSandboxConfig(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("restore rules mutated the referenced Sandbox config")
	}
}

func TestApplyRestoreRulesReappliesTargetAllocatablePolicy(t *testing.T) {
	artifact := validPortableConfig()
	artifactDeflate := true
	artifact.Resources.Allocatable = AllocatableConfig{CPU: 1.5, Memory: "768MiB", DeflateOnOOM: &artifactDeflate}
	before, err := MarshalPortableSandboxConfig(artifact)
	if err != nil {
		t.Fatal(err)
	}
	host := restoreHostConfig()
	host.Resources.Capacity = artifact.Resources.Capacity
	host.Resources.Allocatable.Memory = "512MiB"
	host.Resources.Control.CgroupPath = "/sys/fs/cgroup/target"
	runtime, c0, err := ApplyRestoreRules(artifact, host, restorePresence(
		"resources.capacity.cpu", "resources.capacity.memory", "resources.allocatable.memory",
	))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Resources.Capacity != artifact.Resources.Capacity {
		t.Fatalf("runtime capacity = %+v, want snapshot capacity %+v", runtime.Resources.Capacity, artifact.Resources.Capacity)
	}
	if runtime.Resources.Allocatable.CPU != 1.5 || runtime.Resources.Allocatable.Memory != "512MiB" ||
		runtime.Resources.Allocatable.DeflateOnOOM == nil || !*runtime.Resources.Allocatable.DeflateOnOOM {
		t.Fatalf("runtime allocatable policy = %+v, want target-node policy", runtime.Resources.Allocatable)
	}
	if c0.Resources.Allocatable.CPU != 1.5 || c0.Resources.Allocatable.Memory != "768MiB" ||
		c0.Resources.Allocatable.DeflateOnOOM == nil || !*c0.Resources.Allocatable.DeflateOnOOM {
		t.Fatalf("immutable C0 allocatable = %+v, want artifact policy", c0.Resources.Allocatable)
	}
	after, err := MarshalPortableSandboxConfig(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("restore rules mutated the referenced Sandbox config")
	}
}

func TestApplyRestoreRulesRejectsColdOnlyConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		presence FieldPresence
		mutate   func(*SandboxConfig)
		want     string
	}{
		{name: "present launch", presence: restorePresence("launch"), want: "launch is cold-start-only"},
		{name: "persistent files", mutate: func(c *SandboxConfig) {
			c.Files = []FileConfig{{Path: "/etc/value", Content: "persistent"}}
		}, want: "mounts/files/init/metadata"},
		{name: "ephemeral files", mutate: func(c *SandboxConfig) {
			c.EphemeralFiles = []FileConfig{{Path: "/run/value", Content: "ephemeral"}}
		}, want: "mounts/files/init/metadata"},
		{name: "ephemeral env", mutate: func(c *SandboxConfig) {
			c.Launch.EphemeralEnv = map[string]string{"TOKEN": "secret"}
		}, want: "launch configuration is cold-start-only"},
		{name: "init", mutate: func(c *SandboxConfig) {
			c.Init = []InitConfig{{Exec: "/bin/true"}}
		}, want: "mounts/files/init/metadata"},
		{name: "plugin", mutate: func(c *SandboxConfig) {
			c.Launch.Plugin = []PluginConfig{{Exec: "/bin/plugin"}}
		}, want: "launch configuration is cold-start-only"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host := restoreHostConfig()
			if tc.mutate != nil {
				tc.mutate(host)
			}
			_, _, err := ApplyRestoreRules(validPortableConfig(), host, tc.presence)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestApplyRestoreRulesRejectsResourceDiskAndNetworkConflicts(t *testing.T) {
	t.Run("capacity", func(t *testing.T) {
		host := restoreHostConfig()
		host.Resources.Capacity = CapacityConfig{CPU: 8, Memory: "8GiB"}
		_, _, err := ApplyRestoreRules(validPortableConfig(), host, restorePresence("resources.capacity.cpu"))
		if err == nil || !strings.Contains(err.Error(), "resources.capacity conflicts") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("allocatable above snapshot capacity", func(t *testing.T) {
		host := restoreHostConfig()
		host.Resources.Allocatable = AllocatableConfig{CPU: 1, Memory: "8GiB"}
		host.Resources.Control.CgroupPath = "/sys/fs/cgroup/target"
		_, _, err := ApplyRestoreRules(validPortableConfig(), host, restorePresence("resources.allocatable.memory"))
		if err == nil || !strings.Contains(err.Error(), "resources.allocatable.memory must be > 0 and <= capacity.memory") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("fractional allocatable without cgroup", func(t *testing.T) {
		host := restoreHostConfig()
		host.Resources.Control.CgroupPath = ""
		host.Resources.Allocatable.CPU = 0.5
		_, _, err := ApplyRestoreRules(validPortableConfig(), host, restorePresence("resources.allocatable.cpu"))
		if err == nil || !strings.Contains(err.Error(), "must equal capacity.cpu") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("deflate on OOM", func(t *testing.T) {
		artifact := validPortableConfig()
		artifactDeflate := true
		artifact.Resources.Allocatable.DeflateOnOOM = &artifactDeflate
		host := restoreHostConfig()
		targetDeflate := false
		host.Resources.Allocatable.DeflateOnOOM = &targetDeflate
		_, _, err := ApplyRestoreRules(artifact, host, restorePresence("resources.allocatable.deflate_on_oom"))
		if err == nil || !strings.Contains(err.Error(), "deflate_on_oom conflicts") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("disk graph", func(t *testing.T) {
		host := restoreHostConfig()
		host.Boot.Root.Base = "file:///host/override.overlay"
		_, _, err := ApplyRestoreRules(validPortableConfig(), host, restorePresence("boot.root.base"))
		if err == nil || !strings.Contains(err.Error(), "immutable graph") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("explicit empty data disks", func(t *testing.T) {
		artifact := validPortableConfig()
		artifact.Boot.Disks = []PortableDiskConfig{{
			Name:               "data",
			PortableRootConfig: PortableRootConfig{Base: "file://data.overlay@sha256:" + testSHA},
		}}
		artifact.Mounts = []MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
		host := restoreHostConfig()
		_, _, err := ApplyRestoreRules(artifact, host, restorePresence("boot.disks"))
		if err == nil || !strings.Contains(err.Error(), "boot.disks count 0 conflicts") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("missing provider", func(t *testing.T) {
		artifact := validPortableConfig()
		artifact.Network = PortableNetworkConfig{Enabled: true, Interface: "eth0"}
		host := restoreHostConfig()
		_, _, err := ApplyRestoreRules(artifact, host, FieldPresence{})
		if err == nil || !strings.Contains(err.Error(), "provider presence=false") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("interface", func(t *testing.T) {
		artifact := validPortableConfig()
		artifact.Network = PortableNetworkConfig{Enabled: true, Interface: "eth0"}
		host := restoreHostConfig()
		host.Network = NetworkConfig{TAP: "tap0", Interface: "ens3"}
		_, _, err := ApplyRestoreRules(artifact, host, restorePresence("network.interface"))
		if err == nil || !strings.Contains(err.Error(), "portable topology") {
			t.Fatalf("error = %v", err)
		}
	})
}
