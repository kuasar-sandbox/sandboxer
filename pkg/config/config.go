// Package config is the sandbox.yaml schema: parsing, defaults, validation,
// the per-field timeout resolvers, the file:// / manifest:// URI helper, and
// the ManifestConfig alias. It is the leaf every other sandbox package reads.
package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
	"github.com/kuasar-sandbox/sandboxer/pkg/util"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// ParseStopSignal resolves a signal name ("SIGTERM", "TERM") or decimal
// number ("15") to its number. Used host-side so the guest receives a plain
// int (signal names are arch-independent for the x86_64/arm64 targets where
// host and guest share the signal table).
func ParseStopSignal(s string) (int, error) {
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("invalid signal number %q", s)
		}
		return n, nil
	}
	name := strings.ToUpper(s)
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + name
	}
	if sig := unix.SignalNum(name); sig != 0 {
		return int(sig), nil
	}
	return 0, fmt.Errorf("unknown signal %q", s)
}

// SandboxConfig is the schema parsed from sandbox.yaml.
//
// The file co-exists with the manifest config YAML — manifest /
// store / crypto / cache fields live there (loaded separately via
// pkg/config). Sandbox.yaml carries only the per-sandbox
// resource / network / boot / launch fields.
type SandboxConfig struct {
	Resources ResourcesConfig `yaml:"resources"`
	Network   NetworkConfig   `yaml:"network"`
	Boot      BootConfig      `yaml:"boot"`
	Launch    LaunchConfig    `yaml:"launch"`

	// Timeouts tunes host-side restore/lifecycle deadlines. Every field
	// defaults to 0 = NO FORCED TIMEOUT — the host waits as long as the
	// guest, CH, or lazy page-in needs (dial/connect probes stay bounded),
	// so a slow remote/cache or a debugger pause never aborts a restore.
	// Set positive values (examples/timeouts-production.yaml) to fail fast
	// in production.
	Timeouts TimeoutsConfig `yaml:"timeouts,omitempty"`

	// Restore carries policy for this host restore invocation. It is not part
	// of snapshot.cfg and is never inherited from the snapshot being restored.
	Restore RestoreConfig `yaml:"restore,omitempty"`

	// Mounts / Files / Init drive guest environment setup (applied before
	// the app is forked). See docs/sandbox.md §3.8.
	Mounts []MountConfig `yaml:"mounts,omitempty"`
	Files  []FileConfig  `yaml:"files,omitempty"`
	// EphemeralFiles are injected only for this cold-start invocation. They
	// override Files by path at launch time and are deliberately excluded from
	// PortableSandboxConfig and every exported Sandbox artifact.
	EphemeralFiles []FileConfig `yaml:"ephemeral_files,omitempty"`
	Init           []InitConfig `yaml:"init,omitempty"`

	// Metadata is an opaque key/value passthrough for the platform above. The
	// runtime never interprets it. Persistent metadata is part of the portable
	// Sandbox configuration; memory Snapshot metadata is limited to its E ref
	// and memory-parent graph.
	Metadata map[string]string `yaml:"metadata,omitempty"`
}

// MarshalYAML keeps the no-active-root shape useful as a restore-host document.
// That shape cannot cold boot an overlay root: it has no active writable source,
// because Snapshot S's referenced Sandbox E owns the complete immutable graph
// and restore creates the active diffs. Some lifecycle callers retain the
// original image base while rebuilding their config for restore; it is stale
// provenance, not a host binding. Emitting it or the cold workload fields would
// make the host document claim that disk provenance and launch/files/init are
// re-applied to already-restored processes. Project the shape to host-owned
// fields instead.
//
// This is only a producer-side projection. Strict restore parsing still rejects
// every cold-only field that is actually present in an input document.
func (c SandboxConfig) MarshalYAML() (any, error) {
	if !c.isRestoreHostProjection() {
		type plain SandboxConfig
		return plain(c), nil
	}

	type restoreRootYAML struct {
		Overlay struct{} `yaml:"overlay"`
	}
	type restoreBootYAML struct {
		Kernel  string          `yaml:"kernel"`
		Runtime string          `yaml:"runtime"`
		Root    restoreRootYAML `yaml:"root"`
	}
	type restoreHostYAML struct {
		Resources ResourcesConfig `yaml:"resources"`
		Network   NetworkConfig   `yaml:"network"`
		Boot      restoreBootYAML `yaml:"boot"`
		Timeouts  TimeoutsConfig  `yaml:"timeouts,omitempty"`
		Restore   RestoreConfig   `yaml:"restore,omitempty"`
	}
	return restoreHostYAML{
		Resources: c.Resources,
		Network:   c.Network,
		Boot: restoreBootYAML{
			Kernel: c.Boot.Kernel, Runtime: c.Boot.Runtime,
		},
		Timeouts: c.Timeouts,
		Restore:  c.Restore,
	}, nil
}

func (c SandboxConfig) isRestoreHostProjection() bool {
	root := c.Boot.Root
	// Lifecycle producers resolve startup memory before materializing a run.
	// Requiring that resolved marker and the complete absence of an immutable or
	// active writable upper keeps every valid cold config lossless: ValidateCold
	// requires at least one of overlay.{base,diff,diff_template}. A prepared
	// restore document deliberately has none because Sandbox E owns that graph.
	if c.Resources.Startup == nil || c.Boot.Kernel == "" || c.Boot.Runtime == "" || len(c.Boot.Disks) != 0 || root.Overlay == nil {
		return false
	}
	return root.Diff == "" && root.DiffTemplate == "" && root.DiffSize == "" &&
		root.Overlay.Base == "" && len(root.Overlay.BaseFromRefs) == 0 &&
		root.Overlay.Diff == "" && root.Overlay.DiffTemplate == "" && root.Overlay.DiffSize == ""
}

// PrefetchMode selects whether restore requests a best-effort warm-up of the
// current memory self Stream. The empty value has the same semantics as off.
type PrefetchMode string

const (
	PrefetchOff    PrefetchMode = "off"
	PrefetchMemory PrefetchMode = "memory"
)

// RestoreConfig contains host-only policy for one restore invocation.
// Prefetch is a string at the YAML boundary so invalid input survives parsing
// and can be rejected with a field-specific validation error.
type RestoreConfig struct {
	Prefetch string `yaml:"prefetch,omitempty"`
}

// ParsePrefetchMode validates the public restore.prefetch spelling and returns
// its normalized mode. Empty means the default, off.
func ParsePrefetchMode(s string) (PrefetchMode, error) {
	switch PrefetchMode(s) {
	case "", PrefetchOff:
		return PrefetchOff, nil
	case PrefetchMemory:
		return PrefetchMemory, nil
	default:
		return "", fmt.Errorf("restore.prefetch %q invalid (want off|memory)", s)
	}
}

func (c RestoreConfig) validate() error {
	_, err := ParsePrefetchMode(c.Prefetch)
	return err
}

// ResourcesConfig separates immutable VM capacity from the settled guest
// headroom maintained by the local memory-Budget controller.
//
// See docs/sandbox.md §4.1 for the three deployment modes driven by
// Control.CgroupPath / Control.Controller presence.
type ResourcesConfig struct {
	Capacity    CapacityConfig    `yaml:"capacity"`
	Allocatable AllocatableConfig `yaml:"allocatable"`

	// Control gates cgroup management and dynamic resource control.
	// Empty CgroupPath = no-cgroup mode; CgroupPath set + Controller empty
	// = static-cgroup mode; both set = dynamic mode (controller-managed).
	Control ControlConfig `yaml:"control,omitempty"`

	// Overhead, WatermarkHigh, Startup use pointers so we can distinguish
	// "not set" from "set to zero". Overhead and WatermarkHigh require a
	// cgroup; Startup is an independent cold-start headroom in every mode.
	Overhead      *OverheadConfig      `yaml:"overhead,omitempty"`
	WatermarkHigh *WatermarkHighConfig `yaml:"watermark_high,omitempty"`
	Startup       *StartupConfig       `yaml:"startup,omitempty"`
}

type CapacityConfig struct {
	CPU    int    `yaml:"cpu"`    // vCPUs declared to guest; written to --cpus boot=N
	Memory string `yaml:"memory"` // human-readable size, e.g. "8GiB"
}

type AllocatableConfig struct {
	// CPU in fractional cores. With cgroup_path set, drives cpu.weight =
	// clamp(round(CPU * 100), 1, 10000). Without cgroup_path, must equal
	// capacity.cpu (no fractional CPU without cgroup).
	CPU float64 `yaml:"cpu"`
	// Memory is settled guest headroom. It is neither a total Budget nor host
	// VMM RSS. The local controller adds it to its DemandMemory estimate.
	Memory string `yaml:"memory"`
	// DeflateOnOOM toggles CH --balloon ,deflate_on_oom=on. Pointer so
	// nil = use default (true). It applies whenever cold or steady policy
	// requires a balloon device; otherwise it has no CH device to configure.
	DeflateOnOOM *bool `yaml:"deflate_on_oom,omitempty"`
}

