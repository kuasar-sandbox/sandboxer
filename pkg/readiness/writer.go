// Package readiness adapts sandbox startup events to the readiness stream used
// by sandbox-ctl: control_ready, ready, then EOF.
package readiness

import (
	"errors"
	"io"
	"sync"
	"syscall"

	"github.com/kuasar-sandbox/sandboxer/internal/wireio"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
)

// Writer serializes readiness notifications and owns its output until ready,
// delivery failure, or Close. Notify and Close may be called concurrently. A
// nil *Writer or zero Writer disables notification.
type Writer struct {
	mu sync.Mutex

	w    io.WriteCloser
	logf func(string, ...any)

	controlReady bool
	ready        bool
	closed       bool
	loggedError  bool
}

// NewWriter transfers ownership of w to the returned Writer. A nil w disables
// notification. Delivery and event-order errors close w and are reported at
// most once through logf; they do not affect the sandbox's lifetime. A nil logf
// disables logging. The callback runs synchronously and must not call methods
// on the same Writer.
func NewWriter(w io.WriteCloser, logf func(string, ...any)) *Writer {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Writer{w: w, logf: logf}
}

// Notify writes each protocol event at most once.
// [sandbox.ReadinessRuntimeReady] is ignored. [sandbox.ReadinessReady] requires
// [sandbox.ReadinessControlReady] and closes the stream after its complete line.
// An unknown event or a delivery error disables further notifications.
func (w *Writer) Notify(event sandbox.ReadinessEvent) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.w == nil {
		return
	}

	switch event {
	case sandbox.ReadinessControlReady:
		if w.controlReady {
			return
		}
		if err := w.writeLineLocked(event); err != nil {
			w.failLocked("write control_ready", err)
			return
		}
		w.controlReady = true

	case sandbox.ReadinessRuntimeReady:
		// The existing readiness stream remains control_ready/ready.
		return

	case sandbox.ReadinessReady:
		if w.ready {
			return
		}
		if !w.controlReady {
			w.logOnceLocked("readiness invariant violation: ready before control_ready")
			_ = w.closeLocked()
			return
		}
		if err := w.writeLineLocked(event); err != nil {
			w.failLocked("write ready", err)
			return
		}
		w.ready = true
		_ = w.closeLocked()

	default:
		w.logOnceLocked("readiness invariant violation: unknown event %q", event)
		_ = w.closeLocked()
	}
}

func (w *Writer) writeLineLocked(event sandbox.ReadinessEvent) error {
	return wireio.WriteAll(w.w, []byte(string(event)+"\n"))
}

func (w *Writer) failLocked(op string, err error) {
	if errors.Is(err, syscall.EPIPE) {
		w.logOnceLocked("readiness fd reader closed early: %v", err)
	} else {
		w.logOnceLocked("readiness fd %s: %v", op, err)
	}
	_ = w.closeLocked()
}

func (w *Writer) logOnceLocked(format string, args ...any) {
	if w.loggedError {
		return
	}
	w.loggedError = true
	w.logf(format, args...)
}

// Close closes the output if it is still open. Repeated calls are harmless.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closeLocked()
}

func (w *Writer) closeLocked() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.w == nil {
		return nil
	}
	return w.w.Close()
}
