package sandbox

import (
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"strings"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// CHCommand assembles the cloud-hypervisor argv for a cold-start sandbox.
//
// vsockSock is the host-side base UDS path; CH proxies guest CID 2 vsock
// traffic to "<vsockSock>_<port>" entries. sandbox-ctl listens on the
// per-port suffix (proto.LaunchPort) for the launch handshake.
//
// Memory sizing: --memory size= is the capacity (what guest sees), with
// shared=on so vhost-user backends in the same process can mmap the
// memfd CH creates. Balloon size = capacity - allocatable, releasing the
// difference back to host at boot. free_page_reporting stays OFF (its
// mmu_notifier traffic starves the guest vsock kthread — see balloon.go);
// runtime adjustments come from the host resctl.BalloonController via
// /api/v1/vm.resize on mem_report feedback.
//
// uffdSock is the path of the va_report UDS server (cloud-hypervisor.md
// §3.3). Patched CH connects to it during create_ram_region.
//
// The memfd fd and uffd fd are inherited via cmd.ExtraFiles; CH sees
// them at fd=3 and fd=4 respectively, referenced in --memory-zone.
//
// consoleArg is the value for CH's `--console` flag, computed by
// stdio.Mode.SetupCHStdio: "tty" (CH writes the guest kernel console /
// hvc0 to its own stdout, which sandbox-ctl points at /dev/null, its
// stderr, or a file) or "off" (CH discards it). CH's stdin is /dev/null,
// so `--console tty` never raw-izes a controlling terminal. `--serial
// off` — no 8250 UART; cmdline pins `console=hvc0`. The application's
// stdin/stdout/stderr do NOT travel via the console — they go over the
// vsock stdio MUX (pkg/mux). See docs/sandbox.md §5.2.
func CHCommand(cfg *config.SandboxConfig, disks []DiskArg, chSock, vsockSock, kernelPath, runtimePath, uffdSock, consoleArg string, tapFDNum int, netMAC string) ([]string, error) {
	capBytes, err := cfg.CapacityMemoryBytes()
	if err != nil {
		return nil, err
	}
	allocBytes, err := cfg.AllocatableMemoryBytes()
	if err != nil {
		return nil, err
	}

	// --memory-zone replaces --memory under the unified-memfd model:
	// fd=3 is the memfd inherited via cmd.ExtraFiles[0]. uffd_socket
	// is the path of the va_report UDS server in sandbox-ctl; CH
	// creates its own uffd in create_ram_region (mm-bound to CH so
	// faults route correctly) and hands the fd back via SCM_RIGHTS.
	memZone := fmt.Sprintf(
		"id=ram0,size=%dM,shared=on,fd=3,uffd_socket=%s",
		capBytes>>20, uffdSock)

	args := []string{
		"--api-socket", chSock,
		"--kernel", kernelPath,
		"--pmem", fmt.Sprintf("file=%s,discard_writes=on,iommu=off", runtimePath),
		// CH 51 requires --memory size=0 when zones are used; the size
		// is taken from the zone config.
		"--memory", "size=0,shared=on",
		"--memory-zone", memZone,
		"--cpus", fmt.Sprintf("boot=%d", cfg.Resources.Capacity.CPU),
		"--vsock", fmt.Sprintf("cid=%d,socket=%s", proto.VsockGuestCID, vsockSock),
		"--console", consoleArg,
		"--serial", "off",
	}

	// One --disk flag with all served vhost devices as values, in order (root
	// first, then each boot.disks[] data disk). readonly=on marks ro erofs bases
	// (overlay lowers); writable ext4 (single root / overlay uppers / single
	// data disks) are read-write. This order fixes the guest /dev/vd[a,b,c…].
	if len(disks) > 0 {
		args = append(args, "--disk")
		for _, d := range disks {
			spec := "vhost_user=on,socket=" + d.Sock
			if d.ReadOnly {
				spec += ",readonly=on"
			}
			args = append(args, spec)
		}
	}

	if allocBytes < capBytes {
		// size = capacity − allocatable at boot: balloon device starts
		// pre-inflated to the static minimum, so the guest sees exactly
		// `allocatable` MiB visible from the moment it boots. No
		// post-Settled inflate transition; the host resctl.BalloonController
		// (pkg/sandbox/balloon.go) seeds its in-memory target to the
		// same value and reconciles a no-op on Start, then handles
		// runtime adjustments via /api/v1/vm.resize on mem_report
		// feedback or controller grant/reclaim events.
		//
		// free_page_reporting is intentionally OFF — its mmu_notifier
		// traffic starves the guest vsock kthread (see balloon.go
		// rationale).
		balloonOpts := fmt.Sprintf("size=%d", capBytes-allocBytes)
		if cfg.DeflateOnOOM() {
			balloonOpts += ",deflate_on_oom=on"
		}
		args = append(args, "--balloon", balloonOpts)
	}

	if netArg := chNetArg(cfg.Network.TAP, tapFDNum, netMAC); netArg != "" {
		args = append(args, "--net", netArg)
	}

	args = append(args, "--cmdline", buildCmdline(cfg))
	return args, nil
}

// chNetArg builds CH's --net value. tapfd handoff (tapFDNum>0) drives
// virtio-net off a pre-opened tap queue fd inherited by CH (vnet_hdr framing,
// docs/tapfd.md §2.6); otherwise CH opens a named host tap. A non-empty mac is
// mirrored onto virtio-net so the provider's data plane accepts the guest
// (docs/tapfd.md §5); empty → CH auto-assigns. id=_net0 names the device so
// restore can re-bind a fresh fd via --restore net_fds (see restore path).
func chNetArg(tapName string, tapFDNum int, mac string) string {
	macPart := ""
	if mac != "" {
		macPart = ",mac=" + mac
	}
	switch {
	case tapFDNum > 0:
		return fmt.Sprintf("fd=%d%s,id=_net0,iommu=off", tapFDNum, macPart)
	case tapName != "":
		return fmt.Sprintf("tap=%s%s,iommu=off", tapName, macPart)
	default:
		return ""
	}
}

// buildCmdline returns the kernel cmdline for guest boot.
//
// Layout:
//  1. Auto-injected fixed boot params (init, root, rootfstype, console)
//     — lock down how sandbox-runtime.erofs is mounted as / via
//     virtio-pmem and that kernel dmesg goes to hvc0 (virtio-console).
//  2. User-supplied boot.cmdline extras (quiet, loglevel=, …; init= /
//     root= / console= are platform-owned and should not be repeated).
//
// The launch spec (exec/args/env/workdir/restart/stdio) is **not** in
// the cmdline — it travels over vsock at runtime via the launch protocol
// (pkg/proto), which also avoids cmdline length / quoting limits.
func buildCmdline(cfg *config.SandboxConfig) string {
	parts := []string{
		"init=/sbin/init",
		"root=/dev/pmem0",
		"ro",
		"rootfstype=erofs",
		"dax=always",
		"console=hvc0",
		// panic=-1 → kernel immediately emergency_restart()s on panic
		// (e.g., "Attempted to kill init!"); on our minimal x86 kernel
		// with no ACPI / PS2 / EFI the reboot path falls through to a
		// triple fault → KVM_EXIT_SHUTDOWN → CH exits → sandbox-ctl
		// returns. Without this, panic=0 leaves the guest spinning
		// forever and sandbox-ctl hangs. User-supplied boot.cmdline
		// appears after, so `panic=0` in yaml still wins if you want to
		// freeze the guest for debugging.
		"panic=-1",
	}
	// Single-disk mode: tell sandbox-init to mount the single rw root disk
	// directly (ext4) instead of assembling the two-disk overlayfs. Read from
	// /proc/cmdline in phase 1 (before the launch handshake, so it can't ride
	// the launch spec). Overlay mode adds nothing (the default).
	if cfg.SingleDisk() {
		parts = append(parts, "sandbox.root.layout=single")
	}
	if cfg.Boot.Cmdline != "" {
		parts = append(parts, cfg.Boot.Cmdline)
	}
	return strings.Join(parts, " ")
}
