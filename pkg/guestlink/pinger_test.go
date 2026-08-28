package guestlink

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// TestPinger_TickAndPause runs a fake guest behind a fakeCHProxy and verifies
// Pause joins an in-flight probe through guest connection close before
// returning, while Resume admits a later probe without tearing down the ticker
// goroutine.
func TestPinger_TickAndPause(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	var seen atomic.Uint64
	requests := make(chan uint64, 4)
	release := make(chan struct{}, 4)
	peerDone := make(chan error, 4)
	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		req, err := proto.ReadMessage(c)
		if err != nil {
			return
		}
		if req.Type != proto.TypePing {
			t.Errorf("guest got %s", req.Type)
			return
		}
		seen.Add(1)
		requests <- req.ID
		<-release
		writeErr := proto.WriteMessage(c, &proto.Message{
			Type:    proto.TypePong,
			ID:      req.ID,
			TSendNs: req.TSendNs,
		})
		peerDone <- writeErr
	})
	defer proxy.close()

	p := &Pinger{
		Client: &HostClient{BasePath: base},
		Cfg:    PingerConfig{Interval: 10 * time.Millisecond, Timeout: 2 * time.Second},
		Stats:  &PingStats{},
		Logf:   t.Logf,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p.Start(ctx)
	defer p.Stop()

	select {
	case <-requests: // first ping is deliberately held in-flight
	case <-ctx.Done():
		t.Fatal("first ping was not admitted")
	}
	pauseDone := make(chan error, 1)
	go func() { pauseDone <- p.PauseContext(ctx) }()
	select {
	case err := <-pauseDone:
		t.Fatalf("PauseContext returned before the in-flight ping completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if got := seen.Load(); got != 1 {
		t.Fatalf("pings admitted before pause barrier = %d, want 1", got)
	}
	release <- struct{}{}
	<-peerDone
	select {
	case err := <-pauseDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("PauseContext did not join the completed ping")
	}
	p.Resume()
	select {
	case <-requests:
	case <-ctx.Done():
		t.Fatal("resume did not admit a new ping")
	}
	release <- struct{}{}
	if err := <-peerDone; err != nil {
		t.Fatalf("second ping response: %v", err)
	}
	if err := p.PauseContext(ctx); err != nil {
		t.Fatal(err)
	}
	if got := seen.Load(); got != 2 {
		t.Fatalf("pings after resume barrier = %d, want 2", got)
	}

	snap := p.Stats.Snapshot()
	if snap.Success == 0 {
		t.Errorf("Snapshot.Success = 0")
	}
	if snap.RTTAvgNs == 0 {
		t.Errorf("Snapshot.RTTAvgNs = 0")
	}
}

func TestPingerPauseWaitsForGuestConnectionClose(t *testing.T) {
	base := filepath.Join(t.TempDir(), "vsock.sock")
	responseWritten := make(chan struct{})
	releaseClose := make(chan struct{})
	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		req, err := proto.ReadMessage(c)
		if err != nil {
			return
		}
		if err := proto.WriteMessage(c, &proto.Message{
			Type: proto.TypePong, ID: req.ID, TSendNs: req.TSendNs,
		}); err != nil {
			return
		}
		close(responseWritten)
		<-releaseClose
	})
	defer proxy.close()

	p := &Pinger{
		Client: &HostClient{BasePath: base},
		Cfg:    PingerConfig{Interval: time.Hour, Timeout: 2 * time.Second},
		Stats:  &PingStats{},
		Logf:   t.Logf,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.Start(ctx)
	defer p.Stop()
	<-responseWritten

	pauseDone := make(chan error, 1)
	go func() { pauseDone <- p.PauseContext(ctx) }()
	select {
	case err := <-pauseDone:
		t.Fatalf("PauseContext returned before the guest closed the ping connection: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseClose)
	select {
	case err := <-pauseDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("PauseContext did not finish after the guest connection closed")
	}
}

func TestPingerPauseBoundsAndJoinsNoForcedTimeoutProbe(t *testing.T) {
	base := filepath.Join(t.TempDir(), "vsock.sock")
	requestRead := make(chan struct{})
	peerEOF := make(chan error, 1)
	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		if _, err := proto.ReadMessage(c); err != nil {
			peerEOF <- err
			return
		}
		close(requestRead)
		var one [1]byte
		_, err := c.Read(one[:])
		peerEOF <- err
	})
	defer proxy.close()

	p := &Pinger{
		Client: &HostClient{BasePath: base},
		Cfg:    PingerConfig{Interval: time.Hour, Timeout: time.Hour},
		Stats:  &PingStats{},
		Logf:   t.Logf,
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	p.Start(runCtx)
	defer func() {
		cancelRun()
		p.Stop()
	}()
	select {
	case <-requestRead:
	case <-time.After(time.Second):
		t.Fatal("ping was not admitted")
	}

	const drainTimeout = 50 * time.Millisecond
	if err := p.pauseContext(context.Background(), drainTimeout); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pauseContext error = %v, want bounded context deadline", err)
	}
	select {
	case err := <-peerEOF:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("guest read after canceled pause = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PauseContext returned without aborting and joining the admitted ping")
	}
	p.tickMu.Lock()
	done := p.tickDone
	p.tickMu.Unlock()
	if done != nil {
		t.Fatal("PauseContext returned before the canceled ping cleared its barrier state")
	}
}

