package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/runidentity"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
	"github.com/kuasar-sandbox/sandboxer/pkg/tapfd"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

type RuntimeDiskSpec struct {
	Name             string
	Overlay          bool
	BaseCapacity     int64
	WritableCapacity int64
}

// RuntimeSpec fixes the base VM shape. It contains no workload or credentials.
type RuntimeSpec struct {
	Kernel, Bundle, Cmdline         string
	Resources                       config.ResourcesConfig
	Network                         NetworkDeviceSpec
	Disks                           []RuntimeDiskSpec
	Timeouts                        config.TimeoutsConfig
	Usage                           config.UsageConfig
	SandboxID, PathID               string
	CHBinary, RuntimeRoot, BaseRoot string
	Console                         stdio.Console
	PingFatalThreshold              int
	StatsJSONPath                   string
	StatsInterval                   time.Duration
	NotifyReadiness                 ReadinessNotify
}

// NetworkDeviceSpec is host-only. Acquired TAP/namespace descriptors are owned
// by Runtime; the provider's original descriptors remain caller-owned.
type NetworkDeviceSpec struct {
	TAP, MAC string
	TapFD    *config.TapFDConfig
}

// GuestNetworkSpec is configured only when the workload launches.
type GuestNetworkSpec struct {
	IP, Nexthop, Hostname, Interface string
	MTU                              int
}

// LaunchSpec includes the entire workload, including ephemeral overrides.
// Storage and Bundle selectors are borrowed: callers must keep them open until
// Wait returns. Credentials creates runtime-owned storage using an explicit key.
// Caller-specified diff paths are never removed by Runtime.
type LaunchSpec struct {
	legacyAccess          *RunOptions
	Root                  config.RootConfig
	Disks                 []config.DiskConfig
	Network               GuestNetworkSpec
	Process               config.LaunchConfig
	Mounts                []config.MountConfig
	Files, EphemeralFiles []config.FileConfig
	Init                  []config.InitConfig
	Metadata              map[string]string
	Stdio                 stdio.Mode
	Forwards              []ForwardSpec
	Storage               *artifact.ProcessStorage
	ManifestConfig        *config.ManifestConfig
	Credentials           *ArtifactCredentials
	Fetcher               fetch.Fetcher
	BundleReader          *manifestbundle.Reader
	BundleFetcher         *manifestbundle.ManifestFetcher
	RefLocations          config.RefLocations
	SourceBinding         *RunSourceBinding
	PortableConfig        *config.PortableSandboxConfig
}

// ArtifactCredentials scopes artifact access without consulting process env.
type ArtifactCredentials struct {
	Config      *config.ManifestConfig
	CustomerKey ingest.CustomerKeyFunc
}
type SandboxSpec struct {
	Runtime RuntimeSpec
	Launch  LaunchSpec
}
type ExitResult struct {
	Code int
	Err  error
}
type runtimeState uint8

const (
	runtimeStarting runtimeState = iota
	runtimeReady
	runtimeLaunching
	runtimeRunning
	runtimeClosing
	runtimeClosed
)

func (s runtimeState) String() string {
	return [...]string{"starting", "runtime_ready", "launching", "running", "closing", "closed"}[s]
}

type runtimeOwnedDisk struct {
	reader    vhost.BlockReader
	base      vhost.BlockReader
	cow       *vhost.BlockCOW
	diffPath  string
	ownedDiff bool
}

// Runtime owns one VM and its service goroutines. Wait observes cleanup;
// Close requests graceful shutdown and waits for the same retained result.
type Runtime struct {
	consoleTTY                         atomic.Bool
	mu                                 sync.Mutex
	opsWG                              sync.WaitGroup
	launchContext                      context.Context
	launchSucceeded                    bool
	launchFailure                      error
	workMu                             sync.Mutex // joins launch preparation before releasing artifact resources
	state                              runtimeState
	spec                               RuntimeSpec
	sandboxID, pathID, runDir, baseDir string
	startUnixNs                        int64
	lifeCtx                            context.Context
	cancelLife                         context.CancelCauseFunc
	stopSignals                        context.CancelFunc
	signals                            chan os.Signal
	deviceSet                          *vhost.BackendSet
	launch                             *guestlink.LaunchServer
	cgroup                             *resctl.CgroupController
	hooks                              *resctl.ControllerHooks
	balloon                            *resctl.BalloonController
	tapFile, netnsFile                 *os.File
	metaIP                             string
	identity                           *runidentity.Guard
	disks                              []runtimeOwnedDisk
	cache                              *vhost.COWCache
	storage                            *artifact.ProcessStorage
	ownStorage                         bool
	exitDone                           chan struct{}
	exit                               ExitResult
	closeOnce                          sync.Once
	cleanupOnce                        sync.Once
	engineStarted                      bool
	readyDone                          chan struct{}
	readyOnce                          sync.Once
	snapshot                           func(context.Context, ctl.Request) (ctl.Response, error)
	export                             func(context.Context, ctl.Request) (ctl.Response, error)
	stats                              func() (ctl.Response, error)
	execSession                        func(context.Context, *proto.ExecSpec) (net.Conn, proto.StdioSpec, error)
	installLaunch                      func(*config.SandboxConfig, *config.PortableSandboxConfig, LaunchSpec, []DiskBackend, *proto.LaunchSpec) error
	logf                               func(string, ...any)
}

