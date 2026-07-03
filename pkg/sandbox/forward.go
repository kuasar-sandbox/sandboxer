package sandbox

import (
	"context"
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/fwd"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// ForwardSpec is one parsed `--connect` directive: accept connections on
// a host-local endpoint (a UDS path or an inherited listening socket fd)
// and splice each to a guest-side endpoint via a per-connection reverse
// channel (proto.TypeConnect → fwd frame relay, docs/sandbox-runtime.md
// §3.7). The host side is identical for every mode (it listens on the
// local endpoint); Accept selects what the guest does at the target —
// dial it (false) or Listen+Accept on it (true).
type ForwardSpec struct {
	// Local endpoint — exactly one of UDSPath / ListenFD is set. The host
	// listens here in every mode.
	UDSPath  string // unix socket path to create+listen ("@name" → abstract)
	ListenFD int    // inherited already-listening socket fd (>0); 0 ⇒ use UDSPath
	// Guest-side endpoint.
	Network string // "tcp" (default) | "tcp4" | "tcp6" | "unix"
	Address string // "host:port" (or a path for network "unix")
	Accept  bool   // false ⇒ guest dials Address; true ⇒ guest Listen+Accepts on it
	Raw     string // the original directive, for logs/errors
}

// ParseForwardSpec parses one `--connect LOCAL:TARGET` (dial mode) or
// `--connect LOCAL::TARGET` (accept mode) directive.
//
// LOCAL is split off at the FIRST colon, so it must not itself contain one:
// it is either "fd=N" (an inherited, already-listening socket) or a UDS
// path ("@name" → abstract). A leading ':' on the remainder selects accept
// mode. The target is unix when it starts with '/' or '@' (an absolute or
// abstract socket path), otherwise a tcp "host:port" parsed with
// net.SplitHostPort (so "[::1]:49983" IPv6 literals work; relative unix
// paths are unsupported — use an absolute or abstract path). Examples:
//
//	/run/envd.sock:127.0.0.1:49983   → UDS listener   → guest dial   tcp  127.0.0.1:49983
//	fd=3:127.0.0.1:49983             → inherited fd 3  → guest dial   tcp  127.0.0.1:49983
//	/run/db.sock:/var/run/pg.sock    → UDS listener   → guest dial   unix /var/run/pg.sock
//	/run/api.sock::0.0.0.0:8080      → UDS listener   → guest accept tcp  0.0.0.0:8080
//	/run/api.sock::/run/up.sock      → UDS listener   → guest accept unix /run/up.sock
func ParseForwardSpec(s string) (ForwardSpec, error) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return ForwardSpec{}, fmt.Errorf("connect %q: want LOCAL:TARGET or LOCAL::TARGET", s)
	}
	local, tail := s[:i], s[i+1:]
	if local == "" {
		return ForwardSpec{}, fmt.Errorf("connect %q: empty local endpoint", s)
	}
	// A leading ':' on the remainder (i.e. "LOCAL::TARGET") selects accept
	// mode; the target is whatever follows it.
	spec := ForwardSpec{Raw: s}
	target := tail
	if rest, ok := strings.CutPrefix(tail, ":"); ok {
		spec.Accept = true
		target = rest
	}
	if target == "" {
		return ForwardSpec{}, fmt.Errorf("connect %q: empty target", s)
	}
	// Target type: '/'-or-'@'-prefixed ⇒ unix path; otherwise tcp host:port.
	if target[0] == '/' || target[0] == '@' {
		spec.Network = "unix"
		spec.Address = target
	} else {
		host, port, err := net.SplitHostPort(target)
		if err != nil {
			return ForwardSpec{}, fmt.Errorf("connect %q: bad target %q: %w", s, target, err)
		}
		if host == "" || port == "" {
			return ForwardSpec{}, fmt.Errorf("connect %q: target needs HOST:PORT", s)
		}
		if _, err := strconv.Atoi(port); err != nil {
			return ForwardSpec{}, fmt.Errorf("connect %q: bad port %q", s, port)
		}
		spec.Network = "tcp"
		spec.Address = net.JoinHostPort(host, port)
	}
	// LOCAL endpoint: "fd=N" (inherited listening socket) or a UDS path.
	// Valid in every mode — the host always listens on LOCAL.
	if fdval, ok := strings.CutPrefix(local, "fd="); ok {
		n, err := strconv.Atoi(fdval)
		if err != nil || n <= 0 {
			return ForwardSpec{}, fmt.Errorf("connect %q: bad fd %q (want fd=N, N>0)", s, fdval)
		}
		spec.ListenFD = n
	} else {
		spec.UDSPath = local
	}
	return spec, nil
}

