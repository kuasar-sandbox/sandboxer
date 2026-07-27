package ctl

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestProxyExecPreservesFirstFrameAndBufferedTail(t *testing.T) {
	payload := []byte(" \n{\"unknown\":{\"nested\":true},\"type\":\"exec_request\",\"exec\":{\"argv\":[\"/bin/true\"]}}\t")
	frame := ctlTestFrame(payload)
	downstream := &memoryRWC{
		reader:   bytes.NewReader(append(append([]byte(nil), frame...), []byte("client-tail")...)),
		maxRead:  1,
		maxWrite: 2,
	}
	ctlConn := &memoryRWC{
		reader:   bytes.NewReader([]byte("ctl-tail")),
		maxRead:  2,
		maxWrite: 3,
	}

	if err := ProxyExec(context.Background(), downstream, ctlConn); err != nil {
		t.Fatalf("ProxyExec: %v", err)
	}
	if got, want := ctlConn.written.Bytes(), append(append([]byte(nil), frame...), []byte("client-tail")...); !bytes.Equal(got, want) {
		t.Fatalf("ctl bytes changed:\n got %q\nwant %q", got, want)
	}
	if got, want := downstream.written.Bytes(), []byte("ctl-tail"); !bytes.Equal(got, want) {
		t.Fatalf("downstream bytes = %q, want %q", got, want)
	}
	assertClosedOnce(t, downstream)
	assertClosedOnce(t, ctlConn)
}

func TestProxyExecFirstFrameSizeBoundary(t *testing.T) {
	prefix := []byte(`{"type":"exec_request","padding":"`)
	suffix := []byte(`"}`)
	payload := append(append(append([]byte(nil), prefix...), bytes.Repeat([]byte{'x'}, MaxMessageBytes-len(prefix)-len(suffix))...), suffix...)
	if len(payload) != MaxMessageBytes {
		t.Fatalf("payload size = %d, want %d", len(payload), MaxMessageBytes)
	}
	frame := ctlTestFrame(payload)
	downstream := &memoryRWC{reader: bytes.NewReader(frame)}
	ctlConn := &memoryRWC{reader: bytes.NewReader(nil)}
	if err := ProxyExec(context.Background(), downstream, ctlConn); err != nil {
		t.Fatalf("ProxyExec at size limit: %v", err)
	}
	if !bytes.Equal(ctlConn.written.Bytes(), frame) {
		t.Fatal("frame at size limit was not forwarded unchanged")
	}

	var oversized [4]byte
	binary.LittleEndian.PutUint32(oversized[:], MaxMessageBytes+1)
	downstream = &memoryRWC{reader: bytes.NewReader(oversized[:])}
	ctlConn = &memoryRWC{reader: bytes.NewReader(nil)}
	if err := ProxyExec(context.Background(), downstream, ctlConn); err == nil {
		t.Fatal("ProxyExec accepted oversized first frame")
	}
	if ctlConn.written.Len() != 0 {
		t.Fatalf("oversized frame wrote %d ctl bytes, want 0", ctlConn.written.Len())
	}
}

func TestProxyExecRejectsInvalidFirstFrameWithoutResponse(t *testing.T) {
	lengthOnly := func(n uint32, body string) []byte {
		var prefix [4]byte
		binary.LittleEndian.PutUint32(prefix[:], n)
		return append(prefix[:], body...)
	}
	tests := []struct {
		name  string
		input []byte
	}{
		{name: "truncated prefix", input: []byte{1, 2, 3}},
		{name: "truncated payload", input: lengthOnly(8, `{}`)},
		{name: "malformed JSON", input: ctlTestFrame([]byte(`{"type":"exec_request","secret":do-not-log-this}`))},
		{name: "null", input: ctlTestFrame([]byte(`null`))},
		{name: "array", input: ctlTestFrame([]byte(`[]`))},
		{name: "missing type", input: ctlTestFrame([]byte(`{}`))},
		{name: "non-string type", input: ctlTestFrame([]byte(`{"type":7}`))},
		{name: "snapshot", input: ctlTestFrame([]byte(`{"type":"snapshot_request"}`))},
		{name: "unknown", input: ctlTestFrame([]byte(`{"type":"do-not-log-this"}`))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			downstream := &memoryRWC{reader: bytes.NewReader(tt.input)}
			ctlConn := &memoryRWC{reader: bytes.NewReader([]byte("must-not-reach-downstream"))}
			err := ProxyExec(context.Background(), downstream, ctlConn)
			if err == nil {
				t.Fatal("ProxyExec accepted invalid first frame")
			}
			if strings.Contains(err.Error(), "do-not-log-this") {
				t.Fatalf("error leaked request type: %v", err)
			}
			if ctlConn.written.Len() != 0 {
				t.Fatalf("invalid frame wrote %d ctl bytes, want 0", ctlConn.written.Len())
			}
			if downstream.written.Len() != 0 {
				t.Fatalf("invalid frame synthesized %d downstream bytes, want 0", downstream.written.Len())
			}
			assertClosedOnce(t, downstream)
			assertClosedOnce(t, ctlConn)
		})
	}
}