func (s RuntimeSpec) config() *config.SandboxConfig {
	c := &config.SandboxConfig{Resources: s.Resources, Timeouts: s.Timeouts, Usage: s.Usage,
		Boot:    config.BootConfig{Kernel: s.Kernel, Runtime: s.Bundle, Cmdline: s.Cmdline},
		Network: config.NetworkConfig{TAP: s.Network.TAP, MAC: s.Network.MAC, TapFD: s.Network.TapFD}}
	if len(s.Disks) > 0 && s.Disks[0].Overlay {
		c.Boot.Root.Overlay = &config.OverlayConfig{}
	}
	return c
}
func (s LaunchSpec) config(base *config.SandboxConfig) (*config.SandboxConfig, error) {
	c := *base
	c.Boot.Root = s.Root
	c.Boot.Disks = s.Disks
	c.Launch = s.Process
	c.Mounts = s.Mounts
	c.Files = s.Files
	c.EphemeralFiles = s.EphemeralFiles
	c.Init = s.Init
	c.Metadata = s.Metadata
	c.Network.IP = s.Network.IP
	c.Network.MTU = s.Network.MTU
	c.Network.Nexthop = s.Network.Nexthop
	c.Network.Hostname = s.Network.Hostname
	c.Network.Interface = s.Network.Interface
	return cloneSDKConfig(&c)
}