// ControlConfig gates cgroup management and dynamic resource control.
// Both fields are optional; their presence selects deployment mode.
type ControlConfig struct {
	// CgroupPath is the absolute path of an existing cgroup directory.
	// sandbox-ctl writes limits there and creates only the VMM process in it;
	// it never creates the directory or removes it on exit. Empty disables all
	// cgroup operations (no-cgroup mode).
	CgroupPath string `yaml:"cgroup_path,omitempty"`
	// CgroupFD is an inherited runtime capability backing CgroupPath. It is
	// never serialized; zero means sandbox-ctl should open CgroupPath itself.
	CgroupFD int `yaml:"-"`
	// Controller is the filesystem UDS path of a sandbox-resource-control
	// protocol endpoint. Linux abstract addresses are unsupported because the
	// recovery owner lock and lease inventory are derived from this path. At
	// runtime the bind/dial spelling is retained, while parent symlinks are
	// canonicalized for inventory; ambiguous final, dangling, or hard-link
	// aliases fail closed.
	// Non-empty enables dynamic mode (M2+). Requires CgroupPath.
	Controller string `yaml:"controller,omitempty"`
	// Sensor tunes the per-sandbox memory pressure sensor (data source +
	// reaction). Optional; nil = use PSI mode with default thresholds.
	Sensor *SensorConfig `yaml:"sensor,omitempty"`
}

// SensorConfig configures the sandbox-local pressure sensor. Its signals feed
// the same MemoryController grow path in static and dynamic cgroup modes;
// dynamic mode adds a reservation RPC before the local high/balloon update.
//
// Mode selects the signal source:
//
//   - "psi" (default): epoll on cgroup memory.pressure with a "some"
//     trigger. Sub-millisecond reaction once the trigger fires. Falls
//     back to events_poll if the kernel rejects the trigger write.
//   - "events_poll": legacy 100ms poll of memory.events.local high
//     counter. Worst-case 100ms reaction latency.
//   - "none": disable sensor (admission + balloon only).
type SensorConfig struct {
	Mode string `yaml:"mode,omitempty"`
	// PSI trigger format: "some <stall_us> <window_us>". Sensor wakes
	// when the cgroup accumulates StallUs microseconds of "some-task
	// stalled in reclaim" within a WindowUs sliding window.
	//
	// Defaults: 10_000 us stall in 1_000_000 us (1s) window. Empirical
	// sweet spot on dense workloads — looser (50ms) misses brief stalls
	// that don't sum to threshold; tighter (1ms) yields no further hang
	// reduction (limit becomes allocator throughput + single reclaim-
	// iteration time).
	PSISomeStallUs  uint64 `yaml:"psi_some_stall_us,omitempty"`
	PSISomeWindowUs uint64 `yaml:"psi_some_window_us,omitempty"`
	// MinIntervalMs debounces consecutive RequestBudget calls so a
	// stream of PSI wakeups becomes at most one RPC per interval.
	// Default 100ms.
	MinIntervalMs int `yaml:"min_interval_ms,omitempty"`
}

// OverheadConfig adjusts the VMM cgroup memory.max above capacity.memory for
// Cloud Hypervisor's host-side allocations. sandbox-ctl is deliberately kept
// outside this cgroup and does not consume this allowance.
type OverheadConfig struct {
	Memory string `yaml:"memory"` // memory.max = capacity.memory + this
}

// WatermarkHighConfig controls how much granted guest headroom remains above
// cgroup memory.high. Ratio is node-owned policy resolved into sandbox.yaml.
type WatermarkHighConfig struct {
	Ratio float64 `yaml:"ratio"`
}

// UnmarshalYAML keeps this replaced schema strict even though the historical
// top-level sandbox loader is permissive for unrelated fields.
func (c *WatermarkHighConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("resources.watermark_high must be a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value != "ratio" {
			return fmt.Errorf("field %s not found in type config.WatermarkHighConfig", node.Content[i].Value)
		}
	}
	type plain WatermarkHighConfig
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*c = WatermarkHighConfig(decoded)
	return nil
}

// StartupConfig sets the node's configured startup headroom. A cold start uses
// it for the initial Budget; restore records the policy in its reservation but
// admits the captured BudgetAtSnapshot instead. It applies in both static and
// dynamic modes and is never part of PortableSandboxConfig.
type StartupConfig struct {
	Memory string `yaml:"memory"`
}

// SensorRuntime returns the resolved sensor parameters, applying defaults
// for any unset fields. Mode defaults to "psi"; trigger defaults to 10ms
// of "some" stall accumulated within a 1s window (density-perf empirical
// sweet spot — see docs/sandbox.md §10.3 tuning notes); debounce defaults
// to 100ms between RequestBudget calls.
func (c *SandboxConfig) SensorRuntime() (mode string, stallUs, windowUs uint64, minInterval time.Duration) {
	mode = "psi"
	stallUs = 10_000
	windowUs = 1_000_000
	minInterval = 100 * time.Millisecond
	if c == nil || c.Resources.Control.Sensor == nil {
		return
	}
	s := c.Resources.Control.Sensor
	if s.Mode != "" {
		mode = s.Mode
	}
	if s.PSISomeStallUs != 0 {
		stallUs = s.PSISomeStallUs
	}
	if s.PSISomeWindowUs != 0 {
		windowUs = s.PSISomeWindowUs
	}
	if s.MinIntervalMs > 0 {
		minInterval = time.Duration(s.MinIntervalMs) * time.Millisecond
	}
	return
}

// NetworkConfig declares the optional host-side network source (TAP / TapFD)
// plus the guest-side IP layer config. The IP/Nexthop/MTU/Hostname/Interface
// fields are propagated to sandbox-init via the launch protocol (and re-applied
// on restore); sandbox-init applies them via netlink. Replaces the kernel's
// `ip=...` cmdline + CONFIG_IP_PNP path.
//
// Source modes (at most one, see ValidateCold):
//   - None: no virtio-net device; all guest-side network fields must be empty.
//   - TAP: a pre-existing host tap; CH opens it by name (dev/e2e, no provider).
//   - TapFD: tapfd handoff (docs/tapfd.md §3) — sandbox-ctl execs a helper that
//     hands over a tap queue fd (with virtio-net header) + metadata.
//
// In TapFD mode the handoff metadata OVERRIDES the static identity attributes:
// meta.mac→MAC, meta.ip→IP (address replaces, configured mask preserved).
// MTU stays a sandbox/node configuration value.
type NetworkConfig struct {
	TAP   string       `yaml:"tap,omitempty"`   // host tap name; CH opens it (attach, don't create)
	TapFD *TapFDConfig `yaml:"tapfd,omitempty"` // tapfd handoff helper (docs/tapfd.md §3)

	MAC       string `yaml:"mac,omitempty"`       // virtio-net MAC (CH --net mac=); empty + TAP mode → CH auto-assigns
	IP        string `yaml:"ip,omitempty"`        // guest CIDR (IPv4/IPv6), e.g. "169.254.1.1/31". Empty → no IP config.
	MTU       int    `yaml:"mtu,omitempty"`       // guest iface MTU; 0 → leave kernel default
	Nexthop   string `yaml:"nexthop,omitempty"`   // default route next-hop; empty → no default route
	Hostname  string `yaml:"hostname,omitempty"`  // guest hostname (sethostname)
	Interface string `yaml:"interface,omitempty"` // guest iface name; source set + empty → "eth0"
}

// TapFDConfig configures tapfd-handoff acquisition (docs/tapfd.md §3).
// Exec mode spawns a provider helper with TAPFD_SOCKET pointing at an
// inherited socketpair end. Socket mode dials a long-running provider and sends
// Request as the OPEN request fields. Exactly one of Exec or Socket is set.
type TapFDConfig struct {
	Exec    []string `yaml:"exec,omitempty"`    // helper argv, e.g. ["connector-ctl","vswitch","open-port","sw0","--port=3"]
	Socket  string   `yaml:"socket,omitempty"`  // persistent provider unix socket
	Request string   `yaml:"request,omitempty"` // provider request fields, e.g. "VSWITCH=sw0 PORT=3"
	Timeout string   `yaml:"timeout,omitempty"` // handoff timeout (Go duration); empty → default
}