func TestProxyExecRejectsNilArgumentsAndCanceledContext(t *testing.T) {
	t.Run("nil context", func(t *testing.T) {
		downstream := &memoryRWC{}
		ctlConn := &memoryRWC{}
		if err := ProxyExec(nil, downstream, ctlConn); err == nil {
			t.Fatal("ProxyExec accepted nil context")
		}
		assertClosedOnce(t, downstream)
		assertClosedOnce(t, ctlConn)
	})
	t.Run("nil downstream", func(t *testing.T) {
		ctlConn := &memoryRWC{}
		if err := ProxyExec(context.Background(), nil, ctlConn); err == nil {
			t.Fatal("ProxyExec accepted nil downstream")
		}
		assertClosedOnce(t, ctlConn)
	})
	t.Run("nil ctl connection", func(t *testing.T) {
		downstream := &memoryRWC{}
		if err := ProxyExec(context.Background(), downstream, nil); err == nil {
			t.Fatal("ProxyExec accepted nil ctl connection")
		}
		assertClosedOnce(t, downstream)
	})
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		downstream := &memoryRWC{}
		ctlConn := &memoryRWC{}
		err := ProxyExec(ctx, downstream, ctlConn)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ProxyExec error = %v, want context.Canceled", err)
		}
		assertClosedOnce(t, downstream)
		assertClosedOnce(t, ctlConn)
	})
}

func TestProxyExecPreservesReverseDirectionAfterDownstreamEOF(t *testing.T) {
	client, downstream := ctlTestUnixPair(t, "downstream")
	ctlConn, backend := ctlTestUnixPair(t, "backend")
	defer client.Close()
	defer backend.Close()
	setDeadline(t, client)
	setDeadline(t, backend)

	done := make(chan error, 1)
	go func() { done <- ProxyExec(context.Background(), downstream, ctlConn) }()

	frame := ctlTestFrame([]byte(`{"type":"exec_request","exec":{"argv":["true"]}}`))
	wantRequest := append(append([]byte(nil), frame...), []byte("client-tail")...)
	if _, err := client.Write(wantRequest); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("client CloseWrite: %v", err)
	}
	gotRequest, err := io.ReadAll(backend)
	if err != nil {
		t.Fatalf("backend read: %v", err)
	}
	if !bytes.Equal(gotRequest, wantRequest) {
		t.Fatalf("backend request = %q, want %q", gotRequest, wantRequest)
	}

	wantResponse := []byte("exec-ack/stdout/stderr/exit-status")
	if _, err := backend.Write(wantResponse); err != nil {
		t.Fatalf("backend write after request EOF: %v", err)
	}
	if err := backend.CloseWrite(); err != nil {
		t.Fatalf("backend CloseWrite: %v", err)
	}
	gotResponse, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if !bytes.Equal(gotResponse, wantResponse) {
		t.Fatalf("client response = %q, want %q", gotResponse, wantResponse)
	}
	if err := ctlTestWaitError(done); err != nil {
		t.Fatalf("ProxyExec: %v", err)
	}
}

