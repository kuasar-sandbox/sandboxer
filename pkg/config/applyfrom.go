package config

import (
	"errors"
	"fmt"
)

// ApplyFromRules applies one presence-aware host/instance document to a
// Sandbox artifact. It returns the runtime config and immutable portable C0.
// Artifact disk topology is never merged through the generic YAML loader.
func ApplyFromRules(artifact *PortableSandboxConfig, host *SandboxConfig, presence FieldPresence) (*SandboxConfig, *PortableSandboxConfig, error) {
	if artifact == nil {
		return nil, nil, errors.New("run --from: missing Sandbox portable config")
	}
	if host == nil {
		return nil, nil, errors.New("run --from: missing host config")
	}
	if err := artifact.Validate(); err != nil {
		return nil, nil, err
	}
	if err := validateFromProtectedFields(artifact, host, presence); err != nil {
		return nil, nil, err
	}

	c0, err := artifact.Clone()
	if err != nil {
		return nil, nil, err
	}
	applyPersistentFromOverrides(c0, host, presence)
	if err := c0.Validate(); err != nil {
		return nil, nil, fmt.Errorf("run --from persistent override: %w", err)
	}

	runtime := sandboxConfigFromPortable(c0)
	runtime.Boot.Kernel = host.Boot.Kernel
	runtime.Boot.Runtime = host.Boot.Runtime
	if runtime.Boot.Kernel == "" {
		return nil, nil, errors.New("run --from: host boot.kernel binding is required")
	}
	if runtime.Boot.Runtime == "" {
		return nil, nil, errors.New("run --from: host boot.runtime binding is required")
	}

	// Host-owned resource and protocol policy.
	runtime.Resources.Control = host.Resources.Control
	runtime.Resources.Overhead = host.Resources.Overhead
	runtime.Resources.WatermarkHigh = host.Resources.WatermarkHigh
	runtime.Resources.Startup = host.Resources.Startup
	runtime.Timeouts = host.Timeouts
	runtime.Restore = host.Restore
	runtime.Launch.StartTimeout = host.Launch.StartTimeout

	// Instance-only launch/file input is applied to the cold launch but absent
	// from C0 and every exported C1.
	runtime.Launch.EphemeralEnv = cloneMap(host.Launch.EphemeralEnv)
	runtime.EphemeralFiles = cloneFiles(host.EphemeralFiles)

	// Provider and identity are host-owned; the artifact owns only whether a
	// NIC exists and its guest-facing interface name.
	hostHasNetwork := host.Network.hasSource()
	if hostHasNetwork != c0.Network.Enabled {
		return nil, nil, fmt.Errorf("run --from: portable network.enabled=%t conflicts with host provider presence=%t", c0.Network.Enabled, hostHasNetwork)
	}
	if presence.Has("network.interface") && host.Network.Interface != c0.Network.Interface {
		return nil, nil, fmt.Errorf("run --from: host network.interface %q conflicts with portable topology %q", host.Network.Interface, c0.Network.Interface)
	}
	runtime.Network = host.Network
	if c0.Network.Enabled {
		runtime.Network.Interface = c0.Network.Interface
	} else {
		runtime.Network.Interface = ""
	}

	applyActiveDiskBindings(runtime, host)
	return runtime, c0, nil
}