func runtimeDeviceSpecs(disks []RuntimeDiskSpec) ([]vhost.BlockDeviceSpec, error) {
	if len(disks) > 1+config.MaxDataDisks {
		return nil, errors.New("runtime disk count exceeds supported layout")
	}
	names := map[string]bool{}
	if len(disks) == 0 {
		return nil, errors.New("runtime spec: at least root disk is required")
	}
	var out []vhost.BlockDeviceSpec
	for i, d := range disks {
		if i > 0 {
			if d.Name == "" || names[d.Name] {
				return nil, fmt.Errorf("runtime disk %d requires a unique name", i)
			}
			names[d.Name] = true
		}
		if d.BaseCapacity%vhost.SectorSize != 0 || d.WritableCapacity%vhost.SectorSize != 0 {
			return nil, fmt.Errorf("runtime disk %d capacity is not sector-aligned", i)
		}
		if d.WritableCapacity <= 0 {
			return nil, fmt.Errorf("runtime disk %d: writable capacity required", i)
		}
		if d.Overlay {
			if d.BaseCapacity <= 0 {
				return nil, fmt.Errorf("runtime disk %d: overlay base capacity required", i)
			}
			out = append(out, vhost.BlockDeviceSpec{Capacity: d.BaseCapacity, ReadOnly: true})
		} else if d.BaseCapacity != 0 {
			return nil, fmt.Errorf("runtime disk %d: single disk cannot expose separate base capacity", i)
		}
		out = append(out, vhost.BlockDeviceSpec{Capacity: d.WritableCapacity})
	}
	return out, nil
}
func validateRuntimeSpec(s RuntimeSpec) error {
	if s.Kernel == "" || s.Bundle == "" {
		return errors.New("runtime spec: boot.kernel and boot.runtime are required")
	}
	if _, err := runtimeDeviceSpecs(s.Disks); err != nil {
		return err
	}
	return s.config().ValidateRuntime()
}
func StartRuntime(ctx context.Context, spec RuntimeSpec) (_ *Runtime, retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRuntimeSpec(spec); err != nil {
		return nil, err
	}
	spec.Disks = append([]RuntimeDiskSpec(nil), spec.Disks...)
	cfg, err := cloneSDKConfig(spec.config())
	if err != nil {
		return nil, err
	}
	spec.Resources = cfg.Resources
	spec.Network.TapFD = cfg.Network.TapFD
	if err := VerifyKernelArtifact(cfg.Boot.Kernel); err != nil {
		return nil, err
	}
	if _, err := ResolveRuntimeProjection(cfg); err != nil {
		return nil, err
	}
	sid := spec.SandboxID
	if sid == "" {
		sid = generateSandboxID()
	}
	if err := validateSandboxID(sid); err != nil {
		return nil, err
	}
	pathID, err := ResolvePathID(sid, spec.PathID)
	if err != nil {
		return nil, err
	}
	runtimeRoot := spec.RuntimeRoot
	if runtimeRoot == "" {
		runtimeRoot = "/run/sandbox"
	}
	baseRoot := spec.BaseRoot
	if baseRoot == "" {
		baseRoot = DefaultBaseRoot
	}
	chBinary := spec.CHBinary
	if chBinary == "" {
		chBinary = "cloud-hypervisor"
	}
	runDir := filepath.Join(runtimeRoot, pathID)
	identity, err := runidentity.Acquire(runDir, sid)
	if err != nil {
		return nil, err
	}
	life, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 4)
	engineCtx, stopSignals := newRunSignalContext(life, signals, nil)
	r := &Runtime{state: runtimeStarting, spec: spec, sandboxID: sid, pathID: pathID, runDir: runDir, baseDir: DefaultBaseDir(baseRoot, pathID), startUnixNs: time.Now().UnixNano(), lifeCtx: engineCtx, cancelLife: cancel, signals: signals, stopSignals: stopSignals, identity: identity, exitDone: make(chan struct{}), logf: func(f string, a ...any) { log.Printf("[sandbox-sdk] "+f, a...) }}
	stopOperation := context.AfterFunc(ctx, func() { signals <- syscall.SIGTERM })
	defer stopOperation()
	r.spec.CHBinary = chBinary
	r.spec.RuntimeRoot = runtimeRoot
	r.spec.BaseRoot = baseRoot
	defer func() {
		if retErr != nil {
			if r.engineStarted {
				_ = r.Close()
			} else {
				r.cleanup()
			}
		}
	}()
	deviceSpecs, err := runtimeDeviceSpecs(spec.Disks)
	if err != nil {
		return nil, err
	}
	set, err := vhost.NewBackendSet(deviceSpecs)
	if err != nil {
		return nil, err
	}
	r.deviceSet = set
	capBytes, err := cfg.CapacityMemoryBytes()
	if err != nil {
		return nil, err
	}
	allocBytes, err := cfg.AllocatableMemoryBytes()
	if err != nil {
		return nil, err
	}
	startup, err := cfg.StartupBytes()
	if err != nil {
		return nil, err
	}
	if capBytes > uint64(^uint(0)>>1) {
		return nil, errors.New("runtime memory exceeds host address space")
	}
	initial := resctl.AlignedBudget(capBytes, startup)
	if err := resctl.ValidateBalloonSize(capBytes, resctl.TargetForBudget(capBytes, initial)); err != nil {
		return nil, err
	}
	if err := resctl.ValidateBalloonSize(capBytes, resctl.TargetForBudget(capBytes, allocBytes)); err != nil {
		return nil, err
	}
	chSock := filepath.Join(runDir, "ch.sock")
	if allocBytes < capBytes || initial < capBytes {
		r.balloon = resctl.NewBalloonController(chSock, capBytes, cfg.CHApiDeadline(), r.logf)
	}
	hooks, err := resctl.NewControllerHooks(resctl.ControllerHookOptions{SocketPath: cfg.Resources.Control.Controller, SandboxID: sid, Context: engineCtx, Logf: r.logf}, cfg)
	if err != nil {
		return nil, err
	}
	r.hooks = hooks
	initial, err = hooks.Admit(sid, 0)
	if err != nil {
		return nil, err
	}
	if r.balloon != nil {
		if err := r.balloon.SeedColdTarget(resctl.TargetForBudget(capBytes, initial)); err != nil {
			return nil, err
		}
	}
	cgCfg, err := resctl.BuildCgroupConfig(cfg)
	if err != nil {
		return nil, err
	}
	cgCfg.MemoryHighBytes = 0
	cg, err := resctl.SetupCgroup(cgCfg)
	if err != nil {
		return nil, err
	}
	r.cgroup = cg
	hooks.SetLocalCgroupPath(cg.LocalPath())
	mc, err := resctl.NewMemoryController(resctl.MemoryControllerOptions{Config: cfg, CgroupPath: cg.LocalPath(), Balloon: r.balloon, Reservation: hooks, InitialBudget: initial, Logf: r.logf})
	if err != nil {
		return nil, err
	}
	if cfg.Network.TAP != "" {
		if err := VerifyTAP(cfg.Network.TAP); err != nil {
			return nil, err
		}
	}
	var metaMAC string
	if cfg.Network.TapFD != nil {
		f, nsf, meta, e := tapfd.AcquireConfig(ctx, cfg.Network.TapFD)
		if e != nil {
			return nil, e
		}
		r.tapFile, r.netnsFile, metaMAC, r.metaIP = f, nsf, meta.MAC, meta.IP
	}
	netMAC, _ := cfg.Network.Effective(metaMAC, "")
	baseReady := make(chan struct{})
	params := VMParams{Ctx: engineCtx, SandboxID: sid, BaseDir: r.baseDir, RunDir: runDir,
		Balloon: r.balloon, Hooks: hooks, Memory: mc, Cgroup: cg, CapBytes: int64(capBytes), UffdSource: uffd.ZeroSource{},
		StdioMode: stdio.Mode{Console: spec.Console}, TapFile: r.tapFile, NetnsFile: r.netnsFile, NetMAC: netMAC,
		Logf: r.logf, StartUnixNs: r.startUnixNs, StatsJSONPath: spec.StatsJSONPath, StatsInterval: spec.StatsInterval,
		PingFatalThreshold: spec.PingFatalThreshold, VAReportDeadline: cfg.VAReportDeadline(), PingTimeout: cfg.PingDeadline(), AppNotifyDeadline: cfg.AppNotifyDeadline(),
		SnapCfg: cfg, WireLaunchMUX: true, ReadyOnAppStarted: true, NotifyReadiness: spec.NotifyReadiness,
		owner: r,
		BuildCmd: func(e CmdEnv) (*exec.Cmd, func(), error) {
			cmd := exec.Command(chBinary)
			consoleArg, cleanup, err := (stdio.Mode{Console: spec.Console}).SetupCHStdio(cmd)
			if err != nil {
				return nil, nil, err
			}
			_, kernelPath, _ := config.SchemeAndPath(cfg.Boot.Kernel)
			_, runtimePath, _ := config.SchemeAndPath(cfg.Boot.Runtime)
			args, err := CHCommandWithInitialBudget(cfg, initial, e.Disks, e.CHSock, e.VsockBase, kernelPath, runtimePath, e.UffdSock, consoleArg, e.TapFDNum, e.NetMAC)
			if err != nil {
				cleanup()
				return nil, nil, err
			}
			r.logf("CH args: %s", strings.Join(args, " "))
			if spec.Console.Kind == stdio.ConsoleStderr {
				cmd.Stdout = runtimeConsoleWriter{owner: r, dst: cmd.Stdout}
			}
			cmd.Args = append(cmd.Args, args...)
			return cmd, cleanup, nil
		},
		PostSpawn: func(pc PostSpawnCtx) error {
			select {
			case <-pc.Launch.RuntimeReady():
			case <-pc.Ctx.Done():
				return pc.Ctx.Err()
			}
			pc.Pinger.Start(pc.Ctx)
			if hooks.Enabled() {
				hooks.StartHeartbeat(pc.Ctx, 5*time.Second)
			}
			if err := pc.Ctx.Err(); err != nil {
				return err
			}
			if spec.NotifyReadiness != nil {
				spec.NotifyReadiness(ReadinessRuntimeReady)
			}
			r.mu.Lock()
			if r.state == runtimeStarting {
				r.state = runtimeReady
				close(baseReady)
			}
			r.mu.Unlock()
			return nil
		},
	}
	r.engineStarted = true
	go func() { code, err := ServeAndWait(params); r.finishExit(ExitResult{Code: code, Err: err}) }()
	select {
	case <-baseReady:
		stopOperation()
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if r.state != runtimeReady {
			return nil, errors.New("runtime exited during startup")
		}
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.exitDone:
		return nil, fmt.Errorf("runtime startup: %w", errors.Join(errors.New("runtime exited before runtime_ready"), r.exit.Err))
	}
}

