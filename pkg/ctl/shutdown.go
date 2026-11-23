package ctl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var (
	ErrShutdownUnsupported    = errors.New("normal shutdown is not supported by this owner")
	ErrShutdownTargetMismatch = errors.New("shutdown target does not match the current run")
	ErrShutdownUnavailable    = errors.New("normal shutdown is unavailable in the current state")
)

const (
	ShutdownUnsupportedCode    = "shutdown_unsupported"
	ShutdownTargetMismatchCode = "shutdown_target_mismatch"
	ShutdownUnavailableCode    = "shutdown_unavailable"
)

// ShutdownTarget pins the logical sandbox and its exact live generation. A
// directory may be reused, so its socket path alone never authorizes shutdown.
type ShutdownTarget struct {
	SandboxID string `json:"sandbox_id"`
	RunID     string `json:"run_id"`
}

func (t ShutdownTarget) Validate() error {
	for _, id := range []string{t.SandboxID, t.RunID} {
		if id == "" || len(id) > 128 || id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00\r\n") {
			return errors.New("shutdown requires a valid sandbox and exact run identity")
		}
	}
	return nil
}

// ShutdownRequest is host-local, never part of Guest Launch or a saved E/S.
// DeadlineMonotonicNS is an absolute CLOCK_MONOTONIC deadline on the same Linux
// boot and time namespace as the owner. Zero leaves the owner's normal bound.
type ShutdownRequest struct {
	Target              ShutdownTarget `json:"target"`
	DeadlineMonotonicNS int64          `json:"deadline_monotonic_ns,omitempty,string"`
}

func monotonicNow() (int64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return ts.Nano(), nil
}

// Deadline translates the same-host absolute deadline once to the owner's Go
// monotonic clock. Taking now before the syscall avoids extending the budget.
func (r ShutdownRequest) Deadline() (time.Time, error) {
	if err := r.Target.Validate(); err != nil {
		return time.Time{}, err
	}
	if r.DeadlineMonotonicNS == 0 {
		return time.Time{}, nil
	}
	if r.DeadlineMonotonicNS < 0 {
		return time.Time{}, errors.New("invalid shutdown monotonic deadline")
	}
	now := time.Now()
	clock, err := monotonicNow()
	if err != nil {
		return time.Time{}, err
	}
	remaining := r.DeadlineMonotonicNS - clock
	if remaining <= 0 {
		return time.Time{}, context.DeadlineExceeded
	}
	return now.Add(time.Duration(remaining)), nil
}

// ShutdownOperation separates validation from irreversible acceptance. Server
// detaches the response connection before Begin, then reports the fixed owner
// deadline before waiting for actual retirement. Both callbacks are independent
// of the client connection's lifetime.
type ShutdownOperation struct {
	Begin func() (deadlineMonotonicNS int64, err error)
	Wait  func() error
}

// MonotonicDeadline converts a Go deadline to the same-host wire clock. The
// caller stores the returned value once so retries report the identical bound.
func MonotonicDeadline(deadline time.Time) (int64, error) {
	clock, err := monotonicNow()
	if err != nil {
		return 0, err
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, context.DeadlineExceeded
	}
	value := clock + int64(remaining)
	if value <= clock {
		return 0, errors.New("shutdown deadline exceeds monotonic clock range")
	}
	return value, nil
}