// TimeoutDuration parses Timeout; 0 (empty/invalid) lets the handoff apply its
// own default.
func (t *TapFDConfig) TimeoutDuration() time.Duration {
	if t == nil || t.Timeout == "" {
		return 0
	}
	d, err := time.ParseDuration(t.Timeout)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// ResolvedExec returns the helper argv with argv[0] resolved via
// util.LocateBinary — the same lookup rule cloud-hypervisor / mkfs.erofs use:
// a bare name prefers a copy next to the running sandbox-ctl binary, then
// $PATH; a name with a path separator is used as-is. argv[1:] is unchanged.
// Keeps path resolution (a deployment concern) out of tapfd.Acquire, which
// stays a pure exec-the-argv protocol consumer.
func (t *TapFDConfig) ResolvedExec() ([]string, error) {
	if t == nil || len(t.Exec) == 0 {
		return nil, errors.New("tapfd: empty exec argv")
	}
	bin, err := util.LocateBinary(t.Exec[0])
	if err != nil {
		return nil, fmt.Errorf("tapfd helper: %w", err)
	}
	return append([]string{bin}, t.Exec[1:]...), nil
}

// Validate checks the tapfd transport-specific invariants. field is used as
// the error prefix so callers can report their config path.
func (t *TapFDConfig) Validate(field string) error {
	if t == nil {
		return nil
	}
	hasExec := len(t.Exec) > 0
	hasSocket := t.Socket != ""
	if hasExec == hasSocket {
		return fmt.Errorf("%s: exactly one of exec or socket is required", field)
	}
	if hasSocket {
		if !filepath.IsAbs(t.Socket) {
			return fmt.Errorf("%s.socket must be absolute", field)
		}
		if strings.TrimSpace(t.Request) == "" {
			return fmt.Errorf("%s.request is required when socket is set", field)
		}
	} else if strings.TrimSpace(t.Request) != "" {
		return fmt.Errorf("%s.request requires socket mode", field)
	}
	return nil
}

func (n NetworkConfig) hasSource() bool {
	return n.TAP != "" || n.TapFD != nil
}

func (n NetworkConfig) validate() error {
	if n.TAP != "" && n.TapFD != nil {
		return errors.New("network: `tap` and `tapfd` are mutually exclusive")
	}
	if err := n.TapFD.Validate("network.tapfd"); err != nil {
		return err
	}
	if n.hasSource() {
		return nil
	}

	var field string
	switch {
	case n.MAC != "":
		field = "mac"
	case n.IP != "":
		field = "ip"
	case n.MTU != 0:
		field = "mtu"
	case n.Nexthop != "":
		field = "nexthop"
	case n.Hostname != "":
		field = "hostname"
	case n.Interface != "":
		field = "interface"
	default:
		return nil
	}
	return fmt.Errorf("network.%s requires network.tap or network.tapfd", field)
}

// Effective merges the static network attributes with optional handoff
// metadata (metaMAC/metaIP; empty = no override) and returns the
// MAC for CH (--net mac=) plus the guest NetworkSpec to push. The IP override
// replaces the address while preserving the configured mask when the override
// carries none. spec is nil when there is no IP to configure (matches the
// "no IP → skip guest network" cold-start behavior).
func (n NetworkConfig) Effective(metaMAC, metaIP string) (mac string, spec *proto.NetworkSpec) {
	mac = n.MAC
	if metaMAC != "" {
		mac = metaMAC
	}
	ip := n.IP
	if metaIP != "" {
		ip = mergeIPMask(metaIP, n.IP)
	}
	mtu := n.MTU
	if ip == "" {
		return mac, nil
	}
	return mac, &proto.NetworkSpec{
		Interface: n.Interface,
		IPCIDR:    ip,
		Nexthop:   n.Nexthop,
		MTU:       mtu,
		Hostname:  n.Hostname,
	}
}

// mergeIPMask returns metaIP unchanged if it already carries a prefix;
// otherwise it appends the configured CIDR's mask (preserving the operator's
// prefix), falling back to /32 (IPv4) or /128 (IPv6) when none is available.
func mergeIPMask(metaIP, cfgIP string) string {
	if strings.Contains(metaIP, "/") {
		return metaIP
	}
	addr := net.ParseIP(metaIP)
	if addr == nil {
		return metaIP // malformed; let the guest-side parse surface it
	}
	if cfgIP != "" {
		if _, ipnet, err := net.ParseCIDR(cfgIP); err == nil {
			ones, _ := ipnet.Mask.Size()
			return fmt.Sprintf("%s/%d", metaIP, ones)
		}
	}
	if addr.To4() != nil {
		return metaIP + "/32"
	}
	return metaIP + "/128"
}

// BootConfig is everything the kernel needs to start: kernel image,
// extra cmdline, sandbox-runtime image, and the rootfs (base + COW
// overlay). The init parameters and rootfs mount options are auto-
// injected by sandbox-ctl; user only provides extras like console=
// and ip= via cmdline.
type BootConfig struct {
	Kernel  string     `yaml:"kernel"`  // file:// only (cold-start)
	Cmdline string     `yaml:"cmdline"` // user extras; merged with auto-injected base
	Runtime string     `yaml:"runtime"` // file:// sandbox-runtime.bundle path
	Root    RootConfig `yaml:"root"`

	// Disks are additional data disks (beyond the root). Each follows the same
	// rules as Root (single-disk diff or two-disk overlay) plus a Name, and
	// MUST be mounted by exactly one mounts[].type=disk entry (validate enforces
	// 1:1). Device order is root first, then Disks in array order; each disk's
	// ordinal (its index here) is its identity on the guest (the name is config
	// convenience only). At most MaxDataDisks (the count baked into the runtime
	// erofs as /sysdisks/disk-<N>).
	Disks []DiskConfig `yaml:"disks,omitempty"`
}

// MaxDataDisks bounds boot.disks[]. It must equal the number of
// /sysdisks/disk-<N> mountpoint sets baked into sandbox-runtime.bundle (see
// the repo Makefile). The guest also rejects a disk whose dir is absent, so a
// mismatched (older) erofs fails closed rather than silently.
const MaxDataDisks = 8

// DiskConfig is one boot.disks[] entry: a Name plus the same disk fields as
// RootConfig (single-disk diff / two-disk overlay). The name is used only to
// wire mounts[].source → this disk at config time; internally the disk is
// addressed by its ordinal (index in boot.disks[]).
type DiskConfig struct {
	Name       string `yaml:"name"`
	RootConfig `yaml:",inline"`
}

type RootConfig struct {
	// Base is the read-only bottom layer of the root.
	//   - overlay mode (Overlay != nil): the flattened container image
	//     (erofs), mounted read-only as disk0 and used as the overlayfs lower.
	//   - single-disk mode (Overlay == nil): an OPTIONAL ext4 CoW base under
	//     the writable root diff (file:// or manifest://). Omit it when
	//     diff_template / diff already provide the filesystem.
	Base string `yaml:"base"`

	// Overlay, when present, selects two-disk overlay mode: a separate
	// writable ext4 upper layer (disk1) is overlaid on Base. When OMITTED
	// (nil), the sandbox runs in single-disk mode — the root disk (disk0)
	// is itself a writable ext4 CoW built from Base + Diff/DiffTemplate below,
	// mounted directly with no overlayfs and no disk1 (SandboxConfig.SingleDisk).
	Overlay *OverlayConfig `yaml:"overlay"`

	// The fields below apply ONLY in single-disk mode (Overlay == nil); they
	// are the root-disk analogue of overlay.* and are mutually exclusive with
	// Overlay. The root disk is always writable ext4, so a mountable ext4
	// source is required (Diff/DiffTemplate/Base) — see validate.

	// Diff is the local sparse ext4 file collecting writes since boot
	// (file:// only). Empty → auto-default to
	// file://<base-dir>/<sid>.overlay.diff; an auto-defaulted diff is removed
	// when the sandbox ends, an explicitly set one is never removed.
	Diff string `yaml:"diff"`
	// DiffTemplate (file:// only) is the logical initialization source for a
	// freshly-created Diff. Its sparse plaintext view is encoded according to
	// crypto.local, so a single-disk cold boot gets a mountable rw root without
	// mkfs. Ignored if Diff already exists.
	DiffTemplate string `yaml:"diff_template"`
	// DiffSize sizes a freshly-created Diff over Base (no template). Applied
	// only at creation; an existing diff keeps its own size. Empty → 1 GiB.
	DiffSize string `yaml:"diff_size"`
	// BaseFromRefs is the single-disk immutable layer chain below Base. Exported
	// Sandbox E owns this graph; Snapshot S never duplicates it.
	BaseFromRefs []string `yaml:"base_from_refs,omitempty"`
}

// single reports whether this disk runs in single-disk mode (no overlay node):
// one writable ext4 CoW mounted directly. The negation is two-disk overlay mode
// (ro base + writable ext4 upper). Shared by the root and boot.disks[] entries.
func (r *RootConfig) Single() bool { return r.Overlay == nil }

// SingleDisk reports whether the root runs in single-disk mode (no
// boot.root.overlay): the root disk is a writable ext4 CoW mounted directly,
// with no overlayfs and no second disk. The negation is two-disk overlay mode.
func (c *SandboxConfig) SingleDisk() bool { return c.Boot.Root.Single() }

type OverlayConfig struct {
	// Base is an optional read-only ext4 layer (e.g. a snapshot's prior dirty
	// state). May be file:// or manifest://. Empty for fresh sandboxes.
	Base string `yaml:"base"`
	// BaseFromRefs is the explicit top-to-bottom chain below Base. It is used
	// by both cold template starts and snapshot restore; layer composition is
	// never encoded into a manifest URI.
	BaseFromRefs []string `yaml:"base_from_refs,omitempty"`
	// Diff is the local sparse ext4 file collecting writes since boot.
	// file:// only. Optional: empty → auto-default to
	// file://<base-dir>/<sid>.overlay.diff (on disk, see docs/sandbox.md §3.1).
	// An auto-defaulted diff is removed when the sandbox ends; an explicitly
	// set diff is never removed.
	Diff string `yaml:"diff"`
	// DiffTemplate, when set (file:// only), is the logical initialization
	// source for a freshly-created diff. Its sparse plaintext view is encoded
	// according to crypto.local, so cold boot gets a mountable upper layer
	// without mkfs. Ignored if the diff already exists.
	DiffTemplate string `yaml:"diff_template"`
	// DiffSize is the size of a freshly-created blank diff (no template, no
	// base). Applied ONLY at creation; an existing diff keeps its own size.
	// Optional; defaults to 1 GiB.
	DiffSize string `yaml:"diff_size"`
}

// LaunchConfig overrides the container's default launch (which lives
// in the flattened image's appended config.json). For v1 the override
// is mandatory — auto-extraction of image config is future work.
type LaunchConfig struct {
	Exec string            `yaml:"exec"`
	Args []string          `yaml:"args"`
	Env  map[string]string `yaml:"env"`
	// EphemeralEnv overrides Env by key for the current cold-start process. It
	// is host/instance input and is never serialized into a Sandbox artifact.
	EphemeralEnv map[string]string `yaml:"ephemeral_env,omitempty"`
	Workdir      string            `yaml:"workdir"`
	Restart      string            `yaml:"restart"` // never|on-failure|always

	// CgroupControl delegates the application cgroup namespace root to the
	// application. false (default) runs application processes directly in the
	// /app namespace root. true keeps /app empty, enables its subtree
	// controllers, and runs sandbox-init-managed application processes in
	// /app/init so in-guest managers can create controlled child cgroups.
	CgroupControl bool `yaml:"cgroup_control,omitempty"`

	// Placeholder, when true, starts an empty "anchor" app that runs no
	// external program (no exec): the app child sets up its namespaces/cgroup/
	// stdio then waits for stop. exec must be empty (mutually exclusive). The
	// app always restarts (always), so killing it from an `exec` session
	// restarts the anchor instead of rebooting the sandbox. Disk config is
	// unchanged — you still configure boot.root as usual.
	Placeholder bool `yaml:"placeholder,omitempty"`

	// PIDNamespace selects the app's PID namespace: "private" (default) ⇒ the
	// app is PID 1 of its own namespace; "shared" ⇒ the app runs in
	// sandbox-init's namespace, reusing its PID-1 reaper for the app's
	// orphaned descendants. Empty → private.
	PIDNamespace string `yaml:"pid_namespace,omitempty"`

	// Plugin is the companion-process list: long-running sidecars launched
	// alongside the app in the same rootfs + cgroup, each supervised by its
	// own restart policy. A plugin exit never reboots the sandbox.
	Plugin []PluginConfig `yaml:"plugin,omitempty"`

	// User is the run-as identity ("uid:gid" or "name:group"); overrides
	// image config User. Empty → image User else root.
	User string `yaml:"user,omitempty"`
	// StopSignal is the shutdown signal name or number ("SIGTERM"/"15");
	// overrides image config StopSignal. Empty → image StopSignal else SIGTERM.
	StopSignal string `yaml:"stop_signal,omitempty"`
	// StopGracePeriod is the grace before SIGKILL after StopSignal (Go
	// duration). Empty → 10s.
	StopGracePeriod string `yaml:"stop_grace_period,omitempty"`
	// StartTimeout bounds the host's wait for launch_ack (which the guest
	// sends only after applying the whole spec incl. init). Go duration;
	// empty / "0" → wait indefinitely. Host-side only; not sent to guest.
	StartTimeout string `yaml:"start_timeout,omitempty"`
}

// FileConfig declares a file injected into the guest rootfs. Mirrors
// proto.FileSpec; content is inline text.
type FileConfig struct {
	Path     string `yaml:"path"`
	Content  string `yaml:"content,omitempty"`
	Mode     string `yaml:"mode,omitempty"`
	Owner    string `yaml:"owner,omitempty"`
	ReadOnly bool   `yaml:"read_only,omitempty"`
}

// MountConfig declares a guest mount. Type is tmpfs|empty|disk (empty default
// → empty). For type=disk, Source names a boot.disks[] entry (the disk is
// assembled and bound onto Target); each data disk must be mounted exactly once
// (validate enforces 1:1).
type MountConfig struct {
	Target  string `yaml:"target"`
	Type    string `yaml:"type,omitempty"`
	Source  string `yaml:"source,omitempty"`
	Options string `yaml:"options,omitempty"`
}

// InitConfig declares a one-shot init command (run to completion before the
// app, in order; non-zero exit or timeout aborts startup).
type InitConfig struct {
	Exec    string            `yaml:"exec"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	Workdir string            `yaml:"workdir,omitempty"`
	User    string            `yaml:"user,omitempty"`
	Timeout string            `yaml:"timeout,omitempty"` // Go duration; empty/"0" → no timeout
}

// PluginConfig declares a companion ("plugin") process supervised alongside
// the app. Restart is its policy (never|on-failure|always; empty → always).
type PluginConfig struct {
	Exec    string            `yaml:"exec"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
	Workdir string            `yaml:"workdir,omitempty"`
	User    string            `yaml:"user,omitempty"`
	Restart string            `yaml:"restart,omitempty"` // never|on-failure|always; empty → always
}

// Load reads one sandbox.yaml and applies defaults.
func Load(path string) (*SandboxConfig, error) {
	return LoadMerged([]string{path})
}

// LoadMerged reads one or more sandbox.yaml files and deep-merges them
// front-to-back (later files override earlier), then applies defaults once.
// Merge follows yaml.v3 sequential-decode semantics into a single value:
// scalars and lists are replaced by the last file that sets them; nested
// maps and structs merge recursively (a later file's `resources.capacity.cpu`
// overrides without clobbering sibling keys). An absent key keeps the prior
// file's value. This is the same loader `sandbox-ctl run --config a:b:c` uses.
func LoadMerged(paths []string) (*SandboxConfig, error) {
	if len(paths) == 0 {
		return nil, errors.New("sandbox: no config files given")
	}
	var cfg SandboxConfig
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("sandbox: read %s: %w", p, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("sandbox: parse %s: %w", p, err)
		}
	}
	cfg.ApplyDefaults()
	return &cfg, nil
}

