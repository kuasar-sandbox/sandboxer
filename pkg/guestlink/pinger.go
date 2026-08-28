package guestlink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// PingerConfig tunes the host→guest ping ticker. Defaults match
// docs/sandbox-init.md §4.5.
type PingerConfig struct {
	Interval time.Duration // default 1 s
	Timeout  time.Duration // default proto.DeadlinePing (200 ms)

	// FatalThreshold: after this many consecutive failed pings, fire
	// the Pinger's OnFatal callback (set via SetOnFatal). 0 disables
	// (the default — sandbox-ctl waits for the user / outer signal).
	// With Interval=1s, FatalThreshold=30 ≈ 30 s of unreachability;
	// the lifecycle uses this to SIGTERM CH so cmd.Wait() returns
	// rather than hanging forever on a wedged-but-alive guest.
	FatalThreshold int
}

func (c *PingerConfig) withDefaults() PingerConfig {
	out := *c
	if out.Interval <= 0 {
		out.Interval = 1 * time.Second
	}
	if out.Timeout <= 0 {
		out.Timeout = proto.DeadlinePing
	}
	// FatalThreshold: leave as-is. 0 = disabled.
	return out
}

// Pinger drives the periodic ping/pong probe against sandbox-init's
// reverse-channel listener. It owns a HostClient and a PingStats
// counter aggregator.
//
// Lifecycle (docs/sandbox-init.md §4.9):
//
//   - Created by sandbox.Run / restore.Run with the per-sandbox vsock
//     base path.
//   - Start fires when the cold-start `launch` was written (or when a
//     `restored` was acked after restore).
//   - Pause / Resume bracket the snapshot quiesce window so we don't
//     race with /vm.pause.
//   - Stop is final — typically driven by ctx cancellation when CH
//     exits.
type Pinger struct {
	Client *HostClient
	Cfg    PingerConfig
	Stats  *PingStats
	Logf   func(string, ...any)

	mu       sync.Mutex
	running  atomic.Bool
	paused   atomic.Bool
	cancel   context.CancelFunc
	doneCh   chan struct{}
	nextID   atomic.Uint64
	tickMu   sync.Mutex
	tickDone chan error
	tickEnd  context.CancelFunc

	// FatalThreshold tracking: consecutiveFails counts failures since
	// the last success; once it reaches Cfg.FatalThreshold (and that's
	// > 0), onFatal is invoked exactly once via fatalOnce. Reset to 0
	// on every successful tick. SetOnFatal installs the callback;
	// safe to call once after the pinger is created and before Start.
	consecutiveFails atomic.Uint32
	onFatalMu        sync.Mutex
	onFatal          func(err error)
	fatalOnce        sync.Once
}

// SetOnFatal installs the callback invoked once when FatalThreshold
// consecutive ping failures accumulate. Pass nil to clear. Safe to call
// before Start; thread-safe.
func (p *Pinger) SetOnFatal(fn func(err error)) {
	p.onFatalMu.Lock()
	p.onFatal = fn
	p.onFatalMu.Unlock()
}

// Start launches the ticker goroutine. ctx cancellation stops the
// ticker. Calling Start more than once is a no-op until Stop is called.
func (p *Pinger) Start(ctx context.Context) {
	if !p.running.CompareAndSwap(false, true) {
		return
	}
	if p.Logf == nil {
		p.Logf = func(string, ...any) {}
	}
	cfg := p.Cfg.withDefaults()
	ctx, cancel := context.WithCancel(ctx)

	p.mu.Lock()
	p.cancel = cancel
	p.doneCh = make(chan struct{})
	p.mu.Unlock()

	go p.loop(ctx, cfg)
}

// Stop signals the ticker to exit and waits for it. Idempotent.
func (p *Pinger) Stop() {
	if !p.running.CompareAndSwap(true, false) {
		return
	}
	p.mu.Lock()
	cancel := p.cancel
	doneCh := p.doneCh
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if doneCh != nil {
		<-doneCh
	}
}

// Pause halts ping sends temporarily and joins any probe already admitted by
// the ticker through the guest-side connection-close barrier. Used during
// capture so no half-closed management connection can cross the quiesce →
// /vm.pause boundary.
func (p *Pinger) Pause() { _ = p.PauseContext(context.Background()) }

