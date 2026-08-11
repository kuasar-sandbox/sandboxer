// Package proto defines the wire protocol for the sandbox-ctl ↔
// sandbox-init control channel that runs over virtio-vsock, plus the
// constants shared with the stdio MUX sub-protocol (pkg/mux).
//
// Two kinds of connections (docs/sandbox-init.md §4):
//
//	(1) Management short connections — one request + one response per
//	    connection, then close. Both directions reuse port 5000:
//	      - guest → host: sandbox-init dial(CID=2, port=5000); CH hybrid
//	        vsock proxies to "<vsock-base>_5000" UDS that sandbox-ctl
//	        listens on. Carries: hello/launch handshake, app_started,
//	        app_exited, mem_report.
//	      - host → guest: sandbox-ctl dial(<vsock-base>) UDS, write
//	        "CONNECT 5000\n" first; CH proxies the rest to guest port
//	        5000 listener. Carries: ping, quiesce, restore, attach.
//	    Wire: [4 bytes LE length][JSON Message].
//
//	(2) The MUX long connection — at most one. The launch / restore /
//	    attach operations DON'T close their connection after the *_ack:
//	    it stays open and switches to the framed MUX sub-protocol
//	    (pkg/mux) which carries the app's stdin/stdout/stderr
//	    (or a pty). See StdioSpec for what's negotiated in the *_ack.
//
//	(3) Forward long connections — 0..N. Each `sandbox-ctl run --connect`
//	    port forward opens one reverse-channel conn per accepted local
//	    connection: a `connect{ConnectSpec}` → `connect_ack` handshake,
//	    then the conn switches to the fwd frame sub-protocol
//	    (pkg/fwd) which splices the bytes to a guest-side dial
//	    target with TCP half-close preserved (docs/sandbox-init.md §3.7).
//
// This package is dependency-light (stdlib only) so the guest
// sandbox-init binary can import it without dragging in YAML or other
// heavy deps.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kuasar-sandbox/sandboxer/internal/wireio"
)

// VsockHostCID is the well-known guest-side address of the host (always 2).
const VsockHostCID = 2

// VsockGuestCID is the CID we assign to our guest VM. Any value > 2 works;
// 3 is conventional for single-VM-per-host setups.
const VsockGuestCID = 3

// LaunchPort is the vsock port both directions use. sandbox-init dials
// host:LaunchPort for the cold-start hello/launch handshake and for
// app_started/app_exited/mem_report notifications; sandbox-init also
// listens on guest:LaunchPort so sandbox-ctl can push ping/restore/
// quiesce/attach.
const LaunchPort = 5000

// MaxMessageBytes bounds the JSON payload size for one management message.
// 1 MiB leaves headroom for launch / restore specs that carry inline file
// content (FileSpec.Content) and mount/init lists; larger per-instance data
// belongs on a device, not the control channel.
const MaxMessageBytes = 1 << 20

// HostConnectLine is the ASCII prefix sandbox-ctl writes as the first
// bytes of a host→guest connection, telling CH's hybrid vsock proxy
// which guest port to connect to. CH consumes this line and forwards
// everything after it to the guest listener. CH then writes back an
// "OK <local_port>\n" line which the caller must drain before reading
// any protocol payload. Trailing "\n" is required.
var HostConnectLine = []byte(fmt.Sprintf("CONNECT %d\n", LaunchPort))

// --- LaunchSpec ------------------------------------------------------

