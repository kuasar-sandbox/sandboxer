package resctl

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func TestUsageActualOnlyRefreshesOnSuccessfulRead(t *testing.T) {
	f := newFakeCHMemory(t, 1<<30)
	b := NewBalloonController(f.sock, 1<<30, time.Second, nil)
	if err := b.SeedRestoredState(1<<28, 1<<27); err != nil {
		t.Fatal(err)
	}
	if b.ActualObservation().Live {
		t.Fatal("restore seed claimed live")
	}
	f.mu.Lock()
	f.acceptedTarget, f.currentBudget = 1<<28, (1<<30)-(1<<27)
	f.mu.Unlock()
	start := time.Now()
	b.now = func() time.Time { start = start.Add(time.Millisecond); return start }
	a, err := b.TryObserveActual(context.Background())
	if err != nil || !a.Live || a.Current != 1<<27 || a.Sequence != 1 || a.Finished.Sub(a.Started) != time.Millisecond {
		t.Fatalf("%+v %v", a, err)
	}
	if err := b.SetDesiredTarget(0); err != nil {
		t.Fatal(err)
	}
	b.setAcceptedTarget(0)
	if b.ActualObservation() != a {
		t.Fatal("target write refreshed actual")
	}
	f.mu.Lock()
	f.infoFailures++
	f.mu.Unlock()
	failed, err := b.TryObserveActual(context.Background())
	if err == nil || failed != a {
		t.Fatal("failed read refreshed cached observation")
	}
	if err := b.SeedColdTarget(0); err != nil {
		t.Fatal(err)
	}
	if b.ActualObservation().Live {
		t.Fatal("cold seed retained live observation")
	}
}

func TestUsageActualBusyDoesNotWaitOrCreateWorkers(t *testing.T) {
	b := NewBalloonController("unused", 1<<30, time.Second, nil)
	for _, apiLock := range []bool{false, true} {
		if apiLock {
			b.apiMu.Lock()
		} else {
			<-b.mutationGate
		}
		before := runtime.NumGoroutine()
		start := time.Now()
		for i := 0; i < 10000; i++ {
			_, err := b.TryObserveActual(context.Background())
			if !errors.Is(err, ErrBalloonObservationBusy) {
				t.Fatal(err)
			}
		}
		if time.Since(start) > time.Second || runtime.NumGoroutine() > before+1 {
			t.Fatal("busy observation waited/allocated workers")
		}
		if apiLock {
			b.apiMu.Unlock()
		} else {
			b.mutationGate <- struct{}{}
		}
	}
}