// PauseContext is the cancellable capture barrier form of Pause. An admitted
// probe is allowed to finish through guest EOF, but can never hold the capture
// barrier beyond the protocol quiesce budget even when timeouts.ping disables
// its ordinary health-probe deadline.
func (p *Pinger) PauseContext(ctx context.Context) error {
	return p.pauseContext(ctx, proto.DeadlineQuiesce)
}

func (p *Pinger) pauseContext(ctx context.Context, drainTimeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.paused.Store(true)
	p.tickMu.Lock()
	done := p.tickDone
	end := p.tickEnd
	p.tickMu.Unlock()
	if done == nil {
		return ctx.Err()
	}
	drainCtx, cancelDrain := context.WithTimeout(ctx, drainTimeout)
	defer cancelDrain()
	select {
	case err := <-done:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	case <-drainCtx.Done():
		// Context cancellation is wired to the concrete short connection's
		// deadline. Join after cancel so capture never continues while a stale
		// ping still owns a host/guest vsock flow.
		if end != nil {
			end()
		}
		<-done
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("ping capture drain exceeded %s: %w", drainTimeout, drainCtx.Err())
	}
}

func (p *Pinger) Resume() { p.paused.Store(false) }

// Running reports whether the ticker is active (Start called, Stop not yet).
func (p *Pinger) Running() bool { return p.running.Load() }

func (p *Pinger) loop(ctx context.Context, cfg PingerConfig) {
	defer func() {
		p.mu.Lock()
		dc := p.doneCh
		p.mu.Unlock()
		if dc != nil {
			close(dc)
		}
	}()

	t := time.NewTicker(cfg.Interval)
	defer t.Stop()

	// Fire first ping immediately — we want sub-ms detection of "guest
	// agent ready" right after launch is sent.
	p.tick(ctx, cfg.Timeout)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx, cfg.Timeout)
		}
	}
}

func (p *Pinger) tick(parent context.Context, timeout time.Duration) {
	p.tickMu.Lock()
	if p.paused.Load() {
		p.tickMu.Unlock()
		return
	}
	tickCtx, tickEnd := context.WithCancel(parent)
	done := make(chan error, 1)
	p.tickDone = done
	p.tickEnd = tickEnd
	p.tickMu.Unlock()
	var barrierErr error
	defer func() {
		tickEnd()
		p.tickMu.Lock()
		if p.tickDone == done {
			p.tickDone = nil
			p.tickEnd = nil
		}
		p.tickMu.Unlock()
		done <- barrierErr
		close(done)
	}()
	if p.Stats == nil {
		p.Stats = &PingStats{}
	}
	id := p.nextID.Add(1)
	tSend := time.Now()
	p.Stats.Attempts.Add(1)
	resp, err := p.Client.roundTripUntilEOFContext(tickCtx, &proto.Message{
		Type:    proto.TypePing,
		ID:      id,
		TSendNs: tSend.UnixNano(),
	}, timeout)
	if err != nil {
		barrierErr = err
		if tickCtx.Err() != nil {
			return
		}
		// Best-effort classification — RoundTrip returns wrapped errors.
		// We treat any error containing "deadline" / "i/o timeout" as a
		// ping_timeout and everything else as a dial error. This keeps
		// the metric meaningful without requiring a typed-error sprawl.
		s := err.Error()
		if containsAny(s, "deadline", "i/o timeout", "timed out") {
			p.Stats.Timeout.Add(1)
		} else {
			p.Stats.DialError.Add(1)
		}
		p.Logf("ping id=%d err=%v", id, err)
		p.recordFailure(err)
		return
	}
	if resp.Type != proto.TypePong || resp.ID != id {
		p.Stats.DialError.Add(1)
		p.Logf("ping id=%d unexpected resp %+v", id, resp)
		barrierErr = fmt.Errorf("unexpected response %q (id=%d)", resp.Type, resp.ID)
		p.recordFailure(barrierErr)
		return
	}
	p.Stats.Success.Add(1)
	p.Stats.observeRTT(time.Since(tSend))
	p.consecutiveFails.Store(0)
}

