package main

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

// Vsock dial uses exponential backoff because sandbox boot latency is
// the project's primary metric — every additional millisecond on the
// cold-start path matters. Start tight (microsecond grain) so the first
// ECONNREFUSED is followed almost immediately by a retry, then ramp
// until we hit a 10 ms cap. Total budget bounded by vsockDialDeadline.
const (
	vsockDialStart    = 100 * time.Microsecond
	vsockDialMax      = 10 * time.Millisecond
	vsockDialDeadline = 5 * time.Second
)

// vsockConn wraps an AF_VSOCK SOCK_STREAM fd as io.ReadWriteCloser
// implementing the deadline-aware net.Conn surface that
// proto.{Read,Write}Message needs.
type vsockConn struct {
	fd        int
	closeOnce sync.Once
	closeErr  error
}

type fdIOFunc func(int, []byte) (int, error)

// retryInterruptedIO hides signal delivery from the stream abstraction. PID 1
// receives SIGCHLD while management connections are active, so raw AF_VSOCK
// syscalls can return (-1, EINTR) even though the connection is still healthy.
// Retry EINTR unless the operation reports a positive partial result, which
// must be returned to the caller unchanged.
func retryInterruptedIO(op fdIOFunc, fd int, b []byte) (int, error) {
	for {
		n, err := op(fd, b)
		if n > 0 || !errors.Is(err, syscall.EINTR) {
			return n, err
		}
	}
}

func (c *vsockConn) Read(b []byte) (int, error) {
	return retryInterruptedIO(syscall.Read, c.fd, b)
}

func (c *vsockConn) Write(b []byte) (int, error) {
	return retryInterruptedIO(syscall.Write, c.fd, b)
}
func (c *vsockConn) Close() error {
	c.closeOnce.Do(func() { c.closeErr = syscall.Close(c.fd) })
	return c.closeErr
}

// SetDeadline applies SO_RCVTIMEO + SO_SNDTIMEO. AF_VSOCK supports both
// (kernel >= 5.16). Coarse-grained but adequate for our ms-level
// per-message budgets. A zero t means "no timeout" (block indefinitely)
// — used to clear the handshake deadline before handing a conn to a
// mux.Session, which relies on the conn closing (not a deadline) to
// unblock its read loop.
func (c *vsockConn) SetDeadline(t time.Time) error {
	var tv unix.Timeval // {0,0} = no timeout
	if !t.IsZero() {
		d := time.Until(t)
		if d <= 0 {
			d = 1 * time.Microsecond
		}
		tv = unix.NsecToTimeval(d.Nanoseconds())
	}
	if err := unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return fmt.Errorf("SO_RCVTIMEO: %w", err)
	}
	if err := unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv); err != nil {
		return fmt.Errorf("SO_SNDTIMEO: %w", err)
	}
	return nil
}

// SetLinger arms SO_LINGER so that Close blocks (up to sec seconds) until
// the connection is fully torn down rather than returning while the
// kernel defers removal. Without it, virtio-vsock keeps a guest-closed
// connection alive for VSOCK_CLOSE_TIMEOUT (8s) waiting for the peer's
// RST — which a snapshot taken in that window captures as a half-closed
// remnant. With it, Close returns only once the peer RST has removed the
// socket (virtio_transport_wait_close), so the MUX teardown is confirmed
// complete before quiesce acks (deterministic steady state, §3.4).
func (c *vsockConn) SetLinger(sec int) error {
	return unix.SetsockoptLinger(c.fd, unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 1, Linger: int32(sec)})
}

// dialVsock opens an AF_VSOCK SOCK_STREAM socket and connects to (cid,
// port). The first attempt fires immediately; on ECONNREFUSED (host
// listener not yet ready) we exponentially back off from vsockDialStart
// up to vsockDialMax until vsockDialDeadline elapses. Returns a
// *vsockConn (a deadline-aware net.Conn-ish surface) so callers can
// SetDeadline both for management exchanges and before handing the conn
// to a mux.Session.
func dialVsock(cid, port uint32) (*vsockConn, error) {
	deadline := time.Now().Add(vsockDialDeadline)
	delay := vsockDialStart
	var lastErr error
	for {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
		if err != nil {
			return nil, fmt.Errorf("socket: %w", err)
		}
		err = unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port})
		if err == nil {
			return &vsockConn{fd: fd}, nil
		}
		_ = unix.Close(fd)
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("dial deadline %s exceeded: %w", vsockDialDeadline, lastErr)
		}
		time.Sleep(delay)
		if delay < vsockDialMax {
			delay *= 2
			if delay > vsockDialMax {
				delay = vsockDialMax
			}
		}
	}
}

