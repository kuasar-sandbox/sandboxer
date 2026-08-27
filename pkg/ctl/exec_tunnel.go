package ctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/kuasar-sandbox/sandboxer/internal/wireio"
)

const (
	// DefaultExecFirstRequestTimeout is the default and maximum time an
	// accepted exec tunnel may wait for its complete first ctl request frame.
	DefaultExecFirstRequestTimeout = 10 * time.Second
)

// ErrExecRequestRejected is returned after a post-accept request-admission or
// backend failure. The underlying callback error is deliberately not exposed.
var ErrExecRequestRejected = errors.New("ctl: exec request rejected")

// ExecTunnelOptions supplies the policy- and transport-specific callbacks for
// ServeExecTunnel. The ctl package does not interpret credentials, KAT claims,
// CEL expressions, HTTP requests, routes, or sandbox lifecycle state.
type ExecTunnelOptions struct {
	// Authorize performs identity or token admission before the downstream is
	// accepted.
	Authorize func(context.Context) error
	// AcceptDownstream accepts the tunnel (for example by flushing CONNECT 200)
	// and returns the resulting duplex stream.
	AcceptDownstream func(context.Context) (io.ReadWriteCloser, error)
	// AuthorizeRequest evaluates caller policy after ctl has strictly parsed the
	// complete first frame and before any backend is dialed.
	AuthorizeRequest func(context.Context, *ExecRequestFrame) error
	// DialBackend establishes the backend only after request admission. The
	// returned stream receives frame.Raw exactly once before relay begins.
	DialBackend func(context.Context, *ExecRequestFrame) (io.ReadWriteCloser, error)
	// FirstRequestTimeout may shorten the fixed ten-second bound for a caller or
	// test. Zero selects DefaultExecFirstRequestTimeout.
	FirstRequestTimeout time.Duration
}

// ServeExecTunnel serves one remote exec tunnel in the fixed order:
//
//	Authorize
//	AcceptDownstream
//	ReadExecRequestFrame
//	AuthorizeRequest
//	DialBackend
//	write frame.Raw once
//	duplex relay
//
// Request admission therefore cannot dial or mutate a backend. Once accepted,
// a recognized admission or backend failure is reported only as the generic ctl
// error "exec request rejected"; an unrecoverable first-frame error closes the
// stream. Both streams are owned and closed by ServeExecTunnel. Clean EOF uses
// CloseWrite where supported so stdout, stderr, and exit status can finish after
// stdin closes.
func ServeExecTunnel(ctx context.Context, options ExecTunnelOptions) error {
	if ctx == nil {
		return errors.New("ctl: serve exec tunnel: nil context")
	}
	timeout, err := validateExecFirstRequestTimeout(options.FirstRequestTimeout)
	if err != nil {
		return err
	}
	if options.Authorize == nil {
		return errors.New("ctl: serve exec tunnel: nil Authorize callback")
	}
	if options.AcceptDownstream == nil {
		return errors.New("ctl: serve exec tunnel: nil AcceptDownstream callback")
	}
	if options.AuthorizeRequest == nil {
		return errors.New("ctl: serve exec tunnel: nil AuthorizeRequest callback")
	}
	if options.DialBackend == nil {
		return errors.New("ctl: serve exec tunnel: nil DialBackend callback")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := options.Authorize(ctx); err != nil {
		return err
	}

	streams := &execTunnelStreams{}
	defer streams.Close()
	downstream, acceptErr := options.AcceptDownstream(ctx)
	if downstream != nil {
		if !streams.SetDownstream(downstream) {
			return ctx.Err()
		}
	}
	if acceptErr != nil {
		return acceptErr
	}
	if downstream == nil {
		return errors.New("ctl: serve exec tunnel: AcceptDownstream returned nil stream")
	}

	return execTunnelRunWithContext(ctx, streams.Close, func() error {
		frame, err := readExecRequestFrameWithTimeout(ctx, downstream, timeout, streams.Close)
		if err != nil {
			return err
		}
		// Keep the admitted wire bytes private from callback mutation. Callbacks
		// receive the parsed frame for authorization/backend selection, while the
		// helper alone owns the exact bytes eventually written to the backend.
		raw := append([]byte(nil), frame.Raw...)
		if err := options.AuthorizeRequest(ctx, frame); err != nil {
			writeExecRequestRejected(downstream)
			return ErrExecRequestRejected
		}
		backend, dialErr := options.DialBackend(ctx, frame)
		if backend != nil {
			if !streams.SetBackend(backend) {
				return ctx.Err()
			}
		}
		if dialErr != nil || backend == nil {
			writeExecRequestRejected(downstream)
			return ErrExecRequestRejected
		}
		if err := wireio.WriteAll(backend, raw); err != nil {
			writeExecRequestRejected(downstream)
			return ErrExecRequestRejected
		}
		if err := proxyExecRelay(downstream, backend, streams.Close); err != nil {
			return fmt.Errorf("ctl: exec tunnel relay: %w", err)
		}
		return nil
	})
}

func validateExecFirstRequestTimeout(timeout time.Duration) (time.Duration, error) {
	if timeout == 0 {
		return DefaultExecFirstRequestTimeout, nil
	}
	if timeout < 0 || timeout > DefaultExecFirstRequestTimeout {
		return 0, fmt.Errorf(
			"ctl: serve exec tunnel: first request timeout must be within (0, %s]",
			DefaultExecFirstRequestTimeout,
		)
	}
	return timeout, nil
}

func readExecRequestFrameWithTimeout(
	ctx context.Context,
	downstream io.Reader,
	timeout time.Duration,
	closeStreams func(),
) (*ExecRequestFrame, error) {
	frameCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var frame *ExecRequestFrame
	err := execTunnelRunWithContext(frameCtx, closeStreams, func() error {
		var err error
		frame, err = ReadExecRequestFrame(downstream)
		return err
	})
	return frame, err
}

func writeExecRequestRejected(downstream io.Writer) {
	_ = WriteMessage(downstream, Response{Type: TypeError, Msg: "exec request rejected"})
}

type execTunnelStreams struct {
	mu         sync.Mutex
	downstream io.ReadWriteCloser
	backend    io.ReadWriteCloser
	closed     bool
}

func (s *execTunnelStreams) SetDownstream(stream io.ReadWriteCloser) bool {
	return s.set(stream, true)
}

func (s *execTunnelStreams) SetBackend(stream io.ReadWriteCloser) bool {
	return s.set(stream, false)
}

func (s *execTunnelStreams) set(stream io.ReadWriteCloser, downstream bool) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = stream.Close()
		return false
	}
	if downstream {
		s.downstream = stream
	} else {
		s.backend = stream
	}
	s.mu.Unlock()
	return true
}

func (s *execTunnelStreams) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	downstream, backend := s.downstream, s.backend
	s.downstream, s.backend = nil, nil
	s.mu.Unlock()
	if downstream != nil {
		_ = downstream.Close()
	}
	if backend != nil {
		_ = backend.Close()
	}
}

func execTunnelRunWithContext(ctx context.Context, closeStreams func(), run func() error) error {
	if err := ctx.Err(); err != nil {
		closeStreams()
		return err
	}
	stopWatch := make(chan struct{})
	watchResult := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			closeStreams()
			watchResult <- ctx.Err()
		case <-stopWatch:
			if err := ctx.Err(); err != nil {
				closeStreams()
				watchResult <- err
				return
			}
			watchResult <- nil
		}
	}()
	runErr := run()
	close(stopWatch)
	if watchErr := <-watchResult; watchErr != nil {
		return watchErr
	}
	return runErr
}