func cloneSDKConfig(c *config.SandboxConfig) (*config.SandboxConfig, error) {
	if c == nil {
		return nil, errors.New("nil sandbox config")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var out config.SandboxConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	out.Resources.Control.CgroupFD = c.Resources.Control.CgroupFD
	return &out, nil
}
func (r *Runtime) Launch(ctx context.Context, spec LaunchSpec) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if spec.Storage != nil && spec.Credentials != nil {
		return errors.New("launch: Storage and Credentials are mutually exclusive")
	}
	cfg, err := spec.config(r.spec.config())
	if err != nil {
		return err
	}
	if err := cfg.ValidateCold(); err != nil && spec.SourceBinding == nil {
		return err
	}
	r.mu.Lock()
	if r.state != runtimeReady {
		state := r.state.String()
		r.mu.Unlock()
		return fmt.Errorf("launch invalid in state %s", state)
	}
	r.state = runtimeLaunching
	r.workMu.Lock()
	r.mu.Unlock()
	workHeld := true
	defer func() {
		if workHeld {
			r.workMu.Unlock()
		}
	}()
	opCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(r.lifeCtx, cancel)
	defer stop()
	ctx = opCtx
	fail := func(err error) error {
		if workHeld {
			workHeld = false
			r.workMu.Unlock()
		}
		_ = r.Close()
		return err
	}
	if spec.Storage == nil {
		var credentials ArtifactCredentials
		if spec.Credentials != nil {
			credentials = *spec.Credentials
		}
		spec.ManifestConfig = credentials.Config
		spec.Storage, err = artifact.NewProcessStorageWithCustomerKey(credentials.Config, credentials.CustomerKey)
		if err != nil {
			return fail(err)
		}
		r.ownStorage = true
	}
	r.storage = spec.Storage
	if spec.SourceBinding != nil {
		if err := config.BindPortableDiskGraph(cfg, spec.SourceBinding.RuntimeRef, spec.SourceBinding.RelativeDir); err != nil {
			return fail(err)
		}
	}
	if len(cfg.Boot.Disks)+1 != len(r.spec.Disks) {
		return fail(fmt.Errorf("launch disk count mismatch"))
	}
	for i, shape := range r.spec.Disks {
		if i > 0 && cfg.Boot.Disks[i-1].Name != shape.Name {
			return fail(fmt.Errorf("launch disk %d identity mismatch", i))
		}
		var overlay bool
		if i == 0 {
			overlay = cfg.Boot.Root.Overlay != nil
		} else {
			overlay = cfg.Boot.Disks[i-1].Overlay != nil
		}
		if overlay != shape.Overlay {
			return fail(fmt.Errorf("launch disk %d layout mismatch", i))
		}
	}
	if err := cfg.ValidateCold(); err != nil {
		return fail(err)
	}
	opener := FileStreamOpener(func(ctx context.Context, path string, ref manifest.Ref) (fetch.Stream, error) {
		return openLaunchFile(ctx, spec, path, ref, spec.RefLocations)
	})
	if err := canonicalizeConfiguredTarRefsWithOpener(ctx, cfg, spec.RefLocations, launchCodec(spec), launchRequired(spec), opener); err != nil {
		return fail(err)
	}
	var diffKey [32]byte
	defer clear(diffKey[:])
	if launchCodec(spec) != nil {
		fn := launchKey(spec)
		if fn == nil {
			return fail(errors.New("launch encrypted storage requires customer key"))
		}
		diffKey, err = fn()
		if err != nil {
			return fail(err)
		}
	}
	defaults, err := preflightColdArtifacts(ctx, cfg, launchFetcher(spec), spec.RefLocations, r.baseDir, r.sandboxID, launchCodec(spec), diffKey, launchRequired(spec), opener)
	if err != nil {
		return fail(err)
	}
	if err := MaterializeImageDefaults(cfg, defaults); err != nil {
		return fail(err)
	}
	var portable *config.PortableSandboxConfig
	if spec.PortableConfig != nil {
		// PortableConfig is also the immutable C0 projected from an ordinary
		// host-bound cold config. A SourceBinding is required only when this
		// launch actually came from a portable Sandbox artifact; local C0 has
		// complete host disk bindings in cfg and no parent Sandbox identity.
		if spec.SourceBinding != nil {
			if _, e := manifest.ParseRef(spec.SourceBinding.SandboxRef); e != nil {
				return fail(e)
			}
		}
		portable, err = spec.PortableConfig.Clone()
		if err == nil {
			ref, e := ResolveRuntimeProjection(cfg)
			if e != nil {
				return fail(e)
			}
			if portable.Boot.Runtime != ref {
				return fail(errors.New("boot.runtime identity mismatch"))
			}
		}
	} else {
		var ids config.PortableProjection
		ids, err = ResolvePortableProjection(cfg)
		if err == nil {
			portable, err = config.ProjectPortableCold(cfg, ids)
		}
	}
	if err != nil {
		return fail(err)
	}
	cacheSize, dirtySize, err := cfg.Resources.DiffCOW.Bytes()
	if err != nil {
		return fail(err)
	}
	cache, err := vhost.NewCOWCache(cacheSize, dirtySize)
	if err != nil {
		return fail(err)
	}
	r.cache = cache
	// Retained readers must survive successful Launch's operation context.
	readCtx, cancelReads := context.WithCancel(VMLifecycleContext(r.lifeCtx))
	stopReadCancel := context.AfterFunc(ctx, cancelReads)
	launched := false
	defer func() {
		stopReadCancel()
		if !launched {
			cancelReads()
		}
	}()
	var imageCfg *ImageConfig
	var logical []DiskBackend
	prepare := func(root *config.RootConfig, index int, isRoot bool) (_ DiskBackend, retErr error) {
		var base, ro vhost.BlockReader
		defer func() {
			if retErr != nil {
				if base != nil {
					_ = base.Close()
				}
				if ro != nil {
					_ = ro.Close()
				}
			}
		}()
		var diffURI, diffTemplate string
		if root.Overlay != nil {
			x, _, e := OpenRootImageBlockReaderWithOpener(readCtx, root.Base, launchFetcher(spec), spec.RefLocations, launchCodec(spec), launchRequired(spec), opener)
			if e != nil {
				return DiskBackend{}, e
			}
			ro = x
			diffURI, diffTemplate = root.Overlay.Diff, root.Overlay.DiffTemplate
			if root.Overlay.Base != "" {
				refs := append([]string{root.Overlay.Base}, root.Overlay.BaseFromRefs...)
				x, _, e := OpenLayeredBlockReaderWithOpener(readCtx, refs, launchFetcher(spec), spec.RefLocations, launchCodec(spec), launchRequired(spec), opener)
				if e != nil {
					return DiskBackend{}, e
				}
				base = x
			}
		} else {
			diffURI, diffTemplate = root.Diff, root.DiffTemplate
			if root.Base != "" {
				refs := append([]string{root.Base}, root.BaseFromRefs...)
				x, _, e := OpenLayeredBlockReaderWithOpener(readCtx, refs, launchFetcher(spec), spec.RefLocations, launchCodec(spec), launchRequired(spec), opener)
				if e != nil {
					return DiskBackend{}, e
				}
				base = x
			}
		}
		owned := diffURI == ""
		if owned {
			if err := os.MkdirAll(r.baseDir, 0755); err != nil {
				return DiskBackend{}, err
			}
			if isRoot {
				diffURI = DefaultDiffURIForBaseDir(r.baseDir, r.sandboxID)
			} else {
				diffURI = DefaultDiskDiffURI(r.baseDir, r.sandboxID, fmt.Sprintf("disk%d", index-1))
			}
		}
		_, diffPath, ok := config.SchemeAndPath(diffURI)
		if !ok {
			return DiskBackend{}, fmt.Errorf("invalid diff URI %q", diffURI)
		}
		baseSize := int64(0)
		if base != nil {
			baseSize = base.Size()
		}
		if owned {
			defer func() {
				if retErr != nil {
					_ = os.Remove(diffPath)
				}
			}()
		}
		init, err := PrepareDiff(diffPath, diffTemplate, baseSize)
		if err != nil {
			return DiskBackend{}, err
		}
		opts := []vhost.BlockCOWOption{vhost.WithCOWCache(cache)}
		if launchCodec(spec) != nil {
			opts = append(opts, vhost.WithDiffEncryption(diffKey, launchRequired(spec)))
		}
		cow, err := vhost.OpenBlockCOW(diffPath, base, init, opts...)
		if err != nil {
			return DiskBackend{}, err
		}
		r.disks = append(r.disks, runtimeOwnedDisk{reader: ro, base: base, cow: cow, diffPath: diffPath, ownedDiff: owned})
		return DiskBackend{Overlay: root.Overlay != nil, Reader: ro, Cow: cow, BasePath: root.Base, DiffPath: diffPath, OwnedDiff: owned}, nil
	}
	root, err := prepare(&cfg.Boot.Root, 0, true)
	if err != nil {
		return fail(err)
	}
	logical = append(logical, root)
	if root.Reader != nil {
		imageCfg, err = LoadImageConfigFrom(root.Reader, root.Reader.Size())
		if err != nil {
			return fail(err)
		}
		r.logf("image config: cmd=%v entrypoint=%v workdir=%q env-keys=%d", imageCfg.Cmd, imageCfg.Entrypoint, imageCfg.WorkingDir, len(imageCfg.Env))
	} else {
		imageCfg = &ImageConfig{}
	}
	for i := range cfg.Boot.Disks {
		d, e := prepare(&cfg.Boot.Disks[i].RootConfig, i+1, false)
		if e != nil {
			return fail(e)
		}
		logical = append(logical, d)
	}

	ls, err := MergeLaunch(imageCfg, cfg.Launch)
	if err != nil {
		return fail(err)
	}
	_, netSpec := cfg.Network.Effective("", r.metaIP)
	ls.Network = netSpec
	ls.Mounts = effectiveMounts(cfg.Mounts, imageCfg.Volumes)
	ls.Files = cfg.ProtoFiles()
	ls.Init = toProtoInit(cfg.Init)
	ls.Plugins = toProtoPlugins(cfg.Launch.Plugin)
	ls.SharePID = cfg.Launch.PIDNamespace == "shared"
	ls.StopGraceSec = cfg.StopGraceSeconds()
	ls.Stdio = spec.Stdio.ProtoSpec()
	if cols, rows, ok := spec.Stdio.InitialWinsize(); ok {
		ls.Stdio.Winsize = &proto.Winsize{Cols: cols, Rows: rows}
	}
	resolveDiskMounts(ls.Mounts, cfg.Boot.Disks, cfg.SingleDisk())
	if _, err := config.WritePortableSandboxConfig(r.runDir, portable); err != nil {
		return fail(err)
	}
	r.mu.Lock()
	if r.state != runtimeLaunching || ctx.Err() != nil {
		r.mu.Unlock()
		return fail(errors.Join(errors.New("launch interrupted"), ctx.Err()))
	}
	stopReadCancel()
	r.launchContext = ctx
	err = r.installLaunch(cfg, portable, spec, logical, ls)
	r.mu.Unlock()
	if err != nil {
		return fail(err)
	}
	workHeld = false
	r.workMu.Unlock()
	select {
	case <-r.launch.AppStartedDone():
	case <-ctx.Done():
	case <-r.exitDone:
	}
	r.mu.Lock()
	succeeded := r.launchSucceeded
	r.mu.Unlock()
	if succeeded {
		launched = true
		return nil
	}
	return fail(errors.Join(errors.New("runtime launch did not complete"), ctx.Err()))

}
func openLaunchFile(ctx context.Context, s LaunchSpec, path string, ref manifest.Ref, locations config.RefLocations) (fetch.Stream, error) {
	// A supplied ProcessStorage is an opaque artifact-access capability: it owns
	// manifest configuration, customer key, codec, and lazy clients. Preserve
	// that configuration instead of reconstructing it from public LaunchSpec
	// fields when the caller did not explicitly override access.
	if s.legacyAccess == nil && s.Storage != nil && s.ManifestConfig == nil && s.Fetcher == nil && s.Credentials == nil {
		return s.Storage.OpenFileWithLocations(ctx, path, ref, locations)
	}
	return artifact.OpenFileWithLocations(ctx, path, ref, s.ManifestConfig, launchKey(s), launchFetcher(s), locations, launchCodec(s), launchRequired(s))
}

