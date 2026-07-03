package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

// The guest side of the stdio MUX (docs/sandbox-runtime.md §3.5 / §4.5).
//
// consoleBridge owns the user app's stdio fds — a pty master (tty mode)
// or the sandbox-init ends of stdin/stdout/stderr pipes (pipe mode) —
// which are STABLE for the life of the sandbox, and a sessionHolder
// pointing at the current MUX session, which is swapped on restore /
// attach. A small set of long-lived pump goroutines copy bytes between
// the stable fds and "whatever the current session is", parking when
// there is no session (so the app blocks on its next write — the
// "detached = app blocked" semantics).

// childStdio carries the three fds the user app should get as 0/1/2.
// Tty reports whether they are the slave end of a pty (then the child
// also needs setsid + TIOCSCTTY).
type childStdio struct {
	stdin, stdout, stderr *os.File
	tty                   bool
}

// appRole identifies which app stdio fd a pump operates on.
type appRole int

const (
	rolePTY appRole = iota
	roleStdin
	roleStdout
	roleStderr
)

// consoleBridge bridges the app's stdio to the current MUX session. The MUX
// session (host side) is swapped by reattach across restore/attach; the
// app-side fds are swapped by rewireApp across an in-place app restart. The
// pumps are persistent (per sandbox) and re-fetch BOTH the session (from
// holder) and the app fd (from this bridge, guarded by appMu) so the MUX link
// survives an app restart with no pump churn (docs/sandbox-runtime.md §3.2).
type consoleBridge struct {
	tty bool
	// negotiated channel set (immutable; what start/protoSpec key off).
	hasStdin, hasStdout, hasStderr bool

	holder *sessionHolder // current MUX session (host side)

	// app-side fds for the CURRENT app generation, guarded by appMu. ptyMaster
	// in tty mode; stdinW/stdoutR/stderrR in pipe mode (any nil = channel off).
	// appGen bumps on each rewireApp; appClosed = sandbox shutdown.
	appMu     sync.Mutex
	appCond   *sync.Cond
	appGen    uint64
	appClosed bool
	ptyMaster *os.File
	stdinW    *os.File
	stdoutR   *os.File
	stderrR   *os.File

	started bool
	wg      sync.WaitGroup // guest→host pumps (the ones that CloseWrite on shutdown)
}

// appEnds is the sandbox-init side of one app generation's stdio fds.
type appEnds struct {
	ptyMaster, stdinW, stdoutR, stderrR *os.File
}

// fdForLocked returns the current fd for role (caller holds appMu).
func (b *consoleBridge) fdForLocked(role appRole) *os.File {
	switch role {
	case rolePTY:
		return b.ptyMaster
	case roleStdin:
		return b.stdinW
	case roleStdout:
		return b.stdoutR
	case roleStderr:
		return b.stderrR
	}
	return nil
}

// currentFd returns (fd, gen) for role at the current generation.
func (b *consoleBridge) currentFd(role appRole) (*os.File, uint64) {
	b.appMu.Lock()
	defer b.appMu.Unlock()
	return b.fdForLocked(role), b.appGen
}

// writeFd returns the current fd for role (host→app pumps re-fetch the dst per
// write so a restart's new fd is picked up; nil when the app is momentarily
// down between instances).
func (b *consoleBridge) writeFd(role appRole) *os.File {
	b.appMu.Lock()
	defer b.appMu.Unlock()
	return b.fdForLocked(role)
}

// waitNextFd blocks until a generation newer than gen is wired (an app
// restart) or the bridge is shut down. Returns (fd, newGen, true) on a new
// generation, (nil, 0, false) on shutdown.
func (b *consoleBridge) waitNextFd(role appRole, gen uint64) (*os.File, uint64, bool) {
	b.appMu.Lock()
	defer b.appMu.Unlock()
	for b.appGen <= gen && !b.appClosed {
		b.appCond.Wait()
	}
	if b.appClosed {
		return nil, 0, false
	}
	return b.fdForLocked(role), b.appGen, true
}

// setEndsLocked installs a generation's fds and bumps appGen (caller holds appMu).
func (b *consoleBridge) setEndsLocked(e appEnds) {
	b.ptyMaster, b.stdinW, b.stdoutR, b.stderrR = e.ptyMaster, e.stdinW, e.stdoutR, e.stderrR
	b.appGen++
	b.appCond.Broadcast()
}

