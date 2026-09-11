package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
	"github.com/kuasar-sandbox/sandboxer/pkg/usage"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
	"golang.org/x/sys/unix"
)

// CmdEnv carries the resolved per-sandbox socket paths + the memfd to a
// caller's BuildCmd closure. ServeAndWait owns both the path layout and
// the memfd, so the cold / restore CH cmdlines (which differ — full
// boot args vs `--restore source_url=`) are the only thing the callers
// supply, computed against paths ServeAndWait already decided.
type CmdEnv struct {
	Memfd  *memory.Memfd
	CHSock string
	// Disks are the CH --disk devices in order (root first, then data disks);
	// each carries its socket + readonly flag. BuildCmd emits one --disk per
	// entry in this order, fixing the guest /dev/vd[a,b,c…] assignment.
	Disks     []DiskArg
	VsockBase string
	UffdSock  string
	RunDir    string

	// TapFDNum is the CH-visible fd of the inherited tap queue (cmd.ExtraFiles
	// after memfd → 4), or 0 when there is no fd-handoff network (tap-name or
	// no network). NetMAC is the effective virtio-net MAC the cmd builder puts
	// in CH's --net mac= (tapfd metadata override or config). See docs/sandbox.md.
	TapFDNum int
	NetMAC   string
}

// PostSpawnCtx is handed to a caller's PostSpawn closure right after CH
// is started. It exposes exactly the shared machinery the two settle
// protocols need:
//
//   - cold start: spawn a goroutine that gates pinger.Start on
//     Launch.HelloDone and local Memory.StartCold + Hooks.Settled on
//     Launch.LaunchAckDone, then return nil (fire-and-forget).
//   - restore: synchronously waitAPI → /vm.resume → guestlink.OpenMUXViaRestore →
//     EstablishMUX → guestlink.Pinger.Start → Hooks.Settled → Memory.StartRestore;
//     Ctx is cancelled by a retained shutdown signal so every synchronous
//     barrier unwinds. A non-signal error aborts and kills CH; a retained signal
//     continues into the normal graceful CH shutdown/escalation path while
//     backend services remain on their separate VM lifecycle context.
type PostSpawnCtx struct {
	Ctx          context.Context
	Cmd          *exec.Cmd
	Pinger       *guestlink.Pinger
	Launch       *guestlink.LaunchServer
	EstablishMUX func(net.Conn, proto.StdioSpec) error
	Hooks        *resctl.ControllerHooks
	Memory       *resctl.MemoryController
	CHSock       string
	Logf         func(string, ...any)
	NotifyReady  func()
}