func applyPersistentFromOverrides(c0 *PortableSandboxConfig, host *SandboxConfig, presence FieldPresence) {
	if presence.Has("resources.capacity.cpu") {
		c0.Resources.Capacity.CPU = host.Resources.Capacity.CPU
	}
	if presence.Has("resources.capacity.memory") {
		c0.Resources.Capacity.Memory = host.Resources.Capacity.Memory
	}
	if presence.Has("resources.allocatable.cpu") {
		c0.Resources.Allocatable.CPU = host.Resources.Allocatable.CPU
	}
	if presence.Has("resources.allocatable.memory") {
		c0.Resources.Allocatable.Memory = host.Resources.Allocatable.Memory
	}
	if presence.Has("resources.allocatable.deflate_on_oom") {
		c0.Resources.Allocatable.DeflateOnOOM = cloneAllocatable(host.Resources.Allocatable).DeflateOnOOM
	}

	launch := &c0.Launch
	if presence.Has("launch.exec") {
		launch.Exec = host.Launch.Exec
	}
	if presence.Has("launch.args") {
		launch.Args = append([]string(nil), host.Launch.Args...)
	}
	if presence.Has("launch.env") {
		launch.Env = cloneMap(host.Launch.Env)
	}
	if presence.Has("launch.workdir") {
		launch.Workdir = host.Launch.Workdir
	}
	if presence.Has("launch.restart") {
		launch.Restart = host.Launch.Restart
	}
	if presence.Has("launch.cgroup_control") {
		launch.CgroupControl = host.Launch.CgroupControl
	}
	if presence.Has("launch.placeholder") {
		launch.Placeholder = host.Launch.Placeholder
	}
	if presence.Has("launch.pid_namespace") {
		launch.PIDNamespace = host.Launch.PIDNamespace
	}
	if presence.Has("launch.plugin") {
		launch.Plugin = clonePlugins(host.Launch.Plugin)
	}
	if presence.Has("launch.user") {
		launch.User = host.Launch.User
	}
	if presence.Has("launch.stop_signal") {
		launch.StopSignal = host.Launch.StopSignal
	}
	if presence.Has("launch.stop_grace_period") {
		launch.StopGracePeriod = host.Launch.StopGracePeriod
	}
	if presence.Has("mounts") {
		c0.Mounts = cloneMounts(host.Mounts)
	}
	if presence.Has("files") {
		c0.Files = cloneFiles(host.Files)
	}
	if presence.Has("init") {
		c0.Init = cloneInit(host.Init)
	}
	if presence.Has("metadata") {
		c0.Metadata = cloneMap(host.Metadata)
	}
}

func validateFromProtectedFields(artifact *PortableSandboxConfig, host *SandboxConfig, presence FieldPresence) error {
	if presence.Has("boot.cmdline") && host.Boot.Cmdline != artifact.Boot.Cmdline {
		return errors.New("run --from: boot.cmdline is owned by the Sandbox artifact")
	}
	if presence.Has("mounts") {
		if err := validateFromDiskMountTopology(artifact.Mounts, host.Mounts); err != nil {
			return err
		}
	}
	if presence.Has("boot.root.base") || presence.Has("boot.root.base_from_refs") || host.Boot.Root.Base != "" || len(host.Boot.Root.BaseFromRefs) != 0 {
		return errors.New("run --from: boot.root immutable graph is owned by the Sandbox artifact")
	}
	if artifact.Boot.Root.Overlay == nil {
		if host.Boot.Root.Overlay != nil {
			return errors.New("run --from: host boot.root.overlay changes the artifact's single-disk topology")
		}
	} else {
		if host.Boot.Root.Diff != "" || host.Boot.Root.DiffTemplate != "" || host.Boot.Root.DiffSize != "" {
			return errors.New("run --from: root overlay topology requires active diff fields under boot.root.overlay")
		}
		if host.Boot.Root.Overlay != nil && (host.Boot.Root.Overlay.Base != "" || len(host.Boot.Root.Overlay.BaseFromRefs) != 0) {
			return errors.New("run --from: boot.root.overlay immutable graph is owned by the Sandbox artifact")
		}
		if presence.Has("boot.root.overlay.base") || presence.Has("boot.root.overlay.base_from_refs") {
			return errors.New("run --from: boot.root.overlay immutable graph is owned by the Sandbox artifact")
		}
	}

	if (presence.Has("boot.disks") || len(host.Boot.Disks) != 0) && len(host.Boot.Disks) != len(artifact.Boot.Disks) {
		return fmt.Errorf("run --from: host boot.disks count %d conflicts with artifact count %d", len(host.Boot.Disks), len(artifact.Boot.Disks))
	}
	for i := range host.Boot.Disks {
		hostDisk := &host.Boot.Disks[i]
		artifactDisk := &artifact.Boot.Disks[i]
		if hostDisk.Name != artifactDisk.Name {
			return fmt.Errorf("run --from: host boot.disks[%d].name %q conflicts with artifact name %q", i, hostDisk.Name, artifactDisk.Name)
		}
		if hostDisk.Base != "" || len(hostDisk.BaseFromRefs) != 0 {
			return fmt.Errorf("run --from: boot.disks[%d] immutable graph is owned by the Sandbox artifact", i)
		}
		if artifactDisk.Overlay == nil {
			if hostDisk.Overlay != nil {
				return fmt.Errorf("run --from: host boot.disks[%d].overlay changes single-disk topology", i)
			}
			continue
		}
		if hostDisk.Diff != "" || hostDisk.DiffTemplate != "" || hostDisk.DiffSize != "" {
			return fmt.Errorf("run --from: boot.disks[%d] overlay active diff fields must be under overlay", i)
		}
		if hostDisk.Overlay != nil && (hostDisk.Overlay.Base != "" || len(hostDisk.Overlay.BaseFromRefs) != 0) {
			return fmt.Errorf("run --from: boot.disks[%d].overlay immutable graph is owned by the Sandbox artifact", i)
		}
	}
	return nil
}