func launchFetcher(s LaunchSpec) fetch.Fetcher {
	if s.legacyAccess != nil {
		return s.legacyAccess.Fetcher
	}
	if s.Fetcher != nil {
		return s.Fetcher
	}
	return s.Storage.Fetcher()
}

func Start(ctx context.Context, spec SandboxSpec) (*Runtime, error) {
	r, err := StartRuntime(ctx, spec.Runtime)
	if err != nil {
		return nil, err
	}
	if err := r.Launch(ctx, spec.Launch); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}
func (r *Runtime) State() string { r.mu.Lock(); defer r.mu.Unlock(); return r.state.String() }
func (r *Runtime) Wait(ctx context.Context) (ExitResult, error) {
	select {
	case <-r.exitDone:
		return r.exit, nil
	default:
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-r.exitDone:
		return r.exit, nil
	case <-ctx.Done():
		return ExitResult{}, ctx.Err()
	}
}
func (r *Runtime) stopForExit() {
	r.mu.Lock()
	if r.state != runtimeClosed {
		r.state = runtimeClosing
	}
	r.mu.Unlock()
	if r.cancelLife != nil {
		r.cancelLife(context.Canceled)
	}
}
func (r *Runtime) finishExit(x ExitResult) {
	r.cleanupOnce.Do(func() {
		r.stopForExit()
		r.opsWG.Wait()
		err := r.cleanupResources()
		r.mu.Lock()
		x.Err = errors.Join(x.Err, r.launchFailure, err)
		if x.Err != nil {
			x.Code = -1
		}
		r.exit = x
		r.state = runtimeClosed
		r.mu.Unlock()
		close(r.exitDone)
	})
}
func (r *Runtime) requestClose() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		closed := r.state == runtimeClosed
		if !closed {
			r.state = runtimeClosing
		}
		r.mu.Unlock()
		if closed {
			return
		}
		if r.engineStarted {
			r.signals <- syscall.SIGTERM
		} else {
			r.finishExit(ExitResult{})
		}
	})
}
func (r *Runtime) Close() error { r.requestClose(); <-r.exitDone; return r.exit.Err }
func (r *Runtime) cleanup() {
	if r.cancelLife != nil {
		r.cancelLife(context.Canceled)
	}
	_ = r.cleanupResources()
}
func (r *Runtime) cleanupResources() (retErr error) {
	r.workMu.Lock()
	defer r.workMu.Unlock()
	if r.stopSignals != nil {
		r.stopSignals()
	}
	if r.cancelLife != nil {
		r.cancelLife(context.Canceled)
	}
	for i := len(r.disks) - 1; i >= 0; i-- {
		d := r.disks[i]
		if d.cow != nil {
			retErr = errors.Join(retErr, d.cow.Close())
		}
		if d.base != nil {
			retErr = errors.Join(retErr, d.base.Close())
		}
		if d.reader != nil {
			retErr = errors.Join(retErr, d.reader.Close())
		}
		if d.ownedDiff {
			_ = os.Remove(d.diffPath)
		}
	}
	// Automatic diffs are the only reason an otherwise-empty baseDir may have
	// been created. Preserve retained files by using non-recursive Remove.
	if r.baseDir != "" {
		_ = os.Remove(r.baseDir)
	}
	if r.cache != nil {
		retErr = errors.Join(retErr, r.cache.Close())
	}
	if r.ownStorage {
		retErr = errors.Join(retErr, r.storage.Close())
	}
	if r.hooks != nil {
		r.hooks.Release("runtime-close")
	}
	if r.cgroup != nil {
		_ = r.cgroup.Cleanup()
	}
	if r.tapFile != nil {
		_ = r.tapFile.Close()
	}
	if r.netnsFile != nil {
		_ = r.netnsFile.Close()
	}
	if r.identity != nil {
		retErr = errors.Join(retErr, r.identity.RemoveRunDir(), r.identity.Close())
	}
	return retErr
}