// VMParams is the input to ServeAndWait — everything the shared
// back-half needs that differs between cold start and restore. The
// callers (sandbox.Run / restore.Run) keep their own thin front-half
// (config resolution, cgroup, controller admit) and converge here.
type VMParams struct {
	Ctx                context.Context
	SandboxID          string
	BaseDir            string
	Balloon            *resctl.BalloonController
	RunDir             string // already created by the caller (caller defers RemoveAll)
	Logf               func(string, ...any)
	StdioMode          stdio.Mode
	PingFatalThreshold int
	StartUnixNs        int64         // T0 for the stats wallclock window
	StatsJSONPath      string        // if non-empty, dump the stats JSON on exit
	StatsInterval      time.Duration // if > 0, periodically log lazy-load stats (uffd + vhost); 0 = off

	CapBytes   int64               // RAM capacity (memfd size); also stats UffdRAMSize
	UffdSource uffd.SnapshotReader // ZeroSource (cold) | snapshot source (restore)

	// Disks are the logical disks served to the guest, in order: Disks[0] is
	// the root, Disks[1:] are the boot.disks[] data disks (boot.disks[] order).
	// ServeAndWait expands each into its vhost device(s) in CH --disk order
	// (single → one writable device; overlay → ro base then rw upper). The
	// guest's /dev/vd[a,b,c…] follow this order.
	Disks []DiskBackend

	LaunchSpec    *proto.LaunchSpec // cold: real spec; restore: &proto.LaunchSpec{} placeholder
	WireLaunchMUX bool              // cold: true (launch conn → MUX); restore: false (MUX via PostSpawn)
	StartTimeout  time.Duration     // host wait for launch_ack (covers guest init); 0 = indefinite
	// VAReportDeadline bounds the CH→host uffd-fd handoff handshake (shared
	// cold/restore); 0 = no forced timeout. From cfg.VAReportDeadline().
	VAReportDeadline time.Duration
	// PingTimeout bounds the host→guest ping round-trip; 0 → no forced timeout
	// (resolved to config.NoForcedTimeout, since DialRaw needs a value). cfg.PingDeadline().
	PingTimeout time.Duration
	// AppNotifyDeadline bounds the host read of one guest→host launch-port
	// message (hello / app_started / app_exited / mem_report); 0 = no forced
	// timeout. From cfg.AppNotifyDeadline().
	AppNotifyDeadline time.Duration
	Hooks             *resctl.ControllerHooks
	Memory            *resctl.MemoryController

	// TapFile, when non-nil, is a tap queue fd acquired via the tapfd handoff
	// (docs/tapfd.md). ServeAndWait inherits it into CH after the memfd (CH
	// fd 4) and surfaces CmdEnv.TapFDNum=4; the caller closes it after the run.
	// NetMAC is the effective virtio-net MAC, surfaced as CmdEnv.NetMAC.
	TapFile *os.File
	NetMAC  string

	// Cgroup, when active, creates CH directly in the per-sandbox cgroup.
	// sandbox-ctl deliberately
	// stays in its parent cgroup — see pkg/sandbox/cgroup.go header for the
	// memcg-throttle deadlock that caused.
	Cgroup *resctl.CgroupController

	// NetnsFile, when non-nil, is the tap's network-namespace fd from the same
	// handoff (docs/tapfd.md §2.5). ServeAndWait fork/execs CH on a thread that
	// setns()'d into it, so CH runs inside the tap's netns. CH does NOT inherit
	// this fd (it's not an ExtraFile); the caller closes it after the run.
	NetnsFile *os.File

	SnapCfg        *config.SandboxConfig // ctl.sock SnapshotHandler.Cfg
	PortableConfig *config.PortableSandboxConfig
	SourceBinding  *RunSourceBinding
	MemoryBinding  *MemorySourceBinding
	ManifestCfg    *config.ManifestConfig
	Fetcher        fetch.Fetcher
	BundleReader   *manifestbundle.Reader
	BundleFetcher  *manifestbundle.ManifestFetcher
	RefLocations   config.RefLocations
	CustomerKeyFn  ingest.CustomerKeyFunc
	LocalCodec     tarstream.Codec
	LocalRequired  bool

	// Forwards are the parsed `--connect` port-forward directives. Each
	// gets a host-local listener whose accepted connections are spliced to
	// a guest-side target via a reverse channel (docs/sandbox-init.md
	// §3.7). Empty → no port forwarding.
	Forwards []ForwardSpec

	BuildCmd  func(CmdEnv) (cmd *exec.Cmd, cleanup func(), err error)
	PostSpawn func(PostSpawnCtx) error

	// NotifyReadiness is optional. ReadyOnAppStarted selects the cold-start
	// barrier; restore instead calls PostSpawnCtx.NotifyReady explicitly after
	// its restore ACK and host-side MUX establishment.
	NotifyReadiness   ReadinessNotify
	ReadyOnAppStarted bool
}

// DiskBackend is one logical disk served to the guest (root or a boot.disks[]
// data disk). Single-disk: Cow only (one writable ext4 device). Overlay:
// Reader (ro erofs base) THEN Cow (rw ext4 upper) — two devices. ServeAndWait
// expands each into its vhost device(s) in CH --disk order. DiffPath/OwnedDiff
// describe the Cow's writable diff for diagnostics and lifecycle cleanup;
// snapshot data comes from Cow.SnapshotView.
type DiskBackend struct {
	Overlay   bool
	Reader    vhost.BlockReader // overlay only (ro base); nil in single-disk
	Cow       *vhost.BlockCOW   // the writable ext4 (always present)
	BasePath  string            // overlay: ro base stats path
	DiffPath  string            // the Cow's diff file (stats/diagnostics/cleanup path)
	OwnedDiff bool              // diff is auto-created (ours) → eligible for zero-copy move on destroy-snapshot
}

// servedDevice is one expanded vhost device (a DiskBackend yields 1 or 2),
// in CH --disk order. ReadOnly marks the CH --disk readonly=on.
type servedDevice struct {
	sock     string
	readonly bool
}

// DiskArg is one CH --disk in order (socket + readonly), handed to BuildCmd so
// the cold/restore cmdlines emit the right device set without re-deriving it.
type DiskArg struct {
	Sock     string
	ReadOnly bool
}