func validateFromDiskMountTopology(artifactMounts, hostMounts []MountConfig) error {
	type diskMount struct {
		source string
		target string
	}
	topology := func(mounts []MountConfig) []diskMount {
		result := make([]diskMount, 0, len(mounts))
		for _, mount := range mounts {
			if mount.Type == "disk" {
				result = append(result, diskMount{source: mount.Source, target: mount.Target})
			}
		}
		return result
	}
	want := topology(artifactMounts)
	got := topology(hostMounts)
	if len(got) != len(want) {
		return fmt.Errorf("run --from: host mounts change artifact-owned data-disk mount topology")
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("run --from: host mounts change artifact-owned disk mount[%d] %q -> %q", i, want[i].source, want[i].target)
		}
	}
	return nil
}

func sandboxConfigFromPortable(portable *PortableSandboxConfig) *SandboxConfig {
	cfg := &SandboxConfig{
		Resources: ResourcesConfig{
			Capacity:    portable.Resources.Capacity,
			Allocatable: cloneAllocatable(portable.Resources.Allocatable),
		},
		Network: NetworkConfig{Interface: portable.Network.Interface},
		Boot: BootConfig{
			Cmdline: portable.Boot.Cmdline,
			Root:    rootConfigFromPortable(&portable.Boot.Root),
		},
		Launch: LaunchConfig{
			Exec: portable.Launch.Exec, Args: append([]string(nil), portable.Launch.Args...),
			Env: cloneMap(portable.Launch.Env), Workdir: portable.Launch.Workdir,
			Restart: portable.Launch.Restart, CgroupControl: portable.Launch.CgroupControl,
			Placeholder: portable.Launch.Placeholder, PIDNamespace: portable.Launch.PIDNamespace,
			Plugin: clonePlugins(portable.Launch.Plugin), User: portable.Launch.User,
			StopSignal: portable.Launch.StopSignal, StopGracePeriod: portable.Launch.StopGracePeriod,
		},
		Mounts:   cloneMounts(portable.Mounts),
		Files:    cloneFiles(portable.Files),
		Init:     cloneInit(portable.Init),
		Metadata: cloneMap(portable.Metadata),
	}
	cfg.Boot.Disks = make([]DiskConfig, len(portable.Boot.Disks))
	for i := range portable.Boot.Disks {
		cfg.Boot.Disks[i] = DiskConfig{
			Name:       portable.Boot.Disks[i].Name,
			RootConfig: rootConfigFromPortable(&portable.Boot.Disks[i].PortableRootConfig),
		}
	}
	return cfg
}

func rootConfigFromPortable(root *PortableRootConfig) RootConfig {
	out := RootConfig{Base: root.Base, BaseFromRefs: append([]string(nil), root.BaseFromRefs...)}
	if root.Overlay != nil {
		out.Overlay = &OverlayConfig{
			Base: root.Overlay.Base, BaseFromRefs: append([]string(nil), root.Overlay.BaseFromRefs...),
		}
	}
	return out
}

func applyActiveDiskBindings(runtime, host *SandboxConfig) {
	if runtime.Boot.Root.Overlay == nil {
		runtime.Boot.Root.Diff = host.Boot.Root.Diff
		runtime.Boot.Root.DiffTemplate = host.Boot.Root.DiffTemplate
		runtime.Boot.Root.DiffSize = host.Boot.Root.DiffSize
	} else if host.Boot.Root.Overlay != nil {
		runtime.Boot.Root.Overlay.Diff = host.Boot.Root.Overlay.Diff
		runtime.Boot.Root.Overlay.DiffTemplate = host.Boot.Root.Overlay.DiffTemplate
		runtime.Boot.Root.Overlay.DiffSize = host.Boot.Root.Overlay.DiffSize
	}
	for i := range runtime.Boot.Disks {
		if i >= len(host.Boot.Disks) {
			continue
		}
		target := &runtime.Boot.Disks[i].RootConfig
		source := &host.Boot.Disks[i].RootConfig
		if target.Overlay == nil {
			target.Diff, target.DiffTemplate, target.DiffSize = source.Diff, source.DiffTemplate, source.DiffSize
		} else if source.Overlay != nil {
			target.Overlay.Diff = source.Overlay.Diff
			target.Overlay.DiffTemplate = source.Overlay.DiffTemplate
			target.Overlay.DiffSize = source.Overlay.DiffSize
		}
	}
}
