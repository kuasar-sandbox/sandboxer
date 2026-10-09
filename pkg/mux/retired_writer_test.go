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

type terminalTestConn struct{ bytes.Buffer }

func (c *terminalTestConn) Close() error { return nil }

// Keep the transport writable after the terminal frame so transport-level
// rejection cannot mask a missing MUX write barrier.
func TestTerminalMUXRejectsLateFrames(t *testing.T) {
	for _, terminal := range []uint8{FrameMuxClose, FrameMuxCloseAck} {
		c := &terminalTestConn{}
		s := &Session{conn: c}
		st := newStream(s, StreamStdout, 8)
		if err := s.writeFrame(Frame{Stream: StreamControl, Type: terminal}); err != nil {
			t.Fatal(err)
		}
		before := c.Len()
		for name, write := range map[string]func() error{
			// Each operation starts with its own retained handle; Reset must not
			// turn a later EOF into an idempotent no-op through map iteration order.
			"EOF":     func() error { return newStream(s, StreamStdout, 8).CloseWrite() },
			"reset":   func() error { return newStream(s, StreamStdout, 8).Reset() },
			"winsize": func() error { return s.SetWinsize(80, 24) },
			"exit":    func() error { return s.SendExitStatus(0) },
			"data": func() error {
				return s.writeFrame(Frame{Stream: StreamStdout, Type: FrameData, Payload: []byte("late")})
			},
		} {
			if err := write(); !errors.Is(err, ErrClosed) {
				t.Errorf("%s after %d: %v", name, terminal, err)
			}
		}
		// Draining already received data must still work, but its window update
		// must not escape the terminal barrier.
		st.buf = []byte("drain")
		if n, err := st.Read(make([]byte, 5)); n != 5 || err != nil {
			t.Fatalf("drain = %d, %v", n, err)
		}
		if c.Len() != before {
			t.Fatalf("late frames reached transport after %d: % x", terminal, c.Bytes()[before:])
		}
	}
}

type enteredWriteConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *enteredWriteConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(p)
}

type enteredWaitLocker struct {
	sync.Locker
	entered chan struct{}
	once    sync.Once
}

func (l *enteredWaitLocker) Unlock() {
	// Cond.Wait registers its wakeup ticket before calling Unlock.
	l.once.Do(func() { close(l.entered) })
	l.Locker.Unlock()
}

func cleanupMUXWriters(t *testing.T, s *Session, peer net.Conn, writers *sync.WaitGroup) {
	t.Helper()
	// Release real blocked I/O even if Close regresses to waiting on wmu.
	_ = peer.Close()
	done := make(chan struct{})
	go func() {
		_ = s.Close()
		writers.Wait()
		<-s.Done()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("MUX writer cleanup did not complete")
	}
}

func TestForceCloseInterruptsMUXWriter(t *testing.T) {
	a, b := net.Pipe()
	c := &enteredWriteConn{Conn: a, entered: make(chan struct{})}
	s := NewSession(c, PipeStreams(false, true, false), Options{})
	var writers sync.WaitGroup
	t.Cleanup(func() { cleanupMUXWriters(t, s, b, &writers) })
	writer := make(chan error, 1)
	writers.Add(1)
	go func() {
		defer writers.Done()
		_, err := s.Stream(StreamStdout).Write([]byte("blocked"))
		writer <- err
	}()
	select {
	case <-c.entered: // writer owns wmu; peer deliberately never reads
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not enter transport write")
	}
	handshake := make(chan error, 1)
	writers.Add(1)
	go func() {
		defer writers.Done()
		handshake <- s.InitMuxClose()
	}()
	closed := make(chan error, 1)
	writers.Add(1)
	go func() {
		defer writers.Done()
		closed <- s.Close()
	}()
	for name, ch := range map[string]<-chan error{"close": closed, "writer": writer, "handshake": handshake} {
		select {
		case err := <-ch:
			if name == "writer" && err == nil {
				t.Fatal("blocked write succeeded")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s stuck behind blocked write", name)
		}
	}
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not terminate")
	}
	if _, err := s.Stream(StreamStdout).Read(make([]byte, 1)); err != ErrClosed && err != io.EOF {
		t.Fatalf("read after close: %v", err)
	}
}

// A terminal write holds wmu while a retained stream starts CloseWrite.
// Releasing the terminal syscall must not admit that late EOF afterward.
func TestTerminalMUXSerializesQueuedEOF(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := &enteredWriteConn{Conn: a, entered: make(chan struct{})}
	s := &Session{conn: c}
	defer s.Close()
	terminal := make(chan error, 1)
	go func() { terminal <- s.writeFrame(Frame{Stream: StreamControl, Type: FrameMuxClose}) }()
	<-c.entered
	late := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		late <- newStream(s, StreamStdout, 8).CloseWrite()
	}()
	<-started
	if _, err := ReadFrame(b); err != nil {
		t.Fatal(err)
	}
	for name, ch := range map[string]<-chan error{"terminal": terminal, "EOF": late} {
		select {
		case err := <-ch:
			if name == "terminal" && err != nil {
				t.Fatal(err)
			}
			if name == "EOF" && !errors.Is(err, ErrClosed) {
				t.Fatalf("queued EOF: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s blocked on transport after terminal frame", name)
		}
	}
}

func TestMuxCloseCancelsCreditWaiter(t *testing.T) {
	a, b := net.Pipe()
	c := &enteredWriteConn{Conn: a, entered: make(chan struct{})}
	s := NewSession(c, PipeStreams(false, true, false), Options{Window: 1})
	var writers sync.WaitGroup
	t.Cleanup(func() { cleanupMUXWriters(t, s, b, &writers) })
	// Bound peer frame I/O as well as the explicit goroutine waits below.
	if err := b.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	st := s.Stream(StreamStdout)
	// Exhaust credit without a peer window update.
	wrote := make(chan error, 1)
	writers.Add(1)
	go func() {
		defer writers.Done()
		_, err := st.Write([]byte("x"))
		wrote <- err
	}()
	if _, err := ReadFrame(b); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first write did not complete")
	}
	// Only Cond.Wait uses this wrapper; ordinary stream locking still uses mu.
	// Install after the first write and before starting the credit waiter.
	entered := make(chan struct{})
	st.mu.Lock()
	st.cond.L = &enteredWaitLocker{Locker: &st.mu, entered: entered}
	st.mu.Unlock()
	writers.Add(1)
	go func() {
		defer writers.Done()
		_, err := st.Write([]byte("waiting"))
		wrote <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("credit waiter did not enter condition wait")
	}
	handshake := make(chan error, 1)
	writers.Add(1)
	go func() {
		defer writers.Done()
		handshake <- s.InitMuxClose()
	}()
	f, err := ReadFrame(b)
	if err != nil || f.Type != FrameMuxClose {
		t.Fatalf("terminal frame: %+v %v", f, err)
	}
	select {
	case err := <-wrote:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("credit waiter: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("credit waiter not canceled before ACK")
	}
	if err := WriteFrame(b, Frame{Stream: StreamControl, Type: FrameMuxCloseAck}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-handshake:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handshake did not complete")
	}
}