// closeEndsLocked closes the current generation's sandbox-init fd ends (caller
// holds appMu).
func (b *consoleBridge) closeEndsLocked() {
	for _, f := range []*os.File{b.ptyMaster, b.stdinW, b.stdoutR, b.stderrR} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// rewireApp creates a fresh app stdio generation (for an in-place restart),
// closing the old ends and waking the pumps onto the new ones; the MUX session
// is untouched. Returns the new childStdio for the re-fork.
func (b *consoleBridge) rewireApp(spec proto.StdioSpec) (childStdio, error) {
	cs, ends, err := makeAppFds(spec)
	if err != nil {
		return childStdio{}, err
	}
	b.appMu.Lock()
	b.closeEndsLocked()
	b.setEndsLocked(ends)
	b.appMu.Unlock()
	return cs, nil
}

// setupAppStdio creates the first app stdio generation per spec and returns
// the child's 0/1/2 plus a consoleBridge holding the sandbox-init ends. The
// caller wires the child fds onto the user-app exec.Cmd, then (after the
// launch_ack handshake) calls bridge.attach(sess) and bridge.start(). An
// in-place app restart later calls bridge.rewireApp to swap in a fresh
// generation.
func setupAppStdio(spec proto.StdioSpec) (childStdio, *consoleBridge, error) {
	cs, ends, err := makeAppFds(spec)
	if err != nil {
		return childStdio{}, nil, err
	}
	b := &consoleBridge{
		tty:       spec.TTY,
		hasStdin:  spec.Stdin,
		hasStdout: spec.Stdout,
		hasStderr: spec.Stderr,
		holder:    newSessionHolder(),
		appGen:    1,
		ptyMaster: ends.ptyMaster,
		stdinW:    ends.stdinW,
		stdoutR:   ends.stdoutR,
		stderrR:   ends.stderrR,
	}
	b.appCond = sync.NewCond(&b.appMu)
	return cs, b, nil
}

// makeAppFds creates one generation of app stdio fds per spec: the child's
// 0/1/2 (childStdio) plus the sandbox-init ends (appEnds). Shared by cold-start
// setup and in-place restart re-wire.
func makeAppFds(spec proto.StdioSpec) (childStdio, appEnds, error) {
	if spec.TTY {
		master, slave, err := openPTY()
		if err != nil {
			return childStdio{}, appEnds{}, fmt.Errorf("openpty: %w", err)
		}
		if spec.Winsize != nil {
			_ = setWinsize(int(master.Fd()), spec.Winsize.Cols, spec.Winsize.Rows)
		}
		return childStdio{stdin: slave, stdout: slave, stderr: slave, tty: true},
			appEnds{ptyMaster: master}, nil
	}

	// pipe mode
	var cs childStdio
	var e appEnds
	if spec.Stdin {
		pr, pw, err := appPipe() // child reads pr (fd 0); sandbox-init writes pw
		if err != nil {
			return childStdio{}, appEnds{}, fmt.Errorf("pipe stdin: %w", err)
		}
		cs.stdin, e.stdinW = pr, pw
	} else {
		devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
		if err != nil {
			return childStdio{}, appEnds{}, fmt.Errorf("open /dev/null: %w", err)
		}
		cs.stdin = devnull
	}
	if spec.Stdout {
		pr, pw, err := appPipe() // child writes pw (fd 1); sandbox-init reads pr
		if err != nil {
			return childStdio{}, appEnds{}, fmt.Errorf("pipe stdout: %w", err)
		}
		cs.stdout, e.stdoutR = pw, pr
	} else {
		devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return childStdio{}, appEnds{}, fmt.Errorf("open /dev/null: %w", err)
		}
		cs.stdout = devnull
	}
	if spec.Stderr {
		pr, pw, err := appPipe()
		if err != nil {
			return childStdio{}, appEnds{}, fmt.Errorf("pipe stderr: %w", err)
		}
		cs.stderr, e.stderrR = pw, pr
	} else {
		devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return childStdio{}, appEnds{}, fmt.Errorf("open /dev/null: %w", err)
		}
		cs.stderr = devnull
	}
	return cs, e, nil
}