// TestPinger_FatalThreshold simulates a guest that accepts but never
// replies to pings; verifies the host fires OnFatal after the configured
// threshold and exactly once (sync.Once), regardless of subsequent ticks.
func TestPinger_FatalThreshold(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	// Guest reads the request and never replies — host RoundTrip's read
	// times out, counts as failure. Each ping = fresh conn = fresh handler.
	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		_, _ = proto.ReadMessage(c)
		time.Sleep(500 * time.Millisecond) // hold so the read deadline trips on the host
	})
	defer proxy.close()

	p := &Pinger{
		Client: &HostClient{BasePath: base},
		Cfg: PingerConfig{
			Interval:       20 * time.Millisecond,
			Timeout:        40 * time.Millisecond,
			FatalThreshold: 3,
		},
		Stats: &PingStats{},
		Logf:  t.Logf,
	}
	var fatalCount atomic.Uint32
	var firstErr atomic.Value
	p.SetOnFatal(func(err error) {
		if firstErr.Load() == nil {
			firstErr.Store(err)
		}
		fatalCount.Add(1)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	// 3 failed ticks × (~40ms timeout + ~20ms interval) ≈ 180ms.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && fatalCount.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := fatalCount.Load(); got != 1 {
		t.Fatalf("OnFatal fired %d times, want 1", got)
	}
	if firstErr.Load() == nil {
		t.Fatal("OnFatal received nil err")
	}

	// Additional failing ticks must NOT re-fire OnFatal (sync.Once guard).
	time.Sleep(200 * time.Millisecond)
	if got := fatalCount.Load(); got != 1 {
		t.Fatalf("OnFatal fired %d times after grace, want still 1", got)
	}
}

// TestPinger_FatalThresholdZeroDisabled: with FatalThreshold = 0 (default)
// OnFatal never fires even if every tick fails.
func TestPinger_FatalThresholdZeroDisabled(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")
	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		_, _ = proto.ReadMessage(c)
		time.Sleep(500 * time.Millisecond)
	})
	defer proxy.close()

	p := &Pinger{
		Client: &HostClient{BasePath: base},
		Cfg: PingerConfig{
			Interval:       20 * time.Millisecond,
			Timeout:        40 * time.Millisecond,
			FatalThreshold: 0, // explicitly disabled
		},
		Stats: &PingStats{},
		Logf:  t.Logf,
	}
	var fatalCount atomic.Uint32
	p.SetOnFatal(func(err error) { fatalCount.Add(1) })

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	<-ctx.Done()
	if got := fatalCount.Load(); got != 0 {
		t.Fatalf("OnFatal fired %d times despite FatalThreshold=0", got)
	}
}

