package resctl

import (
	"context"
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCHResize spins a tiny HTTP/1.1-over-UDS server that records every
// /api/v1/vm.resize PUT and replies 204. Mirrors enough of CH to exercise
// BalloonController.callResize end-to-end.
type fakeCHResize struct {
	sock    string
	listen  net.Listener
	wg      sync.WaitGroup
	mu      sync.Mutex
	calls   []uint64 // desired_balloon values seen, in order
	stopped atomic.Bool
}

func newFakeCHResize(t *testing.T) *fakeCHResize {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "ch.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeCHResize{sock: sock, listen: l}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.resize", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DesiredBalloon uint64 `json:"desired_balloon"`
		}
		// minimal JSON parse; CH's body is one short line.
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		_ = body
		var v uint64
		// Locate "desired_balloon": <num> manually to avoid an extra dep.
		raw := string(buf[:n])
		for i := 0; i < len(raw); i++ {
			if raw[i] >= '0' && raw[i] <= '9' {
				for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
					v = v*10 + uint64(raw[i]-'0')
					i++
				}
				break
			}
		}
		s.mu.Lock()
		s.calls = append(s.calls, v)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = srv.Serve(l)
	}()
	t.Cleanup(func() {
		s.stopped.Store(true)
		_ = srv.Shutdown(context.Background())
		s.wg.Wait()
	})
	return s
}

func (s *fakeCHResize) seen() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint64, len(s.calls))
	copy(out, s.calls)
	return out
}

// TestBalloon_SetAllocatableComputesTarget: SetAllocatable(alloc) should
// store balloon = Capacity - alloc; clamp to 0 when alloc >= Capacity.
func TestBalloon_SetAllocatableComputesTarget(t *testing.T) {
	b := NewBalloonController("/dev/null", 8<<30, nil)
	b.SetAllocatable(256 << 20)
	if got := b.CurrentTarget(); got != (8<<30)-(256<<20) {
		t.Errorf("target = %d, want %d", got, (8<<30)-(256<<20))
	}
	b.SetAllocatable(8 << 30)
	if got := b.CurrentTarget(); got != 0 {
		t.Errorf("alloc == cap → target should be 0, got %d", got)
	}
	b.SetAllocatable(16 << 30) // > cap
	if got := b.CurrentTarget(); got != 0 {
		t.Errorf("alloc > cap → target should be 0, got %d", got)
	}
}

func TestBalloon_SeedAppliedAllocatableSkipsInitialResize(t *testing.T) {
	srv := newFakeCHResize(t)
	b := NewBalloonController(srv.sock, 8<<30, nil)
	b.Interval = 100 * time.Millisecond
	b.SeedAppliedAllocatable(256 << 20)
	wantInitial := uint64((8 << 30) - (256 << 20))
	if got := b.CurrentTarget(); got != wantInitial {
		t.Fatalf("target = %d, want %d", got, wantInitial)
	}
	if got := b.CurrentActual(); got != wantInitial {
		t.Fatalf("actual = %d, want %d", got, wantInitial)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()
	if calls := srv.seen(); len(calls) != 0 {
		t.Fatalf("seeded Start issued resize calls: %v", calls)
	}

	time.Sleep(b.Interval + 20*time.Millisecond)
	b.SetAllocatable(512 << 20)
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) && len(srv.seen()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	calls := srv.seen()
	if len(calls) != 1 || calls[0] != (8<<30)-(512<<20) {
		t.Fatalf("later resize calls = %v", calls)
	}
}

// TestBalloon_KickFiresImmediatelyWhenElapsed: when the previous
// reconcile is older than Interval, a SetAllocatable should trigger an
// immediate /vm.resize call (not wait for the ticker).
func TestBalloon_KickFiresImmediatelyWhenElapsed(t *testing.T) {
	srv := newFakeCHResize(t)
	b := NewBalloonController(srv.sock, 8<<30, nil)
	b.Interval = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.SetAllocatable(256 << 20) // target = (8G - 256M)
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// Initial reconcile during Start. Wait long enough for the next-allowed
	// kick window to open (just past Interval), then send a new target.
	time.Sleep(b.Interval + 50*time.Millisecond)

	b.SetAllocatable(512 << 20) // target = (8G - 512M)
	// Kick should fire within a few ms; allow generous wait for CI jitter.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(srv.seen()) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	calls := srv.seen()
	if len(calls) < 2 {
		t.Fatalf("expected ≥ 2 resize calls (initial + kick), got %d: %v", len(calls), calls)
	}
	if calls[0] != (8<<30)-(256<<20) {
		t.Errorf("call[0] = %d, want %d (initial)", calls[0], (8<<30)-(256<<20))
	}
	if calls[1] != (8<<30)-(512<<20) {
		t.Errorf("call[1] = %d, want %d (kicked)", calls[1], (8<<30)-(512<<20))
	}
}

// TestBalloon_KickRateLimited: two rapid SetAllocatable calls within
// MinInterval should produce at most ONE extra Reconcile beyond the
// initial Start-time Reconcile (the second is rate-limited and falls
// through to the next ticker tick, which lands on the latest target —
// so we observe at most 2 distinct on-wire values).
func TestBalloon_KickRateLimited(t *testing.T) {
	srv := newFakeCHResize(t)
	b := NewBalloonController(srv.sock, 8<<30, nil)
	b.Interval = 500 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.SetAllocatable(256 << 20) // target 0
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// Two kicks back-to-back, well within MinInterval of the Start
	// reconcile (which just happened):
	time.Sleep(10 * time.Millisecond)
	b.SetAllocatable(300 << 20)
	time.Sleep(5 * time.Millisecond)
	b.SetAllocatable(400 << 20)

	// Wait less than MinInterval — no extra reconcile yet.
	time.Sleep(b.Interval / 4)
	calls := srv.seen()
	if len(calls) != 1 {
		t.Fatalf("within Interval/4 expected just 1 call (the initial), got %d: %v",
			len(calls), calls)
	}

	// Now wait past MinInterval — the pending target (400 MiB alloc) is
	// applied by the next ticker tick.
	time.Sleep(b.Interval)
	calls = srv.seen()
	if len(calls) < 2 {
		t.Fatalf("after one full Interval expected ≥ 2 calls, got %d: %v",
			len(calls), calls)
	}
	last := calls[len(calls)-1]
	wantLast := uint64((8 << 30) - (400 << 20))
	if last != wantLast {
		t.Errorf("last call = %d, want %d (latest target, dropped intermediates)",
			last, wantLast)
	}
}

// TestHooks_OnAllocatableChangedTouchesBalloon: dynamic-mode runtime
// adjustment must call balloonCtl.SetAllocatable (and not open its own
// HTTP path).
func TestHooks_OnAllocatableChangedTouchesBalloon(t *testing.T) {
	b := NewBalloonController("/dev/null", 8<<30, nil)
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "1GiB"},
		},
	}
	cfg.ApplyDefaults()
	h := &ControllerHooks{
		opts: ControllerHookOptions{Balloon: b, Logf: func(string, ...any) {}},
		cfg:  cfg,
	}
	if err := h.OnAllocatableChanged(2 << 30); err != nil {
		t.Fatalf("OnAllocatableChanged: %v", err)
	}
	want := uint64(8<<30) - uint64(2<<30)
	if got := b.CurrentTarget(); got != want {
		t.Errorf("balloon target = %d, want %d", got, want)
	}
	if got := h.AllocatableNowMem(); got != 2<<30 {
		t.Errorf("allocatableNowMem = %d, want %d", got, 2<<30)
	}
}

