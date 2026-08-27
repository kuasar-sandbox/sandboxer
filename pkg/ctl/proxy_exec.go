package ctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/kuasar-sandbox/sandboxer/internal/wireio"
)

const proxyExecCopyBufferBytes = 32 * 1024

// Relay errors use a fixed direction priority after both pumps exit. The
// request direction is primary because it contains the gated ctl/MUX input.
const (
	proxyExecDownstreamToCtl = iota
	proxyExecCtlToDownstream
	proxyExecRelayDirections
)

type closeWriter interface {
	CloseWrite() error
}

// ProxyExec gates an authorized downstream exec tunnel before relaying it to
// ctlConn. It accepts only an exec_request as the first ctl frame and forwards
// that frame byte-for-byte; after the gate, the remaining ctl/MUX stream is
// relayed transparently in both directions.
//
// ProxyExec takes ownership of both streams. It closes every non-nil stream on
// every return path, including invalid arguments, gate rejection, I/O failure,
// and context cancellation. A clean EOF is propagated with CloseWrite when the
// destination supports it so the reverse direction can finish draining.
func ProxyExec(ctx context.Context, downstream io.ReadWriteCloser, ctlConn io.ReadWriteCloser) error {
	streams := &execTunnelStreams{}
	if downstream != nil {
		streams.SetDownstream(downstream)
	}
	if ctlConn != nil {
		streams.SetBackend(ctlConn)
	}
	defer streams.Close()

	if ctx == nil {
		return errors.New("ctl: proxy exec: nil context")
	}
	if downstream == nil {
		return errors.New("ctl: proxy exec: nil downstream")
	}
	if ctlConn == nil {
		return errors.New("ctl: proxy exec: nil ctl connection")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	return execTunnelRunWithContext(ctx, streams.Close, func() error {
		frame, err := readExecRequestFrameWithTimeout(
			ctx,
			downstream,
			DefaultExecFirstRequestTimeout,
			streams.Close,
		)
		if err != nil {
			return fmt.Errorf("ctl: proxy exec gate: %w", err)
		}
		if err := wireio.WriteAll(ctlConn, frame.Raw); err != nil {
			return fmt.Errorf("ctl: proxy exec gate write: %w", err)
		}
		if err := proxyExecRelay(downstream, ctlConn, streams.Close); err != nil {
			return fmt.Errorf("ctl: proxy exec relay: %w", err)
		}
		return nil
	})
}

func proxyExecRelay(downstream, ctlConn io.ReadWriteCloser, closeBoth func()) error {
	type relayResult struct {
		direction int
		err       error
	}
	results := make(chan relayResult, proxyExecRelayDirections)
	var pumps sync.WaitGroup
	pumps.Add(proxyExecRelayDirections)
	pump := func(direction int, dst io.Writer, src io.Reader) {
		defer pumps.Done()
		_, err := io.CopyBuffer(dst, src, make([]byte, proxyExecCopyBufferBytes))
		if err == nil {
			if dst, ok := dst.(closeWriter); ok {
				err = dst.CloseWrite()
			}
		}
		results <- relayResult{direction: direction, err: err}
	}

	go pump(proxyExecDownstreamToCtl, ctlConn, downstream)
	go pump(proxyExecCtlToDownstream, downstream, ctlConn)

	var directionErrors [proxyExecRelayDirections]error
	primaryDirection := -1
	for range proxyExecRelayDirections {
		result := <-results
		directionErrors[result.direction] = result.err
		if result.err != nil && primaryDirection < 0 {
			primaryDirection = result.direction
			closeBoth()
		}
	}
	pumps.Wait()
	// Select only after both pumps have stopped so independent simultaneous
	// errors do not acquire a nondeterministic priority from channel scheduling.
	// Once one error has triggered full close, however, a peer pump commonly
	// reports net.ErrClosed (or an equivalent closed-stream error). That teardown
	// artifact must not replace the original error merely because its direction
	// has a higher fixed priority.
	for direction, err := range directionErrors {
		if err != nil {
			if direction != primaryDirection && proxyExecTeardownError(err) {
				continue
			}
			return err
		}
	}
	if primaryDirection >= 0 {
		return directionErrors[primaryDirection]
	}
	return nil
}

func proxyExecTeardownError(err error) bool {
	return errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}