// LaunchSpec is the payload sandbox-ctl pushes to sandbox-init telling
// it how to start the user application after rootfs is assembled.
//
// Fields mirror the merged result of:
//   - the OCI image config.json appended to boot.root.base (defaults)
//   - the sandbox.yaml `launch:` section (overrides)
//
// Network (optional) carries the IP-layer config for sandbox-init's
// netlink-based applyNetwork pass. nil → no network configuration.
// Stdio carries the app stdio mode the host wants (see StdioSpec).
type LaunchSpec struct {
	Exec    string            `json:"exec"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	// CgroupControl selects the application cgroup topology. false runs the
	// application in the cgroup-namespace root (/); true keeps that real root
	// empty, delegates its controllers, and runs sandbox-init-managed processes
	// in /init. The namespace/freezer root is real /app in both modes.
	CgroupControl bool `json:"cgroup_control,omitempty"`
	// Placeholder, when true, runs no external program: the app child does
	// all its namespace/cgroup/stdio setup but, instead of execve, waits for
	// SIGTERM/SIGINT and exits — an empty "anchor" app for a sandbox driven
	// entirely by `exec`. Exec/Args (and any image Entrypoint/Cmd) are ignored.
	// A placeholder always uses Restart=always (host forces it) so killing it
	// from an exec session restarts it rather than reboots the sandbox.
	Placeholder bool `json:"placeholder,omitempty"`
	// Restart is the app's restart policy: never (default) | on-failure |
	// always. never ⇒ app exit reboots the sandbox (one-shot). on-failure ⇒
	// restart in place on non-zero/​signal exit, reboot on clean exit. always
	// ⇒ always restart in place. Restarts use the shared backoff (10ms→60s,
	// reset after 60s uptime); the stdio MUX survives across restarts.
	Restart string       `json:"restart,omitempty"`
	Network *NetworkSpec `json:"network,omitempty"`
	Stdio   StdioSpec    `json:"stdio,omitempty"`

	// SharePID selects the app's PID namespace: false (default) ⇒ the app is
	// PID 1 of its own namespace (CLONE_NEWPID); true ⇒ the app runs in
	// sandbox-init's PID namespace, so sandbox-init (PID 1) reaps the app and
	// any orphaned descendants (config: launch.pid_namespace=shared).
	SharePID bool `json:"share_pid,omitempty"`

	// Plugins are companion ("plugin") processes launched alongside the app,
	// in the same guest rootfs + cgroup, each supervised by its own restart
	// policy + the shared backoff. A plugin exit never affects the sandbox
	// lifecycle (only the app's exit does). docs/sandbox-init.md §3.2.
	Plugins []PluginSpec `json:"plugins,omitempty"`

	// User is the run-as identity for the app process: "uid:gid" or
	// "name:group" (named forms resolved guest-side against the rootfs
	// /etc/passwd). Empty → root.
	User string `json:"user,omitempty"`
	// StopSignal is the signal number sandbox-init forwards to the app on
	// shutdown (host already resolved any signal name → number). 0 → SIGTERM.
	StopSignal int `json:"stop_signal,omitempty"`
	// StopGraceSec is how long to wait after StopSignal before SIGKILL.
	// 0 → guest default (10 s).
	StopGraceSec int `json:"stop_grace_sec,omitempty"`

	// Mounts / Files / Init drive guest environment setup before the app
	// is forked (docs/sandbox-init.md §3.1-§3.2). Mounts: tmpfs + empty
	// (volume) mounts. Files: content injected via tmpfs+bind (memory-only).
	// Init: one-shot commands run sequentially (initContainers semantics).
	Mounts []MountSpec `json:"mounts,omitempty"`
	Files  []FileSpec  `json:"files,omitempty"`
	Init   []InitSpec  `json:"init,omitempty"`
}

// MountSpec is one declarative guest mount. Type is "tmpfs", "empty"
// (volume dir masking image content; empty → "empty"), or "disk" (a
// boot.disks[] data disk, assembled + bound onto Target before switch-root).
// Options is a mount option string (e.g. "nosuid,nodev,mode=1777").
//
// For Type=="disk" the host resolves the config's source disk name to its
// ordinal + device(s); the name itself is not sent. DiskIndex is the
// boot.disks[] ordinal (used only for the /sysdisks/disk-<N> staging label).
// DiskOverlay selects two-disk overlay mode (ro erofs base + rw ext4 upper)
// vs single-disk (one rw ext4). DiskDevs are the resolved guest block
// devices in CH --disk order: single = [rw-ext4]; overlay = [ro-erofs, rw-ext4].
type MountSpec struct {
	Target  string `json:"target"`
	Type    string `json:"type,omitempty"`
	Source  string `json:"source,omitempty"`
	Options string `json:"options,omitempty"`

	DiskIndex   int      `json:"disk_index,omitempty"`
	DiskOverlay bool     `json:"disk_overlay,omitempty"`
	DiskDevs    []string `json:"disk_devs,omitempty"`
}

// FileSpec is one file injected into the guest rootfs at Path. Content is
// inline (text). Mode is an octal string ("0644"; empty → 0644). Owner is
// "uid:gid" or "name:group" (empty → 0:0). ReadOnly makes the bind
// read-only. Injection is tmpfs-backed + bind, so content never lands on
// the writable disk (suitable for secrets).
type FileSpec struct {
	Path     string `json:"path"`
	Content  string `json:"content,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Owner    string `json:"owner,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// InitSpec is one one-shot init command run (in order, to completion)
// before the app is forked (initContainers semantics: a non-zero exit — or
// a TimeoutMs overrun — aborts sandbox startup). User is an optional run-as
// identity (same form as LaunchSpec.User).
type InitSpec struct {
	Exec      string            `json:"exec"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`     // merged over the default PATH
	Workdir   string            `json:"workdir,omitempty"` // cwd; empty → "/"
	User      string            `json:"user,omitempty"`
	TimeoutMs int64             `json:"timeout_ms,omitempty"` // 0 → no timeout
}

// PluginSpec is one companion ("plugin") process run alongside the app and
// supervised independently (LaunchSpec.Plugins). Restart is its policy
// (never | on-failure | always; empty → always — plugins are meant to stay
// up). It runs in the same guest rootfs + cgroup as the app, with stdout/
// stderr on the guest console. A plugin's exit never reboots the sandbox.
type PluginSpec struct {
	Exec    string            `json:"exec"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	User    string            `json:"user,omitempty"`
	Restart string            `json:"restart,omitempty"`
}

// NetworkSpec is the resolved guest IP-layer config sandbox-init applies
// via netlink. Cold start applies it to a fresh iface (LaunchSpec.Network);
// restore re-applies it flush-and-replace (Message.Network on the restore
// notify) so a clone takes a fresh identity. Empty fields fall back to "skip
// that step" (empty Nexthop → no default route, MTU 0 → leave kernel default).
type NetworkSpec struct {
	Interface string `json:"interface,omitempty"`
	IPCIDR    string `json:"ip_cidr,omitempty"`
	Nexthop   string `json:"nexthop,omitempty"`
	MTU       int    `json:"mtu,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
}

// StdioSpec describes the app's stdin/stdout/stderr wiring. It appears
// in LaunchSpec (what the host wants) and is echoed back — possibly
// narrowed — in launch_ack / restore_ack / attach_ack (what the guest
// actually set up). It maps directly onto the MUX stream set
// (docs/sandbox-init.md §3.5 / §4.5):
//
//   - TTY == true  → pty mode: the app gets a real pty (isatty()=true);
//     MUX streams = {control, pty}. stdin/stdout/stderr ignored
//     (stdout+stderr merge onto the pty). Winsize is the initial size.
//   - TTY == false → pipe mode: MUX streams = {control} ∪ {stdin if
//     Stdin, stdout if Stdout, stderr if Stderr}. An app fd with no
//     channel is wired to /dev/null (stdin) by the guest.
type StdioSpec struct {
	TTY     bool     `json:"tty,omitempty"`
	Winsize *Winsize `json:"winsize,omitempty"`
	Stdin   bool     `json:"stdin,omitempty"`
	Stdout  bool     `json:"stdout,omitempty"`
	Stderr  bool     `json:"stderr,omitempty"`
}

// Winsize is a terminal window size (cols × rows).
type Winsize struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// --- ExecSpec --------------------------------------------------------

// ExecSpec is the payload of an `exec` reverse-channel op: run an
// ad-hoc command inside the already-running sandbox (a sibling of the
// user app, not a replacement). It is independent of LaunchSpec — exec
// sessions are concurrent and short-lived, each with its own stdio MUX.
//
// Argv[0] is the program: resolved via PATH lookup when it has no '/'.
// Env is merged over a default PATH (empty → just the default PATH).
// Cwd empty / "/" → the guest root. Stdio works exactly like
// LaunchSpec.Stdio (pty iff TTY, else the requested pipe subset).
type ExecSpec struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
	// User is an optional run-as identity for the command: "uid[:gid]"
	// or a user name resolved against the APP rootfs's /etc/passwd
	// (same forms as LaunchSpec.User). Empty keeps the session's
	// default (root).
	User  string    `json:"user,omitempty"`
	Stdio StdioSpec `json:"stdio,omitempty"`
}