// bindVsockListener creates an AF_VSOCK SOCK_STREAM socket bound to
// (CID=any, port). Returns the listening fd. Must be called after
// phase 1 chroot (the syscall itself doesn't depend on rootfs but
// keeping a single ordering rule simplifies reasoning).
func bindVsockListener(port uint32) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}
	// VMADDR_CID_ANY = 0xffffffff means "accept on any local CID".
	addr := &unix.SockaddrVM{CID: 0xffffffff, Port: port}
	if err := unix.Bind(fd, addr); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("bind: %w", err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("listen: %w", err)
	}
	return fd, nil
}

// serveReverseChannel accepts host-initiated connections on the bound
// vsock listener and dispatches each in its own goroutine.
//
// Most operations are management short-conns: read one request, write one
// response, close. Two — restore and attach — instead hand the connection
// off to the stdio MUX after their *_ack (it stays open as a mux.Session);
// handleReverseConn reports that via its return value so the goroutine
// doesn't close the conn out from under the session.
//
// The listener fd lives for the entire sandbox lifetime and is **never
// closed by quiesce** (docs/sandbox-init.md §3.4) — closing it would cut off the host's
// subsequent restore / attach.
//
// sup carries the exec registry (exec sessions register their children
// for reaping + honour the quiesce gate) and is reserved for other
// restore-side hooks.
func serveReverseChannel(listenFD int, sup *supervisorState, bridge *consoleBridge) {
	for {
		nfd, _, err := unix.Accept(listenFD)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			logf("reverse-channel: accept error %v (continuing)", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		go func(fd int) {
			c := &vsockConn{fd: fd}
			if handedToMUX := handleReverseConn(c, sup, bridge); !handedToMUX {
				_ = c.Close()
			}
		}(nfd)
	}
}

// handleReverseConn services one host→guest connection. It returns true
// iff the connection was handed to a mux.Session (restore / attach) and
// must therefore NOT be closed by the caller; false for management
// short-conns (ping / quiesce / unknown), which the caller closes.
func handleReverseConn(c *vsockConn, sup *supervisorState, bridge *consoleBridge) (handedToMUX bool) {
	// Per-conn handshake deadline: tight enough that a stuck host can't
	// pin a goroutine forever, generous enough for quiesce (drop_caches +
	// the MUX-close round-trip can take a few seconds). Cleared before a
	// conn is handed to a mux.Session (which relies on Close, not a
	// deadline, to unblock its read loop).
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	req, err := proto.ReadMessage(c)
	if err != nil {
		logf("reverse-channel: read: %v", err)
		return false
	}
	switch req.Type {
	case proto.TypePing:
		// Echo id + t_send_ns; host computes RTT.
		// Capture pauses the host ticker and waits for EOF. Linger makes that
		// EOF exchange a bounded transport teardown instead of leaving this
		// management 4-tuple half-closed in snapshot memory.
		if err := c.SetLinger(muxCloseLingerSec); err != nil {
			logf("reverse-channel: ping SO_LINGER: %v (continuing)", err)
		}
		if err := proto.WriteMessage(c, &proto.Message{Type: proto.TypePong, ID: req.ID, TSendNs: req.TSendNs}); err != nil {
			logf("reverse-channel: write pong: %v", err)
		}
		return false

	case proto.TypeExec:
		// Ad-hoc command inside the running sandbox. runExecSession owns
		// c for the whole session (it closes it); never closed by the
		// caller — hence handedToMUX=true.
		runExecSession(c, req, sup)
		return true

	case proto.TypeConnect:
		// Port-forward: splice this reverse-channel conn to a guest-side
		// dial target. runConnectSession owns c for the whole session (the
		// relay closes it); never closed by the caller — handedToMUX=true.
		runConnectSession(c, req, sup)
		return true

	case proto.TypeRestore:
		logf("reverse-channel: restore epoch=%d — re-establishing stdio MUX", req.Epoch)
		bridge.closeLiveMUX() // drop any stale session first (normally already gone via quiesce)
		// CH reloaded the snapshot's CLOCK_REALTIME verbatim, so the
		// guest wall clock is stale by the whole dormant interval. Jump
		// it to the host's now before the post-reattach thaw, so the
		// thawed app never observes the stale clock. Monotonic clocks are
		// unaffected (Go timers, ping RTT, mem_report ticker keep
		// running). Best-effort: a failure just leaves the stale clock.
		if req.WallclockNs > 0 {
			ts := unix.NsecToTimespec(req.WallclockNs)
			if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
				logf("reverse-channel: restore clock_settime: %v", err)
			} else {
				logf("reverse-channel: restore wall clock set to host now (epoch=%d)", req.Epoch)
			}
		}
		// Re-apply the IP layer flush-and-replace when the host supplies a
		// fresh NetworkSpec (clone from a golden snapshot taking a new
		// identity). Best-effort + logged, like clock_settime: applied before
		// the thaw so the app resumes onto the new identity. nil → keep the
		// snapshot's network.
		if req.Network != nil {
			logf("reverse-channel: restore network re-apply starting (epoch=%d)", req.Epoch)
			if err := applyNetworkReplace(req.Network); err != nil {
				logf("reverse-channel: restore applyNetwork: %v", err)
			} else {
				logf("reverse-channel: restore network re-applied (epoch=%d)", req.Epoch)
			}
		}
		// Establish a new observation epoch and pause sampling before
		// restore_ack. This serializes with an in-flight mem_report exchange, so
		// neither an old-epoch delivery nor a pre-ACK sample can become the first
		// trusted restore observation.
		logf("reverse-channel: restore mem_report epoch transition starting")
		reportEpoch, err := guestMemReports.advanceEpochAndPause()
		if err != nil {
			logf("reverse-channel: restore mem_report epoch: %v", err)
			return false
		}
		logf("reverse-channel: restore mem_report epoch=%d", reportEpoch)
		spec := bridge.protoSpec()
		resp := &proto.Message{Type: proto.TypeRestoreAck, Epoch: req.Epoch, Stdio: &spec, AppState: proto.AppStateRunning}
		if err := proto.WriteMessage(c, resp); err != nil {
			logf("reverse-channel: write restore_ack: %v", err)
			return false
		}
		_ = c.SetDeadline(time.Time{})
		bridge.reattach(c)
		// Env rebuilt (wall clock fixed, MUX reattached) — thaw the app
		// LAST so it resumes only into a wired env, never observing the
		// stale clock / missing MUX (the freeze rode the snapshot from
		// quiesce). Idempotent.
		if err := resumeAfterThaw(sup, guestMemReports, cgroupThaw); err != nil {
			logf("reverse-channel: restore thaw: %v", err)
			return true
		}
		// Restore, like cold boot, sends the first trustworthy observation
		// immediately after its lifecycle barrier instead of waiting up to one
		// periodic interval.
		go guestMemReports.attempt(readMemInfo, notifyMemReport, logf)
		return true

	case proto.TypeAttach:
		logf("reverse-channel: attach epoch=%d — re-establishing stdio MUX", req.Epoch)
		bridge.closeLiveMUX() // gracefully close the old session, then switch
		spec := bridge.protoSpec()
		resp := &proto.Message{Type: proto.TypeAttachAck, Epoch: req.Epoch, Stdio: &spec, AppState: proto.AppStateRunning}
		if err := proto.WriteMessage(c, resp); err != nil {
			logf("reverse-channel: write attach_ack: %v", err)
			return false
		}
		_ = c.SetDeadline(time.Time{})
		bridge.reattach(c)
		// attach itself is only MUX-transport reconnect — NOT
		// "post-snapshot resume" (it also serves plain live-VM MUX
		// breaks where nothing was ever quiesced). Thaw belongs to the
		// quiesce lifecycle and is state-driven: thaw iff the app is
		// still frozen, which only holds for the resume_after=true path
		// (VM resumed in place; this attach is just its first
		// post-resume contact). Plain reconnect → not frozen → skipped
		// (docs/sandbox-init.md §4.3, sandbox.md §6.2 resume recovery).
		var thaw func() error
		if frozen, err := cgroupFrozen(); err != nil {
			logf("reverse-channel: attach cgroupFrozen: %v", err)
			return true
		} else if frozen {
			thaw = cgroupThaw
		}
		// Quiesce drains and pauses guest→host memory observations before the
		// VM freeze. Attach resumes the same live epoch; a true restore instead
		// advances the epoch in the TypeRestore branch above.
		if err := resumeAfterThaw(sup, guestMemReports, thaw); err != nil {
			logf("reverse-channel: attach thaw: %v", err)
			return true
		}
		return true

	case proto.TypeQuiesce:
		logf("reverse-channel: quiesce — freeze + prep + MUX/forward close")
		// Gate restarts: the snapshot must not fork a new app/plugin into the
		// freeze window. Running plugins stay frozen with the app cgroup; the
		// app's own restart goroutine waits this out (restartApp). Cleared at
		// endQuiesce (restore/attach).
		sup.quiescing.Store(true)
		sup.pluginReg.beginQuiesce()
		// Reject new exec, terminate every in-flight child, and join the full
		// fork/MUX/socket session before freezing. Capturing only after child
		// SIGKILL but before its Go cleanup completed could restore with the
		// process-wide fork path permanently wedged.
		if err := quiesceExecAndMemoryReports(sup.execReg, guestMemReports); err != nil {
			logf("reverse-channel: quiesce exec drain failed, NOT sending quiesced: %v", err)
			return false
		}
		// Freeze the app tree BEFORE sync: no new dirty pages after
		// sync (cleaner deterministic image) and the snapshot captures
		// the app stopped. NOT best-effort — an unconfirmed freeze is a
		// half-frozen snapshot, exactly the resume-vs-env race we
		// eliminate — so on failure skip `quiesced`; the host deadline
		// lapses and it abandons this snapshot (docs/sandbox-init.md
		// §3.4 错误处理).
		if err := cgroupFreeze(); err != nil {
			logf("reverse-channel: quiesce freeze failed, NOT sending quiesced: %v", err)
			return false
		}
		dropCachesResult := runQuiesce(req.SkipDropCaches)
		// Tear down port-forward relays the same way as the stdio MUX: a
		// live forward left open would be captured as a half-open vsock
		// remnant. Each vsock conn closes with SO_LINGER so the teardown is
		// confirmed before `quiesced` (deterministic steady state, §3.4). Also
		// closes the cached accept-mode listeners (unblocking parked accepts).
		closeConnectSessions(sup.connReg, sup.acceptLn)
		bridge.closeLiveMUX() // stop forwarding app output, then the MUX_CLOSE handshake
		// This management connection is the final host→guest vsock flow before
		// /vm.pause. Arm the same bounded linger as MUX/forward connections;
		// the host waits for EOF after quiesced, so Close completes a transport
		// teardown barrier instead of leaving a half-closed 4-tuple in memory S.
		if err := c.SetLinger(muxCloseLingerSec); err != nil {
			logf("reverse-channel: quiesce SO_LINGER: %v (continuing)", err)
		}
		if err := proto.WriteMessage(c, &proto.Message{
			Type:             proto.TypeQuiesced,
			DropCachesResult: dropCachesResult,
		}); err != nil {
			logf("reverse-channel: write quiesced: %v", err)
		}
		return false

	default:
		logf("reverse-channel: unknown type %q", req.Type)
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeError, Msg: "unknown type"})
		return false
	}
}

