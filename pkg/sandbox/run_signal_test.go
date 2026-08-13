package sandbox

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRunSignalContextRetainsSignalBeforeServeAndWait(t *testing.T) {
	source := make(chan os.Signal, 2)
	var stopped atomic.Bool
	ctx, stop := newRunSignalContext(context.Background(), source, func() { stopped.Store(true) })
	t.Cleanup(stop)

	// Model SIGTERM after Admit but before ServeAndWait obtains its shutdown
	// channel. Cancellation and later CH delivery must both survive that gap.
	source <- syscall.SIGTERM
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("run context was not cancelled")
	}
	signals := runSignalsFromContext(ctx)
	select {
	case sig := <-signals:
		if sig != syscall.SIGTERM {
			t.Fatalf("retained signal = %v", sig)
		}
	case <-time.After(time.Second):
		t.Fatal("pre-ServeAndWait signal was lost")
	}

	// Later signals remain distinct so waitForCHWithSignalEscalation preserves
	// its existing immediate escalation on the user's second signal.
	source <- syscall.SIGINT
	select {
	case sig := <-signals:
		if sig != syscall.SIGINT {
			t.Fatalf("second signal = %v", sig)
		}
	case <-time.After(time.Second):
		t.Fatal("second signal was lost")
	}
	stop()
	if !stopped.Load() {
		t.Fatal("signal source was not stopped")
	}
}
