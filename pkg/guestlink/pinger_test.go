package guestlink

import (
	"context"
	"io"
	"net"
	"path/filepath"
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

	conn, spec, err := OpenMUXViaRestore(&HostClient{BasePath: base}, 3, nil, nil, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if spec.TTY || !spec.Stdout || !spec.Stderr {
		t.Errorf("restore_ack stdio mismatch: %+v", spec)
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
