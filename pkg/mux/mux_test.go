package mux

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type shortWriter struct {
	bytes.Buffer
	max int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.Buffer.Write(p)
}

func TestFrameRoundTrip(t *testing.T) {
	cases := []Frame{
		{Stream: StreamStdout, Type: FrameData, Payload: []byte("hello world")},
		{Stream: StreamControl, Type: FrameMuxClose},
		{Stream: StreamControl, Type: FrameSetWinsize, Payload: encodeWinsize(120, 40)},
		{Stream: StreamStdin, Type: FrameEOF},
		{Stream: StreamPTY, Type: FrameWindowUpdate, Payload: encodeUint32(4096)},
		{Stream: StreamStdout, Type: FrameData, Payload: bytes.Repeat([]byte{0xAB}, MaxFramePayload)},
	}
	var buf bytes.Buffer
	for _, f := range cases {
		buf.Reset()
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatalf("WriteFrame(%v): %v", f, err)
		}
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if got.Stream != f.Stream || got.Type != f.Type || !bytes.Equal(got.Payload, f.Payload) {
			t.Fatalf("round-trip mismatch: got %+v want %+v", got, f)
		}
	}

	// too-large payload rejected
	if err := WriteFrame(&buf, Frame{Stream: StreamStdout, Type: FrameData, Payload: make([]byte, MaxFramePayload+1)}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestFrameRoundTripWithShortWrites(t *testing.T) {
	w := &shortWriter{max: 3}
	want := Frame{
		Stream:  StreamStdout,
		Type:    FrameData,
		Payload: bytes.Repeat([]byte("frame-payload-"), 4096),
	}
	if err := WriteFrame(w, want); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := ReadFrame(bytes.NewReader(w.Bytes()))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Stream != want.Stream || got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("round-trip mismatch: got payload=%d bytes, want=%d", len(got.Payload), len(want.Payload))
	}
}

// newPipePair returns a host Session and a guest Session over an
// in-memory pipe with the pipe-mode stream set {stdin, stdout, stderr}.
// Both sides get the same Options (the window must match the peer's).
func newPipePair(t *testing.T, opt Options) (host, guest *Session, closeAll func()) {
	t.Helper()
	hc, gc := net.Pipe()
	ss := PipeStreams(true, true, true)
	host = NewSession(hc, ss, opt)
	guest = NewSession(gc, ss, opt)
	return host, guest, func() { host.Close(); guest.Close() }
}

// readAll drains a stream to completion. io.ReadAll already returns the
// bytes read before any terminal error (io.EOF on a clean half-close,
// ErrClosed if the session died first), so the error is discarded —
// callers assert on the content.
func readAll(st *Stream) []byte {
	b, _ := io.ReadAll(st)
	return b
}

func TestSessionPipeRoundTrip(t *testing.T) {
	host, guest, closeAll := newPipePair(t, Options{})
	defer closeAll()

	const msg = "the quick brown fox"
	const reply = "jumps over the lazy dog"

	var wg sync.WaitGroup
	// guest: read stdin, echo a fixed reply on stdout, then EOF stdout.
	wg.Add(1)
	go func() {
		defer wg.Done()
		in := readAll(guest.Stream(StreamStdin))
		if string(in) != msg {
			t.Errorf("guest got stdin %q want %q", in, msg)
		}
		if _, err := guest.Stream(StreamStdout).Write([]byte(reply)); err != nil {
			t.Errorf("guest stdout write: %v", err)
		}
		if err := guest.Stream(StreamStdout).CloseWrite(); err != nil {
			t.Errorf("guest stdout CloseWrite: %v", err)
		}
	}()

	// host: write stdin + EOF, then read stdout.
	if _, err := host.Stream(StreamStdin).Write([]byte(msg)); err != nil {
		t.Fatalf("host stdin write: %v", err)
	}
	if err := host.Stream(StreamStdin).CloseWrite(); err != nil {
		t.Fatalf("host stdin CloseWrite: %v", err)
	}
	out := readAll(host.Stream(StreamStdout))
	if string(out) != reply {
		t.Fatalf("host got stdout %q want %q", out, reply)
	}
	wg.Wait()
}

func TestSessionFlowControl(t *testing.T) {
	// Small window to make backpressure observable.
	host, guest, closeAll := newPipePair(t, Options{Window: 4096})
	defer closeAll()

	payload := bytes.Repeat([]byte("x"), 100*1024) // >> window
	done := make(chan error, 1)
	go func() {
		_, err := host.Stream(StreamStdin).Write(payload)
		if err == nil {
			err = host.Stream(StreamStdin).CloseWrite()
		}
		done <- err
	}()

	// Without the guest reading, the writer must block (credit exhausts).
	select {
	case err := <-done:
		t.Fatalf("write completed without reader (no backpressure): err=%v", err)
	case <-time.After(100 * time.Millisecond):
	}

	got := readAll(guest.Stream(StreamStdin))
	if !bytes.Equal(got, payload) {
		t.Fatalf("guest stdin: got %d bytes, want %d", len(got), len(payload))
	}
	if err := <-done; err != nil {
		t.Fatalf("host stdin write: %v", err)
	}
}