// --- ConnectSpec ------------------------------------------------------

// ConnectSpec is the payload of a `connect` reverse-channel op: obtain one
// guest-side stream connection at Address and splice it to the host-side
// listener that accepted the local connection. It backs `sandbox-ctl run
// --connect` port forwarding. How the guest obtains that connection is set
// by Accept (docs/sandbox-init.md §3.7):
//
//   - Accept == false (dial mode, `LOCAL:TARGET`): guest `net.Dial`s
//     Address — reaches a server already listening inside the sandbox.
//   - Accept == true (accept mode, `LOCAL::TARGET`): guest `Listen`s on
//     Address (lazily, once per address, cached) and `Accept`s one
//     connection — pairs the host client with a guest client that connects
//     out to Address. The accept may block arbitrarily long, so the host
//     parks for connect_ack without the dial deadline.
//
// After connect_ack the reverse-channel conn switches to the fwd frame
// sub-protocol (pkg/fwd), which carries the spliced bytes with TCP
// half-close preserved. Each accepted local connection gets its own
// ConnectSpec / reverse-channel conn — forwards are concurrent and
// independent (the port-forward analogue of exec sessions).
type ConnectSpec struct {
	// Network is the guest-side network: "tcp" (default), "tcp4", "tcp6",
	// or "unix".
	Network string `json:"network,omitempty"`
	// Address is the guest-side target: a dial target (Accept==false) or a
	// listen address (Accept==true), e.g. "127.0.0.1:49983" (or a path for
	// network "unix").
	Address string `json:"address"`
	// Accept selects accept mode: the guest Listen+Accepts on Address
	// instead of dialing it. Default false (dial mode).
	Accept bool `json:"accept,omitempty"`
}