// recordFailure bumps the consecutive-failure counter and fires OnFatal
// the first time the count reaches FatalThreshold. Threshold == 0
// disables the mechanism (counter still advances harmlessly).
func (p *Pinger) recordFailure(err error) {
	threshold := p.Cfg.FatalThreshold
	if threshold <= 0 {
		return
	}
	n := p.consecutiveFails.Add(1)
	if int(n) < threshold {
		return
	}
	p.onFatalMu.Lock()
	fn := p.onFatal
	p.onFatalMu.Unlock()
	if fn == nil {
		return
	}
	p.fatalOnce.Do(func() {
		p.Logf("ping: %d consecutive failures (last=%v) — invoking OnFatal", n, err)
		go fn(err)
	})
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

// indexOf is a 0-dep substring search to avoid pulling in strings.Contains
// in this file (kept package-local; pkg/sandbox already imports strings
// elsewhere — this is just to keep the helper close to its caller).
func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// SendQuiesce performs a host→guest quiesce request/response on a
// short-lived connection and returns the guest's cache-drop outcome.
// Unknown means the guest predates outcome reporting. Caller is expected to
// have paused the ping ticker first to avoid concurrent host→guest traffic
// during the snapshot pre-pause window.
func SendQuiesce(client *HostClient, skipDropCaches bool) (proto.DropCachesResult, error) {
	return SendQuiesceContext(context.Background(), client, skipDropCaches)
}

// SendQuiesceContext is SendQuiesce with cancellation covering the full
// request, response, and guest-side connection-close transport barrier.
func SendQuiesceContext(ctx context.Context, client *HostClient, skipDropCaches bool) (proto.DropCachesResult, error) {
	resp, err := client.roundTripUntilEOFContext(ctx, &proto.Message{
		Type:           proto.TypeQuiesce,
		SkipDropCaches: skipDropCaches,
	}, proto.DeadlineQuiesce)
	if err != nil {
		return proto.DropCachesUnknown, err
	}
	if resp.Type != proto.TypeQuiesced {
		return proto.DropCachesUnknown, &protoMismatchErr{want: proto.TypeQuiesced, got: resp.Type, msg: resp.Msg}
	}
	switch resp.DropCachesResult {
	case proto.DropCachesSkipped, proto.DropCachesSucceeded, proto.DropCachesFailed:
		return resp.DropCachesResult, nil
	default:
		return proto.DropCachesUnknown, nil
	}
}

// OpenMUXViaRestore notifies the guest that a restore has completed and
// turns that connection into the stdio MUX (docs/sandbox-init.md
// §4.3 / §4.5): it sends `restore{epoch}`, reads `restore_ack{stdio,
// app_state}`, clears the handshake deadline, and returns the live
// connection together with the channel set the guest established. The
// caller wraps the conn in a mux.Session and bridges those streams, then
// (re)starts the ping ticker. epoch distinguishes successive restores.
//
// network (optional) carries a fresh guest IP-layer config the guest
// re-applies flush-and-replace before thawing, so a clone restored from a
// golden snapshot takes a new network identity. nil → keep the snapshot's.
func OpenMUXViaRestore(client *HostClient, epoch uint32, network *proto.NetworkSpec, deadline time.Duration) (net.Conn, proto.StdioSpec, error) {
	return OpenMUXViaRestoreContext(context.Background(), client, epoch, network, deadline)
}

// OpenMUXViaRestoreContext is OpenMUXViaRestore with cancellation covering
// both the pre-request CONNECT/OK retry window and the one-shot restore_ack.
func OpenMUXViaRestoreContext(ctx context.Context, client *HostClient, epoch uint32, network *proto.NetworkSpec, deadline time.Duration) (net.Conn, proto.StdioSpec, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := dialRawForRestoreContext(ctx, client, deadline, restoreDialAttemptTimeout)
	if err != nil {
		return nil, proto.StdioSpec{}, err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })

	// WallclockNs lets the guest jump CLOCK_REALTIME forward by the
	// dormant interval (CH reloads the snapshot's stale clock verbatim).
	// Captured after the connection is ready, as close to the send as
	// possible; the residual
	// host→guest propagation skew is sub-ms (kernel-microsecond dial,
	// the guest's reverse-channel listener survived the snapshot).
	rawConn := conn
	resultConn, spec, err := finishOpenMUX(rawConn, &proto.Message{
		Type:        proto.TypeRestore,
		Epoch:       epoch,
		WallclockNs: time.Now().UnixNano(),
		Network:     network,
	}, proto.TypeRestoreAck)
	if !stopCancel() {
		_ = rawConn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, proto.StdioSpec{}, ctxErr
		}
		return nil, proto.StdioSpec{}, context.Canceled
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, proto.StdioSpec{}, ctxErr
		}
		return nil, proto.StdioSpec{}, err
	}
	// finishOpenMUX clears the handshake deadline before returning. Clear it
	// once more after stopping the cancellation callback to make the ownership
	// handoff explicit.
	_ = resultConn.SetDeadline(time.Time{})
	return resultConn, spec, nil
}