func TestSessionMuxCloseHandshake(t *testing.T) {
	host, guest, closeAll := newPipePair(t, Options{})
	defer closeAll()

	// Host drains stdout in the background so MUX_CLOSE_ACK / EOF aren't
	// blocked behind unread data (there is none here, but be safe).
	go io.Copy(io.Discard, host.Stream(StreamStdout))

	// Guest initiates the graceful close; should return once the host
	// auto-replies MUX_CLOSE_ACK.
	errc := make(chan error, 1)
	go func() { errc <- guest.InitMuxClose() }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("InitMuxClose: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("InitMuxClose did not return")
	}

	// After InitMuxClose, data writes are refused.
	if _, err := guest.Stream(StreamStdout).Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed after InitMuxClose, got %v", err)
	}

	// Host observed the peer-close.
	select {
	case <-host.PeerClosed():
	case <-time.After(time.Second):
		t.Fatal("host did not observe MUX_CLOSE")
	}

	// Guest closes the conn → host read loop ends with clean EOF.
	guest.Close()
	select {
	case <-host.Done():
		if err := host.Err(); err != nil {
			t.Fatalf("host Err after clean close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("host Done not signalled")
	}
}

func TestSessionExitStatus(t *testing.T) {
	for _, code := range []int{0, 42, 137} {
		// net.Pipe is synchronous: SendExitStatus blocks until the host
		// read loop has consumed (and dispatched) the frame, so once it
		// returns ExitStatus is already observable.
		hc, gc := net.Pipe()
		host := NewSession(hc, PTYStreams(), Options{})
		guest := NewSession(gc, PTYStreams(), Options{})

		if _, ok := host.ExitStatus(); ok {
			t.Fatalf("code=%d: ExitStatus ok before any frame", code)
		}
		if err := guest.SendExitStatus(code); err != nil {
			t.Fatalf("code=%d: SendExitStatus: %v", code, err)
		}
		guest.Close()
		select {
		case <-host.Done():
		case <-time.After(2 * time.Second):
			t.Fatalf("code=%d: host Done not signalled", code)
		}
		got, ok := host.ExitStatus()
		if !ok || got != code {
			t.Fatalf("code=%d: ExitStatus = (%d,%v) want (%d,true)", code, got, ok, code)
		}
		host.Close()
	}
}

func TestSessionReset(t *testing.T) {
	host, guest, closeAll := newPipePair(t, Options{})
	defer closeAll()

	if err := host.Stream(StreamStdin).Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	// Guest's Read on the reset stream returns ErrStreamReset.
	buf := make([]byte, 16)
	if _, err := guest.Stream(StreamStdin).Read(buf); !errors.Is(err, ErrStreamReset) {
		t.Fatalf("expected ErrStreamReset, got %v", err)
	}
}

func TestSessionSetWinsize(t *testing.T) {
	got := make(chan [2]uint16, 1)
	hc, gc := net.Pipe()
	host := NewSession(hc, PTYStreams(), Options{})
	guest := NewSession(gc, PTYStreams(), Options{OnSetWinsize: func(c, r uint16) { got <- [2]uint16{c, r} }})
	defer host.Close()
	defer guest.Close()

	if err := host.SetWinsize(132, 50); err != nil {
		t.Fatalf("SetWinsize: %v", err)
	}
	select {
	case ws := <-got:
		if ws != [2]uint16{132, 50} {
			t.Fatalf("OnSetWinsize got %v want [132 50]", ws)
		}
	case <-time.After(time.Second):
		t.Fatal("OnSetWinsize not called")
	}
}

func TestSessionProtocolViolation(t *testing.T) {
	// Send a frame for a stream not in the negotiated set → the receiver
	// tears the session down.
	hc, gc := net.Pipe()
	guest := NewSession(gc, PipeStreams(true, false, false), Options{}) // only stdin active
	defer guest.Close()
	defer hc.Close()

	// Raw-write a STDOUT DATA frame onto the host end (stdout not active).
	go func() { _ = WriteFrame(hc, Frame{Stream: StreamStdout, Type: FrameData, Payload: []byte("x")}) }()

	select {
	case <-guest.Done():
		if !errors.Is(guest.Err(), ErrProtocol) {
			t.Fatalf("expected ErrProtocol, got %v", guest.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guest did not tear down on protocol violation")
	}
}