func DeriveRuntimeSpec(ctx context.Context, cfg *config.SandboxConfig, storage *artifact.ProcessStorage, locations config.RefLocations) (RuntimeSpec, error) {
	return deriveRuntimeSpec(ctx, cfg, LaunchSpec{Storage: storage, RefLocations: locations})
}
func deriveRuntimeSpec(ctx context.Context, cfg *config.SandboxConfig, launch LaunchSpec) (RuntimeSpec, error) {
	storage, locations := launch.Storage, launch.RefLocations
	if cfg == nil || storage == nil {
		return RuntimeSpec{}, errors.New("derive runtime spec: config and storage required")
	}
	opener := FileStreamOpener(func(ctx context.Context, path string, ref manifest.Ref) (fetch.Stream, error) {
		return openLaunchFile(ctx, launch, path, ref, locations)
	})

	clone, err := cloneSDKConfig(cfg)
	if err != nil {
		return RuntimeSpec{}, err
	}
	var shapes []RuntimeDiskSpec
	var diffOpts []vhost.BlockCOWOption
	if launchCodec(launch) != nil {
		keyFn := launchKey(launch)
		if keyFn == nil {
			return RuntimeSpec{}, errors.New("diff encryption requires customer key")
		}
		key, e := keyFn()
		if e != nil {
			return RuntimeSpec{}, e
		}
		diffOpts = append(diffOpts, vhost.WithDiffEncryption(key, launchRequired(launch)))
		clear(key[:])
	}
	capacity := func(diffURI, template string, baseSize int64) (int64, error) {
		if diffURI != "" {
			_, p, ok := config.SchemeAndPath(diffURI)
			if !ok {
				return 0, fmt.Errorf("invalid diff URI %q", diffURI)
			}
			if st, e := os.Stat(p); e == nil && st.Size() > 0 {
				return vhost.DiffSourceCapacity(p, false, diffOpts...)
			}
		}
		if template != "" {
			scheme, p, ok := config.SchemeAndPath(template)
			if !ok || scheme != "file" {
				return 0, fmt.Errorf("invalid diff template %q", template)
			}
			return vhost.DiffSourceCapacity(p, true, diffOpts...)
		}
		if baseSize > 0 {
			return baseSize, nil
		}
		return 0, errors.New("no source for writable disk capacity")
	}
	derive := func(root *config.RootConfig, name string) (RuntimeDiskSpec, error) {
		d := RuntimeDiskSpec{Name: name, Overlay: root.Overlay != nil}
		var cowBaseSize int64
		if root.Overlay != nil {
			ro, _, e := OpenRootImageBlockReaderWithOpener(ctx, root.Base, launchFetcher(launch), locations, launchCodec(launch), launchRequired(launch), opener)
			if e != nil {
				return d, e
			}
			d.BaseCapacity = ro.Size()
			_ = ro.Close()
			if root.Overlay.Base != "" {
				x, _, e := OpenLayeredBlockReaderWithOpener(ctx, append([]string{root.Overlay.Base}, root.Overlay.BaseFromRefs...), launchFetcher(launch), locations, launchCodec(launch), launchRequired(launch), opener)
				if e != nil {
					return d, e
				}
				cowBaseSize = x.Size()
				_ = x.Close()
			}
			d.WritableCapacity, err = capacity(root.Overlay.Diff, root.Overlay.DiffTemplate, cowBaseSize)
		} else {
			if root.Base != "" {
				x, _, e := OpenLayeredBlockReaderWithOpener(ctx, append([]string{root.Base}, root.BaseFromRefs...), launchFetcher(launch), locations, launchCodec(launch), launchRequired(launch), opener)
				if e != nil {
					return d, e
				}
				cowBaseSize = x.Size()
				_ = x.Close()
			}
			d.WritableCapacity, err = capacity(root.Diff, root.DiffTemplate, cowBaseSize)
		}
		return d, err
	}
	root, err := derive(&cfg.Boot.Root, "root")
	if err != nil {
		return RuntimeSpec{}, err
	}
	shapes = append(shapes, root)
	for i := range cfg.Boot.Disks {
		d, e := derive(&cfg.Boot.Disks[i].RootConfig, cfg.Boot.Disks[i].Name)
		if e != nil {
			return RuntimeSpec{}, e
		}
		shapes = append(shapes, d)
	}
	clone.Boot.Root = config.RootConfig{}
	if root.Overlay {
		clone.Boot.Root.Overlay = &config.OverlayConfig{}
	}
	clone.Boot.Disks = nil
	clone.Mounts = nil
	clone.Files = nil
	clone.Init = nil
	clone.Launch = config.LaunchConfig{}
	return runtimeSpecFromConfig(clone, shapes), nil
}