// appPipeSize is the buffer we request for the app's stdin/stdout/stderr
// pipes. Bigger than the kernel default (64 KiB) so a transiently
// congested stdio MUX — e.g. the vsock kthread briefly starved by a
// balloon inflate — doesn't immediately backpressure the user app's
// writes. Kernel rounds to a power of two and caps at
// /proc/sys/fs/pipe-max-size (typically 1 MiB).
const appPipeSize = 1 << 20

// appPipe is os.Pipe() with both ends grown to appPipeSize (best-effort —
// F_SETPIPE_SZ failure just leaves the default size).
func appPipe() (r, w *os.File, err error) {
	r, w, err = os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	_, _ = unix.FcntlInt(w.Fd(), unix.F_SETPIPE_SZ, appPipeSize)
	return r, w, nil
}

// streamSetFor maps a negotiated StdioSpec onto the MUX data-stream set:
// {pty} in tty mode, the requested subset of {stdin,stdout,stderr} in
// pipe mode. The control stream is implicit (always present).
func streamSetFor(s proto.StdioSpec) mux.StreamSet {
	if s.TTY {
		return mux.PTYStreams()
	}
	return mux.PipeStreams(s.Stdin, s.Stdout, s.Stderr)
}

// onSetWinsize is the OnSetWinsize callback handed to every mux.Session;
// it pushes the host's terminal size onto the guest pty master (the
// kernel then SIGWINCHes the app's pgrp).
func (b *consoleBridge) onSetWinsize(cols, rows uint16) {
	if b.tty && b.ptyMaster != nil {
		_ = setWinsize(int(b.ptyMaster.Fd()), cols, rows)
	}
}

// attach makes sess the current MUX session and wakes the pump
// goroutines. Called once at cold boot (after mux.NewSession on the
// launch conn) and again on each restore / attach.
func (b *consoleBridge) attach(sess *mux.Session) { b.holder.set(sess) }

// start spawns the persistent pump goroutines (one set for the sandbox's
// life, re-fetching the app fd across restarts). Idempotent.
func (b *consoleBridge) start() {
	if b.started {
		return
	}
	b.started = true
	if b.tty {
		// app output → host (CloseWrite only on sandbox shutdown)
		b.wg.Add(1)
		go func() { defer b.wg.Done(); pumpAppToHost(b, rolePTY, b.holder, mux.StreamPTY) }()
		// host keystrokes → app pty (no CloseWrite — pty is full-duplex)
		go pumpHostToApp(b, rolePTY, b.holder, mux.StreamPTY, false)
		return
	}
	if b.hasStdin { // host→app stdin
		go pumpHostToApp(b, roleStdin, b.holder, mux.StreamStdin, true)
	}
	if b.hasStdout {
		b.wg.Add(1)
		go func() { defer b.wg.Done(); pumpAppToHost(b, roleStdout, b.holder, mux.StreamStdout) }()
	}
	if b.hasStderr {
		b.wg.Add(1)
		go func() { defer b.wg.Done(); pumpAppToHost(b, roleStderr, b.holder, mux.StreamStderr) }()
	}
}

// protoSpec reconstructs the negotiated StdioSpec from the bridge's fds —
// the channel set it established at launch, which never changes. Echoed in
// restore_ack / attach_ack so the host builds a matching mux.StreamSet.
// Winsize is omitted (transient; the host re-sends SET_WINSIZE on reattach).
func (b *consoleBridge) protoSpec() proto.StdioSpec {
	if b.tty {
		return proto.StdioSpec{TTY: true}
	}
	return proto.StdioSpec{
		Stdin:  b.hasStdin,
		Stdout: b.hasStdout,
		Stderr: b.hasStderr,
	}
}

// reattach builds a fresh MUX session over conn, makes it the current one
// (waking the parked pumps and flushing whatever they were holding), and
// returns it. Used by the restore / attach handlers after writing their
// *_ack. The caller must NOT close conn afterward — the session owns it.
func (b *consoleBridge) reattach(conn *vsockConn) *mux.Session {
	// Arm SO_LINGER so the eventual Close — notably closeLiveMUX during
	// quiesce — blocks until the peer RST has fully removed the socket,
	// instead of leaving the guest-closed connection in virtio-vsock's
	// 8s deferred-removal window. A snapshot taken in that window would
	// capture the half-closed remnant; on a later restore CH's muxer
	// reuses the same local port (0x40000000) for the first host
	// connection, collides with the remnant's 4-tuple, and the guest
	// silently drops the restore-notify. Lingering here makes the MUX
	// teardown confirmed-complete before quiesce acks.
	if err := conn.SetLinger(muxCloseLingerSec); err != nil {
		logf("reattach: SO_LINGER: %v (continuing)", err)
	}
	sess := mux.NewSession(conn, streamSetFor(b.protoSpec()), mux.Options{OnSetWinsize: b.onSetWinsize})
	b.attach(sess)
	return sess
}