// App lifecycle state reported in restore_ack / attach_ack.
const (
	AppStateRunning = "running"
	AppStateExited  = "exited"
)

// --- Message ---------------------------------------------------------

// Message is the typed envelope for management-connection messages.
// Only fields relevant to Type are populated.
type Message struct {
	Type string `json:"type"`

	// hello: guest → host first message; phase carries an optional hint
	// (e.g. "ready"). v1 just uses presence of the message.
	Phase string `json:"phase,omitempty"`

	// launch: host → guest immediately after hello.
	Launch *LaunchSpec `json:"launch,omitempty"`

	// launch_ack / restore_ack / attach_ack: guest → host. Stdio = the
	// channel set the guest actually established (the host bridges
	// exactly these MUX streams). For restore_ack / attach_ack, AppState
	// reports whether the user app is still running, plus Code/TermSignal
	// if it already exited.
	Stdio    *StdioSpec `json:"stdio,omitempty"`
	AppState string     `json:"app_state,omitempty"`

	// app_started: guest → host after fork/exec succeeds.
	PID int `json:"pid,omitempty"`

	// app_exited: guest → host before reboot. TermSignal != 0 means the
	// app was killed by that signal (Code is then the signal number too,
	// per shell 128+sig convention applied host-side).
	Code       int `json:"code,omitempty"`
	TermSignal int `json:"term_signal,omitempty"`

	// ping/pong: monotonic id host-assigned; host clock t_send_ns
	// echoed back in pong so host computes RTT without time-sync.
	ID      uint64 `json:"id,omitempty"`
	TSendNs int64  `json:"t_send_ns,omitempty"`

	// quiesce: SkipDropCaches asks a new guest to preserve its page,
	// dentry, and inode caches after freeze+sync. Old guests ignore this
	// additive field and retain their historical drop behavior.
	SkipDropCaches bool `json:"skip_drop_caches,omitempty"`
	// quiesced: DropCachesResult reports what the guest did. Its absence
	// means the peer predates result reporting.
	DropCachesResult DropCachesResult `json:"drop_caches_result,omitempty"`

	// restore / attach: incremented on each restore/attach. Lets guest
	// distinguish "fresh wake" from a duplicate message in flight.
	Epoch uint32 `json:"epoch,omitempty"`

	// exec: the command + stdio to run inside the running sandbox. The
	// exec / exec_ack handshake mirrors restore / attach — same conn
	// becomes the stdio MUX after exec_ack.
	Exec *ExecSpec `json:"exec,omitempty"`

	// connect: the guest-side dial target for one port-forward connection.
	// The connect / connect_ack handshake mirrors exec — same conn becomes
	// the fwd frame relay after connect_ack (§3.7).
	Connect *ConnectSpec `json:"connect,omitempty"`

	// restore: host wall clock (UnixNano) captured just before the
	// notify is sent. A snapshot's CLOCK_REALTIME is reloaded verbatim
	// by CH on restore, so the guest's wall clock is stale by the whole
	// dormant interval; the guest jumps CLOCK_REALTIME to this on
	// restore. Unset (0) on attach — a live VM's clock is fine.
	WallclockNs int64 `json:"wallclock_ns,omitempty"`

	// restore: optional new guest IP-layer config. When set, the guest
	// re-applies it flush-and-replace before thawing, so a clone restored
	// from a golden snapshot takes a fresh network identity. nil → keep the
	// snapshot's network as-is. (Cold start carries network via LaunchSpec.)
	Network *NetworkSpec `json:"network,omitempty"`

	// restore: optional per-instance files. When set, the guest injects
	// them (same tmpfs+bind mechanism as cold start) before thawing, so a
	// clone gets instance-specific secrets / resolv.conf that were never
	// baked into the golden snapshot. nil → no per-instance file injection.
	Files []FileSpec `json:"files,omitempty"`

	// mem_report: guest → host periodic /proc/meminfo snapshot used by
	// the host-side balloon controller to drive vm.resize.
	MemAvailableBytes uint64 `json:"mem_avail_bytes,omitempty"`
	MemTotalBytes     uint64 `json:"mem_total_bytes,omitempty"`

	// error: human-readable reason on rejection paths.
	Msg string `json:"msg,omitempty"`
}