func TestProxyExecPreservesDownstreamDirectionAfterCtlEOF(t *testing.T) {
	client, downstream := ctlTestUnixPair(t, "downstream")
	ctlConn, backend := ctlTestUnixPair(t, "backend")
	defer client.Close()
	defer backend.Close()
	setDeadline(t, client)
	setDeadline(t, backend)

	done := make(chan error, 1)
	go func() { done <- ProxyExec(context.Background(), downstream, ctlConn) }()

	frame := ctlTestFrame([]byte(`{"type":"exec_request"}`))
	if _, err := client.Write(frame); err != nil {
		t.Fatalf("client write frame: %v", err)
	}
	gotFrame := make([]byte, len(frame))
	if _, err := io.ReadFull(backend, gotFrame); err != nil {
		t.Fatalf("backend read frame: %v", err)
	}
	if !bytes.Equal(gotFrame, frame) {
		t.Fatal("backend received changed frame")
	}

	wantResponse := []byte("exit-status")
	if _, err := backend.Write(wantResponse); err != nil {
		t.Fatalf("backend write: %v", err)
	}
	if err := backend.CloseWrite(); err != nil {
		t.Fatalf("backend CloseWrite: %v", err)
	}
	gotResponse, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("client read response: %v", err)
	}
	if !bytes.Equal(gotResponse, wantResponse) {
		t.Fatalf("client response = %q, want %q", gotResponse, wantResponse)
	}

	wantTail := []byte("late-stdin")
	if _, err := client.Write(wantTail); err != nil {
		t.Fatalf("client write after response EOF: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("client CloseWrite: %v", err)
	}
	gotTail, err := io.ReadAll(backend)
	if err != nil {
		t.Fatalf("backend read tail: %v", err)
	}
	if !bytes.Equal(gotTail, wantTail) {
		t.Fatalf("backend tail = %q, want %q", gotTail, wantTail)
	}
	if err := ctlTestWaitError(done); err != nil {
		t.Fatalf("ProxyExec: %v", err)
	}
}

func TestProxyExecLargeBidirectionalRelay(t *testing.T) {
	client, downstream := ctlTestUnixPair(t, "large-downstream")
	ctlConn, backend := ctlTestUnixPair(t, "large-backend")
	defer client.Close()
	defer backend.Close()
	setDeadline(t, client)
	setDeadline(t, backend)

	done := make(chan error, 1)
	go func() { done <- ProxyExec(context.Background(), downstream, ctlConn) }()

	frame := ctlTestFrame([]byte(`{"type":"exec_request"}`))
	wantRequest := append(append([]byte(nil), frame...), bytes.Repeat([]byte("request-data-"), 32*1024)...)
	wantResponse := bytes.Repeat([]byte("response-data-"), 32*1024)
	backendRead := make(chan ctlTestReadResult, 1)
	clientRead := make(chan ctlTestReadResult, 1)
	go func() {
		data, err := io.ReadAll(backend)
		backendRead <- ctlTestReadResult{data: data, err: err}
	}()
	go func() {
		data, err := io.ReadAll(client)
		clientRead <- ctlTestReadResult{data: data, err: err}
	}()
	clientWrite := make(chan error, 1)
	backendWrite := make(chan error, 1)
	go func() { clientWrite <- ctlTestWriteAndClose(client, wantRequest) }()
	go func() { backendWrite <- ctlTestWriteAndClose(backend, wantResponse) }()

	if err := ctlTestWaitError(clientWrite); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if err := ctlTestWaitError(backendWrite); err != nil {
		t.Fatalf("backend write: %v", err)
	}
	gotRequest := ctlTestWaitRead(t, backendRead)
	gotResponse := ctlTestWaitRead(t, clientRead)
	if !bytes.Equal(gotRequest, wantRequest) {
		t.Fatalf("request size/content mismatch: got %d bytes, want %d", len(gotRequest), len(wantRequest))
	}
	if !bytes.Equal(gotResponse, wantResponse) {
		t.Fatalf("response size/content mismatch: got %d bytes, want %d", len(gotResponse), len(wantResponse))
	}
	if err := ctlTestWaitError(done); err != nil {
		t.Fatalf("ProxyExec: %v", err)
	}
}

