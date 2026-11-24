package sandbox

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
)

type runtimeShutdown struct {
	guestDeadline       time.Time
	finalDeadline       time.Time
	deadlineMonotonicNS int64
	escalated           atomic.Bool
}

func resolveRuntimeRunID(id string, current *artifact.CurrentRun) (string, error) {
	if current != nil {
		if id != "" && id != current.RunID {
			return "", errors.New("run identity conflicts with retained current disks")
		}
		id = current.RunID
	}
	if id == "" {
		id = rand.Text()
	}
	if err := (ctl.ShutdownTarget{SandboxID: "runtime", RunID: id}).Validate(); err != nil {
		return "", err
	}
	return id, nil
}

// RunID identifies this execution generation, independently of disk retention.
// A new cold start or restore gets a fresh value unless its owner supplies one.
func (r *Runtime) RunID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spec.RunID
}

// Shutdown requests normal Guest shutdown and waits for actual retirement.
// The first accepted request fixes the owner deadline. A canceled caller only
// stops waiting; the accepted operation continues. Close retains its separate
// resource-reclamation meaning, including destruction of frozen capture sources.
func (r *Runtime) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	if err := r.beginShutdown(deadline); err != nil {
		return err
	}
	return r.waitShutdown(ctx)
}

func (r *Runtime) waitShutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	stop := r.shutdown
	r.mu.Unlock()
	if stop != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, stop.finalDeadline)
		defer cancel()
	}
	// A missing reaper/cleanup completion is a bounded error, never success.
	// The owner still retains its resources and completes physical retirement.
	exit, err := r.Wait(ctx)
	if err != nil {
		return err
	}
	resultErr := exit.Err
	if exit.Code != 0 {
		resultErr = errors.Join(resultErr, fmt.Errorf("runtime exited with code %d during shutdown", exit.Code))
	}
	if stop != nil && stop.escalated.Load() {
		resultErr = errors.Join(resultErr, fmt.Errorf("Guest shutdown required forced retirement: %w", context.DeadlineExceeded))
	}
	return resultErr
}

// beginShutdown never holds an opsWG slot or a connection cleanup obligation.
// The localctl adapter calls it only after detaching the response connection.
func (r *Runtime) beginShutdown(deadline time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shutdown != nil || r.state == runtimeClosed {
		return nil
	}
	now := time.Now()
	if !deadline.IsZero() && !deadline.After(now) {
		return context.DeadlineExceeded
	}
	if r.state != runtimeRunning {
		return fmt.Errorf("%w: state %s", ctl.ErrShutdownUnavailable, r.state)
	}
	// Direct environments retain their existing StopUnit/fence shutdown
	// behavior. The new normal Guest shutdown applies only to systemd runs.
	if !r.environmentActive {
		return ctl.ErrShutdownUnsupported
	}
	if r.shutdownGuest == nil {
		return ctl.ErrShutdownUnsupported
	}
	// A capture may have passed Runtime admission before this lock. Claim its
	// existing gate atomically: never send shutdown to a freezing/frozen source,
	// and never let a later capture freeze a normally stopping environment.
	if h := r.diskSnapshot; h != nil {
		h.capture.mu.Lock()
		defer h.capture.mu.Unlock()
		if h.capture.active || h.capture.terminal {
			return fmt.Errorf("%w: capture is active or committed", ctl.ErrShutdownUnavailable)
		}
	}
	grace := r.shutdownGrace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	stop := &runtimeShutdown{
		guestDeadline: now.Add(grace).Add(chShutdownGrace),
		finalDeadline: now.Add(grace).Add(2 * chShutdownGrace),
	}
	if !deadline.IsZero() && deadline.Before(stop.finalDeadline) {
		stop.finalDeadline = deadline
	}
	if stop.finalDeadline.Before(stop.guestDeadline) {
		stop.guestDeadline = stop.finalDeadline
	}
	var err error
	stop.deadlineMonotonicNS, err = ctl.MonotonicDeadline(stop.finalDeadline)
	if err != nil {
		return err
	}
	if h := r.diskSnapshot; h != nil {
		h.capture.terminal = true
	}
	r.shutdown = stop
	r.state = runtimeClosing
	go r.driveShutdown(stop, r.shutdownGuest, r.reaped)
	return nil
}

func (r *Runtime) driveShutdown(stop *runtimeShutdown, send func(context.Context) error, reaped <-chan struct{}) {
	ctx, cancel := context.WithDeadline(context.Background(), stop.guestDeadline)
	defer cancel()
	// The owner timer is independent of a stalled CONNECT/ACK or pinger drain.
	replied := make(chan error, 1)
	go func() { replied <- send(ctx) }()
	for {
		select {
		case <-r.exitDone:
			return
		case <-reaped:
			return // real VMM Wait ends escalation; cleanup has its own final bound
		case err := <-replied:
			if err != nil && r.logf != nil {
				r.logf("Guest shutdown acknowledgement: %v; retaining original deadline", err)
			}
			replied = nil // ACK is acceptance only, including a lost response.
		case <-ctx.Done():
			select {
			case <-r.exitDone:
				return
			case <-reaped:
				return
			default:
			}
			stop.escalated.Store(true)
			r.requestClose()
			return
		}
	}
}

func (r *Runtime) shutdownFinalDeadline() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shutdown != nil {
		return r.shutdown.finalDeadline
	}
	return time.Time{}
}

func (r *Runtime) prepareShutdown(req ctl.ShutdownRequest) (ctl.ShutdownOperation, error) {
	deadline, err := req.Deadline()
	if err != nil {
		return ctl.ShutdownOperation{}, err
	}
	r.mu.Lock()
	current := ctl.ShutdownTarget{SandboxID: r.sandboxID, RunID: r.spec.RunID}
	r.mu.Unlock()
	if req.Target != current {
		return ctl.ShutdownOperation{}, ctl.ErrShutdownTargetMismatch
	}
	// No stop worker may start here: ctl.Stop could otherwise close this
	// response connection before Server has detached it from connsWG.
	return ctl.ShutdownOperation{
		Begin: func() (int64, error) {
			if err := r.beginShutdown(deadline); err != nil {
				return 0, err
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.shutdown == nil {
				return 0, ctl.ErrShutdownUnavailable
			}
			return r.shutdown.deadlineMonotonicNS, nil
		},
		Wait: func() error { return r.waitShutdown(context.Background()) },
	}, nil
}