// LoadConfigBytes parses a single SANDBOX_CONFIG YAML document held in memory
// (e.g. delivered over the config-socket) and applies defaults — the in-memory
// equivalent of LoadMerged for one document, with nothing read from disk.
func LoadConfigBytes(data []byte) (*SandboxConfig, error) {
	var cfg SandboxConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("sandbox: parse config bytes: %w", err)
	}
	cfg.ApplyDefaults()
	return &cfg, nil
}

func (c *SandboxConfig) ApplyDefaults() {
	if c.Resources.Capacity.CPU == 0 {
		c.Resources.Capacity.CPU = 1
	}
	if c.Resources.Allocatable.CPU == 0 {
		c.Resources.Allocatable.CPU = float64(c.Resources.Capacity.CPU)
	}
	if c.Resources.Allocatable.Memory == "" {
		c.Resources.Allocatable.Memory = c.Resources.Capacity.Memory
	}
	if c.Launch.Restart == "" {
		c.Launch.Restart = "never"
	}
	if c.Launch.Workdir == "" {
		c.Launch.Workdir = "/"
	}
	if c.Network.hasSource() && c.Network.Interface == "" {
		c.Network.Interface = "eth0"
	}
	for i := range c.Mounts {
		if c.Mounts[i].Type == "" {
			c.Mounts[i].Type = "empty"
		}
	}
}

// StopGraceSeconds parses launch.stop_grace_period to whole seconds,
// defaulting to 10 when unset/invalid. Used to fill LaunchSpec.StopGraceSec.
func (c *SandboxConfig) StopGraceSeconds() int {
	if c.Launch.StopGracePeriod == "" {
		return 10
	}
	d, err := time.ParseDuration(c.Launch.StopGracePeriod)
	if err != nil || d <= 0 {
		return 10
	}
	return int(d.Seconds())
}