func TestProxyExecDoesNotFullCloseWhenCloseWriteIsUnavailable(t *testing.T) {
	frame := ctlTestFrame([]byte(`{"type":"exec_request"}`))
	eofSeen := make(chan struct{})
	downstream := &memoryRWC{reader: &eofSignalReader{reader: bytes.NewReader(frame), seen: eofSeen}}
	backendReader, backendWriter := io.Pipe()
	ctlClosed := make(chan struct{})
	ctlConn := &memoryRWC{
		reader: backendReader,
		onClose: func() error {
			_ = backendReader.Close()
			close(ctlClosed)
			return nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- ProxyExec(context.Background(), downstream, ctlConn) }()

	select {
	case <-eofSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("downstream EOF was not observed")
	}
	select {
	case <-ctlClosed:
		t.Fatal("clean downstream EOF full-closed ctl stream without CloseWrite")
	case <-time.After(100 * time.Millisecond):
	}

	want := []byte("trailing-output")
	if _, err := backendWriter.Write(want); err != nil {
		t.Fatalf("write backend output: %v", err)
	}
	if err := backendWriter.Close(); err != nil {
		t.Fatalf("close backend output: %v", err)
	}
	if err := ctlTestWaitError(done); err != nil {
		t.Fatalf("ProxyExec: %v", err)
	}
	if got := downstream.written.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("downstream output = %q, want %q", got, want)
	}
}

func TestProxyExecContextCancellationStopsGateAndRelay(t *testing.T) {
	t.Run("gate", func(t *testing.T) {
		client, downstreamConn := net.Pipe()
		backend, ctlConn := net.Pipe()
		defer client.Close()
		defer backend.Close()
		downstream := &countingRWC{ReadWriteCloser: downstreamConn}
		ctlStream := &countingRWC{ReadWriteCloser: ctlConn}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- ProxyExec(ctx, downstream, ctlStream) }()
		cancel()
		if err := ctlTestWaitError(done); !errors.Is(err, context.Canceled) {
			t.Fatalf("ProxyExec error = %v, want context.Canceled", err)
		}
		assertClosedOnce(t, downstream)
		assertClosedOnce(t, ctlStream)
	})

	t.Run("relay", func(t *testing.T) {
		client, downstreamConn := net.Pipe()
		backend, ctlConn := net.Pipe()
		defer client.Close()
		defer backend.Close()
		downstream := &countingRWC{ReadWriteCloser: downstreamConn}
		ctlStream := &countingRWC{ReadWriteCloser: ctlConn}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- ProxyExec(ctx, downstream, ctlStream) }()

		frame := ctlTestFrame([]byte(`{"type":"exec_request"}`))
		writeDone := make(chan error, 1)
		go func() {
			_, err := client.Write(frame)
			writeDone <- err
		}()
		gotFrame := make([]byte, len(frame))
		if _, err := io.ReadFull(backend, gotFrame); err != nil {
			t.Fatalf("backend read frame: %v", err)
		}
		if err := ctlTestWaitError(writeDone); err != nil {
			t.Fatalf("client write frame: %v", err)
		}
		cancel()
		if err := ctlTestWaitError(done); !errors.Is(err, context.Canceled) {
			t.Fatalf("ProxyExec error = %v, want context.Canceled", err)
		}
		assertClosedOnce(t, downstream)
		assertClosedOnce(t, ctlStream)
	})

	t.Run("deadline", func(t *testing.T) {
		client, downstreamConn := net.Pipe()
		backend, ctlConn := net.Pipe()
		defer client.Close()
		defer backend.Close()
		downstream := &countingRWC{ReadWriteCloser: downstreamConn}
		ctlStream := &countingRWC{ReadWriteCloser: ctlConn}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		err := ProxyExec(ctx, downstream, ctlStream)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ProxyExec error = %v, want context.DeadlineExceeded", err)
		}
		assertClosedOnce(t, downstream)
		assertClosedOnce(t, ctlStream)
	})
}