func TestSendQuiesce_Quiesced(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		req, _ := proto.ReadMessage(c)
		if req.Type != proto.TypeQuiesce || !req.SkipDropCaches {
			t.Errorf("got %+v", req)
		}
		_ = proto.WriteMessage(c, &proto.Message{
			Type:             proto.TypeQuiesced,
			DropCachesResult: proto.DropCachesSkipped,
		})
	})
	defer proxy.close()

	result, err := SendQuiesce(&HostClient{BasePath: base}, true)
	if err != nil {
		t.Fatal(err)
	}
	if result != proto.DropCachesSkipped {
		t.Fatalf("result=%q", result)
	}
}

func TestSendQuiesceWaitsForGuestConnectionClose(t *testing.T) {
	base := filepath.Join(t.TempDir(), "vsock.sock")
	ackWritten := make(chan struct{})
	releaseClose := make(chan struct{})
	released := false
	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		_, _ = proto.ReadMessage(c)
		_ = proto.WriteMessage(c, &proto.Message{
			Type: proto.TypeQuiesced, DropCachesResult: proto.DropCachesSkipped,
		})
		close(ackWritten)
		<-releaseClose
	})
	defer func() {
		if !released {
			close(releaseClose)
		}
		proxy.close()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := SendQuiesce(&HostClient{BasePath: base}, true)
		done <- err
	}()
	<-ackWritten
	select {
	case err := <-done:
		t.Fatalf("SendQuiesce returned before guest connection close: %v", err)
	case <-time.After(50 * time.Millisecond): // deadlock guard for the close barrier
	}
	close(releaseClose)
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("SendQuiesce did not finish after guest connection close")
	}
}

func TestSendQuiesceContextCancellationInterruptsCloseBarrier(t *testing.T) {
	base := filepath.Join(t.TempDir(), "vsock.sock")
	ackWritten := make(chan struct{})
	peerClosed := make(chan struct{})
	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		_, _ = proto.ReadMessage(c)
		_ = proto.WriteMessage(c, &proto.Message{
			Type: proto.TypeQuiesced, DropCachesResult: proto.DropCachesSkipped,
		})
		close(ackWritten)
		var one [1]byte
		_, _ = c.Read(one[:])
		close(peerClosed)
	})
	defer proxy.close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := SendQuiesceContext(ctx, &HostClient{BasePath: base}, true)
		done <- err
	}()
	<-ackWritten
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SendQuiesceContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SendQuiesceContext did not interrupt the close barrier")
	}
	select {
	case <-peerClosed:
	case <-time.After(time.Second):
		t.Fatal("canceled quiesce did not close the transport")
	}
}

func TestSendQuiesce_OldGuestResultUnknown(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		_, _ = proto.ReadMessage(c)
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeQuiesced})
	})
	defer proxy.close()

	result, err := SendQuiesce(&HostClient{BasePath: base}, true)
	if err != nil {
		t.Fatal(err)
	}
	if result != proto.DropCachesUnknown {
		t.Fatalf("result=%q, want unknown", result)
	}
}

func TestOpenMUXViaRestore(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		req, _ := proto.ReadMessage(c)
		if req.Type != proto.TypeRestore || req.Epoch != 3 {
			t.Errorf("got %+v", req)
		}
		_ = proto.WriteMessage(c, &proto.Message{
			Type:     proto.TypeRestoreAck,
			Epoch:    req.Epoch,
			Stdio:    &proto.StdioSpec{Stdout: true, Stderr: true},
			AppState: proto.AppStateRunning,
		})
		// Keep the conn alive (it would become the MUX) until the test
		// closes its end.
		_, _ = io.Copy(io.Discard, c)
	})
	defer proxy.close()

	conn, spec, err := OpenMUXViaRestore(&HostClient{BasePath: base}, 3, nil, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if spec.TTY || !spec.Stdout || !spec.Stderr {
		t.Errorf("restore_ack stdio mismatch: %+v", spec)
	}
}

