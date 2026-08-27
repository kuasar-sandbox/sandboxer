package ctl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func TestServeExecTunnelCallbackOrderAndRawWriteOnce(t *testing.T) {
	frame := ctlTestValidExecFrame()
	downstream := &memoryRWC{reader: bytes.NewReader(append(append([]byte(nil), frame...), []byte("mux-in")...))}
	backend := &memoryRWC{reader: bytes.NewReader([]byte("mux-out"))}
	var order []string
	var dialCalls int

	err := ServeExecTunnel(context.Background(), ExecTunnelOptions{
		Authorize: func(context.Context) error {
			order = append(order, "authorize")
			return nil
		},
		AcceptDownstream: func(context.Context) (io.ReadWriteCloser, error) {
			order = append(order, "accept")
			return downstream, nil
		},
		AuthorizeRequest: func(_ context.Context, got *ExecRequestFrame) error {
			order = append(order, "authorize-request")
			if !bytes.Equal(got.Raw, frame) || got.Request.Exec == nil {
				t.Fatalf("AuthorizeRequest frame = %#v", got)
			}
			return nil
		},
		DialBackend: func(_ context.Context, got *ExecRequestFrame) (io.ReadWriteCloser, error) {
			order = append(order, "dial")
			dialCalls++
			if !bytes.Equal(got.Raw, frame) {
				t.Fatal("DialBackend received changed Raw")
			}
			return backend, nil
		},
	})
	if err != nil {
		t.Fatalf("ServeExecTunnel: %v", err)
	}
	if got, want := order, []string{"authorize", "accept", "authorize-request", "dial"}; !equalStrings(got, want) {
		t.Fatalf("callback order = %v, want %v", got, want)
	}
	if dialCalls != 1 {
		t.Fatalf("DialBackend calls = %d, want 1", dialCalls)
	}
	if got, want := backend.written.Bytes(), append(append([]byte(nil), frame...), []byte("mux-in")...); !bytes.Equal(got, want) {
		t.Fatalf("backend bytes = %q, want raw once + tail %q", got, want)
	}
	if got, want := downstream.written.Bytes(), []byte("mux-out"); !bytes.Equal(got, want) {
		t.Fatalf("downstream bytes = %q, want %q", got, want)
	}
	assertClosedOnce(t, downstream)
	assertClosedOnce(t, backend)
}