// resumeAfterThaw reopens every cold-process launch/forward gate only after
// the application cgroup is runnable again. os/exec holds Go's process-wide
// ForkLock while clone3(CLONE_INTO_CGROUP) enters the target cgroup. Reopening
// exec before thaw lets clone3 block in the frozen cgroup while cgroupThaw's
// os.WriteFile waits for the same ForkLock to open cgroup.freeze, producing a
// permanent post-restore deadlock.
func resumeAfterThaw(sup *supervisorState, reports *memReportStream, thaw func() error) error {
	if thaw != nil {
		if err := thaw(); err != nil {
			return err
		}
	}
	reports.resumeEpoch()
	sup.execReg.endQuiesce()
	sup.connReg.endQuiesce()
	sup.acceptLn.reopen()
	sup.pluginReg.endQuiesce()
	sup.quiescing.Store(false)
	return nil
}

// quiesceExecAndMemoryReports establishes the guest-side process/management
// channel barrier before the app freezer. The reporter is paused only after
// exec teardown succeeds, so a failed exec drain leaves ordinary guest
// observations running while the host abandons and recovers the capture.
func quiesceExecAndMemoryReports(execReg *execRegistry, reports *memReportStream) error {
	if err := quiesceExecSessions(execReg); err != nil {
		return err
	}
	// Drain the periodic guest→host memory report before freezing. Its
	// stream mutex covers the whole vsock exchange; capturing it locked would
	// restore a goroutine waiting on an obsolete host connection and
	// permanently block TypeRestore's epoch transition.
	reports.pauseAndDrain()
	return nil
}