func TestOpenMUXViaRestoreContextCancelsStalledAck(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")
	requestRead := make(chan struct{})
	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		if _, err := proto.ReadMessage(c); err == nil {
			close(requestRead)
		}
		_, _ = io.Copy(io.Discard, c)
	})
	defer proxy.close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := OpenMUXViaRestoreContext(ctx, &HostClient{BasePath: base}, 3, nil, 24*time.Hour)
		errCh <- err
	}()
	select {
	case <-requestRead:
	case <-time.After(time.Second):
		t.Fatal("restore request was not received")
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("OpenMUXViaRestoreContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled restore_ack was not cancelled")
	}
}

func TestOpenMUXViaRestore_RetriesTransientEOFBeforeRequest(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	var connects atomic.Uint32
	var requests atomic.Uint32
	proxy := newFakeCHProxyWithBeforeOK(t, base, func(net.Conn) bool {
		// Model CH accepting CONNECT immediately after /vm.resume, then
		// dropping that first connection before the guest listener is ready.
		return connects.Add(1) == 1
	}, func(c net.Conn) {
		req, err := proto.ReadMessage(c)
		if err != nil {
			t.Errorf("guest read: %v", err)
			return
		}
		requests.Add(1)
		_ = proto.WriteMessage(c, &proto.Message{
			Type:     proto.TypeRestoreAck,
			Epoch:    req.Epoch,
			AppState: proto.AppStateRunning,
		})
	})
	defer proxy.close()

	conn, _, err := OpenMUXViaRestore(&HostClient{BasePath: base, Logf: t.Logf}, 4, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if got := connects.Load(); got != 2 {
		t.Fatalf("CONNECT attempts = %d, want 2", got)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("restore requests = %d, want exactly 1", got)
	}
}

func TestOpenMUXViaAttachRetriesTransientEOFBeforeRequest(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	var connects atomic.Uint32
	var requests atomic.Uint32
	proxy := newFakeCHProxyWithBeforeOK(t, base, func(net.Conn) bool {
		return connects.Add(1) == 1
	}, func(c net.Conn) {
		req, err := proto.ReadMessage(c)
		if err != nil {
			t.Errorf("guest read: %v", err)
			return
		}
		requests.Add(1)
		if req.Type != proto.TypeAttach {
			t.Errorf("request type = %q, want attach", req.Type)
		}
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeAttachAck, Epoch: req.Epoch})
	})
	defer proxy.close()

	conn, _, err := OpenMUXViaAttach(&HostClient{BasePath: base, Logf: t.Logf}, 4, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if got := connects.Load(); got != 2 {
		t.Fatalf("CONNECT attempts = %d, want 2", got)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("attach requests = %d, want exactly 1", got)
	}
}

func TestOpenMUXViaAttachContextCancelsStalledAck(t *testing.T) {
	base := filepath.Join(t.TempDir(), "vsock.sock")
	requestRead := make(chan struct{})
	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		if _, err := proto.ReadMessage(c); err == nil {
			close(requestRead)
		}
		var one [1]byte
		_, _ = c.Read(one[:])
	})
	defer proxy.close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := OpenMUXViaAttachContext(ctx, &HostClient{BasePath: base}, 1, 24*time.Hour)
		done <- err
	}()
	<-requestRead
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenMUXViaAttachContext error = %v, want context.Canceled", err)
	}
}

func TestOpenMUXViaRestore_DoesNotRetryAfterRequest(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	var connects atomic.Uint32
	var requests atomic.Uint32
	proxy := newFakeCHProxyWithBeforeOK(t, base, func(net.Conn) bool {
		connects.Add(1)
		return false
	}, func(c net.Conn) {
		if _, err := proto.ReadMessage(c); err == nil {
			requests.Add(1)
		}
		// Close without restore_ack. The request may already have taken
		// effect, so this failure must not enter the pre-request retry loop.
	})
	defer proxy.close()

	_, _, err := OpenMUXViaRestore(&HostClient{BasePath: base, Logf: t.Logf}, 5, nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "read restore_ack") {
		t.Fatalf("error = %v, want restore_ack read failure", err)
	}
	if got := connects.Load(); got != 1 {
		t.Fatalf("CONNECT attempts = %d, want 1", got)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("restore requests = %d, want exactly 1", got)
	}
}