// Shutdown requests normal Guest shutdown and waits for actual runtime
// retirement. Connection, request write and acceptance share one five-second
// bound. Acceptance supplies the owner's fixed final deadline; a canceled
// caller only stops waiting and never cancels an accepted stop.
func Shutdown(ctx context.Context, socket string, target ShutdownTarget) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := target.Validate(); err != nil {
		return err
	}
	request := ShutdownRequest{Target: target}
	if deadline, ok := ctx.Deadline(); ok {
		var err error
		request.DeadlineMonotonicNS, err = MonotonicDeadline(deadline)
		if err != nil {
			return err
		}
	}
	acceptCtx, cancelAccept := context.WithTimeout(ctx, shutdownAcceptTimeout)
	defer cancelAccept()
	conn, err := (&net.Dialer{}).DialContext(acceptCtx, "unix", socket)
	if err != nil {
		return contextTransportError(acceptCtx, err)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() { stopCancel(); _ = conn.Close() }()
	acceptDeadline, _ := acceptCtx.Deadline()
	if err := conn.SetDeadline(acceptDeadline); err != nil {
		return err
	}
	transport := &contextConn{Conn: conn, ctx: acceptCtx}
	if err := WriteMessage(transport, Request{Type: TypeShutdownRequest, Shutdown: &request}); err != nil {
		return err
	}
	var response Response
	if err := ReadMessage(transport, &response); err != nil {
		return err
	}
	if response.Type == TypeError {
		return shutdownResponseError(response)
	}
	if response.Type != TypeShutdownAccepted || response.Shutdown == nil || *response.Shutdown != target || response.ShutdownDeadlineMonotonicNS <= 0 {
		return errors.New("ctl: invalid shutdown acceptance or run identity")
	}
	ownerDeadline, err := (ShutdownRequest{Target: target, DeadlineMonotonicNS: response.ShutdownDeadlineMonotonicNS}).Deadline()
	if err != nil {
		return err
	}
	waitCtx, cancelWait := context.WithDeadline(ctx, ownerDeadline)
	defer cancelWait()
	deadline, _ := waitCtx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	cancelAccept()
	transport.ctx = waitCtx
	response = Response{}
	if err := ReadMessage(transport, &response); err != nil {
		return err
	}
	if response.Type == TypeError {
		return shutdownResponseError(response)
	}
	if response.Type != TypeShutdownDone || response.Shutdown == nil || *response.Shutdown != target {
		return errors.New("ctl: invalid shutdown completion or run identity")
	}
	return nil
}

const shutdownAcceptTimeout = 5 * time.Second

func shutdownResponseError(response Response) error {
	switch response.Code {
	case ShutdownUnsupportedCode:
		return fmt.Errorf("%w: %s", ErrShutdownUnsupported, response.Msg)
	case ShutdownTargetMismatchCode:
		return fmt.Errorf("%w: %s", ErrShutdownTargetMismatch, response.Msg)
	case ShutdownUnavailableCode:
		return fmt.Errorf("%w: %s", ErrShutdownUnavailable, response.Msg)
	case "shutdown_deadline":
		return fmt.Errorf("%w: %s", context.DeadlineExceeded, response.Msg)
	}
	if response.Msg == "unknown type: "+TypeShutdownRequest {
		return ErrShutdownUnsupported
	}
	return errors.New(response.Msg)
}

func (s *Server) serveShutdown(conn *net.UnixConn, req Request) {
	defer conn.Close()
	writeError := func(err error) {
		_ = conn.SetWriteDeadline(time.Now().Add(shutdownAcceptTimeout))
		_ = WriteMessage(conn, Response{Type: TypeError, Code: shutdownErrorCode(err), Msg: err.Error()})
	}
	if s.ShutdownHandler == nil {
		writeError(ErrShutdownUnsupported)
		return
	}
	if req.Shutdown == nil {
		writeError(errors.New("missing shutdown request"))
		return
	}
	op, err := s.ShutdownHandler(*req.Shutdown)
	if err != nil {
		writeError(err)
		return
	}
	if op.Begin == nil || op.Wait == nil {
		writeError(errors.New("shutdown handler has no retirement continuation"))
		return
	}
	// No owner work begins while Runtime cleanup can still close this conn.
	s.detachResponse(conn)
	deadline, err := op.Begin()
	if err != nil {
		writeError(err)
		return
	}
	accepted := Response{Type: TypeShutdownAccepted, Shutdown: &req.Shutdown.Target, ShutdownDeadlineMonotonicNS: deadline}
	_ = conn.SetWriteDeadline(time.Now().Add(shutdownAcceptTimeout))
	if err := WriteMessage(conn, accepted); err != nil {
		s.Logf("ctl.sock shutdown acceptance: %v; owner continues", err)
		return
	}
	response := Response{Type: TypeShutdownDone, Shutdown: &req.Shutdown.Target}
	if err := op.Wait(); err != nil {
		response.Type, response.Code, response.Msg = TypeError, shutdownErrorCode(err), err.Error()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(captureResponseWriteTimeout))
	if err := WriteMessage(conn, response); err != nil {
		s.Logf("ctl.sock shutdown completion: %v", err)
	}
}

func shutdownErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrShutdownUnsupported):
		return ShutdownUnsupportedCode
	case errors.Is(err, ErrShutdownTargetMismatch):
		return ShutdownTargetMismatchCode
	case errors.Is(err, ErrShutdownUnavailable):
		return ShutdownUnavailableCode
	case errors.Is(err, context.DeadlineExceeded):
		return "shutdown_deadline"
	default:
		return ""
	}
}