func TestServeExecTunnelAuthorizeFailureDoesNotAccept(t *testing.T) {
	wantErr := errors.New("unauthorized")
	var accepts, requestGates, dials atomic.Int32
	err := ServeExecTunnel(context.Background(), ExecTunnelOptions{
		Authorize: func(context.Context) error { return wantErr },
		AcceptDownstream: func(context.Context) (io.ReadWriteCloser, error) {
			accepts.Add(1)
			return &memoryRWC{}, nil
		},
		AuthorizeRequest: func(context.Context, *ExecRequestFrame) error {
			requestGates.Add(1)
			return nil
		},
		DialBackend: func(context.Context, *ExecRequestFrame) (io.ReadWriteCloser, error) {
			dials.Add(1)
			return &memoryRWC{}, nil
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("ServeExecTunnel error = %v, want %v", err, wantErr)
	}
	if accepts.Load() != 0 || requestGates.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("post-authorization callback ran: accept=%d request=%d dial=%d", accepts.Load(), requestGates.Load(), dials.Load())
	}
}

func TestServeExecTunnelCallbackCannotChangeForwardedRaw(t *testing.T) {
	frame := ctlTestValidExecFrame()
	downstream := &memoryRWC{reader: bytes.NewReader(frame)}
	backend := &memoryRWC{reader: bytes.NewReader(nil)}
	err := ServeExecTunnel(context.Background(), ExecTunnelOptions{
		Authorize: func(context.Context) error { return nil },
		AcceptDownstream: func(context.Context) (io.ReadWriteCloser, error) {
			return downstream, nil
		},
		AuthorizeRequest: func(_ context.Context, got *ExecRequestFrame) error {
			for i := range got.Raw {
				got.Raw[i] = 0xff
			}
			return nil
		},
		DialBackend: func(context.Context, *ExecRequestFrame) (io.ReadWriteCloser, error) {
			return backend, nil
		},
	})
	if err != nil {
		t.Fatalf("ServeExecTunnel: %v", err)
	}
	if !bytes.Equal(backend.written.Bytes(), frame) {
		t.Fatalf("callback changed forwarded raw:\n got %x\nwant %x", backend.written.Bytes(), frame)
	}
}

func TestServeExecTunnelRequestRejectionIsGenericAndDoesNotDial(t *testing.T) {
	downstream := &memoryRWC{reader: bytes.NewReader(ctlTestValidExecFrame())}
	var dials atomic.Int32
	secretErr := errors.New("condition 0 exposed secret expression")
	err := ServeExecTunnel(context.Background(), ExecTunnelOptions{
		Authorize: func(context.Context) error { return nil },
		AcceptDownstream: func(context.Context) (io.ReadWriteCloser, error) {
			return downstream, nil
		},
		AuthorizeRequest: func(context.Context, *ExecRequestFrame) error { return secretErr },
		DialBackend: func(context.Context, *ExecRequestFrame) (io.ReadWriteCloser, error) {
			dials.Add(1)
			return &memoryRWC{}, nil
		},
	})
	if !errors.Is(err, ErrExecRequestRejected) || errors.Is(err, secretErr) ||
		bytes.Contains([]byte(err.Error()), []byte("secret")) {
		t.Fatalf("ServeExecTunnel exposed callback failure: %v", err)
	}
	if dials.Load() != 0 {
		t.Fatalf("DialBackend calls = %d, want 0", dials.Load())
	}
	assertGenericExecRejection(t, downstream.written.Bytes())
	assertClosedOnce(t, downstream)
}

func TestServeExecTunnelInvalidFrameClosesWithoutResponseOrDial(t *testing.T) {
	downstream := &memoryRWC{reader: bytes.NewReader(ctlTestFrame([]byte(
		`{"type":"exec_request","exec":{"argv":[]}}`,
	)))}
	var requestGates, dials atomic.Int32
	err := ServeExecTunnel(context.Background(), ExecTunnelOptions{
		Authorize: func(context.Context) error { return nil },
		AcceptDownstream: func(context.Context) (io.ReadWriteCloser, error) {
			return downstream, nil
		},
		AuthorizeRequest: func(context.Context, *ExecRequestFrame) error {
			requestGates.Add(1)
			return nil
		},
		DialBackend: func(context.Context, *ExecRequestFrame) (io.ReadWriteCloser, error) {
			dials.Add(1)
			return &memoryRWC{}, nil
		},
	})
	if err == nil {
		t.Fatal("ServeExecTunnel accepted invalid frame")
	}
	if requestGates.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("invalid frame reached callbacks: request=%d dial=%d", requestGates.Load(), dials.Load())
	}
	if downstream.written.Len() != 0 {
		t.Fatalf("invalid framing synthesized %d bytes", downstream.written.Len())
	}
	assertClosedOnce(t, downstream)
}

