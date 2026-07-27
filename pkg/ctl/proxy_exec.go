package ctl

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			if downstream != nil {
				_ = downstream.Close()
			}
			if ctlConn != nil {
				_ = ctlConn.Close()
			}
		})
	}
	defer closeBoth()

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

	// Closing both streams is the only portable way to interrupt a blocked
	// Read or Write through the deliberately small io.ReadWriteCloser API.
	stopWatch := make(chan struct{})
	watchResult := make(chan error, 1)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			err := ctx.Err()
			closeBoth()
			watchResult <- err
		case <-stopWatch:
			// If completion and cancellation become ready together, prefer
			// cancellation. This removes select's otherwise random choice
			// while preserving completion that wins before ctx is canceled.
			if err := ctx.Err(); err != nil {
				closeBoth()
				watchResult <- err
				return
			}
			watchResult <- nil
		}
	}()

	var result error
	if err := proxyExecFirstFrame(downstream, ctlConn); err != nil {
		result = fmt.Errorf("ctl: proxy exec gate: %w", err)
	} else if err := proxyExecRelay(downstream, ctlConn, closeBoth); err != nil {
		result = fmt.Errorf("ctl: proxy exec relay: %w", err)
	}

	close(stopWatch)
	watchErr := <-watchResult
	<-watchDone
	if watchErr != nil {
		return watchErr
	}
	return result
}

func proxyExecFirstFrame(downstream io.Reader, ctlConn io.Writer) error {
	var prefix [4]byte
	if _, err := io.ReadFull(downstream, prefix[:]); err != nil {
		return fmt.Errorf("read length: %w", err)
	}

	n := binary.LittleEndian.Uint32(prefix[:])
	if n > MaxMessageBytes {
		return fmt.Errorf("oversized message: %d", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(downstream, payload); err != nil {
		return fmt.Errorf("read payload: %w", err)
	}

	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return errors.New("invalid request envelope")
	}
	if envelope.Type != TypeExecRequest {
		return errors.New("request is not exec_request")
	}

	if err := wireio.WriteAll(ctlConn, prefix[:]); err != nil {
		return fmt.Errorf("write length: %w", err)
	}
	if err := wireio.WriteAll(ctlConn, payload); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
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
	closing := false
	for range proxyExecRelayDirections {
		result := <-results
		directionErrors[result.direction] = result.err
		if result.err != nil && !closing {
			closing = true
			closeBoth()
		}
	}
	pumps.Wait()
	// Select only after both pumps have stopped so simultaneous errors do not
	// acquire a nondeterministic priority from channel scheduling.
	for _, err := range directionErrors {
		if err != nil {
			return err
		}
	}
	return nil
}