// muxCloseTimeout bounds how long closeLiveMUX waits for the host's
// MUX_CLOSE_ACK before forcing the connection shut. Comfortably under the
// host-side DeadlineQuiesce so a wedged host fails the snapshot rather
// than hanging the guest's quiesce handler.
const muxCloseTimeout = 5 * time.Second

// muxCloseLingerSec is the SO_LINGER bound (seconds) armed on the MUX
// conn so closeLiveMUX's Close blocks until the vsock teardown completes.
// The peer (the run process's MUX reader) RSTs within ms, so this is only
// a safety net; kept small so muxCloseTimeout + this stays under
// DeadlineQuiesce.
const muxCloseLingerSec = 2

// closeLiveMUX gracefully tears down the current MUX session if any:
// initiate MUX_CLOSE (which immediately stops the app→host pumps), wait
// (bounded) for the host's MUX_CLOSE_ACK, then drop the connection so the
// host reads EOF. Used by quiesce (the snapshot needs the app quiescent)
// and by attach (close the stale session before switching to the new one).
// A no-op when there is no live session. After this returns, the pumps are
// parked; a later reattach unparks them.
func (b *consoleBridge) closeLiveMUX() {
	sess := b.holder.peek()
	if sess == nil {
		return
	}
	done := make(chan struct{})
	go func() { _ = sess.InitMuxClose(); close(done) }()
	select {
	case <-done:
	case <-time.After(muxCloseTimeout):
		logf("mux: MUX_CLOSE_ACK timed out after %s — forcing conn close", muxCloseTimeout)
	}
	_ = sess.Close()
	b.holder.invalidate(sess)
}

// appExited marks the bridge shut at sandbox shutdown (reboot): it closes the
// current app fd ends and wakes the app→host pumps parked for a next generation
// so they EOF their streams and exit, then waits (bounded) for them to drain.
// (An in-place restart uses rewireApp instead, which does NOT close the
// session.) Called before reboot.
func (b *consoleBridge) appExited() {
	b.appMu.Lock()
	b.closeEndsLocked()
	b.appClosed = true
	b.appCond.Broadcast() // wake pumps in waitNextFd → CloseWrite + return
	b.appMu.Unlock()

	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// host wedged / no session — give up; we reboot anyway.
	}
	b.holder.shutdown()
}

// --- pumps ----------------------------------------------------------

// pumpAppToHost copies bytes the app wrote (its pty master / stdout|stderr
// pipe read end, fetched from the bridge per app generation) onto the current
// session's stream `id`, re-fetching the session whenever the old one dies.
// On the app fd reaching EOF (the app instance exited) it parks for the next
// app generation (an in-place restart) and resumes on the fresh fd; only on
// bridge shutdown (reboot) does it EOF the stream and return.
func pumpAppToHost(b *consoleBridge, role appRole, holder *sessionHolder, id uint8) {
	buf := make([]byte, mux.DataChunk)
	src, gen := b.currentFd(role)
	for {
		if src == nil {
			nsrc, ngen, ok := b.waitNextFd(role, gen)
			if !ok {
				return
			}
			src, gen = nsrc, ngen
			continue
		}
		n, rerr := src.Read(buf)
		chunk := buf[:n]
		for len(chunk) > 0 {
			sess := holder.wait()
			if sess == nil {
				return // shutting down
			}
			wn, werr := sess.Stream(id).Write(chunk)
			chunk = chunk[wn:]
			if werr != nil {
				holder.invalidate(sess) // → next holder.wait() blocks for a new session
			}
		}
		if rerr != nil {
			// This app instance's fd is done. Park for the next instance
			// (restart); on shutdown, EOF the stream and return.
			nsrc, ngen, ok := b.waitNextFd(role, gen)
			if !ok {
				for {
					sess := holder.wait()
					if sess == nil {
						return
					}
					if err := sess.Stream(id).CloseWrite(); err == nil {
						return
					}
					holder.invalidate(sess)
				}
			}
			src, gen = nsrc, ngen
		}
	}
}