func runtimeSpecFromConfig(c *config.SandboxConfig, disks []RuntimeDiskSpec) RuntimeSpec {
	return RuntimeSpec{Kernel: c.Boot.Kernel, Bundle: c.Boot.Runtime, Cmdline: c.Boot.Cmdline, Resources: c.Resources, Timeouts: c.Timeouts, Usage: c.Usage, Disks: disks, Network: NetworkDeviceSpec{TAP: c.Network.TAP, MAC: c.Network.MAC, TapFD: c.Network.TapFD}}
}

// LaunchSpecFromConfig preserves every workload field in the configuration ABI.
func LaunchSpecFromConfig(c *config.SandboxConfig) LaunchSpec {
	return LaunchSpec{Root: c.Boot.Root, Disks: c.Boot.Disks, Process: c.Launch, Mounts: c.Mounts, Files: c.Files, EphemeralFiles: c.EphemeralFiles, Init: c.Init, Metadata: c.Metadata, Network: GuestNetworkSpec{IP: c.Network.IP, MTU: c.Network.MTU, Nexthop: c.Network.Nexthop, Hostname: c.Network.Hostname, Interface: c.Network.Interface}}
}

func (r *Runtime) requireRunning() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != runtimeRunning {
		return fmt.Errorf("operation requires running workload (state %s)", r.state)
	}
	return nil
}

