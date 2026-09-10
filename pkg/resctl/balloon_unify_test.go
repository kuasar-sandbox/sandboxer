package resctl

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeCHMemory struct {
	sock   string
	server *http.Server

	mu             sync.Mutex
	capacity       uint64
	reportedCap    uint64
	acceptedTarget uint64
	currentBudget  uint64
	resizeCalls    []uint64
	resizeFailures int
	dropResizeACKs int
	infoFailures   int
	infoCalls      int
	onInfo         func(*fakeCHMemory)
	autoConverge   bool
	onResize       func(uint64)
}

func newFakeCHMemory(t *testing.T, capacity uint64) *fakeCHMemory {
	t.Helper()
	socketDir, err := os.MkdirTemp("", "sb-ch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	sock := filepath.Join(socketDir, "ch.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeCHMemory{
		sock: sock, capacity: capacity, reportedCap: capacity,
		currentBudget: capacity,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, _ *http.Request) {
		fake.mu.Lock()
		fake.infoCalls++
		if fake.onInfo != nil {
			fake.onInfo(fake)
		}
		if fake.infoFailures > 0 {
			fake.infoFailures--
			fake.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		capacity := fake.reportedCap
		target := fake.acceptedTarget
		current := fake.currentBudget
		fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"memory_actual_size": current,
			"config": map[string]any{
				"memory": map[string]any{
					"size":  uint64(0),
					"zones": []map[string]uint64{{"size": capacity}},
				},
				"balloon": map[string]uint64{"size": target},
			},
		})
	})
	mux.HandleFunc("/api/v1/vm.resize", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DesiredBalloon uint64 `json:"desired_balloon"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode resize: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fake.mu.Lock()
		fake.resizeCalls = append(fake.resizeCalls, body.DesiredBalloon)
		if fake.resizeFailures > 0 {
			fake.resizeFailures--
			fake.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fake.acceptedTarget = body.DesiredBalloon
		if fake.autoConverge {
			fake.currentBudget = fake.capacity - body.DesiredBalloon
		}
		dropACK := fake.dropResizeACKs > 0
		onResize := fake.onResize
		if dropACK {
			fake.dropResizeACKs--
		}
		fake.mu.Unlock()
		if onResize != nil {
			onResize(body.DesiredBalloon)
		}
		if dropACK {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	fake.server = &http.Server{Handler: mux}
	go func() { _ = fake.server.Serve(listener) }()
	t.Cleanup(func() { _ = fake.server.Shutdown(context.Background()) })
	return fake
}

func (f *fakeCHMemory) configure(fn func(*fakeCHMemory)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeCHMemory) calls() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint64(nil), f.resizeCalls...)
}

func TestBalloonObserveSeparatesAcceptedTargetAndCurrent(t *testing.T) {
	const capacity = uint64(8 << 30)
	fake := newFakeCHMemory(t, capacity)
	fake.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 3 << 30
		f.currentBudget = 6 << 30
	})
	b := NewBalloonController(fake.sock, capacity, time.Second, nil)
	state, err := b.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.AcceptedTarget != 3<<30 || state.CurrentBudget != 6<<30 || state.BalloonCurrent != 2<<30 {
		t.Fatalf("state = %+v", state)
	}
	if state.TargetReached(capacity) {
		t.Fatal("accepted target was incorrectly treated as guest current")
	}
	if observed, ok := state.ObservedBudget(capacity); !ok || observed != 6<<30 {
		t.Fatalf("ObservedBudget = %d, %v; want %d, true", observed, ok, uint64(6<<30))
	}
}

func TestBalloonObserveAssertsExactCHCapacity(t *testing.T) {
	const capacity = uint64(512<<20) + 4096
	fake := newFakeCHMemory(t, capacity)
	fake.configure(func(f *fakeCHMemory) { f.reportedCap = capacity - 4096 })
	b := NewBalloonController(fake.sock, capacity, time.Second, nil)
	if _, err := b.Observe(context.Background()); err == nil {
		t.Fatal("Observe accepted a CH memory-zone capacity mismatch")
	}
}

func TestBalloonApplyGrowDoesNotWaitForCurrent(t *testing.T) {
	const capacity = uint64(8 << 30)
	fake := newFakeCHMemory(t, capacity)
	fake.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 4 << 30
		f.currentBudget = 4 << 30
	})
	b := NewBalloonController(fake.sock, capacity, time.Second, nil)
	state, err := b.ApplyTarget(context.Background(), 3<<30)
	if err != nil {
		t.Fatal(err)
	}
	if state.AcceptedTarget != 3<<30 || state.CurrentBudget != 4<<30 {
		t.Fatalf("state = %+v", state)
	}
	if state.TargetReached(capacity) {
		t.Fatal("grow waited for or fabricated current convergence")
	}
}

func TestBalloonShrinkRechecksCurrentImmediatelyBeforeResize(t *testing.T) {
	const capacity = uint64(8 << 30)
	const nextTarget = uint64(4<<30) + 64<<20
	observed := BalloonState{
		AcceptedTarget: 4 << 30, AcceptedTargetKnown: true,
		CurrentBudget: 4 << 30, BalloonCurrent: 4 << 30, BalloonCurrentKnown: true,
	}
	fake := newFakeCHMemory(t, capacity)
	fake.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 4 << 30
		f.currentBudget = 5 << 30 // autonomous deflate: current balloon is 3 GiB
		f.autoConverge = true
	})
	b := NewBalloonController(fake.sock, capacity, time.Second, nil)
	if err := b.SetDesiredTarget(nextTarget); err != nil {
		t.Fatal(err)
	}
	release, err := b.acquireMutation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.applyShrinkDesiredHeld(context.Background(), observed)
	release()
	if err == nil {
		t.Fatal("actual reversal allowed a shrink resize")
	}
	if calls := fake.calls(); len(calls) != 0 {
		t.Fatalf("invalidated shrink issued resize calls: %v", calls)
	}
	if state := b.State(); state.DesiredTarget != nextTarget {
		t.Fatalf("deferred shrink lost its forward target: %+v", state)
	}

	// Repeating the same decision is valid once the current sample again
	// satisfies its bounds. No equality classifier participates in execution.
	fake.configure(func(f *fakeCHMemory) { f.currentBudget = 4 << 30 })
	release, err = b.acquireMutation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := b.applyShrinkDesiredHeld(context.Background(), observed)
	release()
	if err != nil {
		t.Fatal(err)
	}
	if state.AcceptedTarget != nextTarget || state.CurrentBudget != capacity-nextTarget {
		t.Fatalf("converged shrink state = %+v", state)
	}
	if calls := fake.calls(); !reflect.DeepEqual(calls, []uint64{nextTarget}) {
		t.Fatalf("converged shrink calls = %v", calls)
	}
}

func TestBalloonShrinkRequiresPreResizeObservation(t *testing.T) {
	const capacity = uint64(8 << 30)
	fake := newFakeCHMemory(t, capacity)
	fake.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 4 << 30
		f.currentBudget = 4 << 30
		f.infoFailures = 1
	})
	b := NewBalloonController(fake.sock, capacity, time.Second, nil)
	if err := b.SeedColdTarget(4 << 30); err != nil {
		t.Fatal(err)
	}
	if err := b.SetDesiredTarget(4<<30 + 64<<20); err != nil {
		t.Fatal(err)
	}
	release, err := b.acquireMutation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.applyShrinkDesiredHeld(context.Background(), BalloonState{
		AcceptedTarget: 4 << 30, AcceptedTargetKnown: true,
		CurrentBudget: 4 << 30, BalloonCurrent: 4 << 30, BalloonCurrentKnown: true,
	})
	release()
	if err == nil {
		t.Fatal("shrink proceeded without a fresh pre-resize observation")
	}
	if calls := fake.calls(); len(calls) != 0 {
		t.Fatalf("unobserved shrink issued resize calls: %v", calls)
	}
}

func TestBalloonApplyRetainsDesiredAndRetriesForward(t *testing.T) {
	const capacity = uint64(8 << 30)
	fake := newFakeCHMemory(t, capacity)
	fake.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 4 << 30
		f.currentBudget = 4 << 30
		f.resizeFailures = 1
		f.autoConverge = true
	})
	b := NewBalloonController(fake.sock, capacity, time.Second, nil)
	if _, err := b.ApplyTarget(context.Background(), 5<<30); err == nil {
		t.Fatal("ApplyTarget hid the explicit CH rejection")
	}
	if state := b.State(); state.DesiredTarget != 5<<30 || state.AcceptedTarget != 4<<30 {
		t.Fatalf("state after failure = %+v", state)
	}
	if calls := fake.calls(); !reflect.DeepEqual(calls, []uint64{5 << 30}) {
		t.Fatalf("resize calls after failure = %v; a rollback must not be issued", calls)
	}
	state, err := b.ApplyTarget(context.Background(), 5<<30)
	if err != nil {
		t.Fatal(err)
	}
	if state.AcceptedTarget != 5<<30 || state.CurrentBudget != 3<<30 {
		t.Fatalf("state after retry = %+v", state)
	}
	if calls := fake.calls(); !reflect.DeepEqual(calls, []uint64{5 << 30, 5 << 30}) {
		t.Fatalf("resize calls = %v, want forward retry only", calls)
	}
}

func TestBalloonApplyConfirmsLostACKWithoutCompensation(t *testing.T) {
	const capacity = uint64(8 << 30)
	fake := newFakeCHMemory(t, capacity)
	fake.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 4 << 30
		f.currentBudget = 4 << 30
		f.dropResizeACKs = 1
	})
	b := NewBalloonController(fake.sock, capacity, time.Second, nil)
	state, err := b.ApplyTarget(context.Background(), 3<<30)
	if err != nil {
		t.Fatal(err)
	}
	if state.DesiredTarget != 3<<30 || state.AcceptedTarget != 3<<30 {
		t.Fatalf("state = %+v", state)
	}
	if calls := fake.calls(); !reflect.DeepEqual(calls, []uint64{3 << 30}) {
		t.Fatalf("resize calls = %v, want no compensation", calls)
	}
}

func TestBalloonApplyAmbiguityKeepsTargetForLaterConfirmation(t *testing.T) {
	const capacity = uint64(8 << 30)
	fake := newFakeCHMemory(t, capacity)
	fake.configure(func(f *fakeCHMemory) {
		f.acceptedTarget = 4 << 30
		f.currentBudget = 4 << 30
		f.dropResizeACKs = 1
		f.infoFailures = 2
	})
	b := NewBalloonController(fake.sock, capacity, time.Second, nil)
	if _, err := b.ApplyTarget(context.Background(), 3<<30); err == nil {
		t.Fatal("ApplyTarget reported success without vm.info confirmation")
	}
	if state := b.State(); state.DesiredTarget != 3<<30 {
		t.Fatalf("desired target rolled back: %+v", state)
	}
	state, err := b.ApplyTarget(context.Background(), 3<<30)
	if err != nil {
		t.Fatal(err)
	}
	if state.AcceptedTarget != 3<<30 {
		t.Fatalf("confirmed state = %+v", state)
	}
	if calls := fake.calls(); !reflect.DeepEqual(calls, []uint64{3 << 30}) {
		t.Fatalf("confirmation issued a redundant/rollback resize: %v", calls)
	}
}

func TestBalloonSnapshotBarrierBlocksResize(t *testing.T) {
	const capacity = uint64(8 << 30)
	fake := newFakeCHMemory(t, capacity)
	b := NewBalloonController(fake.sock, capacity, time.Second, nil)
	release, err := b.BeginSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := b.ApplyTarget(context.Background(), 1<<30)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if calls := fake.calls(); len(calls) != 0 {
		t.Fatalf("resize crossed snapshot barrier: %v", calls)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls := fake.calls(); !reflect.DeepEqual(calls, []uint64{1 << 30}) {
		t.Fatalf("resize calls after release = %v", calls)
	}
}

func TestBalloonSeedRestoredStateKeepsBothSnapshotSides(t *testing.T) {
	const capacity = uint64(8 << 30)
	b := NewBalloonController("/dev/null", capacity, time.Second, nil)
	if err := b.SeedRestoredState(3<<30, 2<<30); err != nil {
		t.Fatal(err)
	}
	state := b.State()
	if state.DesiredTarget != 3<<30 || state.AcceptedTarget != 3<<30 ||
		state.BalloonCurrent != 2<<30 || state.CurrentBudget != 6<<30 {
		t.Fatalf("state = %+v", state)
	}
}
