package guestlink

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// LaunchServer accepts the guest's launch-channel connections on a UDS
// that cloud-hypervisor's hybrid vsock proxies from CID=2 port=5000.
//
// The protocol (docs/sandbox-init.md §4) is bidirectional and
// short-lived. This server handles the **guest → host** direction:
//
//   - hello       → server replies with the LaunchSpec (one-shot)
//   - launch_ack  → server acks; OnLaunchAck callback fires (Settled trigger)
//   - app_started → server acks; OnAppStarted callback fires
//   - app_exited  → server acks; OnAppExited callback fires
//
// Each connection carries exactly one request + one response, then both
// sides close. Multiple goroutines serve concurrent connections — the
// guest sandbox-init may emit app_started while a (rare) hello retry is
// still in flight, and we don't want one to block the other.
//
// Naming convention follows cloud-hypervisor's hybrid vsock: when the
// guest connects to vsock host:<port>, CH proxies to "<base>_<port>" on
// the host. base is given as --vsock socket=<base>; the port suffix
// is computed by us using proto.LaunchPort.
type LaunchServer struct {
	Path string
	Spec *proto.LaunchSpec
	Logf func(string, ...any)

	// StartTimeout bounds the wait for launch_ack after the launch spec is
	// sent. The guest sends launch_ack only after applying the whole spec
	// (network, mounts, files, and possibly long-running init), so this
	// must accommodate init. 0 → no deadline (wait indefinitely). The first
	// read (hello / app notifications) uses AppNotifyDeadline.
	StartTimeout time.Duration

	// AppNotifyDeadline bounds the read of ONE guest→host launch-port message
	// (the hello probe, and each app_started / app_exited / mem_report). 0 = no
	// forced timeout: the host blocks reading until the guest sends or the conn
	// closes, so a guest briefly blocked on a slow page-in isn't dropped
	// mid-message. Each connection has its own goroutine, so a hung conn is
	// isolated (and unblocks on CH teardown).
	AppNotifyDeadline time.Duration

	// OnLaunchAck fires when the guest reports it has applied the launch
	// spec (network configured, ready to fork the app). Optional; nil →
	// just ack. This is the Settled trigger: post-boot transient is over,
	// safe to engage memory.high / controller RPCs.
	OnLaunchAck func()

	// OnAppStarted fires when the guest reports its user app has been
	// fork/execed. Optional; nil → just ack.
	OnAppStarted func(pid int)

	// OnAppExited fires when the guest reports its user app has
	// exited. Optional; nil → just ack.
	OnAppExited func(code int)

	// OnMemReport fires on every periodic mem_report from the guest
	// (sandbox-init's /proc/meminfo sampler). Drives the host-side
	// balloon controller (replaces virtio-balloon free-page-reporting).
	// Optional; nil → just ack.
	OnMemReport func(memAvailableBytes, memTotalBytes uint64)

	// OnMUXReady, if non-nil, takes ownership of the hello/launch_ack
	// connection AFTER the final ack is sent: instead of closing it, the
	// server clears its deadline and hands it to OnMUXReady, which wraps
	// it in a mux.Session and bridges the app's stdio (the launch conn
	// becomes the stdio MUX — docs/sandbox-init.md §4.5). `established`
	// is the channel set echoed by the guest in launch_ack. If nil, the
	// connection is closed as before (used by tests).
	OnMUXReady func(conn net.Conn, established proto.StdioSpec)

	listener      *net.UnixListener
	stopOnce      sync.Once
	stopped       chan struct{}
	helloDone     chan struct{}
	helloOnce     sync.Once
	helloSent     atomic.Bool
	launchAckDone chan struct{}
	launchAckOnce sync.Once
	connsWG       sync.WaitGroup
}

// Listen binds the UDS for the launch port. Must be called before CH
// spawns; otherwise the guest's first connect attempt fails (it will
// retry but warning will appear in logs).
func (s *LaunchServer) Listen() error {
	if s.Spec == nil {
		return errors.New("launchsrv: Spec is nil")
	}
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	s.stopped = make(chan struct{})
	s.helloDone = make(chan struct{})
	s.launchAckDone = make(chan struct{})

	_ = os.Remove(s.Path)
	addr, err := net.ResolveUnixAddr("unix", s.Path)
	if err != nil {
		return fmt.Errorf("launchsrv: resolve %s: %w", s.Path, err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("launchsrv: listen %s: %w", s.Path, err)
	}
	s.listener = l
	return nil
}

// Serve runs the accept loop until ctx is cancelled or Stop is called.
// Each accepted connection is handled in its own goroutine: read one
// message, dispatch, write the response, close — UNLESS it was the
// hello/launch_ack handshake and OnMUXReady took ownership of the conn
// (which then lives on as the stdio MUX).
func (s *LaunchServer) Serve(ctx context.Context) error {
	defer s.cleanup()

	go func() {
		<-ctx.Done()
		s.Stop()
	}()

	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			select {
			case <-s.stopped:
				s.connsWG.Wait()
				return nil
			default:
				return fmt.Errorf("launchsrv: accept: %w", err)
			}
		}
		s.connsWG.Add(1)
		go func() {
			defer s.connsWG.Done()
			if keep := s.handleConn(conn); !keep {
				_ = conn.Close()
			}
		}()
	}
}