// OpenForward opens a reverse channel to the guest and asks it to dial spec
// (proto.TypeConnect → connect_ack), returning the live conn ready for the
// fwd frame relay. Mirrors guestlink.OpenMUXViaExec but yields a raw conn (no stdio
// MUX). On any error the conn is closed. This is the dial-mode (`LOCAL:TARGET`)
// path; accept mode (`LOCAL::TARGET`) uses Forwarder.openAccept, which parks
// for connect_ack without a deadline.
func OpenForward(client *guestlink.HostClient, spec *proto.ConnectSpec, deadline time.Duration) (net.Conn, error) {
	conn, err := client.DialRaw(deadline)
	if err != nil {
		return nil, err
	}
	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeConnect, Connect: spec}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write connect: %w", err)
	}
	resp, err := proto.ReadMessage(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read connect_ack: %w", err)
	}
	if resp.Type != proto.TypeConnectAck {
		_ = conn.Close()
		if resp.Type == proto.TypeError {
			return nil, fmt.Errorf("guest refused connect: %s", resp.Msg)
		}
		return nil, fmt.Errorf("connect: unexpected response %q", resp.Type)
	}
	_ = conn.SetDeadline(time.Time{}) // hand off to the relay
	return conn, nil
}

// Forwarder runs the host side of `sandbox-ctl run --connect`: one
// listener per ForwardSpec, each accepted connection spliced to the guest
// via a per-connection reverse channel (OpenForward/openAccept → fwd.Relay).
// It tracks live relays so a snapshot can gate new ones (Pause) and collapse
// active ones (CloseActive), mirroring the guest's quiesce teardown; the
// guest closes its ends authoritatively (lingered), this just promptly
// drops the host halves. Accept-mode forwards also park a reverse conn while
// waiting for the guest's accept; those pre-relay conns are tracked in
// `pending` so the same snapshot/shutdown teardown collapses them too.
type Forwarder struct {
	vsockBase string
	logf      func(string, ...any)

	listeners []net.Listener

	mu        sync.Mutex
	relays    map[*fwd.Relay]struct{}
	pending   map[net.Conn]struct{} // accept-mode reverse conns awaiting connect_ack
	quiescing bool
	closed    bool
}

// NewForwarder builds a forwarder dialing the guest via vsockBase
// (<run-dir>/<sid>/vsock.sock — the same base guestlink.HostClient uses).
func NewForwarder(vsockBase string, logf func(string, ...any)) *Forwarder {
	return &Forwarder{
		vsockBase: vsockBase,
		logf:      logf,
		relays:    make(map[*fwd.Relay]struct{}),
		pending:   make(map[net.Conn]struct{}),
	}
}

// Start opens every spec's listener and runs its accept loop under ctx. On
// the first listener error it closes any already-opened listeners and
// returns. Accept loops (and tracked relays) end when ctx is cancelled or
// Close is called.
func (f *Forwarder) Start(ctx context.Context, specs []ForwardSpec) error {
	for _, spec := range specs {
		ln, err := f.listen(spec)
		if err != nil {
			f.closeListeners()
			return fmt.Errorf("connect %s: %w", spec.Raw, err)
		}
		f.listeners = append(f.listeners, ln)
		f.logf("port-forward: listening %s → guest %s %s", localLabel(spec), modeLabel(spec), spec.Address)
		go f.acceptLoop(ctx, ln, spec)
	}
	if len(specs) > 0 {
		go func() { <-ctx.Done(); f.Close() }()
	}
	return nil
}

func (f *Forwarder) listen(spec ForwardSpec) (net.Listener, error) {
	if spec.ListenFD > 0 {
		// Inherited, already-listening socket. FileListener dups the fd; we
		// then close our File (the original inherited fd) so it neither leaks
		// into CH on the next fork/exec nor is double-owned.
		file := os.NewFile(uintptr(spec.ListenFD), fmt.Sprintf("connect-fd-%d", spec.ListenFD))
		ln, err := net.FileListener(file)
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("fd=%d: %w", spec.ListenFD, err)
		}
		return ln, nil
	}
	// UDS path: remove a stale node first (not for abstract "@name").
	if !strings.HasPrefix(spec.UDSPath, "@") {
		_ = os.Remove(spec.UDSPath)
	}
	return net.Listen("unix", spec.UDSPath)
}

func (f *Forwarder) acceptLoop(ctx context.Context, ln net.Listener, spec ForwardSpec) {
	for {
		local, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			f.mu.Lock()
			closed := f.closed
			f.mu.Unlock()
			if !closed {
				f.logf("port-forward %s: accept: %v", spec.Raw, err)
			}
			return // listener closed/broken — stop this loop
		}
		go f.serve(local, spec)
	}
}

func (f *Forwarder) serve(local net.Conn, spec ForwardSpec) {
	// Gate: refuse new forwards while a snapshot is quiescing or after
	// shutdown (the guest would reject the connect anyway; fail fast).
	f.mu.Lock()
	gated := f.quiescing || f.closed
	f.mu.Unlock()
	if gated {
		_ = local.Close()
		return
	}
	client := &guestlink.HostClient{BasePath: f.vsockBase, Logf: f.logf}
	cs := &proto.ConnectSpec{Network: spec.Network, Address: spec.Address, Accept: spec.Accept}
	var vconn net.Conn
	var err error
	if spec.Accept {
		vconn, err = f.openAccept(client, cs)
	} else {
		vconn, err = OpenForward(client, cs, proto.DeadlineConnect)
	}
	if err != nil {
		f.logf("port-forward %s → %s: %v", spec.Raw, spec.Address, err)
		_ = local.Close()
		return
	}
	relay := fwd.NewRelay(vconn, local)
	if !f.track(relay) {
		relay.Shutdown(nil) // raced with quiesce/close
		return
	}
	defer f.untrack(relay)
	relay.Run()
}

