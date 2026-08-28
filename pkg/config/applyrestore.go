package config

import (
	"errors"
	"fmt"
)

// ApplyRestoreRules binds host-only runtime policy to the immutable C0 carried
// by the Sandbox E referenced from a memory Snapshot S. Restore never applies
// cold-start workload overrides: launch, mounts, files, init, metadata and the
// immutable disk graph continue to come exclusively from E.
func ApplyRestoreRules(artifact *PortableSandboxConfig, host *SandboxConfig, presence FieldPresence) (*SandboxConfig, *PortableSandboxConfig, error) {
	if artifact == nil {
		return nil, nil, errors.New("run --restore: referenced Sandbox has no portable config")
	}
	if host == nil {
		return nil, nil, errors.New("run --restore: missing host config")
	}
	if err := artifact.Validate(); err != nil {
		return nil, nil, fmt.Errorf("run --restore Sandbox config: %w", err)
	}
	if err := rejectRestoreColdOnly(host, presence); err != nil {
		return nil, nil, err
	}
	if err := validateRestoreDiskBindings(artifact, host, presence); err != nil {
		return nil, nil, err
	}

	c0, err := artifact.Clone()
	if err != nil {
		return nil, nil, err
	}
	if err := validateRestoreResourcePolicy(c0, host, presence); err != nil {
		return nil, nil, err
	}
	runtime := sandboxConfigFromPortable(c0)
	runtime.Boot.Kernel = host.Boot.Kernel
	runtime.Boot.Runtime = host.Boot.Runtime
	if runtime.Boot.Kernel == "" {
		return nil, nil, errors.New("run --restore: host boot.kernel binding is required")
	}
	if runtime.Boot.Runtime == "" {
		return nil, nil, errors.New("run --restore: host boot.runtime binding is required")
	}

	// These fields control the replacement host process and do not claim to
	// alter already-restored guest execution state. Allocatable is the target
	// node's settled workload policy; applying it here leaves immutable C0 and
	// the Budget captured in Snapshot S unchanged.
	if presence.Any("resources.allocatable") {
		runtime.Resources.Allocatable = cloneAllocatable(host.Resources.Allocatable)
	}
	runtime.Resources.Control = host.Resources.Control
	runtime.Resources.Overhead = host.Resources.Overhead
	runtime.Resources.WatermarkHigh = host.Resources.WatermarkHigh
	runtime.Resources.Startup = host.Resources.Startup
	runtime.Timeouts = host.Timeouts
	runtime.Restore = host.Restore

	hostHasNetwork := host.Network.hasSource()
	if hostHasNetwork != c0.Network.Enabled {
		return nil, nil, fmt.Errorf("run --restore: portable network.enabled=%t conflicts with host provider presence=%t", c0.Network.Enabled, hostHasNetwork)
	}
	if presence.Has("network.interface") && host.Network.Interface != c0.Network.Interface {
		return nil, nil, fmt.Errorf("run --restore: host network.interface %q conflicts with portable topology %q", host.Network.Interface, c0.Network.Interface)
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

func rejectRestoreColdOnly(host *SandboxConfig, presence FieldPresence) error {
	for _, field := range []string{
		"boot.cmdline", "launch", "mounts", "files", "ephemeral_files", "init", "metadata",
	} {
		if presence.Any(field) {
			return fmt.Errorf("run --restore: %s is cold-start-only and cannot be applied to restored execution state", field)
		}
	}
	// Direct API callers may not have YAML presence. Reject materially nonempty
	// values while tolerating ApplyDefaults' inert launch defaults.
	if host.Boot.Cmdline != "" {
		return errors.New("run --restore: boot.cmdline is cold-start-only")
	}
	if host.Launch.Exec != "" || len(host.Launch.Args) != 0 || len(host.Launch.Env) != 0 ||
		len(host.Launch.EphemeralEnv) != 0 || host.Launch.Placeholder || len(host.Launch.Plugin) != 0 ||
		host.Launch.CgroupControl || host.Launch.PIDNamespace != "" || host.Launch.User != "" ||
		host.Launch.StopSignal != "" || host.Launch.StopGracePeriod != "" || host.Launch.StartTimeout != "" ||
		(host.Launch.Workdir != "" && host.Launch.Workdir != "/") ||
		(host.Launch.Restart != "" && host.Launch.Restart != "never") {
		return errors.New("run --restore: non-empty launch configuration is cold-start-only")
	}
	if len(host.Mounts) != 0 || len(host.Files) != 0 || len(host.EphemeralFiles) != 0 || len(host.Init) != 0 || len(host.Metadata) != 0 {
		return errors.New("run --restore: mounts/files/init/metadata are cold-start-only")
	}
	return nil
}

func validateRestoreResourcePolicy(artifact *PortableSandboxConfig, host *SandboxConfig, presence FieldPresence) error {
	if presence.Any("resources.capacity") && host.Resources.Capacity != artifact.Resources.Capacity {
		return fmt.Errorf("run --restore: resources.capacity conflicts with referenced Sandbox")
	}
	if presence.Any("resources.allocatable") {
		candidate := *artifact
		candidate.Resources = artifact.Resources
		candidate.Resources.Allocatable = cloneAllocatable(host.Resources.Allocatable)
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("run --restore resources.allocatable: %w", err)
		}
	}
	return nil
}

func validateRestoreDiskBindings(artifact *PortableSandboxConfig, host *SandboxConfig, presence FieldPresence) error {
	if presence.Has("boot.root.base") || presence.Has("boot.root.base_from_refs") || host.Boot.Root.Base != "" || len(host.Boot.Root.BaseFromRefs) != 0 {
		return errors.New("run --restore: boot.root immutable graph is owned by the referenced Sandbox")
	}
	if artifact.Boot.Root.Overlay == nil {
		if host.Boot.Root.Overlay != nil {
			return errors.New("run --restore: host boot.root.overlay changes the Sandbox disk topology")
		}
	} else {
		if host.Boot.Root.Diff != "" || host.Boot.Root.DiffTemplate != "" || host.Boot.Root.DiffSize != "" {
			return errors.New("run --restore: root overlay active diff fields must be under boot.root.overlay")
		}
		if host.Boot.Root.Overlay != nil && (host.Boot.Root.Overlay.Base != "" || len(host.Boot.Root.Overlay.BaseFromRefs) != 0) {
			return errors.New("run --restore: boot.root.overlay immutable graph is owned by the referenced Sandbox")
		}
	}
	if (presence.Has("boot.disks") || len(host.Boot.Disks) != 0) && len(host.Boot.Disks) != len(artifact.Boot.Disks) {
		return fmt.Errorf("run --restore: host boot.disks count %d conflicts with Sandbox count %d", len(host.Boot.Disks), len(artifact.Boot.Disks))
	}
	for i := range host.Boot.Disks {
		hostDisk := &host.Boot.Disks[i]
		artifactDisk := &artifact.Boot.Disks[i]
		if hostDisk.Name != artifactDisk.Name {
			return fmt.Errorf("run --restore: host boot.disks[%d].name %q conflicts with Sandbox name %q", i, hostDisk.Name, artifactDisk.Name)
		}
		if hostDisk.Base != "" || len(hostDisk.BaseFromRefs) != 0 {
			return fmt.Errorf("run --restore: boot.disks[%d] immutable graph is owned by the referenced Sandbox", i)
		}
		if artifactDisk.Overlay == nil {
			if hostDisk.Overlay != nil {
				return fmt.Errorf("run --restore: host boot.disks[%d].overlay changes the Sandbox topology", i)
			}
			continue
		}
		if hostDisk.Diff != "" || hostDisk.DiffTemplate != "" || hostDisk.DiffSize != "" {
			return fmt.Errorf("run --restore: boot.disks[%d] overlay active diff fields must be under overlay", i)
		}
		if hostDisk.Overlay != nil && (hostDisk.Overlay.Base != "" || len(hostDisk.Overlay.BaseFromRefs) != 0) {
			return fmt.Errorf("run --restore: boot.disks[%d].overlay immutable graph is owned by the referenced Sandbox", i)
		}
	}
	return nil
}