// RunLifecycle is the integration point for snapshot reconstruction. The
// reconstruction stack and all its defers stay owned by the returned Runtime;
// success waits for the existing restore ACK/MUX barrier, never runtime_ready.
func RunLifecycle(ctx context.Context, run func(context.Context) (int, error)) (*Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancelCause(context.Background())
	signals := make(chan os.Signal, 4)
	engineCtx, stop := newRunSignalContext(life, signals, nil)
	r := &Runtime{state: runtimeStarting, lifeCtx: engineCtx, cancelLife: cancel, signals: signals, stopSignals: stop, engineStarted: true, exitDone: make(chan struct{}), readyDone: make(chan struct{})}
	engineCtx = context.WithValue(engineCtx, runtimeOwnerKey{}, r)
	go func() { code, err := run(engineCtx); r.finishExit(ExitResult{Code: code, Err: err}) }()
	select {
	case <-r.readyDone:
		r.mu.Lock()
		err := ctx.Err()
		running := r.state == runtimeRunning
		r.mu.Unlock()
		if err != nil || !running {
			_ = r.Close()
			return nil, errors.Join(errors.New("restore interrupted"), err)
		}
		return r, nil
	case <-ctx.Done():
		_ = r.Close()
		return nil, ctx.Err()
	case <-r.exitDone:
		return nil, errors.Join(errors.New("restore exited before readiness"), r.exit.Err)
	}
}

type runtimeOwnerKey struct{}

// Stats is valid for a base runtime as well as a launched workload.
func (r *Runtime) Stats(ctx context.Context) (*ctl.ResourceStats, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	fn := r.stats
	live := r.state >= runtimeReady && r.state < runtimeClosing
	if live && fn != nil {
		r.opsWG.Add(1)
	}
	r.mu.Unlock()
	if !live || fn == nil {
		return nil, errors.New("runtime stats unavailable")
	}
	defer r.opsWG.Done()
	resp, err := fn()
	return resp.ResourceStats, err
}

// Snapshot captures memory and disks using the same operation as ctl.sock.
// A destroy-mode result schedules teardown after the capture is committed.
func (r *Runtime) Snapshot(ctx context.Context, req ctl.Request) (ctl.Response, error) {
	return r.capture(ctx, req, false)
}

// Export captures portable workload disk state without changing the format.
func (r *Runtime) Export(ctx context.Context, req ctl.Request) (ctl.Response, error) {
	return r.capture(ctx, req, true)
}
func (r *Runtime) capture(ctx context.Context, req ctl.Request, export bool) (ctl.Response, error) {
	if err := ctx.Err(); err != nil {
		return ctl.Response{}, err
	}
	if err := r.beginOperation(); err != nil {
		return ctl.Response{}, err
	}
	defer r.opsWG.Done()
	r.mu.Lock()
	fn := r.snapshot
	if export {
		fn = r.export
	}
	r.mu.Unlock()
	if fn == nil {
		return ctl.Response{}, errors.New("capture unavailable")
	}
	resp, err := fn(ctx, req)
	if resp.AfterWrite != nil {
		after := resp.AfterWrite
		resp.AfterWrite = nil
		after()
	}
	return resp, err
}

// Exec opens a guest MUX session. The caller owns the returned connection and
// must close it; capture and Runtime.Close also terminate outstanding sessions.
func (r *Runtime) Exec(ctx context.Context, spec proto.ExecSpec) (net.Conn, proto.StdioSpec, error) {
	if err := ctx.Err(); err != nil {
		return nil, proto.StdioSpec{}, err
	}
	if err := r.beginOperation(); err != nil {
		return nil, proto.StdioSpec{}, err
	}
	defer r.opsWG.Done()
	r.mu.Lock()
	fn := r.execSession
	r.mu.Unlock()
	if fn == nil {
		return nil, proto.StdioSpec{}, errors.New("exec unavailable")
	}
	return fn(ctx, &spec)
}

func launchCodec(s LaunchSpec) tarstream.Codec {
	if s.legacyAccess != nil {
		return s.legacyAccess.LocalCodec
	}
	return s.Storage.LocalCodec()
}
func launchKey(s LaunchSpec) ingest.CustomerKeyFunc {
	if s.legacyAccess != nil {
		return s.legacyAccess.CustomerKeyFn
	}
	return s.Storage.CustomerKeyFunc()
}
func launchRequired(s LaunchSpec) bool {
	if s.legacyAccess != nil {
		return s.legacyAccess.LocalRequired
	}
	return s.Storage.LocalRequired()
}

func (r *Runtime) beginOperation() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != runtimeRunning {
		return fmt.Errorf("operation requires running workload (state %s)", r.state)
	}
	r.opsWG.Add(1)
	return nil
}

// commitLaunch is the successful launch linearization point, after the guest's
// app_started ACK. Close/exit and operation cancellation are checked together.
func (r *Runtime) commitLaunch() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.launchSucceeded {
		return true
	}
	if r.state != runtimeLaunching || r.launchContext == nil || r.launchContext.Err() != nil {
		return false
	}
	r.launchSucceeded = true
	r.state = runtimeRunning
	return true
}

type runtimeConsoleWriter struct {
	owner *Runtime
	dst   io.Writer
}

func (w runtimeConsoleWriter) Write(p []byte) (int, error) {
	if w.owner.consoleTTY.Load() {
		return stdio.CRLF(w.dst).Write(p)
	}
	return w.dst.Write(p)
}