const (
	restoreDialAttemptTimeout = 2 * time.Second
	restoreDialRetryInitial   = 25 * time.Millisecond
	restoreDialRetryMax       = 200 * time.Millisecond
)

// dialRawForRestore retries only the pre-request hybrid-vsock handshake. After
// /vm.resume, CH can transiently reset a host-initiated connection before it
// emits its "OK <port>" line while the restored guest listener is becoming
// runnable. DialRaw has not written a proto request at that point, so redialing
// cannot replay restore. finishOpenMUX deliberately remains outside this loop:
// once restore is written, every error fails closed. Each CONNECT/OK attempt is
// bounded independently so a wedged CH proxy cannot consume the whole restore
// budget; transient failures keep retrying until the configured overall
// restore deadline or context cancellation. A successful CONNECT/OK restores
// the overall deadline before the request is written.
func dialRawForRestore(client *HostClient, deadline, attemptTimeout time.Duration) (net.Conn, error) {
	return dialRawPreRequestContext(context.Background(), client, deadline, attemptTimeout, "restore")
}

func dialRawForRestoreContext(ctx context.Context, client *HostClient, deadline, attemptTimeout time.Duration) (net.Conn, error) {
	return dialRawPreRequestContext(ctx, client, deadline, attemptTimeout, "restore")
}

func dialRawPreRequestContext(ctx context.Context, client *HostClient, deadline, attemptTimeout time.Duration, operation string) (net.Conn, error) {
	deadlineAt := time.Now().Add(deadline)
	if attemptTimeout <= 0 {
		attemptTimeout = deadline
	}

	backoff := restoreDialRetryInitial
	var lastErr error
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptDeadline := time.Now().Add(attemptTimeout)
		if deadlineAt.Before(attemptDeadline) {
			attemptDeadline = deadlineAt
		}
		remaining := time.Until(attemptDeadline)
		if remaining <= 0 {
			if lastErr != nil {
				return nil, fmt.Errorf("%s pre-request connect failed after %d attempts: %w", operation, attempt-1, lastErr)
			}
			return nil, fmt.Errorf("%s pre-request connect: deadline exceeded after %d attempts", operation, attempt-1)
		}

		conn, err := client.DialRawContext(ctx, remaining)
		if err == nil {
			// A retry's CONNECT/OK exchange is capped by retryUntil. Restore
			// write+ACK still owns the original overall restore deadline.
			if err := conn.SetDeadline(deadlineAt); err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("%s pre-request connect: overall deadline: %w", operation, err)
			}
			if attempt > 1 && client.Logf != nil {
				client.Logf("%s pre-request connect recovered on attempt %d", operation, attempt)
			}
			return conn, nil
		}
		if !retryableRestoreDialError(err) {
			return nil, err
		}
		lastErr = err

		retryRemaining := time.Until(deadlineAt)
		if retryRemaining <= backoff {
			return nil, fmt.Errorf("%s pre-request connect failed after %d attempts: %w", operation, attempt, err)
		}
		if client.Logf != nil {
			client.Logf("%s pre-request connect attempt %d failed: %v; retrying in %s", operation, attempt, err, backoff)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		}
		backoff *= 2
		if backoff > restoreDialRetryMax {
			backoff = restoreDialRetryMax
		}
	}
}

func retryableRestoreDialError(err error) bool {
	var netErr net.Error
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) ||
		(errors.As(err, &netErr) && netErr.Timeout())
}

// OpenMUXViaAttach re-establishes the stdio MUX after the previous one
// broke (sandbox-ctl's own reliability fallback — not a hand-off to a
// different process). Same shape as OpenMUXViaRestore; on receipt the
// guest gracefully closes any still-live old MUX (or hard-drops it),
// then ACKs on the new connection.
func OpenMUXViaAttach(client *HostClient, epoch uint32, deadline time.Duration) (net.Conn, proto.StdioSpec, error) {
	return OpenMUXViaAttachContext(context.Background(), client, epoch, deadline)
}

