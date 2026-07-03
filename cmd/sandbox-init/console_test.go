package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
)

// muxPair wires a guest-side and host-side mux.Session over a net.Pipe so
// the console-bridge pumps can be exercised without a real vsock.
func muxPair(t *testing.T, ss mux.StreamSet) (guest, host *mux.Session) {
	t.Helper()
	g, h := net.Pipe()
	guest = mux.NewSession(g, ss, mux.Options{})
	host = mux.NewSession(h, ss, mux.Options{})
	t.Cleanup(func() { _ = guest.Close(); _ = host.Close() })
	return guest, host
}

// newTestBridge builds a bare bridge (generation 1, no session) whose app fds
// the test sets directly. The pumps read those fds and re-fetch them across
// generations (app restart).
func newTestBridge() *consoleBridge {
	b := &consoleBridge{appGen: 1}
	b.appCond = sync.NewCond(&b.appMu)
	return b
}

// shutdownBridge marks the bridge shut so a pump parked for the next app
// generation EOFs its stream and exits (the reboot path, minus the wg wait).
func shutdownBridge(b *consoleBridge) {
	b.appMu.Lock()
	b.appClosed = true
	b.appCond.Broadcast()
	b.appMu.Unlock()
}

func TestPumpAppToHost(t *testing.T) {
	ss := mux.PipeStreams(false, true, false) // stdout only
	guest, host := muxPair(t, ss)

	holder := newSessionHolder()
	holder.set(guest)
	pr, pw, err := os.Pipe() // pr = sandbox-init's read end of the app's stdout
	if err != nil {
		t.Fatal(err)
	}
	b := newTestBridge()
	b.stdoutR = pr
	pumped := make(chan struct{})
	go func() { pumpAppToHost(b, roleStdout, holder, mux.StreamStdout); close(pumped) }()

	want := []byte("hello from the app\n")
	go func() {
		_, _ = pw.Write(want)
		_ = pw.Close()    // app writes, then exits → pr EOF → pump parks
		shutdownBridge(b) // sandbox shutdown → parked pump CloseWrites + exits
	}()

	got, err := io.ReadAll(host.Stream(mux.StreamStdout))
	if err != nil {
		t.Fatalf("read host StreamStdout: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("host got %q, want %q", got, want)
	}
	select {
	case <-pumped:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpAppToHost did not exit after app EOF + shutdown")
	}
}

// TestPumpAppToHostRestart verifies the app→host pump survives an in-place
// restart: on the first app instance's EOF it parks (no stream EOF), and after
// a rewire (new generation fd) it resumes streaming the new instance's output.
func TestPumpAppToHostRestart(t *testing.T) {
	ss := mux.PipeStreams(false, true, false)
	guest, host := muxPair(t, ss)
	holder := newSessionHolder()
	holder.set(guest)

	pr1, pw1, _ := os.Pipe()
	b := newTestBridge()
	b.stdoutR = pr1
	pumped := make(chan struct{})
	go func() { pumpAppToHost(b, roleStdout, holder, mux.StreamStdout); close(pumped) }()

	hostStream := host.Stream(mux.StreamStdout)
	read := func(n int) string {
		buf := make([]byte, n)
		_, err := io.ReadFull(hostStream, buf)
		if err != nil {
			t.Fatalf("host read: %v", err)
		}
		return string(buf)
	}

	// Instance 1 writes, then "exits" (close its write end → pr1 EOF → pump parks).
	_, _ = pw1.Write([]byte("aaaa"))
	if got := read(4); got != "aaaa" {
		t.Fatalf("instance 1: got %q", got)
	}
	_ = pw1.Close()

	// Re-wire to a fresh fd (simulating restartApp's bridge.rewireApp) — bump
	// the generation so the parked pump resumes on the new fd.
	pr2, pw2, _ := os.Pipe()
	b.appMu.Lock()
	_ = pr1.Close()
	b.stdoutR = pr2
	b.appGen++
	b.appCond.Broadcast()
	b.appMu.Unlock()

	// Instance 2's output must flow on the SAME stream (no EOF in between).
	_, _ = pw2.Write([]byte("bbbb"))
	if got := read(4); got != "bbbb" {
		t.Fatalf("instance 2 (after restart): got %q", got)
	}

	_ = pw2.Close()
	shutdownBridge(b)
	select {
	case <-pumped:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not exit after shutdown")
	}
}

func TestPumpHostToApp(t *testing.T) {
	ss := mux.PipeStreams(true, false, false) // stdin only
	guest, host := muxPair(t, ss)

	holder := newSessionHolder()
	holder.set(guest)
	pr, pw, err := os.Pipe() // pw = sandbox-init writes the app's stdin; pr = app reads
	if err != nil {
		t.Fatal(err)
	}
	b := newTestBridge()
	b.stdinW = pw
	go pumpHostToApp(b, roleStdin, holder, mux.StreamStdin, true)

	want := []byte("keyboard input\n")
	go func() {
		st := host.Stream(mux.StreamStdin)
		_, _ = st.Write(want)
		_ = st.CloseWrite() // host closed its stdin → app's stdin should hit EOF
	}()

	got, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("app read stdin: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("app got %q, want %q", got, want)
	}
}

func TestSessionHolderReattach(t *testing.T) {
	holder := newSessionHolder()

	// Pump starts with no session — it parks. The app writes anyway; the
	// pump reads the bytes and then blocks in holder.wait() holding them.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	b := newTestBridge()
	b.stdoutR = pr
	pumped := make(chan struct{})
	go func() { pumpAppToHost(b, roleStdout, holder, mux.StreamStdout); close(pumped) }()
	_, _ = pw.Write([]byte("buffered while detached\n"))
	_ = pw.Close()

	// Attach a session — the parked pump must flush the held bytes; shutdown
	// then makes it EOF the stream + exit.
	ss := mux.PipeStreams(false, true, false)
	guest, host := muxPair(t, ss)
	holder.set(guest)
	shutdownBridge(b)

	got, err := io.ReadAll(host.Stream(mux.StreamStdout))
	if err != nil {
		t.Fatalf("read after reattach: %v", err)
	}
	if string(got) != "buffered while detached\n" {
		t.Fatalf("got %q, want %q", got, "buffered while detached\n")
	}
	select {
	case <-pumped:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not exit after reattach + shutdown")
	}
}

func TestSessionHolderShutdownWakesWaiters(t *testing.T) {
	holder := newSessionHolder()
	got := make(chan *mux.Session, 1)
	go func() { got <- holder.wait() }()
	time.Sleep(20 * time.Millisecond) // let the goroutine park in cond.Wait

	holder.shutdown()
	select {
	case s := <-got:
		if s != nil {
			t.Fatalf("wait() after shutdown returned %v, want nil", s)
		}
	case <-time.After(time.Second):
		t.Fatal("wait() did not unblock on shutdown")
	}
	if holder.peek() != nil {
		t.Fatal("peek after shutdown should be nil")
	}
}