func TestProxyExecCancellationWinsConcurrentCompletionAndError(t *testing.T) {
	tests := []struct {
		name     string
		terminal error
	}{
		{name: "completion", terminal: io.EOF},
		{name: "relay error", terminal: errors.New("concurrent relay error")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			frame := ctlTestFrame([]byte(`{"type":"exec_request"}`))
			downstream := &memoryRWC{
				reader: &cancelingTerminalReader{
					data:     frame,
					terminal: tt.terminal,
					cancel:   cancel,
				},
			}
			ctlConn := &memoryRWC{reader: bytes.NewReader(nil)}
			err := ProxyExec(ctx, downstream, ctlConn)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("ProxyExec error = %v, want context.Canceled", err)
			}
			assertClosedOnce(t, downstream)
			assertClosedOnce(t, ctlConn)
		})
	}

	t.Run("gate error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		downstream := &memoryRWC{reader: &cancelingTerminalReader{
			data:     []byte{1, 2, 3},
			terminal: io.EOF,
			cancel:   cancel,
		}}
		ctlConn := &memoryRWC{reader: bytes.NewReader(nil)}
		err := ProxyExec(ctx, downstream, ctlConn)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ProxyExec error = %v, want context.Canceled", err)
		}
		assertClosedOnce(t, downstream)
		assertClosedOnce(t, ctlConn)
	})
}

func TestProxyExecRelayErrorStopsOtherDirection(t *testing.T) {
	wantErr := errors.New("injected downstream read failure")
	frame := ctlTestFrame([]byte(`{"type":"exec_request"}`))
	downstream := &memoryRWC{reader: &terminalErrorReader{data: frame, err: wantErr}}
	ctlConn := newBlockingReadRWC()

	done := make(chan error, 1)
	go func() { done <- ProxyExec(context.Background(), downstream, ctlConn) }()
	err := ctlTestWaitError(done)
	if !errors.Is(err, wantErr) {
		t.Fatalf("ProxyExec error = %v, want injected error", err)
	}
	if !bytes.Equal(ctlConn.written.Bytes(), frame) {
		t.Fatal("valid first frame was not forwarded before relay error")
	}
	assertClosedOnce(t, downstream)
	assertClosedOnce(t, ctlConn)
}

func TestProxyExecCloseWriteErrorStopsOtherDirection(t *testing.T) {
	wantErr := errors.New("injected CloseWrite failure")
	frame := ctlTestFrame([]byte(`{"type":"exec_request"}`))
	downstream := &memoryRWC{reader: bytes.NewReader(frame)}
	ctlConn := &failingCloseWriteRWC{
		blockingReadRWC: newBlockingReadRWC(),
		err:             wantErr,
	}

	done := make(chan error, 1)
	go func() { done <- ProxyExec(context.Background(), downstream, ctlConn) }()
	err := ctlTestWaitError(done)
	if !errors.Is(err, wantErr) {
		t.Fatalf("ProxyExec error = %v, want CloseWrite failure", err)
	}
	if got := ctlConn.closeWrites.Load(); got != 1 {
		t.Fatalf("CloseWrite calls = %d, want 1", got)
	}
	if !bytes.Equal(ctlConn.written.Bytes(), frame) {
		t.Fatal("valid first frame was not forwarded before CloseWrite failure")
	}
	assertClosedOnce(t, downstream)
	assertClosedOnce(t, ctlConn)
}

func TestProxyExecSimultaneousRelayErrorsUseDirectionPriority(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		downstreamErr := errors.New("downstream-to-ctl failure")
		ctlErr := errors.New("ctl-to-downstream failure")
		ready := make(chan struct{}, 2)
		release := make(chan struct{})
		frame := ctlTestFrame([]byte(`{"type":"exec_request"}`))
		downstream := &memoryRWC{reader: &barrierErrorReader{
			data:    frame,
			err:     downstreamErr,
			ready:   ready,
			release: release,
		}}
		ctlConn := &memoryRWC{reader: &barrierErrorReader{
			err:     ctlErr,
			ready:   ready,
			release: release,
		}}

		done := make(chan error, 1)
		go func() { done <- ProxyExec(context.Background(), downstream, ctlConn) }()
		for range 2 {
			select {
			case <-ready:
			case <-time.After(3 * time.Second):
				t.Fatalf("iteration %d: relay direction did not reach error barrier", iteration)
			}
		}
		close(release)
		err := ctlTestWaitError(done)
		if !errors.Is(err, downstreamErr) {
			t.Fatalf("iteration %d: ProxyExec error = %v, want downstream-to-ctl priority", iteration, err)
		}
		assertClosedOnce(t, downstream)
		assertClosedOnce(t, ctlConn)
	}
}