// OpenMUXViaAttachContext is OpenMUXViaAttach with cancellation covering both
// the pre-request CONNECT/OK retry window and the one-shot attach_ack.
func OpenMUXViaAttachContext(ctx context.Context, client *HostClient, epoch uint32, deadline time.Duration) (net.Conn, proto.StdioSpec, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := dialRawPreRequestContext(ctx, client, deadline, restoreDialAttemptTimeout, "attach")
	if err != nil {
		return nil, proto.StdioSpec{}, err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	rawConn := conn
	resultConn, spec, err := finishOpenMUX(rawConn, &proto.Message{Type: proto.TypeAttach, Epoch: epoch}, proto.TypeAttachAck)
	if !stopCancel() {
		_ = rawConn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, proto.StdioSpec{}, ctxErr
		}
		return nil, proto.StdioSpec{}, context.Canceled
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, proto.StdioSpec{}, ctxErr
		}
		return nil, proto.StdioSpec{}, err
	}
	_ = resultConn.SetDeadline(time.Time{})
	return resultConn, spec, nil
}

// OpenMUXViaExec starts a fresh ad-hoc command inside the running
// sandbox and turns the reverse-channel connection into that session's
// stdio MUX. Same shape as OpenMUXViaAttach, but the guest forks+execs
// the ExecSpec command (a sibling of the user app) and echoes the
// stdio it established in exec_ack. The returned conn carries the MUX
// end-to-end; the caller (run process) pipes it to the CLI.
func OpenMUXViaExec(client *HostClient, spec *proto.ExecSpec, deadline time.Duration) (net.Conn, proto.StdioSpec, error) {
	return openMUXViaExecContext(context.Background(), client, spec, deadline)
}

// openMUXViaExecContext keeps lifecycle cancellation armed through exec_ack.
// It stays package-private because the context belongs to the run process's
// capture admission; CLI callers continue to use OpenMUXViaExec.
func openMUXViaExecContext(ctx context.Context, client *HostClient, spec *proto.ExecSpec, deadline time.Duration) (net.Conn, proto.StdioSpec, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := client.DialRawContext(ctx, deadline)
	if err != nil {
		return nil, proto.StdioSpec{}, err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	rawConn := conn
	resultConn, established, err := finishOpenMUX(rawConn,
		&proto.Message{Type: proto.TypeExec, Exec: spec}, proto.TypeExecAck)
	if !stopCancel() {
		_ = rawConn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, proto.StdioSpec{}, ctxErr
		}
		return nil, proto.StdioSpec{}, context.Canceled
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, proto.StdioSpec{}, ctxErr
		}
		return nil, proto.StdioSpec{}, err
	}
	// finishOpenMUX clears the handshake deadline. Clear it again after the
	// cancellation callback is stopped to make the live-MUX handoff explicit.
	_ = resultConn.SetDeadline(time.Time{})
	return resultConn, established, nil
}

func finishOpenMUX(conn net.Conn, req *proto.Message, wantAck string) (net.Conn, proto.StdioSpec, error) {
	if err := proto.WriteMessage(conn, req); err != nil {
		_ = conn.Close()
		return nil, proto.StdioSpec{}, fmt.Errorf("write %s: %w", req.Type, err)
	}
	resp, err := proto.ReadMessage(conn)
	if err != nil {
		_ = conn.Close()
		return nil, proto.StdioSpec{}, fmt.Errorf("read %s: %w", wantAck, err)
	}
	if resp.Type != wantAck {
		_ = conn.Close()
		return nil, proto.StdioSpec{}, &protoMismatchErr{want: wantAck, got: resp.Type, msg: resp.Msg}
	}
	// Hand-off: the MUX session manages its own per-frame timing.
	_ = conn.SetDeadline(time.Time{})
	var spec proto.StdioSpec
	if resp.Stdio != nil {
		spec = *resp.Stdio
	}
	return conn, spec, nil
}

type protoMismatchErr struct {
	want, got, msg string
}

func (e *protoMismatchErr) Error() string {
	if e.msg != "" {
		return "proto: want " + e.want + ", got " + e.got + " (msg=" + e.msg + ")"
	}
	return "proto: want " + e.want + ", got " + e.got
}
