package guestlink

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
)

// fakeMUXConn is a minimal io.ReadWriteCloser for MUXLink tests: Read
// blocks until Close, Write is a sink, Close is observable and idempotent.
type fakeMUXConn struct {
	mu     sync.Mutex
	closed bool
	block  chan struct{}
}

func newFakeMUXConn() *fakeMUXConn { return &fakeMUXConn{block: make(chan struct{})} }

func (c *fakeMUXConn) Read([]byte) (int, error) {
	<-c.block
	return 0, io.EOF
}
func (c *fakeMUXConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *fakeMUXConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.block)
	}
	return nil
}
func (c *fakeMUXConn) wasClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func TestMUXLink_TeardownClosesAndCleansOnce(t *testing.T) {
	var link MUXLink
	link.Teardown() // empty link: no-op, must not panic

	fc := newFakeMUXConn()
	sess := mux.NewSession(fc, mux.StreamSet{}, mux.Options{})
	var cleanups int32
	link.Set(sess, func() { atomic.AddInt32(&cleanups, 1) })

	link.Teardown()
	if !fc.wasClosed() {
		t.Fatal("Teardown did not close the session conn")
	}
	if n := atomic.LoadInt32(&cleanups); n != 1 {
		t.Fatalf("cleanup called %d times, want 1", n)
	}
	link.Teardown() // idempotent
	if n := atomic.LoadInt32(&cleanups); n != 1 {
		t.Fatalf("cleanup called %d times after second Teardown, want 1", n)
	}
	select {
	case <-sess.Done():
	case <-time.After(time.Second):
		t.Fatal("session read loop did not exit after Teardown")
	}
}

func TestMUXLink_SetReplacesWithoutCleanup(t *testing.T) {
	var link MUXLink
	fcA, fcB := newFakeMUXConn(), newFakeMUXConn()
	sessA := mux.NewSession(fcA, mux.StreamSet{}, mux.Options{})
	sessB := mux.NewSession(fcB, mux.StreamSet{}, mux.Options{})
	var a, b int32
	link.Set(sessA, func() { atomic.AddInt32(&a, 1) })
	link.Set(sessB, func() { atomic.AddInt32(&b, 1) }) // replaces A; A's cleanup NOT run

	link.Teardown()
	if atomic.LoadInt32(&a) != 0 {
		t.Fatalf("replaced session's cleanup ran %d times, want 0", a)
	}
	if atomic.LoadInt32(&b) != 1 {
		t.Fatalf("current session's cleanup ran %d times, want 1", b)
	}
	if fcA.wasClosed() {
		t.Fatal("replaced session conn was closed by Teardown (should be the caller's job)")
	}
	if !fcB.wasClosed() {
		t.Fatal("current session conn not closed by Teardown")
	}
	// Clean up the leaked sessA read loop for the race detector.
	_ = sessA.Close()
}