// DropCachesResult is the guest's best-effort quiesce cache-drop outcome.
type DropCachesResult string

const (
	DropCachesUnknown   DropCachesResult = "unknown"
	DropCachesSkipped   DropCachesResult = "skipped"
	DropCachesSucceeded DropCachesResult = "succeeded"
	DropCachesFailed    DropCachesResult = "failed"
)

// Message type constants. See docs/sandbox-init.md §4.3 for the
// directions, the request/response pairs, and which connections upgrade
// to the MUX after their *_ack.
const (
	TypeHello        = "hello"
	TypeLaunch       = "launch"
	TypeLaunchAck    = "launch_ack"
	TypeAppStarted   = "app_started"
	TypeAppExited    = "app_exited"
	TypePing         = "ping"
	TypePong         = "pong"
	TypeRestore      = "restore"
	TypeRestoreAck   = "restore_ack"
	TypeAttach       = "attach"
	TypeAttachAck    = "attach_ack"
	TypeQuiesce      = "quiesce"
	TypeQuiesced     = "quiesced"
	TypeExec         = "exec"
	TypeExecAck      = "exec_ack"
	TypeConnect      = "connect"
	TypeConnectAck   = "connect_ack"
	TypeMemReport    = "mem_report"
	TypeMemReportAck = "mem_report_ack"
	TypeAck          = "ack"
	TypeError        = "error"
)

// Default per-message deadlines (docs/sandbox-init.md §4.9). Callers
// pass these to SetDeadline on the underlying conn covering dial+write+
// read of the management exchange. For launch/attach the deadline covers
// only the handshake — the connection then lives on as the MUX. (The restore
// handshake deadline is host-configurable, supplied by the caller.)
const (
	DeadlineAppNotify = 200 * time.Millisecond // app_started / app_exited / launch_ack / mem_report
	DeadlinePing      = 200 * time.Millisecond
	DeadlineQuiesce   = 8 * time.Second // prep + stop-reading-app-pipes + MUX_CLOSE round-trip
	DeadlineAttach    = 5 * time.Second
	// DeadlineExec covers the exec handshake only (dial + exec →
	// exec_ack). Larger than attach: the guest forks+execs the
	// child and resolves argv via PATH before acking. The conn's
	// deadline is cleared once it becomes the stdio MUX.
	DeadlineExec = 10 * time.Second
	// DeadlineConnect covers the port-forward handshake only (dial +
	// connect → connect_ack). The guest dials the target before acking, so
	// it must exceed the guest-side dial timeout; the conn's deadline is
	// cleared once it becomes the fwd frame relay.
	DeadlineConnect = 10 * time.Second
)

// --- Management wire format: [4 bytes LE length][JSON payload] --------

// WriteMessage writes one message in length-prefix-JSON wire format.
func WriteMessage(w io.Writer, m *Message) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("proto: marshal: %w", err)
	}
	if len(payload) > MaxMessageBytes {
		return fmt.Errorf("proto: payload %d > max %d", len(payload), MaxMessageBytes)
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if err := wireio.WriteAll(w, hdr[:]); err != nil {
		return fmt.Errorf("proto: write header: %w", err)
	}
	if err := wireio.WriteAll(w, payload); err != nil {
		return fmt.Errorf("proto: write payload: %w", err)
	}
	return nil
}

// ReadMessage reads exactly one message.
func ReadMessage(r io.Reader) (*Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, errors.New("proto: zero-length payload")
	}
	if n > MaxMessageBytes {
		return nil, fmt.Errorf("proto: payload %d > max %d", n, MaxMessageBytes)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("proto: read payload: %w", err)
	}
	var m Message
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, fmt.Errorf("proto: unmarshal: %w", err)
	}
	return &m, nil
}