// TestHooks_SetAllocatableNowDoesNotTouchBalloon: restore.go pre-resume
// path — record state without any external side effect.
func TestHooks_SetAllocatableNowDoesNotTouchBalloon(t *testing.T) {
	b := NewBalloonController("/dev/null", 8<<30, nil)
	h := &ControllerHooks{opts: ControllerHookOptions{Balloon: b, Logf: func(string, ...any) {}}}
	// Seed balloon target to a known value so we can detect spurious writes.
	b.SetAllocatable(4 << 30)
	wantTarget := b.CurrentTarget()
	h.SetAllocatableNow(1 << 30)
	if got := h.AllocatableNowMem(); got != 1<<30 {
		t.Errorf("allocatableNowMem = %d, want %d", got, 1<<30)
	}
	if got := b.CurrentTarget(); got != wantTarget {
		t.Errorf("balloon target changed under SetAllocatableNow: %d → %d", wantTarget, got)
	}
}

// TestHooks_SettledRestoreBalloonCorrectionOnlyOnMismatch: SettledRestore
// should issue a balloon correction only when the locally-decided
// allocatable differs from allocAtSnap.
func TestHooks_SettledRestoreBalloonCorrectionOnlyOnMismatch(t *testing.T) {
	cfg := &config.SandboxConfig{
		Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "1GiB"},
		},
	}
	cfg.ApplyDefaults()

	// Case 1: match → no correction.
	b1 := NewBalloonController("/dev/null", 8<<30, nil)
	b1.SetAllocatable(2 << 30)
	initial := b1.CurrentTarget()
	h1 := &ControllerHooks{opts: ControllerHookOptions{Balloon: b1, Logf: func(string, ...any) {}}, cfg: cfg}
	h1.SetAllocatableNow(2 << 30) // == allocAtSnap
	if err := h1.SettledRestore(2 << 30); err != nil {
		t.Fatalf("SettledRestore: %v", err)
	}
	if got := b1.CurrentTarget(); got != initial {
		t.Errorf("matching case: balloon target moved %d → %d (should be no-op)", initial, got)
	}

	// Case 2: mismatch → correction applied to balloon.
	b2 := NewBalloonController("/dev/null", 8<<30, nil)
	b2.SetAllocatable(2 << 30) // snapshot value
	h2 := &ControllerHooks{opts: ControllerHookOptions{Balloon: b2, Logf: func(string, ...any) {}}, cfg: cfg}
	h2.SetAllocatableNow(3 << 30) // controller granted different value
	if err := h2.SettledRestore(2 << 30); err != nil {
		t.Fatalf("SettledRestore: %v", err)
	}
	want := uint64(8<<30) - uint64(3<<30)
	if got := b2.CurrentTarget(); got != want {
		t.Errorf("mismatch case: balloon target = %d, want %d", got, want)
	}
}

// silence unused-import linter when build tags strip http use elsewhere.
var _ = fmt.Sprintf