// HelloDone returns a channel that closes once the hello/launch
// handshake has completed (LaunchSpec written, conn closed). Useful
// for callers that want to gate "ping ticker start" on this event.
func (s *LaunchServer) HelloDone() <-chan struct{} { return s.helloDone }

// LaunchAckDone returns a channel that closes once the guest has
// acknowledged that it has received the launch spec and applied
// network config (post-boot transient over). Settled gate.
func (s *LaunchServer) LaunchAckDone() <-chan struct{} { return s.launchAckDone }

// handleConn services one connection. It returns true iff it handed the
// connection off (to OnMUXReady) and the caller must NOT close it.
func (s *LaunchServer) handleConn(conn *net.UnixConn) (handedOff bool) {
	// Deadline for the first read (hello / app notification). AppNotifyDeadline
	// = 0 → no forced timeout (don't drop a guest that's briefly blocked on a
	// slow page-in before it sends); a positive value catches a guest that
	// connects but never speaks. The launch_ack read below uses StartTimeout
	// (spans the guest's whole spec-apply). Cleared before hand-off.
	if s.AppNotifyDeadline > 0 {
		_ = conn.SetDeadline(time.Now().Add(s.AppNotifyDeadline))
	}

	msg, err := proto.ReadMessage(conn)
	if err != nil {
		s.Logf("launch: read: %v", err)
		return false
	}

	switch msg.Type {
	case proto.TypeHello:
		if s.helloSent.Load() {
			s.Logf("launch: duplicate hello rejected")
			_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeError, Msg: "hello already served"})
			return false
		}
		s.Logf("launch: hello received (phase=%q), sending launch spec", msg.Phase)
		if err := proto.WriteMessage(conn, &proto.Message{
			Type:   proto.TypeLaunch,
			Launch: s.Spec,
		}); err != nil {
			s.Logf("launch: send launch: %v", err)
			return false
		}
		s.helloSent.Store(true)
		s.helloOnce.Do(func() { close(s.helloDone) })

		// launch_ack spans the guest's whole spec-apply (incl. init), so
		// switch from the short hello-probe deadline to StartTimeout
		// (0 → no deadline, wait indefinitely).
		if s.StartTimeout > 0 {
			_ = conn.SetDeadline(time.Now().Add(s.StartTimeout))
		} else {
			_ = conn.SetDeadline(time.Time{})
		}

		// Same-connection launch_ack: guest applies network + sets up the
		// app stdio fds, then sends launch_ack{stdio} on this conn.
		// Reading it here (instead of accepting a fresh conn) makes the
		// post-boot transient boundary unambiguous — OnLaunchAck fires
		// only after the guest has actually applied the spec.
		ack, err := proto.ReadMessage(conn)
		if err != nil {
			s.Logf("launch: read launch_ack: %v", err)
			return false
		}
		if ack.Type != proto.TypeLaunchAck {
			s.Logf("launch: expected launch_ack, got %q", ack.Type)
			_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeError, Msg: "expected launch_ack"})
			return false
		}
		s.Logf("launch: launch_ack received")
		if s.OnLaunchAck != nil {
			s.OnLaunchAck()
		}
		s.launchAckOnce.Do(func() { close(s.launchAckDone) })
		if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeAck}); err != nil {
			s.Logf("launch: write ack: %v", err)
			return false
		}
		// Hand the connection off as the stdio MUX (docs/sandbox-runtime
		// .md §4.5). Drop the handshake deadline first.
		if s.OnMUXReady != nil {
			_ = conn.SetDeadline(time.Time{})
			var spec proto.StdioSpec
			if ack.Stdio != nil {
				spec = *ack.Stdio
			} else if s.Spec != nil {
				spec = s.Spec.Stdio
			}
			s.OnMUXReady(conn, spec)
			return true
		}
		return false

	case proto.TypeLaunchAck:
		// Legacy fresh-connection launch_ack (sandbox-init now always
		// sends it on the hello conn). Kept as a defensive fallback.
		s.Logf("launch: launch_ack received (standalone conn)")
		if s.OnLaunchAck != nil {
			s.OnLaunchAck()
		}
		s.launchAckOnce.Do(func() { close(s.launchAckDone) })
		_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeAck})
		return false

	case proto.TypeAppStarted:
		s.Logf("launch: app_started pid=%d", msg.PID)
		if s.OnAppStarted != nil {
			s.OnAppStarted(msg.PID)
		}
		_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeAck})
		return false

	case proto.TypeAppExited:
		s.Logf("launch: app_exited code=%d term_signal=%d", msg.Code, msg.TermSignal)
		if s.OnAppExited != nil {
			s.OnAppExited(msg.Code)
		}
		_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeAck})
		return false

	case proto.TypeMemReport:
		if s.OnMemReport != nil {
			s.OnMemReport(msg.MemAvailableBytes, msg.MemTotalBytes)
		}
		_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeMemReportAck})
		return false

	default:
		s.Logf("launch: unknown message type %q", msg.Type)
		_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeError, Msg: "unknown type"})
		return false
	}
}

// Stop terminates the accept loop. Safe to call multiple times.
func (s *LaunchServer) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopped)
		if s.listener != nil {
			_ = s.listener.Close()
		}
	})
}

func (s *LaunchServer) cleanup() {
	_ = os.Remove(s.Path)
}