func TestProxyExecWithCtlServer(t *testing.T) {
	var snapshotCalls atomic.Int32
	execRequest := make(chan Request, 1)
	execDone := make(chan struct{})
	srv := &Server{
		Path: filepath.Join(t.TempDir(), "ctl.sock"),
		SnapshotHandler: func(Request) (Response, error) {
			snapshotCalls.Add(1)
			return Response{}, nil
		},
		ExecHandler: func(conn net.Conn, req Request) {
			defer conn.Close()
			defer close(execDone)
			execRequest <- req
			if err := WriteMessage(conn, Response{Type: TypeExecAck, Stdio: &proto.StdioSpec{Stdin: true, Stdout: true}}); err != nil {
				return
			}
			tail, err := io.ReadAll(conn)
			if err != nil {
				return
			}
			_, _ = conn.Write(append([]byte("mux:"), tail...))
		},
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx) }()
	defer func() {
		cancel()
		srv.Stop()
		if err := ctlTestWaitError(serveDone); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

	client, downstream := ctlTestUnixPair(t, "exec-client")
	defer client.Close()
	setDeadline(t, client)
	ctlConn, err := net.Dial("unix", srv.Path)
	if err != nil {
		t.Fatalf("dial ctl server: %v", err)
	}
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- ProxyExec(context.Background(), downstream, ctlConn) }()

	wantReq := Request{Type: TypeExecRequest, Exec: &proto.ExecSpec{Argv: []string{"/bin/true"}}}
	if err := WriteMessage(client, wantReq); err != nil {
		t.Fatalf("write exec request: %v", err)
	}
	var ack Response
	if err := ReadMessage(client, &ack); err != nil {
		t.Fatalf("read exec ack: %v", err)
	}
	if ack.Type != TypeExecAck || ack.Stdio == nil || !ack.Stdio.Stdin || !ack.Stdio.Stdout {
		t.Fatalf("unexpected exec ack: %+v", ack)
	}
	wantTail := []byte("opaque-mux-bytes")
	if _, err := client.Write(wantTail); err != nil {
		t.Fatalf("write MUX tail: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("client CloseWrite: %v", err)
	}
	gotTail, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read MUX tail: %v", err)
	}
	if got, want := gotTail, append([]byte("mux:"), wantTail...); !bytes.Equal(got, want) {
		t.Fatalf("MUX tail = %q, want %q", got, want)
	}
	if err := ctlTestWaitError(proxyDone); err != nil {
		t.Fatalf("ProxyExec: %v", err)
	}
	select {
	case gotReq := <-execRequest:
		if gotReq.Exec == nil || len(gotReq.Exec.Argv) != 1 || gotReq.Exec.Argv[0] != "/bin/true" {
			t.Fatalf("ExecHandler request = %+v", gotReq)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExecHandler was not called")
	}
	select {
	case <-execDone:
	case <-time.After(2 * time.Second):
		t.Fatal("ExecHandler did not finish")
	}

	rejectedClient, rejectedDownstream := ctlTestUnixPair(t, "snapshot-client")
	defer rejectedClient.Close()
	rejectedCtl, err := net.Dial("unix", srv.Path)
	if err != nil {
		t.Fatalf("dial ctl server for rejected request: %v", err)
	}
	rejectedDone := make(chan error, 1)
	go func() { rejectedDone <- ProxyExec(context.Background(), rejectedDownstream, rejectedCtl) }()
	if err := WriteMessage(rejectedClient, Request{Type: TypeSnapshotRequest}); err != nil {
		t.Fatalf("write snapshot request: %v", err)
	}
	if err := rejectedClient.CloseWrite(); err != nil {
		t.Fatalf("rejected client CloseWrite: %v", err)
	}
	err = ctlTestWaitError(rejectedDone)
	if errors.Is(err, errCtlTestTimeout) {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("ProxyExec accepted snapshot request")
	}
	if got := snapshotCalls.Load(); got != 0 {
		t.Fatalf("SnapshotHandler calls = %d, want 0", got)
	}
}

type memoryRWC struct {
	reader   io.Reader
	maxRead  int
	maxWrite int
	written  bytes.Buffer
	closes   atomic.Int32
	onClose  func() error
}

func (s *memoryRWC) Read(p []byte) (int, error) {
	if s.reader == nil {
		return 0, io.EOF
	}
	if s.maxRead > 0 && len(p) > s.maxRead {
		p = p[:s.maxRead]
	}
	return s.reader.Read(p)
}