// pumpHostToApp copies bytes the host wrote on stream `id` into the app's
// current stdin pipe / pty master (re-fetched per chunk so an in-place
// restart's new fd is picked up; bytes are dropped while the app is momentarily
// down between instances). On stream EOF (host closed its stdin — permanent for
// the sandbox) it closes the current app fd when closeOnEOF and returns; on
// session death it waits for a new session and resumes.
func pumpHostToApp(b *consoleBridge, role appRole, holder *sessionHolder, id uint8, closeOnEOF bool) {
	buf := make([]byte, mux.DataChunk)
	for {
		sess := holder.wait()
		if sess == nil {
			return // shutting down
		}
		err := copyStreamToApp(b, role, sess.Stream(id), buf)
		switch {
		case err == nil, errors.Is(err, io.EOF), errors.Is(err, mux.ErrStreamReset):
			if closeOnEOF {
				if fd := b.writeFd(role); fd != nil {
					_ = fd.Close()
				}
			}
			return
		default:
			// session died (ErrClosed / conn error) → wait for a new one.
			holder.invalidate(sess)
		}
	}
}

// copyStreamToApp drains stream into the app's current fd for role, re-fetching
// the destination per chunk and dropping bytes while the app is down (fd nil /
// write error). Returns the stream read error (io.EOF / reset / session death).
func copyStreamToApp(b *consoleBridge, role appRole, stream *mux.Stream, buf []byte) error {
	for {
		n, rerr := stream.Read(buf)
		if n > 0 {
			if fd := b.writeFd(role); fd != nil {
				_, _ = fd.Write(buf[:n])
			}
		}
		if rerr != nil {
			return rerr
		}
	}
}

// --- sessionHolder --------------------------------------------------

// sessionHolder is a slot for "the current MUX session", with a cond so
// pumps can block until one is present (or block again after one dies).
type sessionHolder struct {
	mu     sync.Mutex
	cond   *sync.Cond
	sess   *mux.Session
	closed bool
}

func newSessionHolder() *sessionHolder {
	h := &sessionHolder{}
	h.cond = sync.NewCond(&h.mu)
	return h
}

func (h *sessionHolder) set(s *mux.Session) {
	h.mu.Lock()
	h.sess = s
	h.cond.Broadcast()
	h.mu.Unlock()
}

// invalidate clears the slot iff it still holds s (so a concurrent
// attach of a newer session isn't clobbered).
func (h *sessionHolder) invalidate(s *mux.Session) {
	h.mu.Lock()
	if h.sess == s {
		h.sess = nil
	}
	h.cond.Broadcast()
	h.mu.Unlock()
}

// wait blocks until a session is present (or shutdown), then returns it
// (nil on shutdown).
func (h *sessionHolder) wait() *mux.Session {
	h.mu.Lock()
	for h.sess == nil && !h.closed {
		h.cond.Wait()
	}
	s := h.sess
	if h.closed {
		s = nil
	}
	h.mu.Unlock()
	return s
}

// peek returns the current session without blocking (nil if none).
func (h *sessionHolder) peek() *mux.Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	return h.sess
}

func (h *sessionHolder) shutdown() {
	h.mu.Lock()
	h.closed = true
	h.sess = nil
	h.cond.Broadcast()
	h.mu.Unlock()
}

// --- pty / winsize helpers (Linux) ----------------------------------

// openPTY allocates a /dev/ptmx + matching /dev/pts/N pair via the
// glibc-equivalent ioctl sequence: open ptmx → unlockpt → look up the
// slave name → open the slave.
func openPTY() (master, slave *os.File, err error) {
	mfd, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = mfd.Close()
		}
	}()
	if err := unix.IoctlSetPointerInt(int(mfd.Fd()), unix.TIOCSPTLCK, 0); err != nil { // unlockpt
		return nil, nil, fmt.Errorf("unlockpt: %w", err)
	}
	n, err := unix.IoctlGetInt(int(mfd.Fd()), unix.TIOCGPTN)
	if err != nil {
		return nil, nil, fmt.Errorf("TIOCGPTN: %w", err)
	}
	name := fmt.Sprintf("/dev/pts/%d", n)
	sfd, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", name, err)
	}
	return mfd, sfd, nil
}

func setWinsize(fd int, cols, rows uint16) error {
	return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
}