func TestServeExecTunnelBackendFailuresAreGenericAndNotRetried(t *testing.T) {
	tests := []struct {
		name string
		dial func(*atomic.Int32) (io.ReadWriteCloser, error)
	}{
		{
			name: "dial",
			dial: func(calls *atomic.Int32) (io.ReadWriteCloser, error) {
				calls.Add(1)
				return nil, errors.New("backend path and secret")
			},
		},
		{
			name: "first write",
			dial: func(calls *atomic.Int32) (io.ReadWriteCloser, error) {
				calls.Add(1)
				return &failingWriteRWC{err: errors.New("backend write path and secret")}, nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			downstream := &memoryRWC{reader: bytes.NewReader(ctlTestValidExecFrame())}
			var calls atomic.Int32
			err := ServeExecTunnel(context.Background(), ExecTunnelOptions{
				Authorize: func(context.Context) error { return nil },
				AcceptDownstream: func(context.Context) (io.ReadWriteCloser, error) {
					return downstream, nil
				},
				AuthorizeRequest: func(context.Context, *ExecRequestFrame) error { return nil },
				DialBackend: func(context.Context, *ExecRequestFrame) (io.ReadWriteCloser, error) {
					return test.dial(&calls)
				},
			})
			if !errors.Is(err, ErrExecRequestRejected) || bytes.Contains([]byte(err.Error()), []byte("secret")) {
				t.Fatalf("ServeExecTunnel error = %v, want sanitized rejection", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("DialBackend calls = %d, want 1", calls.Load())
			}
			assertGenericExecRejection(t, downstream.written.Bytes())
			assertClosedOnce(t, downstream)
		})
	}
}

func TestServeExecTunnelFirstRequestTimeout(t *testing.T) {
	downstream := newBlockingReadRWC()
	var requestGates, dials atomic.Int32
	start := time.Now()
	err := ServeExecTunnel(context.Background(), ExecTunnelOptions{
		Authorize: func(context.Context) error { return nil },
		AcceptDownstream: func(context.Context) (io.ReadWriteCloser, error) {
			return downstream, nil
		},
		AuthorizeRequest: func(context.Context, *ExecRequestFrame) error {
			requestGates.Add(1)
			return nil
		},
		DialBackend: func(context.Context, *ExecRequestFrame) (io.ReadWriteCloser, error) {
			dials.Add(1)
			return &memoryRWC{}, nil
		},
		FirstRequestTimeout: 20 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ServeExecTunnel error = %v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("first request timeout took %s", elapsed)
	}
	if requestGates.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("timeout reached callbacks: request=%d dial=%d", requestGates.Load(), dials.Load())
	}
	assertClosedOnce(t, downstream)
}

func TestServeExecTunnelCancellationClosesBothStreams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	downstream := newPrefixedBlockingReadRWC(ctlTestValidExecFrame())
	backend := newBlockingReadRWC()
	dialed := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ServeExecTunnel(ctx, ExecTunnelOptions{
			Authorize: func(context.Context) error { return nil },
			AcceptDownstream: func(context.Context) (io.ReadWriteCloser, error) {
				return downstream, nil
			},
			AuthorizeRequest: func(context.Context, *ExecRequestFrame) error { return nil },
			DialBackend: func(context.Context, *ExecRequestFrame) (io.ReadWriteCloser, error) {
				close(dialed)
				return backend, nil
			},
		})
	}()
	select {
	case <-dialed:
	case <-time.After(time.Second):
		t.Fatal("DialBackend was not reached")
	}
	cancel()
	if err := ctlTestWaitError(done); !errors.Is(err, context.Canceled) {
		t.Fatalf("ServeExecTunnel error = %v, want context canceled", err)
	}
	assertClosedOnce(t, downstream)
	assertClosedOnce(t, backend)
}