// SnapDiskRef is one logical disk's writable diff for the snapshot path, in
// logical order (root, then data disks). SnapshotView supplies the live COW's
// upper-only logical view; Size supports predictable preflight checks without
// constructing that view, and DiffPath remains for diagnostics and cleanup.
type SnapDiskRef struct {
	DiffPath     string
	OwnedDiff    bool
	Size         int64
	SnapshotView func() (io.ReadSeeker, []sparse.Extent, error)
}

// ServeAndWait owns the half of the sandbox lifecycle that is identical
// between cold start and restore: memfd + uffd va_report handler,
// vhost-blk backends, the launch server (guest→host mem_report /
// app_exited — and, cold only, the launch→MUX upgrade), the pinger,
// the ctl.sock snapshot server, the backend goroutine fan-out, signal
// escalation around CH, and the stats dump. The genuinely divergent
// parts — config resolution, the CH cmdline, the post-spawn settle
// protocol — stay in the callers via VMParams.BuildCmd / PostSpawn.
func ServeAndWait(p VMParams) (int, error) {
	if err := p.Ctx.Err(); err != nil {
		return -1, fmt.Errorf("sandbox start cancelled: %w", err)
	}
	logf := p.Logf
	readiness := newReadinessEmitter(p.NotifyReadiness)
	var usageManager *usage.Manager
	var usageSampler *usage.Sampler
	usageError := ""
	notifyReady := func() {
		if usageSampler != nil {
			usageSampler.Ready()
		}
		readiness.notifyReady()
	}
	runDir := p.RunDir
	chSock := filepath.Join(runDir, "ch.sock")
	vsockBase := filepath.Join(runDir, "vsock.sock")
	launchSock := fmt.Sprintf("%s_%d", vsockBase, proto.LaunchPort)
	uffdSockPath := filepath.Join(runDir, "uffd.sock")
	ctlSockPath := filepath.Join(runDir, "ctl.sock")

	// backendCtx is the context the vhost / launch / va_report / ctl.sock
	// servers run under (and the stdio MUX bridge). Cancelled when CH
	// exits (or earlier via signal escalation); the deferred cancel is a
	// backstop for the early-error returns below.
	backendCtx, cancelBackends := context.WithCancel(VMLifecycleContext(p.Ctx))
	defer cancelBackends()

	// Exactly one stdio MUX at a time; which conn backs it changes across
	// launch → restore → attach. muxLink guards the pair so the snapshot
	// handler can re-attach (snapshot --resume) without racing teardown.
	var muxLink guestlink.MUXLink
	reattach := func() error {
		return muxLink.Reattach(backendCtx, &guestlink.HostClient{BasePath: vsockBase, Logf: logf}, p.StdioMode)
	}
	establishMUX := func(conn net.Conn, spec proto.StdioSpec) error {
		ss := stdio.StreamSetFor(spec)
		sess := mux.NewSession(conn, ss, mux.Options{})
		cleanup, err := p.StdioMode.Bridge(backendCtx, sess, ss)
		if err != nil {
			_ = sess.Close()
			return err
		}
		muxLink.Set(sess, cleanup)
		return nil
	}

	// Memory setup. sandbox-ctl owns the memfd; CH inherits it via
	// cmd.ExtraFiles[0]=memfd. The uffd is created in CH's process and
	// handed back via SCM_RIGHTS in the va_report message.
	memfd, err := memory.Create("sandbox-"+p.SandboxID+"-ram", p.CapBytes)
	if err != nil {
		return -1, fmt.Errorf("memfd create: %w", err)
	}
	defer memfd.Close()

	// AddressMap is shared between the va_report server (registers
	// ProcessCH on receive) and the uffd Handler (Locate on each fault).
	addrMap := uffd.NewAddressMap(uint64(memfd.Size()))
	if err := addrMap.RegisterVMA(uffd.ProcessBackend, uint64(memfd.Addr()), uint64(memfd.Size()), 0); err != nil {
		return -1, fmt.Errorf("addrmap register backend: %w", err)
	}

	// uffdHandler is constructed inside the va_report OnReady callback
	// once we have CH's uffd fd. Held here so the deferred Close can
	// reach it after ctx cancellation.
	var uffdHandler *uffd.Handler
	var uffdHandlerMu sync.Mutex
	defer func() {
		uffdHandlerMu.Lock()
		h := uffdHandler
		uffdHandlerMu.Unlock()
		if h != nil {
			_ = h.Close()
		}
	}()

	vaReportSrv := &uffd.VAReportServer{
		Path:              uffdSockPath,
		AddrMap:           addrMap,
		Logf:              logf,
		HandshakeDeadline: p.VAReportDeadline,
		OnReady: func(uffdFD int, vaStart, size uint64) error {
			numWorkers := 0
			if p.SnapCfg != nil && p.SnapCfg.Resources.Capacity.CPU > 0 {
				numWorkers = int(p.SnapCfg.Resources.Capacity.CPU)
			}
			h, err := uffd.NewWithBackendUffd(uffdFD, addrMap, uffd.Config{
				MemfdFD:    memfd.FD(),
				BackendVA:  memfd.Addr(),
				Size:       memfd.Size(),
				Source:     p.UffdSource,
				NumWorkers: numWorkers,
				Logf:       logf,
			})
			if err != nil {
				return err
			}
			h.Start()
			uffdHandlerMu.Lock()
			uffdHandler = h
			uffdHandlerMu.Unlock()
			logf("uffd handler: adopted CH uffd region #0 fd=%d size=%d", uffdFD, size)
			return nil
		},
		OnRegister: func(uffdFD int, vaStart, size uint64) error {
			uffdHandlerMu.Lock()
			h := uffdHandler
			uffdHandlerMu.Unlock()
			if h == nil {
				return fmt.Errorf("OnRegister called before OnReady (no handler yet)")
			}
			if err := h.AddUffd(uffdFD); err != nil {
				return err
			}
			logf("uffd handler: attached additional region fd=%d size=%d", uffdFD, size)
			return nil
		},
	}
	if err := vaReportSrv.Listen(); err != nil {
		return -1, fmt.Errorf("va_report listen: %w", err)
	}
	defer vaReportSrv.Stop()

	// vhost-blk backends. Each logical disk (root + data disks) expands to its
	// vhost device(s) in CH --disk order: overlay → ro erofs base (blkN) then
	// rw ext4 upper (blkN+1); single → one rw ext4 (blkN). The device sockets
	// blk0.sock, blk1.sock, … are numbered in this order, fixing the guest's
	// /dev/vd[a,b,c…]. diskServers groups, per logical disk, the writable Cow's
	// snapshot view plus DiffPath/OwnedDiff lifecycle metadata.
	var servers []*vhost.Server // all vhost servers, in device order
	var devs []servedDevice     // CH --disk args, in device order
	var snapDisks []SnapDiskRef // per logical disk: writable diff for snapshot
	addServer := func(sock string, backend vhost.Backend, label, path string, ro bool) error {
		s := vhost.NewServer(sock, backend, logf)
		s.EnableStats(label, path)
		s.SetMemfd(memfd.Inode(), memfd.Bytes())
		if err := s.Listen(); err != nil {
			return err
		}
		servers = append(servers, s)
		devs = append(devs, servedDevice{sock: sock, readonly: ro})
		return nil
	}
	stopServers := func() {
		for _, s := range servers {
			s.Stop()
		}
	}
	for _, d := range p.Disks {
		if d.Overlay {
			i := len(devs)
			sock := filepath.Join(runDir, fmt.Sprintf("blk%d.sock", i))
			if err := addServer(sock, &vhost.ReadOnlyBackend{R: d.Reader}, fmt.Sprintf("blk%d", i), d.BasePath, true); err != nil {
				stopServers()
				return -1, err
			}
		}
		i := len(devs)
		sock := filepath.Join(runDir, fmt.Sprintf("blk%d.sock", i))
		if err := addServer(sock, &vhost.CowBackend{C: d.Cow}, fmt.Sprintf("blk%d", i), d.DiffPath, false); err != nil {
			stopServers()
			return -1, err
		}
		snapDisks = append(snapDisks, SnapDiskRef{
			DiffPath:     d.DiffPath,
			OwnedDiff:    d.OwnedDiff,
			Size:         d.Cow.Size(),
			SnapshotView: d.Cow.SnapshotView,
		})
	}

	// guestlink.LaunchServer: guest→host management short-conns on
	// <vsock-base>_5000. Both cold and restore need this for the
	// periodic mem_report (sandbox-local resctl.MemoryController) and app_exited.
	// Cold additionally upgrades the hello/launch_ack conn to the stdio
	// MUX (OnMUXReady); restore's MUX comes from the reverse channel
	// (PostSpawn → guestlink.OpenMUXViaRestore), so OnMUXReady stays nil there and
	// the placeholder Spec is never sent (guest doesn't re-hello after a
	// restore).
	launch := &guestlink.LaunchServer{
		Path:              launchSock,
		Spec:              p.LaunchSpec,
		StartTimeout:      p.StartTimeout,
		AppNotifyDeadline: p.AppNotifyDeadline,
		Logf:              logf,
		OnAppStarted: func(pid int) {
			logf("guest reports user app pid=%d", pid)
			if p.ReadyOnAppStarted {
				notifyReady()
			}
		},
		OnAppExited: func(code int) { logf("guest reports user app exited code=%d", code) },
		// Open the cold observation barrier synchronously before launch_ack is
		// acknowledged to the guest. The reporter starts immediately after the
		// guest's launch path; deferring this to a separate LaunchAckDone waiter
		// could ACK and then discard its first report due to scheduler ordering.
		OnLaunchAck: func() {
			if p.Memory != nil {
				p.Memory.StartCold(p.Ctx)
			}
		},
		OnMemReport: func(report proto.MemReport) bool {
			if p.Memory != nil {
				return p.Memory.SubmitGuestReport(report)
			}
			return true
		},
	}
	if p.WireLaunchMUX {
		launch.OnMUXReady = func(conn net.Conn, established proto.StdioSpec) {
			if err := establishMUX(conn, established); err != nil {
				logf("stdio MUX bridge: %v", err)
				return
			}
			logf("stdio MUX established (tty=%v stdin=%v stdout=%v stderr=%v)",
				established.TTY, established.Stdin, established.Stdout, established.Stderr)
		}
	}
	if err := launch.Listen(); err != nil {
		stopServers()
		return -1, err
	}

	// guestlink.Pinger drives the host→guest health probe. Started by PostSpawn
	// (cold: after launch handshake; restore: after restore_ack). 0 ping
	// timeout → no forced timeout (resolved to config.NoForcedTimeout, since the
	// ping RoundTrip dials and DialRaw needs a finite value).
	pingTO := p.PingTimeout
	if pingTO <= 0 {
		pingTO = config.NoForcedTimeout
	}
	pinger := &guestlink.Pinger{
		Client: &guestlink.HostClient{BasePath: vsockBase, Logf: logf},
		Cfg:    guestlink.PingerConfig{FatalThreshold: p.PingFatalThreshold, Timeout: pingTO},
		Stats:  &guestlink.PingStats{},
		Logf:   logf,
	}

	if p.SnapCfg != nil && p.SnapCfg.Usage.Enabled {
		sampleInterval, flushInterval, usageErr := p.SnapCfg.Usage.Intervals()
		if usageErr == nil {
			usageErr = os.MkdirAll(p.BaseDir, 0o755)
		}
		var epoch [16]byte
		if usageErr == nil {
			_, usageErr = rand.Read(epoch[:])
		}
		if usageErr == nil {
			usageManager, usageErr = usage.Open(p.BaseDir, p.SandboxID, fmt.Sprintf("%x", epoch), time.Now(), sampleInterval, flushInterval)
		}
		if usageErr == nil {
			usageSampler, usageErr = usage.NewSampler(usageManager, p.SnapCfg.Resources.Capacity.CPU, len(p.Disks), pinger.Client, p.Balloon, nil)
		}
		if usageErr != nil {
			// A failed sampler has released the freshly opened manager without
			// saving. Do not expose or close that uninitialized run as live usage.
			usageManager = nil
			usageError = usageErr.Error()
			logf("usage unavailable: %v", usageErr)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if usageSampler != nil {
				usageSampler.Stop(ctx)
			}
		}()
	}

	// Port-forward listeners. Built here so the snapshot handler can pause
	// + collapse active relays around quiesce (symmetric with the pinger);
	// started below alongside the other backend servers.
	forwarder := NewForwarder(vsockBase, logf)
	cgroupPath := ""
	if p.Cgroup != nil {
		cgroupPath = p.Cgroup.LocalPath()
	}
	chExited := make(chan struct{})
	var chExitedOnce sync.Once
	markCHExited := func() { chExitedOnce.Do(func() { close(chExited) }) }
	defer markCHExited()
	var chProcessMu sync.RWMutex
	var chProcess *os.Process
	currentCHProcess := func() processSignaler {
		chProcessMu.RLock()
		defer chProcessMu.RUnlock()
		return chProcess
	}

	// ctl.sock server for snapshot requests. SnapshotHandler is the
	// shared bundle both Run and restore.Run use.
	snapHandler := &SnapshotHandler{
		Cfg:            p.SnapCfg,
		PortableConfig: p.PortableConfig,
		SourceBinding:  p.SourceBinding,
		MemoryBinding:  p.MemoryBinding,
		ManifestCfg:    p.ManifestCfg,
		Fetcher:        p.Fetcher,
		BundleReader:   p.BundleReader,
		BundleFetcher:  p.BundleFetcher,
		RefLocations:   p.RefLocations,
		CustomerKeyFn:  p.CustomerKeyFn,
		LocalCodec:     p.LocalCodec,
		LocalRequired:  p.LocalRequired,
		SandboxID:      p.SandboxID,
		Memfd:          memfd,
		Disks:          snapDisks,
		Servers:        servers,
		CHSock:         chSock,
		RunDir:         runDir,
		Pinger:         pinger,
		Forwarder:      forwarder,
		Reattach:       reattach,
		Memory:         p.Memory,
		usageSampler:   usageSampler,
		CHProcess:      currentCHProcess,
		Context:        backendCtx,
		Logf:           logf,
	}
	ctlSrv := &ctl.Server{
		Path: ctlSockPath,
		Logf: logf,
		UsageHandler: func(req ctl.Request) (ctl.Response, error) {
			view := usage.View{Enabled: p.SnapCfg != nil && p.SnapCfg.Usage.Enabled, ReadError: usageError}
			if usageManager != nil {
				view = usageManager.View()
				if usageError != "" {
					view.ReadError = usageError
				}
			}
			if req.UsageHistory {
				if usageManager == nil {
					return ctl.Response{}, errors.New("usage history unavailable; read the saved file offline")
				}
				body, err := marshalUsageHistory(usageManager.History, view.SavedEnd, req.UsageCursor, req.UsageLimit)
				return ctl.Response{Usage: body}, err
			}
			body, err := json.Marshal(view)
			return ctl.Response{Usage: body}, err
		},
		SnapshotHandler: func(req ctl.Request) (ctl.Response, error) {
			return snapHandler.handle(req, cgroupPath, chExited)
		},
		ExportHandler: func(req ctl.Request) (ctl.Response, error) {
			return snapHandler.handleExport(req, cgroupPath, chExited)
		},
		ExecHandler: func(conn net.Conn, req ctl.Request) {
			execCtx, admitted := forwarder.beginExec(backendCtx, conn)
			if !admitted {
				defer conn.Close()
				_ = ctl.WriteMessage(conn, ctl.Response{Type: ctl.TypeError, Msg: "exec unavailable during capture"})
				return
			}
			defer forwarder.endExec(conn)
			guestlink.ServeExecRequest(execCtx, conn, req, vsockBase, logf)
		},
	}
	if err := ctlSrv.Listen(); err != nil {
		stopServers()
		return -1, fmt.Errorf("ctl.sock listen: %w", err)
	}
	defer ctlSrv.Stop()
	// Listen has completed and cleanup is registered. A connection made now
	// can wait in the kernel accept queue until ctlSrv.Serve starts below.
	readiness.notifyControlReady()
	// By function return the servers are already stopped (explicit
	// cancelBackends()+backendWG.Wait() below), so this just closes the
	// MUX conn and runs the bridge cleanup (restores the terminal in tty
	// mode, drains the guest→host pumps).
	defer muxLink.Teardown()

	var backendWG sync.WaitGroup
	backendWG.Add(len(servers) + 3) // N vhost servers + launch + vaReport + ctl
	for _, s := range servers {
		go func() { defer backendWG.Done(); _ = s.Serve(backendCtx) }()
	}
	go func() { defer backendWG.Done(); _ = launch.Serve(backendCtx) }()
	go func() { defer backendWG.Done(); _ = vaReportSrv.Serve(backendCtx) }()
	go func() { defer backendWG.Done(); _ = ctlSrv.Serve(backendCtx) }()

	// Optional periodic lazy-load stats logger (observe a slow remote/cache or
	// backed-up fault queue in real time; quiet once warm). Ends on backendCtx.
	if p.StatsInterval > 0 {
		backendWG.Add(1)
		go func() {
			defer backendWG.Done()
			lazyStatsTicker(backendCtx, p.StatsInterval, func() *uffd.Handler {
				uffdHandlerMu.Lock()
				defer uffdHandlerMu.Unlock()
				return uffdHandler
			}, servers, logf)
		}()
	}

	// Port-forward accept loops run under backendCtx (they end when CH
	// exits / the run unwinds). A bad listener (e.g. fd= not a socket)
	// aborts the run. Close on the way out tears down listeners + relays.
	if err := forwarder.Start(backendCtx, p.Forwards); err != nil {
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("port-forward: %w", err)
	}
	defer forwarder.Close()

	defer pinger.Stop()
	defer func() {
		if p.Memory != nil {
			p.Memory.Stop()
		}
	}()

	// memfd is cmd.ExtraFiles[0] → CH fd 3; an optional tapfd-handoff queue
	// fd is appended next → CH fd 4 (referenced by --net fd= / restore net_fds).
	tapFDNum := 0
	if p.TapFile != nil {
		tapFDNum = 4
	}
	// CH --disk args in device order (root first, then data disks); BuildCmd
	// (CHCommand / restore config rewrite) emits one --disk per entry.
	diskArgs := make([]DiskArg, len(devs))
	for i, d := range devs {
		diskArgs[i] = DiskArg{Sock: d.sock, ReadOnly: d.readonly}
	}
	cmd, chStdioCleanup, err := p.BuildCmd(CmdEnv{
		Memfd:     memfd,
		CHSock:    chSock,
		Disks:     diskArgs,
		VsockBase: vsockBase,
		UffdSock:  uffdSockPath,
		RunDir:    runDir,
		TapFDNum:  tapFDNum,
		NetMAC:    p.NetMAC,
	})
	if err != nil {
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("build CH cmd: %w", err)
	}
	defer chStdioCleanup()
	// fd=3 ← memfd in CH (after stdin/out/err). New process group keeps
	// CH out of sandbox-ctl's controlling-terminal foreground group, so
	// terminal-generated ^C/^\/^Z don't hit CH directly — sandbox-ctl
	// owns signal handling (below).
	cmd.ExtraFiles = []*os.File{memfd.File()}
	if p.TapFile != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, p.TapFile) // CH fd 4
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := p.Cgroup.ConfigureSysProcAttr(cmd.SysProcAttr); err != nil {
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("cgroup: configure CH process: %w", err)
	}

	// sandbox-ctl registers this stream before Admit so an early shutdown cannot
	// fall between admission and CH signal registration. Library callers without
	// that context retain the original local registration behavior.
	var sigCh <-chan os.Signal
	if inherited := runSignalsFromContext(p.Ctx); inherited != nil {
		sigCh = inherited
	} else {
		localSignals := make(chan os.Signal, 4)
		signal.Notify(localSignals, syscall.SIGTERM, syscall.SIGINT)
		defer signal.Stop(localSignals)
		sigCh = localSignals
	}

	// A shutdown received while BuildCmd or any preceding setup was in flight
	// must not create a new VM. The retained signal is consumed below only when
	// CH crossed this final pre-spawn boundary.
	if err := p.Ctx.Err(); err != nil {
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("sandbox start cancelled: %w", err)
	}
	if err := startCH(cmd, p.NetnsFile); err != nil {
		cancelBackends()
		backendWG.Wait()
		return -1, fmt.Errorf("spawn CH: %w", err)
	}
	chProcessMu.Lock()
	chProcess = cmd.Process
	chProcessMu.Unlock()
	chPid := cmd.Process.Pid
	logf("CH started pid=%d", chPid)
	if usageSampler != nil {
		usageSampler.Start(backendCtx, chPid)
	}
	// This goroutine is the sole reaper on every post-spawn path, including
	// failed restore setup. WNOWAIT keeps native process counters readable.
	doneCh := make(chan error, 1)
	go func() {
		if usageSampler != nil {
			var info unix.Siginfo
			waitIDErr := observeCHExit(func() error {
				return unix.Waitid(unix.P_PID, chPid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			}, usageSampler.FinalCH)
			if waitIDErr != nil {
				logf("usage final waitid: %v", waitIDErr)
			}
		}
		waitErr := cmd.Wait()
		markCHExited()
		doneCh <- waitErr
	}()

	// PingFatalThreshold wiring: when the threshold is hit (opt-in),
	// SIGTERM CH so cmd.Wait returns; waitForCHWithSignalEscalation then
	// escalates to SIGKILL after chShutdownGrace if CH doesn't drain.
	pinger.SetOnFatal(func(err error) {
		logf("pinger fatal threshold (%d) hit: %v — SIGTERMing CH pid=%d",
			pinger.Cfg.FatalThreshold, err, chPid)
		_ = cmd.Process.Signal(syscall.SIGTERM)
	})

	if err := p.PostSpawn(PostSpawnCtx{
		Ctx:          p.Ctx,
		Cmd:          cmd,
		Pinger:       pinger,
		Launch:       launch,
		EstablishMUX: establishMUX,
		Hooks:        p.Hooks,
		Memory:       p.Memory,
		CHSock:       chSock,
		Logf:         logf,
		NotifyReady:  notifyReady,
	}); err != nil {
		if !runShutdownRequested(p.Ctx) {
			_ = cmd.Process.Kill()
			<-doneCh
			cancelBackends()
			backendWG.Wait()
			return -1, err
		}
		// A retained signal interrupted the synchronous restore barrier. Keep
		// backend services alive and fall through so the queued signal drives
		// the normal CH shutdown/escalation protocol.
		logf("post-spawn interrupted by shutdown: %v", err)
	}

	waitErr := waitForCHWithSignalEscalation(doneCh, sigCh, cmd.Process, chPid, chSock, cgroupPath, p.SnapCfg.CHApiDeadline(), chShutdownGrace, logf)
	cancelBackends()
	backendWG.Wait()
	exit := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exit = exitErr.ExitCode()
		} else {
			return -1, fmt.Errorf("CH wait: %w", waitErr)
		}
	}
	logf("CH exited code=%d", exit)

	// Route stats through log.Default().Writer(): when stdio.Bridge ran
	// in tty mode it CRLF-wrapped that writer (paired with makeRaw), so
	// these multi-line blocks don't stairstep on a still-raw terminal.
	statsW := log.Default().Writer()
	for _, s := range servers {
		_, _ = s.WriteStatsTo(statsW)
	}
	uffdHandlerMu.Lock()
	h := uffdHandler
	uffdHandlerMu.Unlock()
	if h != nil {
		writeUffdStats(statsW, h.Stats())
	}
	if p.StatsJSONPath != "" {
		bundle := statsBundle{
			Servers:     servers,
			StartUnixNs: p.StartUnixNs,
			EndUnixNs:   time.Now().UnixNano(),
		}
		if h != nil {
			bundle.Uffd = h.Stats()
			bundle.UffdRAMSize = p.CapBytes
		}
		if pinger.Stats != nil {
			snap := pinger.Stats.Snapshot()
			bundle.Ping = &snap
		}
		if err := writeStatsJSON(p.StatsJSONPath, bundle); err != nil {
			logf("stats json write %s: %v", p.StatsJSONPath, err)
		} else {
			logf("stats json written to %s", p.StatsJSONPath)
		}
	}
	return exit, nil
}

// startCH starts cmd. When netnsFile is non-nil the fork/exec runs on a thread
// moved into that network namespace (docs/tapfd.md §2.5), so CH — and thus the
// guest's virtio-net — lives inside the tap's netns; otherwise CH starts in the
// host netns. cmd.Process is populated by the time this returns.
func startCH(cmd *exec.Cmd, netnsFile *os.File) error {
	if netnsFile == nil {
		return cmd.Start()
	}
	errCh := make(chan error, 1)
	go func() {
		// setns(CLONE_NEWNET) changes only the calling thread's netns, so lock
		// the goroutine to its OS thread: the scheduler must not migrate us and
		// the change must not leak to other goroutines. We deliberately never
		// UnlockOSThread — this thread now sits in the tap's netns, so let the
		// runtime retire it when the goroutine returns rather than reuse it for
		// host-netns work.
		runtime.LockOSThread()
		if err := unix.Setns(int(netnsFile.Fd()), unix.CLONE_NEWNET); err != nil {
			errCh <- fmt.Errorf("setns(CLONE_NEWNET): %w", err)
			return
		}
		// fork/exec inherits this thread's netns → CH lands in the tap's netns.
		errCh <- cmd.Start()
	}()
	return <-errCh
}
