package sandbox

import "sync"

// ReadinessEvent is one startup milestone emitted by sandbox-ctl run.
type ReadinessEvent string

const (
	// ReadinessControlReady means this run's host-local ctl.sock is listening.
	ReadinessControlReady ReadinessEvent = "control_ready"
	// ReadinessReady means the run-specific cold or restore readiness barrier
	// has completed.
	ReadinessReady ReadinessEvent = "ready"
)

// ReadinessNotify observes startup milestones for one sandbox run. A nil
// callback disables readiness notification.
type ReadinessNotify func(ReadinessEvent)

// readinessEmitter centralizes the per-run ordering and once semantics. The
// CLI-owned fd writer independently validates the same invariant at its trust
// boundary, but callers of ServeAndWait cannot make ready overtake ctl.Listen.
type readinessEmitter struct {
	mu           sync.Mutex
	notify       ReadinessNotify
	controlReady bool
	ready        bool
}

func newReadinessEmitter(notify ReadinessNotify) *readinessEmitter {
	return &readinessEmitter{notify: notify}
}

func (e *readinessEmitter) notifyControlReady() {
	if e == nil || e.notify == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.controlReady {
		return
	}
	e.notify(ReadinessControlReady)
	e.controlReady = true
}

func (e *readinessEmitter) notifyReady() {
	if e == nil {
		return
	}
	e.mu.Lock()
	notify := e.commitReadyLocked()
	e.mu.Unlock()
	if notify != nil {
		notify(ReadinessReady)
	}
}

// commitReadyLocked is the readiness transition. Delivery may block in the
// caller's notifier; it must not hold the lock needed to record a fatal read.
func (e *readinessEmitter) commitReadyLocked() ReadinessNotify {
	if e.notify == nil || !e.controlReady || e.ready {
		return nil
	}
	e.ready = true
	return e.notify
}