func TestServeExecTunnelPreservesHalfClose(t *testing.T) {
	client, downstream := ctlTestUnixPair(t, "tunnel-downstream")
	backendPeer, backend := ctlTestUnixPair(t, "tunnel-backend")
	defer client.Close()
	defer backendPeer.Close()
	setDeadline(t, client)
	setDeadline(t, backendPeer)

	done := make(chan error, 1)
	go func() {
		done <- ServeExecTunnel(context.Background(), ExecTunnelOptions{
			Authorize: func(context.Context) error { return nil },
			AcceptDownstream: func(context.Context) (io.ReadWriteCloser, error) {
				return downstream, nil
			},
			AuthorizeRequest: func(context.Context, *ExecRequestFrame) error { return nil },
			DialBackend: func(context.Context, *ExecRequestFrame) (io.ReadWriteCloser, error) {
				return backend, nil
			},
		})
	}()

	frame := ctlTestValidExecFrame()
	stdin := []byte("stdin-after-frame")
	if _, err := client.Write(append(append([]byte(nil), frame...), stdin...)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("client CloseWrite: %v", err)
	}
	gotFrame, err := ReadExecRequestFrame(backendPeer)
	if err != nil {
		t.Fatalf("backend read frame: %v", err)
	}
	if !bytes.Equal(gotFrame.Raw, frame) {
		t.Fatal("backend received changed frame")
	}
	gotStdin, err := io.ReadAll(backendPeer)
	if err != nil {
		t.Fatalf("backend read stdin: %v", err)
	}
	if !bytes.Equal(gotStdin, stdin) {
		t.Fatalf("backend stdin = %q, want %q", gotStdin, stdin)
	}
	wantOutput := []byte("stdout-after-stdin-eof")
	if _, err := backendPeer.Write(wantOutput); err != nil {
		t.Fatalf("backend write output: %v", err)
	}
	if err := backendPeer.CloseWrite(); err != nil {
		t.Fatalf("backend CloseWrite: %v", err)
	}
	gotOutput, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("client read output: %v", err)
	}
	if !bytes.Equal(gotOutput, wantOutput) {
		t.Fatalf("client output = %q, want %q", gotOutput, wantOutput)
	}
	if err := ctlTestWaitError(done); err != nil {
		t.Fatalf("ServeExecTunnel: %v", err)
	}
}

func TestServeExecTunnelRejectsUnboundedTimeout(t *testing.T) {
	var calls atomic.Int32
	err := ServeExecTunnel(context.Background(), ExecTunnelOptions{
		FirstRequestTimeout: DefaultExecFirstRequestTimeout + time.Nanosecond,
		Authorize: func(context.Context) error {
			calls.Add(1)
			return nil
		},
		AcceptDownstream: func(context.Context) (io.ReadWriteCloser, error) {
			return &memoryRWC{}, nil
		},
		AuthorizeRequest: func(context.Context, *ExecRequestFrame) error { return nil },
		DialBackend: func(context.Context, *ExecRequestFrame) (io.ReadWriteCloser, error) {
			return &memoryRWC{}, nil
		},
	})
	if err == nil {
		t.Fatal("ServeExecTunnel accepted timeout above fixed bound")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid options reached Authorize")
	}
}

func TestServeExecTunnelRepeatedTimeoutReapsWatchers(t *testing.T) {
	for iteration := 0; iteration < 25; iteration++ {
		downstream := newBlockingReadRWC()
		err := ServeExecTunnel(context.Background(), ExecTunnelOptions{
			Authorize: func(context.Context) error { return nil },
			AcceptDownstream: func(context.Context) (io.ReadWriteCloser, error) {
				return downstream, nil
			},
			AuthorizeRequest: func(context.Context, *ExecRequestFrame) error { return nil },
			DialBackend: func(context.Context, *ExecRequestFrame) (io.ReadWriteCloser, error) {
				return &memoryRWC{}, nil
			},
			FirstRequestTimeout: time.Millisecond,
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("iteration %d: error = %v, want deadline", iteration, err)
		}
		assertClosedOnce(t, downstream)
	}
}

func assertGenericExecRejection(t *testing.T, raw []byte) {
	t.Helper()
	var response Response
	if err := ReadMessage(bytes.NewReader(raw), &response); err != nil {
		t.Fatalf("read generic rejection: %v (raw %q)", err, raw)
	}
	if response != (Response{Type: TypeError, Msg: "exec request rejected"}) {
		t.Fatalf("response = %+v, want generic exec rejection", response)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type failingWriteRWC struct {
	err    error
	writes atomic.Int32
	closes atomic.Int32
}

func (s *failingWriteRWC) Read([]byte) (int, error) { return 0, io.EOF }
func (s *failingWriteRWC) Write([]byte) (int, error) {
	s.writes.Add(1)
	return 0, s.err
}
func (s *failingWriteRWC) Close() error {
	s.closes.Add(1)
	return nil
}