func (s *memoryRWC) Write(p []byte) (int, error) {
	if s.maxWrite > 0 && len(p) > s.maxWrite {
		p = p[:s.maxWrite]
	}
	return s.written.Write(p)
}

func (s *memoryRWC) Close() error {
	s.closes.Add(1)
	if s.onClose != nil {
		return s.onClose()
	}
	return nil
}

func (s *memoryRWC) closeCount() int32 { return s.closes.Load() }

type countingRWC struct {
	io.ReadWriteCloser
	closes atomic.Int32
}

func (s *countingRWC) Close() error {
	s.closes.Add(1)
	return s.ReadWriteCloser.Close()
}

func (s *countingRWC) closeCount() int32 { return s.closes.Load() }

type eofSignalReader struct {
	reader io.Reader
	seen   chan struct{}
	once   sync.Once
}

func (r *eofSignalReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) {
		r.once.Do(func() { close(r.seen) })
	}
	return n, err
}

type terminalErrorReader struct {
	data []byte
	err  error
}

type cancelingTerminalReader struct {
	data     []byte
	terminal error
	cancel   context.CancelFunc
	once     sync.Once
}

func (r *cancelingTerminalReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	r.once.Do(r.cancel)
	return 0, r.terminal
}

type barrierErrorReader struct {
	data    []byte
	err     error
	ready   chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *barrierErrorReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	r.once.Do(func() { r.ready <- struct{}{} })
	<-r.release
	return 0, r.err
}

func (r *terminalErrorReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

type blockingReadRWC struct {
	written bytes.Buffer
	closed  chan struct{}
	once    sync.Once
	closes  atomic.Int32
}

func newBlockingReadRWC() *blockingReadRWC {
	return &blockingReadRWC{closed: make(chan struct{})}
}

func (s *blockingReadRWC) Read([]byte) (int, error) {
	<-s.closed
	return 0, net.ErrClosed
}

func (s *blockingReadRWC) Write(p []byte) (int, error) { return s.written.Write(p) }

func (s *blockingReadRWC) Close() error {
	s.closes.Add(1)
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *blockingReadRWC) closeCount() int32 { return s.closes.Load() }

type failingCloseWriteRWC struct {
	*blockingReadRWC
	err         error
	closeWrites atomic.Int32
}

func (s *failingCloseWriteRWC) CloseWrite() error {
	s.closeWrites.Add(1)
	return s.err
}

type closeCounter interface {
	closeCount() int32
}

func assertClosedOnce(t *testing.T, stream closeCounter) {
	t.Helper()
	if got := stream.closeCount(); got != 1 {
		t.Fatalf("Close calls = %d, want 1", got)
	}
}

func ctlTestFrame(payload []byte) []byte {
	frame := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	return frame
}

func ctlTestUnixPair(t *testing.T, name string) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".sock")
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatalf("resolve Unix address: %v", err)
	}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatalf("listen Unix: %v", err)
	}
	defer listener.Close()

	accepted := make(chan *net.UnixConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	client, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		t.Fatalf("dial Unix: %v", err)
	}
	select {
	case server := <-accepted:
		return client, server
	case err := <-acceptErr:
		client.Close()
		t.Fatalf("accept Unix: %v", err)
	case <-time.After(2 * time.Second):
		client.Close()
		t.Fatal("accept Unix timed out")
	}
	return nil, nil
}

func setDeadline(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
}

var errCtlTestTimeout = errors.New("timed out waiting for operation")

func ctlTestWaitError(ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	case <-time.After(3 * time.Second):
		return errCtlTestTimeout
	}
}

func ctlTestWriteAndClose(conn *net.UnixConn, data []byte) error {
	if _, err := io.Copy(conn, bytes.NewReader(data)); err != nil {
		return err
	}
	return conn.CloseWrite()
}

type ctlTestReadResult struct {
	data []byte
	err  error
}

func ctlTestWaitRead(t *testing.T, ch <-chan ctlTestReadResult) []byte {
	t.Helper()
	select {
	case result := <-ch:
		if result.err != nil {
			t.Fatalf("read: %v", result.err)
		}
		return result.data
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for read")
		return nil
	}
}