// StartTimeoutDuration parses launch.start_timeout. Empty / "0" / invalid
// → 0, meaning the host waits for launch_ack indefinitely.
func (c *SandboxConfig) StartTimeoutDuration() time.Duration {
	if c.Launch.StartTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(c.Launch.StartTimeout)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// TimeoutsConfig tunes host-side protocol/lifecycle deadlines, chiefly on the
// restore path. Each is a Go duration string; empty / "0" / invalid → 0,
// meaning NO FORCED TIMEOUT: the host waits as long as the
// guest, CH, or lazy page-in needs, while dial/connect probes stay bounded.
// This default eases deployment in slow or degraded environments (a stalled
// remote/cache or a paused debugger never aborts a restore); set positive
// values (examples/timeouts-production.yaml) to fail fast in production.
type TimeoutsConfig struct {
	Restore  string `yaml:"restore,omitempty"`   // wait for guest restore_ack after /vm.resume
	CHApi    string `yaml:"ch_api,omitempty"`    // CH HTTP API response (e.g. /vm.resume); dial stays bounded
	APIReady string `yaml:"api_ready,omitempty"` // wait for CH API socket to accept after spawn
	VAReport string `yaml:"va_report,omitempty"` // CH→host uffd-fd handoff handshake
	// Ping is the host→guest ping round-trip deadline. Default no-forced means
	// a guest whose vCPU is briefly blocked on a slow page-in still answers
	// instead of the ping spuriously timing out. NOTE: if you enable
	// --ping-fatal-threshold, set this to a bounded value (the production
	// profile uses 200ms) — otherwise a wedged-but-connected guest is never
	// detected (the ping waits rather than failing).
	Ping string `yaml:"ping,omitempty"`
	// AppNotify is the host read deadline for one guest→host launch-port
	// message (hello / launch_ack first read / app_started / app_exited /
	// mem_report). Default no-forced so a slow guest (faulting in pages)
	// doesn't get its mem_report connection dropped mid-restore.
	AppNotify string `yaml:"app_notify,omitempty"`
}

// NoForcedTimeout is the effective-infinity used where an underlying call needs
// a finite deadline (DialRaw) but the operator asked for no forced timeout
// (timeouts.* = 0). Cancellation still flows via ctx / CH teardown.
const NoForcedTimeout = 365 * 24 * time.Hour

// parseTimeout parses a TimeoutsConfig field: empty / "0" / negative / invalid
// → 0, meaning "no forced timeout" to the consumer.
func parseTimeout(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// DefaultCHApiDeadline is the response deadline used for every cloud-hypervisor
// API call when timeouts.ch_api is unset. Unlike the guest/remote-coupled
// timeouts (which default to no-forced), CH API calls are local management ops
// over the UDS — fast and immune to remote/cache slowness — so a generous fixed
// bound is the right default: it catches a wedged CH without ever being a hot
// limit. Set timeouts.ch_api to override, or to "0" for no forced timeout.
const DefaultCHApiDeadline = 60 * time.Second

// RestoreDeadline / APIReadyDeadline / VAReportDeadline / PingDeadline /
// AppNotifyDeadline resolve the corresponding timeouts.* field; 0 = no forced
// timeout. CHApiDeadline is the exception: unset → DefaultCHApiDeadline (see above).
func (c *SandboxConfig) RestoreDeadline() time.Duration { return parseTimeout(c.Timeouts.Restore) }
func (c *SandboxConfig) CHApiDeadline() time.Duration {
	if c.Timeouts.CHApi == "" {
		return DefaultCHApiDeadline
	}
	return parseTimeout(c.Timeouts.CHApi) // explicit "0"/"off"/invalid → 0 (no forced)
}
func (c *SandboxConfig) APIReadyDeadline() time.Duration  { return parseTimeout(c.Timeouts.APIReady) }
func (c *SandboxConfig) VAReportDeadline() time.Duration  { return parseTimeout(c.Timeouts.VAReport) }
func (c *SandboxConfig) PingDeadline() time.Duration      { return parseTimeout(c.Timeouts.Ping) }
func (c *SandboxConfig) AppNotifyDeadline() time.Duration { return parseTimeout(c.Timeouts.AppNotify) }

// validate rejects malformed (non-empty, unparseable) timeouts.* durations.
func (t TimeoutsConfig) validate() error {
	for _, f := range []struct{ name, val string }{
		{"timeouts.restore", t.Restore},
		{"timeouts.ch_api", t.CHApi},
		{"timeouts.api_ready", t.APIReady},
		{"timeouts.va_report", t.VAReport},
		{"timeouts.ping", t.Ping},
		{"timeouts.app_notify", t.AppNotify},
	} {
		if f.val == "" {
			continue
		}
		if _, err := time.ParseDuration(f.val); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
	}
	return nil
}

// CapacityMemoryBytes returns the parsed capacity memory in bytes.
func (c *SandboxConfig) CapacityMemoryBytes() (uint64, error) {
	if c.Resources.Capacity.Memory == "" {
		return 0, errors.New("resources.capacity.memory is required")
	}
	return util.ParseSize(c.Resources.Capacity.Memory)
}

// AllocatableMemoryBytes returns the parsed allocatable memory in bytes.
func (c *SandboxConfig) AllocatableMemoryBytes() (uint64, error) {
	if c.Resources.Allocatable.Memory == "" {
		return c.CapacityMemoryBytes()
	}
	return util.ParseSize(c.Resources.Allocatable.Memory)
}

// DeflateOnOOM returns whether CH --balloon should carry deflate_on_oom=on.
// Default is true; only false if user explicitly set it false.
func (c *SandboxConfig) DeflateOnOOM() bool {
	if c.Resources.Allocatable.DeflateOnOOM == nil {
		return true
	}
	return *c.Resources.Allocatable.DeflateOnOOM
}

// CgroupPath returns the configured cgroup path. Empty means no-cgroup mode
// (no cgroup operations).
func (c *SandboxConfig) CgroupPath() string {
	return c.Resources.Control.CgroupPath
}

// OverheadMemoryBytes returns the cgroup memory.max overhead. Only
// meaningful when CgroupPath is set; default is 32 MiB for Cloud Hypervisor's
// host-side allocations and avoids cgroup OOM under normal operation.
// Returns 0 when CgroupPath is empty (caller should not use).
func (c *SandboxConfig) OverheadMemoryBytes() (uint64, error) {
	if c.Resources.Control.CgroupPath == "" {
		return 0, nil
	}
	if c.Resources.Overhead == nil {
		return 32 << 20, nil
	}
	return util.ParseSize(c.Resources.Overhead.Memory)
}

// WatermarkHighRatio returns the fixed-point memory.high ratio. Byte
// arithmetic never passes through float64.
func (c *SandboxConfig) WatermarkHighRatio() (uint64, error) {
	if c.Resources.WatermarkHigh == nil {
		return resource.DefaultWatermarkHighRatio, nil
	}
	return resource.RatioFromFloat(c.Resources.WatermarkHigh.Ratio)
}

// StartupBytes returns configured startup headroom. The default is full
// Capacity. Restore admission ignores this value for the initial Budget and
// reserves BudgetAtSnapshot instead; callers still read it to materialize the
// node reservation contract.
func (c *SandboxConfig) StartupBytes() (uint64, error) {
	if c.Resources.Startup == nil {
		return c.CapacityMemoryBytes()
	}
	return util.ParseSize(c.Resources.Startup.Memory)
}

// CPUWeight maps allocatable.cpu to a cgroup v2 cpu.weight value in
// [1, 10000]. Mapping is allocatable.cpu * 100 (so 1 core = 100 = kernel
// default). Used only when CgroupPath is set.
func (c *SandboxConfig) CPUWeight() uint64 {
	w := int64(c.Resources.Allocatable.CPU * 100)
	if w < 1 {
		w = 1
	}
	if w > 10000 {
		w = 10000
	}
	return uint64(w)
}

// DiffSizeBytes returns the size used when CREATING a fresh blank diff
// (no template, no base). It never applies to an existing diff — that keeps
// its own on-disk size (truncating it would corrupt its filesystem). If
// unset, defaults to 1 GiB.
// diffSizeBytes returns the configured diff size for this disk — single-disk
// diff_size or overlay.diff_size — defaulting to 1 GiB. field is the config
// path prefix for error messages (e.g. "boot.root", "boot.disks[0]").
func (r *RootConfig) DiffSizeBytes(field string) (int64, error) {
	raw, f := r.DiffSize, field+".diff_size"
	if r.Overlay != nil {
		raw, f = r.Overlay.DiffSize, field+".overlay.diff_size"
	}
	if raw == "" {
		return 1 << 30, nil
	}
	v, err := util.ParseSize(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", f, err)
	}
	return int64(v), nil
}

func (c *SandboxConfig) DiffSizeBytes() (int64, error) {
	return c.Boot.Root.DiffSizeBytes("boot.root")
}

// ValidateCold checks invariants required for the cold-start path.
//
// Resource-control gating rules (see docs/sandbox.md §13):
//   - Controller requires CgroupPath
//   - Overhead / WatermarkHigh require CgroupPath
//   - allocatable.cpu == capacity.cpu when CgroupPath is empty (no
//     fractional CPU without cgroup)
//   - CgroupPath must exist on the host filesystem unless a validated inherited
//     CgroupFD is authoritative
//   - Allocatable.memory and Startup.memory are independently in (0, Capacity]
//   - WatermarkHigh.ratio is in (0, 1)
func (c *SandboxConfig) ValidateCold() error {
	if err := c.Restore.validate(); err != nil {
		return err
	}
	if c.Resources.Capacity.CPU <= 0 {
		return errors.New("resources.capacity.cpu must be > 0")
	}
	capMem, err := c.CapacityMemoryBytes()
	if err != nil {
		return fmt.Errorf("resources.capacity.memory: %w", err)
	}
	allocMem, err := c.AllocatableMemoryBytes()
	if err != nil {
		return fmt.Errorf("resources.allocatable.memory: %w", err)
	}
	if allocMem > capMem {
		return errors.New("resources.allocatable.memory must be ≤ capacity.memory")
	}
	if allocMem == 0 {
		return errors.New("resources.allocatable.memory must be > 0")
	}
	if math.IsNaN(c.Resources.Allocatable.CPU) || math.IsInf(c.Resources.Allocatable.CPU, 0) {
		return errors.New("resources.allocatable.cpu must be finite")
	}
	if c.Resources.Allocatable.CPU > float64(c.Resources.Capacity.CPU) {
		return errors.New("resources.allocatable.cpu must be ≤ capacity.cpu")
	}
	if c.Resources.Allocatable.CPU <= 0 {
		return errors.New("resources.allocatable.cpu must be > 0")
	}

	// Resource-control gating
	cgroupSet := c.Resources.Control.CgroupPath != ""
	controllerSet := c.Resources.Control.Controller != ""

	if controllerSet && !cgroupSet {
		return errors.New("resources.control.controller requires resources.control.cgroup_path")
	}
	if err := validateControllerSocket(c.Resources.Control.Controller); err != nil {
		return err
	}
	if !cgroupSet && c.Resources.Overhead != nil {
		return errors.New("resources.overhead requires resources.control.cgroup_path")
	}
	if !cgroupSet && c.Resources.WatermarkHigh != nil {
		return errors.New("resources.watermark_high requires resources.control.cgroup_path")
	}
	if !cgroupSet && c.Resources.Allocatable.CPU != float64(c.Resources.Capacity.CPU) {
		return fmt.Errorf("resources.allocatable.cpu must equal capacity.cpu (%d) when cgroup_path is not set; got %g (fractional cpu requires cgroup_path)",
			c.Resources.Capacity.CPU, c.Resources.Allocatable.CPU)
	}

	// Cgroup target. An inherited descriptor is the placement authority; its
	// resolved path is retained only as the cross-process controller identity.
	if cgroupSet {
		if !filepath.IsAbs(c.Resources.Control.CgroupPath) {
			return fmt.Errorf("resources.control.cgroup_path must be absolute: %q", c.Resources.Control.CgroupPath)
		}
		if fd := c.Resources.Control.CgroupFD; fd != 0 {
			if fd < 3 {
				return fmt.Errorf("resources.control.cgroup fd must be >= 3, got %d", fd)
			}
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil {
				return fmt.Errorf("resources.control.cgroup fd %d: %w", fd, err)
			}
			if st.Mode&unix.S_IFMT != unix.S_IFDIR {
				return fmt.Errorf("resources.control.cgroup fd %d is not a directory", fd)
			}
			var fs unix.Statfs_t
			if err := unix.Fstatfs(fd, &fs); err != nil {
				return fmt.Errorf("resources.control.cgroup fd %d statfs: %w", fd, err)
			}
			if fs.Type != unix.CGROUP2_SUPER_MAGIC {
				return fmt.Errorf("resources.control.cgroup fd %d is not on cgroup v2", fd)
			}
		} else {
			st, err := os.Stat(c.Resources.Control.CgroupPath)
			if err != nil {
				return fmt.Errorf("resources.control.cgroup_path %q does not exist: %w", c.Resources.Control.CgroupPath, err)
			}
			if !st.IsDir() {
				return fmt.Errorf("resources.control.cgroup_path %q is not a directory", c.Resources.Control.CgroupPath)
			}
		}
	}

	// Overhead value
	if c.Resources.Overhead != nil {
		if _, err := util.ParseSize(c.Resources.Overhead.Memory); err != nil {
			return fmt.Errorf("resources.overhead.memory: %w", err)
		}
	}

	// WatermarkHigh ratio ∈ (0, 1).
	if c.Resources.WatermarkHigh != nil {
		if _, err := c.WatermarkHighRatio(); err != nil {
			return fmt.Errorf("resources.watermark_high.ratio: %w", err)
		}
	}

	// Sensor mode validation
	if c.Resources.Control.Sensor != nil {
		s := c.Resources.Control.Sensor
		switch s.Mode {
		case "", "psi", "events_poll", "none":
		default:
			return fmt.Errorf("resources.control.sensor.mode must be one of psi|events_poll|none, got %q", s.Mode)
		}
		if s.PSISomeStallUs > 0 && s.PSISomeWindowUs == 0 {
			return errors.New("resources.control.sensor.psi_some_window_us required when psi_some_stall_us set")
		}
		if s.PSISomeWindowUs > 0 && s.PSISomeStallUs > s.PSISomeWindowUs {
			return errors.New("resources.control.sensor.psi_some_stall_us must be ≤ psi_some_window_us")
		}
	}

	// Startup headroom is independent of settled headroom.
	if c.Resources.Startup != nil {
		sb, err := util.ParseSize(c.Resources.Startup.Memory)
		if err != nil {
			return fmt.Errorf("resources.startup.memory: %w", err)
		}
		if sb == 0 {
			return errors.New("resources.startup.memory must be > 0")
		}
		if sb > capMem {
			return fmt.Errorf("resources.startup.memory (%d) must be ≤ capacity.memory (%d)", sb, capMem)
		}
	}

	if c.Boot.Kernel == "" {
		return errors.New("boot.kernel is required for cold start")
	}
	if err := requireFileAbs("boot.kernel", c.Boot.Kernel); err != nil {
		return err
	}
	if c.Boot.Runtime == "" {
		return errors.New("boot.runtime is required")
	}
	if err := requireRuntimeFileAbs("boot.runtime", c.Boot.Runtime); err != nil {
		return err
	}

	if err := c.validateRoot(true); err != nil {
		return err
	}
	if err := c.validateDisks(true); err != nil {
		return err
	}

	if err := c.Network.validate(); err != nil {
		return err
	}

	// mounts: target absolute; type ∈ {tmpfs, empty, disk}; nfs deferred. The
	// disk↔boot.disks[] 1:1 correspondence is checked in validateDisks.
	for i, m := range c.Mounts {
		if !filepath.IsAbs(m.Target) {
			return fmt.Errorf("mounts[%d].target must be absolute (got %q)", i, m.Target)
		}
		switch m.Type {
		case "tmpfs", "empty", "disk":
		case "nfs":
			return fmt.Errorf("mounts[%d].type %q not yet implemented", i, m.Type)
		default:
			return fmt.Errorf("mounts[%d].type %q unknown (want tmpfs|empty|disk)", i, m.Type)
		}
	}
	// files: path absolute; mode valid octal if set; duplicates inside either
	// list are ambiguous. Cross-list duplicates are intentional overrides.
	if err := validateFiles("files", c.Files); err != nil {
		return err
	}
	if err := validateFiles("ephemeral_files", c.EphemeralFiles); err != nil {
		return err
	}
	// init: exec required; timeout parseable.
	for i, it := range c.Init {
		if it.Exec == "" {
			return fmt.Errorf("init[%d].exec is required", i)
		}
		if it.Timeout != "" {
			if _, err := time.ParseDuration(it.Timeout); err != nil {
				return fmt.Errorf("init[%d].timeout: %w", i, err)
			}
		}
	}
	// launch.pid_namespace ∈ {private, shared}.
	switch c.Launch.PIDNamespace {
	case "", "private", "shared":
	default:
		return fmt.Errorf("launch.pid_namespace %q invalid (want private|shared)", c.Launch.PIDNamespace)
	}
	// launch.restart + plugin[] restart policies.
	if !validRestart(c.Launch.Restart) {
		return fmt.Errorf("launch.restart %q invalid (want never|on-failure|always)", c.Launch.Restart)
	}
	// placeholder runs no external program — exec is mutually exclusive.
	if c.Launch.Placeholder && c.Launch.Exec != "" {
		return errors.New("launch.placeholder and launch.exec are mutually exclusive")
	}
	for i, p := range c.Launch.Plugin {
		if p.Exec == "" {
			return fmt.Errorf("launch.plugin[%d].exec is required", i)
		}
		if !validRestart(p.Restart) {
			return fmt.Errorf("launch.plugin[%d].restart %q invalid (want never|on-failure|always)", i, p.Restart)
		}
	}
	// launch.stop_signal parseable; durations parseable.
	if c.Launch.StopSignal != "" {
		if _, err := ParseStopSignal(c.Launch.StopSignal); err != nil {
			return fmt.Errorf("launch.stop_signal: %w", err)
		}
	}
	if c.Launch.StopGracePeriod != "" {
		if _, err := time.ParseDuration(c.Launch.StopGracePeriod); err != nil {
			return fmt.Errorf("launch.stop_grace_period: %w", err)
		}
	}
	if c.Launch.StartTimeout != "" {
		if _, err := time.ParseDuration(c.Launch.StartTimeout); err != nil {
			return fmt.Errorf("launch.start_timeout: %w", err)
		}
	}
	if err := c.Timeouts.validate(); err != nil {
		return err
	}

	// launch.exec is no longer required: if the rootfs erofs has an
	// appended config.json with Entrypoint or Cmd, those are used as
	// defaults. MergeLaunch fails late with a clear error if neither
	// the override nor the image provides an executable.

	return nil
}

func validateFiles(field string, files []FileConfig) error {
	seen := make(map[string]int, len(files))
	for i, f := range files {
		if !filepath.IsAbs(f.Path) {
			return fmt.Errorf("%s[%d].path must be absolute (got %q)", field, i, f.Path)
		}
		if previous, duplicate := seen[f.Path]; duplicate {
			return fmt.Errorf("%s[%d].path %q duplicates %s[%d]", field, i, f.Path, field, previous)
		}
		seen[f.Path] = i
		if f.Mode != "" {
			if _, err := strconv.ParseUint(f.Mode, 8, 32); err != nil {
				return fmt.Errorf("%s[%d].mode %q invalid octal", field, i, f.Mode)
			}
		}
	}
	return nil
}

// ValidateRestoreHostConfig checks the host-only bindings and policies a
// restore document can validate before Snapshot S is opened. S supplies only
// memory provenance; its sandbox_ref selects E, which owns capacity, launch and
// the immutable disk graph. ApplyRestoreRules performs the ownership checks and
// applies an explicitly supplied target-node allocatable policy.
func (c *SandboxConfig) ValidateRestoreHostConfig() error {
	if err := c.Restore.validate(); err != nil {
		return err
	}
	cgroupSet := c.Resources.Control.CgroupPath != ""
	if path := c.Resources.Control.CgroupPath; path != "" && !filepath.IsAbs(path) {
		return fmt.Errorf("resources.control.cgroup_path must be absolute: %q", path)
	}
	if c.Resources.Control.Controller != "" && !cgroupSet {
		return errors.New("resources.control.controller requires resources.control.cgroup_path")
	}
	if err := validateControllerSocket(c.Resources.Control.Controller); err != nil {
		return err
	}
	if !cgroupSet && c.Resources.Overhead != nil {
		return errors.New("resources.overhead requires resources.control.cgroup_path")
	}
	if !cgroupSet && c.Resources.WatermarkHigh != nil {
		return errors.New("resources.watermark_high requires resources.control.cgroup_path")
	}
	if c.Resources.Overhead != nil {
		if _, err := util.ParseSize(c.Resources.Overhead.Memory); err != nil {
			return fmt.Errorf("resources.overhead.memory: %w", err)
		}
	}
	if c.Resources.WatermarkHigh != nil {
		if _, err := c.WatermarkHighRatio(); err != nil {
			return fmt.Errorf("resources.watermark_high.ratio: %w", err)
		}
	}
	if err := c.Network.validate(); err != nil {
		return err
	}
	// Capacity is optional in the host yaml. When present it must be well-formed
	// and ApplyRestoreRules requires it to match Sandbox E.
	var capMem uint64
	if c.Resources.Capacity.CPU != 0 || c.Resources.Capacity.Memory != "" {
		if c.Resources.Capacity.CPU <= 0 {
			return errors.New("resources.capacity.cpu must be > 0")
		}
		var err error
		capMem, err = c.CapacityMemoryBytes()
		if err != nil {
			return fmt.Errorf("resources.capacity.memory: %w", err)
		}
	}
	// Allocatable and Startup are host policies. ApplyRestoreRules validates an
	// explicit Allocatable against the referenced Sandbox capacity and applies it
	// to this runtime without rewriting portable C0. Startup affects only the
	// node reservation policy.
	if c.Resources.Allocatable.Memory != "" {
		allocMem, err := util.ParseSize(c.Resources.Allocatable.Memory)
		if err != nil {
			return fmt.Errorf("resources.allocatable.memory: %w", err)
		}
		if allocMem == 0 {
			return errors.New("resources.allocatable.memory must be > 0")
		}
		if capMem != 0 && allocMem > capMem {
			return errors.New("resources.allocatable.memory must be ≤ capacity.memory")
		}
	}
	if c.Resources.Startup != nil {
		startup, err := util.ParseSize(c.Resources.Startup.Memory)
		if err != nil {
			return fmt.Errorf("resources.startup.memory: %w", err)
		}
		if startup == 0 {
			return errors.New("resources.startup.memory must be > 0")
		}
		if capMem != 0 && startup > capMem {
			return fmt.Errorf("resources.startup.memory (%d) must be ≤ capacity.memory (%d)", startup, capMem)
		}
	}
	// Reference formats (when provided).
	if c.Boot.Runtime != "" {
		if err := requireRuntimeFileAbs("boot.runtime", c.Boot.Runtime); err != nil {
			return err
		}
	}
	if err := c.validateRoot(false); err != nil {
		return err
	}
	if err := c.validateDisks(false); err != nil {
		return err
	}
	if err := c.Timeouts.validate(); err != nil {
		return err
	}
	return nil
}

// ValidateRestoreHostConfigWithPresence rejects cold-start-only host input and
// validates the remaining restore policy before a caller opens Snapshot S or
// its referenced Sandbox E. Presence is required because ordinary config
// loading applies inert cold defaults that are indistinguishable by value
// alone; programmatic callers without YAML presence still get the conservative
// non-empty-value checks in rejectRestoreColdOnly.
func (c *SandboxConfig) ValidateRestoreHostConfigWithPresence(presence FieldPresence) error {
	if err := rejectRestoreColdOnly(c, presence); err != nil {
		return err
	}
	return c.ValidateRestoreHostConfig()
}

func validateControllerSocket(path string) error {
	if path == "" {
		return nil
	}
	if strings.HasPrefix(path, "@") || strings.IndexByte(path, 0) >= 0 {
		return errors.New("resources.control.controller must be a filesystem Unix socket path; abstract addresses cannot back lifecycle inventory")
	}
	return nil
}

// validateRoot checks boot.root for both disk modes. cold=true enforces the
// cold-start requirements (a mountable source, and — in single-disk mode —
// an explicit launch.exec since there is no erofs image config); cold=false
// (restore) is lenient: immutable base/layer sources come from Sandbox E.
//
//   - overlay mode (Overlay != nil): Base is the erofs image (required cold),
//     the writable upper is overlay.{base,diff,diff_template}.
//   - single-disk mode (Overlay == nil): the root disk is a writable ext4 CoW
//     of Base (optional) + Diff/DiffTemplate; root.* are mutually exclusive
//     with overlay.
//
// validateDiskSource validates one disk's source config — the root or a
// boot.disks[] entry — sharing all the overlay-vs-single-disk rules. prefix is
// the config path used in error messages ("boot.root", "boot.disks[0]"). The
// root-only launch.exec requirement stays in validateRoot.
func validateDiskSource(r *RootConfig, prefix string, cold bool) error {
	if r.Overlay != nil {
		// single-disk fields are mutually exclusive with overlay.
		if r.Diff != "" || r.DiffTemplate != "" || r.DiffSize != "" || len(r.BaseFromRefs) > 0 {
			return fmt.Errorf("%s.{diff,diff_template,diff_size,base_from_refs} are single-disk only — remove them, or remove %s.overlay to select single-disk mode", prefix, prefix)
		}
		if cold && r.Base == "" {
			return fmt.Errorf("%s.base is required (overlay mode)", prefix)
		}
		if r.Base != "" {
			if err := requireAbsIfFile(prefix+".base", r.Base); err != nil {
				return err
			}
		}
		ov := r.Overlay
		if ov.Base != "" {
			if err := requireAbsIfFile(prefix+".overlay.base", ov.Base); err != nil {
				return err
			}
		}
		if len(ov.BaseFromRefs) > 0 && ov.Base == "" {
			return fmt.Errorf("%s.overlay.base is required when base_from_refs is set", prefix)
		}
		for i, ref := range ov.BaseFromRefs {
			if err := requireAbsIfFile(fmt.Sprintf("%s.overlay.base_from_refs[%d]", prefix, i), ref); err != nil {
				return err
			}
		}
		if ov.Diff != "" {
			if err := requireFileAbs(prefix+".overlay.diff", ov.Diff); err != nil {
				return err
			}
		}
		if ov.DiffTemplate != "" {
			if err := requireFileAbs(prefix+".overlay.diff_template", ov.DiffTemplate); err != nil {
				return err
			}
		}
		if _, err := r.DiffSizeBytes(prefix); err != nil {
			return err
		}
		// Cold boot needs a mountable ext4 source for the upper layer — a
		// fresh blank diff is not a valid filesystem.
		if cold && ov.DiffTemplate == "" && ov.Base == "" && ov.Diff == "" {
			return fmt.Errorf("cold boot needs an ext4 source for the overlay upper: set %s.overlay.diff_template, %s.overlay.base, or an explicit %s.overlay.diff", prefix, prefix, prefix)
		}
		return nil
	}

	// single-disk mode (overlay omitted).
	if r.Diff != "" {
		if err := requireFileAbs(prefix+".diff", r.Diff); err != nil {
			return err
		}
	}
	if r.DiffTemplate != "" {
		if err := requireFileAbs(prefix+".diff_template", r.DiffTemplate); err != nil {
			return err
		}
	}
	if r.Base != "" {
		if err := requireAbsIfFile(prefix+".base", r.Base); err != nil {
			return err
		}
	}
	if len(r.BaseFromRefs) > 0 && r.Base == "" {
		return fmt.Errorf("%s.base is required when base_from_refs is set", prefix)
	}
	for i, ref := range r.BaseFromRefs {
		if err := requireAbsIfFile(fmt.Sprintf("%s.base_from_refs[%d]", prefix, i), ref); err != nil {
			return err
		}
	}
	if _, err := r.DiffSizeBytes(prefix); err != nil {
		return err
	}
	// The single disk is always writable ext4; with no overlayfs lower and no
	// guest-side mkfs it needs a mountable ext4 source.
	if cold && r.DiffTemplate == "" && r.Base == "" && r.Diff == "" {
		return fmt.Errorf("%s single-disk cold boot needs an ext4 source: set %s.diff_template, %s.base, or an explicit %s.diff", prefix, prefix, prefix, prefix)
	}
	return nil
}

func (c *SandboxConfig) validateRoot(cold bool) error {
	if err := validateDiskSource(&c.Boot.Root, "boot.root", cold); err != nil {
		return err
	}
	// Single-disk root has no erofs image ⇒ no appended image config; the
	// launch must be explicit (unless a no-exec placeholder). Root-only —
	// data disks carry no launch.
	if cold && c.Boot.Root.Single() && c.Launch.Exec == "" && !c.Launch.Placeholder {
		return errors.New("single-disk mode has no image config (boot.root.overlay omitted): set launch.exec (or launch.placeholder: true)")
	}
	return nil
}

// validateDisks validates boot.disks[] and the 1:1 correspondence with
// mounts[].type=disk: every data disk is mounted exactly once, and every
// type=disk mount names an existing disk. cold gates the source checks.
func (c *SandboxConfig) validateDisks(cold bool) error {
	if len(c.Boot.Disks) > MaxDataDisks {
		return fmt.Errorf("boot.disks: at most %d data disks (got %d)", MaxDataDisks, len(c.Boot.Disks))
	}
	names := make(map[string]int, len(c.Boot.Disks))
	for i := range c.Boot.Disks {
		d := &c.Boot.Disks[i]
		if d.Name == "" {
			return fmt.Errorf("boot.disks[%d].name is required", i)
		}
		if _, dup := names[d.Name]; dup {
			return fmt.Errorf("boot.disks[%d].name %q duplicated", i, d.Name)
		}
		names[d.Name] = i
		if err := validateDiskSource(&d.RootConfig, fmt.Sprintf("boot.disks[%d]", i), cold); err != nil {
			return err
		}
	}
	// 1:1 with mounts[].type=disk — COLD only. On restore the guest resumes
	// with the data disks already mounted (in the memory image); the restore
	// host yaml lists boot.disks[] for device order + erofs bases but does not
	// re-mount, so it need not carry the mounts[].
	if !cold {
		return nil
	}
	mounted := make(map[string]bool, len(names))
	for i := range c.Mounts {
		m := &c.Mounts[i]
		if m.Type != "disk" {
			continue
		}
		if m.Target == "" {
			return fmt.Errorf("mounts[%d] (type=disk): target is required", i)
		}
		if m.Source == "" {
			return fmt.Errorf("mounts[%d] (type=disk): source (a boot.disks[].name) is required", i)
		}
		if _, ok := names[m.Source]; !ok {
			return fmt.Errorf("mounts[%d] (type=disk): source %q names no boot.disks[] entry", i, m.Source)
		}
		if mounted[m.Source] {
			return fmt.Errorf("mounts[%d] (type=disk): disk %q already mounted (each disk mounts once)", i, m.Source)
		}
		mounted[m.Source] = true
	}
	for name := range names {
		if !mounted[name] {
			return fmt.Errorf("boot.disks[%d] (%q) is defined but not mounted — add a mounts[] entry {type: disk, source: %q}", names[name], name, name)
		}
	}
	return nil
}

// validRestart reports whether s is a valid restart policy (empty = default).
func validRestart(s string) bool {
	switch s {
	case "", "never", "on-failure", "always":
		return true
	}
	return false
}

// requireFileAbs enforces that uri starts with file:// and the path is
// absolute. Used for fields that only accept file:// (kernel, runtime,
// overlay.diff).
func requireFileAbs(field, uri string) error {
	if !strings.HasPrefix(uri, "file://") {
		return fmt.Errorf("%s must be file://", field)
	}
	p := strings.TrimPrefix(uri, "file://")
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%s file:// must be absolute (got %q)", field, uri)
	}
	return nil
}

func requireRuntimeFileAbs(field, uri string) error {
	ref, err := manifest.ParseRef(uri)
	if err != nil || ref.Scheme != manifest.RefSchemeFile {
		return fmt.Errorf("%s must be file://", field)
	}
	if ref.Location != "" {
		return fmt.Errorf("%s does not support named ref locations", field)
	}
	if !filepath.IsAbs(ref.Path) {
		return fmt.Errorf("%s file:// must be absolute (got %q)", field, uri)
	}
	return nil
}

// requireAbsIfFile permits manifest:// and otherwise enforces an
// absolute file:// path. Used for fields that accept both schemes
// (root.base, overlay.base).
func requireAbsIfFile(field, uri string) error {
	ref, err := manifest.ParseRef(uri)
	if err != nil {
		return fmt.Errorf("%s must be file:// or manifest:// (got %q)", field, uri)
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		return nil
	}
	if ref.Location != "" {
		return nil
	}
	if !filepath.IsAbs(ref.Path) {
		return fmt.Errorf("%s file:// must be absolute or located (got %q)", field, uri)
	}
	return nil
}

// SchemeAndPath splits a URI like "file:///path" or "manifest://hexkey"
// into ("file", "/path") or ("manifest", "hexkey").
func SchemeAndPath(uri string) (scheme, value string, ok bool) {
	const filePrefix = "file://"
	const manifestPrefix = "manifest://"
	switch {
	case strings.HasPrefix(uri, filePrefix):
		return "file", strings.TrimPrefix(uri, filePrefix), true
	case strings.HasPrefix(uri, manifestPrefix):
		return "manifest", strings.TrimPrefix(uri, manifestPrefix), true
	}
	return "", "", false
}

// EffectiveFiles merges persistent declarations with instance-only overrides.
// Persistent order is retained; an ephemeral declaration with the same path
// replaces it in place, while a new path is appended in ephemeral order.
func (c *SandboxConfig) EffectiveFiles() []FileConfig {
	if c == nil || (len(c.Files) == 0 && len(c.EphemeralFiles) == 0) {
		return nil
	}
	out := append([]FileConfig(nil), c.Files...)
	indices := make(map[string]int, len(out))
	for i := range out {
		indices[out[i].Path] = i
	}
	for _, file := range c.EphemeralFiles {
		if index, ok := indices[file.Path]; ok {
			out[index] = file
			continue
		}
		indices[file.Path] = len(out)
		out = append(out, file)
	}
	return out
}

// EffectiveLaunchEnv returns the launch environment after instance-only
// overrides. It never aliases either input map.
func (c *SandboxConfig) EffectiveLaunchEnv() map[string]string {
	if c == nil || (len(c.Launch.Env) == 0 && len(c.Launch.EphemeralEnv) == 0) {
		return nil
	}
	out := make(map[string]string, len(c.Launch.Env)+len(c.Launch.EphemeralEnv))
	for key, value := range c.Launch.Env {
		out[key] = value
	}
	for key, value := range c.Launch.EphemeralEnv {
		out[key] = value
	}
	return out
}

// ProtoFiles returns the effective cold-start file declarations as a proto
// slice. Memory restore deliberately does not call this method: a restored
// process already contains its file bindings and cold-only input is rejected.
func (c *SandboxConfig) ProtoFiles() []proto.FileSpec {
	files := c.EffectiveFiles()
	if len(files) == 0 {
		return nil
	}
	out := make([]proto.FileSpec, len(files))
	for i, f := range files {
		out[i] = proto.FileSpec{Path: f.Path, Content: f.Content, Mode: f.Mode, Owner: f.Owner, ReadOnly: f.ReadOnly}
	}
	return out
}

// ManifestConfig is the shared manifest/store/cache/crypto config the
// sandbox-ctl uses for any `manifest://` resource (boot blk0 base,
// snapshot bundle, snapshot --upload). Same shape as manifest-ctl's
// config so the same YAML drives both.
type ManifestConfig = manifest.Config

// ManifestConfigEnv is the process-environment variable consulted as
// a fallback when --manifest-config is not passed on the CLI.
const ManifestConfigEnv = "MANIFEST_CONFIG"

// LoadManifestConfig returns the manifest config, choosing the file
// path in order: flagPath, then $MANIFEST_CONFIG. Returns
// (nil, manifest.ErrConfigNotProvided) when neither is set — callers
// running with file://-only resources may treat that as a soft skip.
func LoadManifestConfig(flagPath string) (*ManifestConfig, error) {
	return manifest.LoadConfig(flagPath, ManifestConfigEnv)
}
