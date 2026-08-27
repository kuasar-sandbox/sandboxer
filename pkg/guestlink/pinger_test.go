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

// TestPinger_TickAndPause runs a fake guest behind a fakeCHProxy and
// verifies the ticker fires, RTT samples accumulate, and Pause stops
// new attempts without tearing the goroutine down.
func TestPinger_TickAndPause(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	var seen atomic.Uint64
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
		_ = proto.WriteMessage(c, &proto.Message{
			Type:    proto.TypePong,
			ID:      req.ID,
			TSendNs: req.TSendNs,
		})
	})
	defer proxy.close()

	p := &Pinger{
		Client: &HostClient{BasePath: base},
		Cfg:    PingerConfig{Interval: 30 * time.Millisecond, Timeout: 200 * time.Millisecond},
		Stats:  &PingStats{},
		Logf:   t.Logf,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p.Start(ctx)
	defer p.Stop()

	// Wait for at least 3 pings.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if seen.Load() >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if seen.Load() < 3 {
		t.Fatalf("only %d pings seen", seen.Load())
	}

	p.Pause()
	frozen := seen.Load()
	time.Sleep(150 * time.Millisecond)
	if seen.Load() > frozen+1 {
		// One more tick may slip in if Pause races with the goroutine
		// already inside RoundTrip; allow at most +1.
		t.Errorf("pause did not stop pings: %d -> %d", frozen, seen.Load())
	}

	p.Resume()
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if seen.Load() > frozen+1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if seen.Load() <= frozen+1 {
		t.Errorf("resume did not restart pings: %d -> %d", frozen, seen.Load())
	}

	snap := p.Stats.Snapshot()
	if snap.Success == 0 {
		t.Errorf("Snapshot.Success = 0")
	}
	if snap.RTTAvgNs == 0 {
		t.Errorf("Snapshot.RTTAvgNs = 0")
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

func TestDialRawForRestore_BoundsStalledRetryAttempt(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")

	var connects atomic.Uint32
	releaseStall := make(chan struct{})
	proxy := newFakeCHProxyWithBeforeOK(t, base, func(net.Conn) bool {
		if connects.Add(1) == 1 {
			return true // first attempt: transient EOF before OK
		}
		<-releaseStall // retry: CONNECT accepted, but CH never emits OK
		return true
	}, func(net.Conn) {
		t.Error("guest received a connection before CH acknowledgement")
	})
	defer proxy.close()
	defer close(releaseStall)

	done := make(chan error, 1)
	go func() {
		conn, err := dialRawForRestore(&HostClient{BasePath: base, Logf: t.Logf}, 2*time.Second, 100*time.Millisecond)
		if conn != nil {
			_ = conn.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "restore pre-request connect failed") {
			t.Fatalf("error = %v, want bounded retry failure", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stalled retry exceeded its retry window")
	}
	if got := connects.Load(); got != 2 {
		t.Fatalf("CONNECT attempts = %d, want 2", got)
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