func TestOpenMUXViaRestore_TransientRetryIsBounded(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	var connects atomic.Uint32
	proxy := newFakeCHProxyWithBeforeOK(t, base, func(net.Conn) bool {
		connects.Add(1)
		return true
	}, func(net.Conn) {
		t.Error("guest received a connection without CH acknowledging it")
	})
	defer proxy.close()

	started := time.Now()
	_, _, err := OpenMUXViaRestore(&HostClient{BasePath: base, Logf: t.Logf}, 6, nil, 250*time.Millisecond)
	if err == nil {
		t.Fatal("expected bounded pre-request connect failure")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v, want wrapped EOF", err)
	}
	if !strings.Contains(err.Error(), "restore pre-request connect failed") {
		t.Fatalf("error = %v, want retry context", err)
	}
	if got := connects.Load(); got < 2 {
		t.Fatalf("CONNECT attempts = %d, want at least 2", got)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("retry took %v, want bounded failure", elapsed)
	}
}

func TestDialRawForRestore_BoundsEachStalledAttemptAndKeepsRetrying(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	var connects atomic.Uint32
	releaseStall := make(chan struct{})
	proxy := newFakeCHProxyWithBeforeOK(t, base, func(net.Conn) bool {
		switch connects.Add(1) {
		case 1:
			return true // first attempt: transient EOF before OK
		case 2:
			<-releaseStall // second attempt: CH never emits OK
			return true
		default:
			return false // a later attempt reaches the guest
		}
	}, func(net.Conn) {
		// dialRawForRestore stops after CONNECT/OK, before a guest request.
	})
	defer proxy.close()
	defer close(releaseStall)

	done := make(chan struct {
		conn net.Conn
		err  error
	}, 1)
	go func() {
		conn, err := dialRawForRestore(&HostClient{BasePath: base, Logf: t.Logf}, 2*time.Second, 100*time.Millisecond)
		done <- struct {
			conn net.Conn
			err  error
		}{conn: conn, err: err}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("dialRawForRestore: %v", result.err)
		}
		if result.conn == nil {
			t.Fatal("dialRawForRestore returned a nil connection")
		}
		_ = result.conn.Close()
	case <-time.After(500 * time.Millisecond):
		t.Fatal("a stalled CONNECT attempt consumed the overall restore budget")
	}
	if got := connects.Load(); got != 3 {
		t.Fatalf("CONNECT attempts = %d, want 3", got)
	}
}

func TestOpenMUXViaRestore_DoesNotRetryNonTransientHandshakeError(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	var connects atomic.Uint32
	proxy := newFakeCHProxyWithBeforeOK(t, base, func(c net.Conn) bool {
		connects.Add(1)
		// A syntactically invalid CH acknowledgement is a protocol error,
		// not the transient EOF/reset boundary. Fill drainLine's cap without
		// a newline so the client fails deterministically.
		_, _ = c.Write(make([]byte, 64))
		return true
	}, func(net.Conn) {
		t.Error("guest received a connection after invalid CH acknowledgement")
	})
	defer proxy.close()

	_, _, err := OpenMUXViaRestore(&HostClient{BasePath: base, Logf: t.Logf}, 7, nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "OK line not terminated") {
		t.Fatalf("error = %v, want non-transient handshake failure", err)
	}
	if got := connects.Load(); got != 1 {
		t.Fatalf("CONNECT attempts = %d, want 1", got)
	}
}

func TestRetryableRestoreDialError(t *testing.T) {
	if !retryableRestoreDialError(io.EOF) {
		t.Error("EOF should be retryable before the restore request")
	}
	if retryableRestoreDialError(errors.New("bad CONNECT protocol")) {
		t.Error("arbitrary protocol error should fail without retry")
	}
}

func TestSendQuiesce_WrongResponse(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	proxy := newFakeCHProxy(t, base, func(c net.Conn) {
		_, _ = proto.ReadMessage(c)
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeError, Msg: "bad"})
	})
	defer proxy.close()

	_, err := SendQuiesce(&HostClient{BasePath: base}, false)
	if err == nil {
		t.Error("expected error on wrong response")
	}
}
