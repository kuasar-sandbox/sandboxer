package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/kuasar-sandbox/sandboxer/internal/wireio"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"golang.org/x/sys/unix"
)

// readinessFDWriter owns the inherited readiness descriptor. It serializes
// all writes and closes the descriptor on ready, error, or runCmd return.
type readinessFDWriter struct {
	mu sync.Mutex

	w    io.WriteCloser
	logf func(string, ...any)

	controlReady bool
	ready        bool
	closed       bool
	loggedError  bool
}

func newReadinessFDWriter(fd int, logf func(string, ...any)) (*readinessFDWriter, error) {
	if fd < 3 {
		return nil, fmt.Errorf("--ready-fd must be at least 3, got %d", fd)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return nil, fmt.Errorf("--ready-fd=%d is invalid: %w", fd, err)
	}
	// Keep the descriptor out of Cloud Hypervisor and every other later exec.
	// node-ctl clears CLOEXEC only for its own exec into sandbox-ctl; ownership
	// begins here and the bit is restored immediately.
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return nil, fmt.Errorf("set FD_CLOEXEC on --ready-fd=%d: %w", fd, err)
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("ready-fd-%d", fd))
	if f == nil {
		return nil, fmt.Errorf("wrap --ready-fd=%d", fd)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &readinessFDWriter{w: f, logf: logf}, nil
}

func (w *readinessFDWriter) Notify(event sandbox.ReadinessEvent) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
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

func (w *readinessFDWriter) writeLineLocked(event sandbox.ReadinessEvent) error {
	return wireio.WriteAll(w.w, []byte(string(event)+"\n"))
}

func (w *readinessFDWriter) failLocked(op string, err error) {
	if errors.Is(err, syscall.EPIPE) {
		w.logOnceLocked("readiness fd reader closed early: %v", err)
	} else {
		w.logOnceLocked("readiness fd %s: %v", op, err)
	}
	_ = w.closeLocked()
}

func (w *readinessFDWriter) logOnceLocked(format string, args ...any) {
	if w.loggedError {
		return
	}
	w.loggedError = true
	w.logf(format, args...)
}

func (w *readinessFDWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closeLocked()
}

func (w *readinessFDWriter) closeLocked() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.w.Close()
}