// notifyAppStarted dials the host launch UDS and sends a short-conn
// app_started notification. Best-effort: errors are logged but not fatal to
// the guest. The host emits no readiness fallback; an observer waiting for
// ready instead fails closed through its own timeout and teardown policy.
func notifyAppStarted(pid int) error {
	conn, err := dialVsock(proto.VsockHostCID, proto.LaunchPort)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(proto.DeadlineAppNotify))
	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeAppStarted, PID: pid}); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	resp, err := proto.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if resp.Type != proto.TypeAck {
		return fmt.Errorf("unexpected response %q", resp.Type)
	}
	return nil
}

// notifyMemReport pushes one sequenced /proc/meminfo observation. The caller
// retains the same report across a failed exchange and retries it later.
func notifyMemReport(report proto.MemReport) error {
	conn, err := dialVsock(proto.VsockHostCID, proto.LaunchPort)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	return exchangeMemReport(conn, report, memReportNotifyDeadline)
}

// exchangeMemReport covers the connected write/ACK exchange. Keeping it
// separate from AF_VSOCK dial makes the pressure-delayed ACK boundary
// deterministic in unit tests. AF_VSOCK implements SetDeadline with socket
// timeouts that restart for each blocking syscall, so the close timer is the
// authoritative wall-clock bound across partial header/payload reads.
func exchangeMemReport(conn interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	SetDeadline(time.Time) error
	Close() error
}, report proto.MemReport, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	timerDone := make(chan struct{})
	timer := time.AfterFunc(time.Until(deadline), func() {
		defer close(timerDone)
		// Closing a file descriptor in another goroutine does not reliably
		// interrupt a blocked Linux socket syscall. Shut down the concrete
		// AF_VSOCK connection first; Close remains the portable fallback for
		// tests and prevents any later operation after the budget expires.
		if c, ok := conn.(*vsockConn); ok {
			_ = unix.Shutdown(c.fd, unix.SHUT_RDWR)
		}
		_ = conn.Close()
	})
	defer func() {
		if !timer.Stop() {
			<-timerDone
		}
	}()
	_ = conn.SetDeadline(deadline)
	if err := proto.WriteMessage(conn, &proto.Message{
		Type:      proto.TypeMemReport,
		MemReport: &report,
	}); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	resp, err := proto.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if resp.Type != proto.TypeMemReportAck {
		return fmt.Errorf("unexpected response %q", resp.Type)
	}
	return nil
}

// notifyAppExited dials the host launch UDS and sends a short-conn
// app_exited notification. sig != 0 means the app was killed by that
// signal (code is then 128+sig per the shell convention). Best-effort —
// the guest reboots regardless of outcome (docs/sandbox-init.md §3.3).
func notifyAppExited(code, sig int) {
	conn, err := dialVsock(proto.VsockHostCID, proto.LaunchPort)
	if err != nil {
		logf("app_exited notify: dial: %v (continuing reboot)", err)
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(proto.DeadlineAppNotify))
	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeAppExited, Code: code, TermSignal: sig}); err != nil {
		logf("app_exited notify: write: %v", err)
		return
	}
	if _, err := proto.ReadMessage(conn); err != nil {
		logf("app_exited notify: read ack: %v (continuing reboot)", err)
	}
}