func (f *Forwarder) track(r *fwd.Relay) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.quiescing || f.closed {
		return false
	}
	f.relays[r] = struct{}{}
	return true
}

func (f *Forwarder) untrack(r *fwd.Relay) {
	f.mu.Lock()
	delete(f.relays, r)
	f.mu.Unlock()
}

// openAccept opens a reverse channel and asks the guest to Listen+Accept on
// spec.Address (accept mode). Unlike OpenForward it parks for connect_ack
// WITHOUT a deadline: the guest's accept blocks until a guest-side client
// connects to the target, which may be arbitrarily long. The parked conn is
// registered in `pending` so a snapshot (CloseActive) or shutdown (Close)
// closes it and unblocks the wait. On any error the conn is closed.
func (f *Forwarder) openAccept(client *guestlink.HostClient, spec *proto.ConnectSpec) (net.Conn, error) {
	conn, err := client.DialRaw(proto.DeadlineConnect)
	if err != nil {
		return nil, err
	}
	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeConnect, Connect: spec}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write connect: %w", err)
	}
	// Drop the handshake deadline and park; register first so a concurrent
	// Close/CloseActive can collapse the wait (closing conn unblocks the read).
	_ = conn.SetDeadline(time.Time{})
	if !f.trackPending(conn) {
		_ = conn.Close()
		return nil, fmt.Errorf("connect: forwarder gated (quiescing/closed)")
	}
	resp, err := proto.ReadMessage(conn)
	f.untrackPending(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read connect_ack: %w", err)
	}
	if resp.Type != proto.TypeConnectAck {
		_ = conn.Close()
		if resp.Type == proto.TypeError {
			return nil, fmt.Errorf("guest refused connect: %s", resp.Msg)
		}
		return nil, fmt.Errorf("connect: unexpected response %q", resp.Type)
	}
	return conn, nil
}

// trackPending registers an accept-mode reverse conn parked for connect_ack,
// returning false (gated) if a snapshot/shutdown is in progress — same gate
// as track, so a forward racing quiesce is dropped rather than parked.
func (f *Forwarder) trackPending(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.quiescing || f.closed {
		return false
	}
	f.pending[c] = struct{}{}
	return true
}

func (f *Forwarder) untrackPending(c net.Conn) {
	f.mu.Lock()
	delete(f.pending, c)
	f.mu.Unlock()
}

// Pause gates new forwards (a snapshot is quiescing); Resume re-enables.
// Symmetric with guestlink.Pinger.Pause/Resume around snapshot.Take.
func (f *Forwarder) Pause()  { f.mu.Lock(); f.quiescing = true; f.mu.Unlock() }
func (f *Forwarder) Resume() { f.mu.Lock(); f.quiescing = false; f.mu.Unlock() }

// CloseActive tears down every live relay AND every parked accept-mode conn
// (host side) — called around a snapshot so neither survives into the paused
// window. The guest closes its ends authoritatively (lingered) so the
// snapshot is clean; this just collapses the host-side halves promptly.
func (f *Forwarder) CloseActive() {
	for _, c := range f.snapshotPending() {
		_ = c.Close()
	}
	for _, r := range f.snapshotRelays() {
		r.Shutdown(nil)
	}
}

// Close stops accepting and tears down all listeners + live relays. Run
// teardown calls it (and the ctx watcher in Start). Idempotent.
func (f *Forwarder) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	f.mu.Unlock()
	f.closeListeners()
	for _, c := range f.snapshotPending() {
		_ = c.Close()
	}
	for _, r := range f.snapshotRelays() {
		r.Shutdown(nil)
	}
}

func (f *Forwarder) snapshotRelays() []*fwd.Relay {
	f.mu.Lock()
	defer f.mu.Unlock()
	rs := make([]*fwd.Relay, 0, len(f.relays))
	for r := range f.relays {
		rs = append(rs, r)
	}
	return rs
}

func (f *Forwarder) snapshotPending() []net.Conn {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := make([]net.Conn, 0, len(f.pending))
	for c := range f.pending {
		cs = append(cs, c)
	}
	return cs
}

func (f *Forwarder) closeListeners() {
	for _, ln := range f.listeners {
		_ = ln.Close()
	}
}

// localLabel renders a spec's host-local endpoint for logging.
func localLabel(spec ForwardSpec) string {
	if spec.ListenFD > 0 {
		return fmt.Sprintf("fd=%d", spec.ListenFD)
	}
	return spec.UDSPath
}

// modeLabel renders a spec's guest-side verb for logging ("dial"/"accept").
func modeLabel(spec ForwardSpec) string {
	if spec.Accept {
		return "accept"
	}
	return "dial"
}
