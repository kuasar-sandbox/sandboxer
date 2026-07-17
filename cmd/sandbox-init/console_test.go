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
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
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
func newTestBridge(outputRoles ...appRole) *consoleBridge {
	var mask appDrainMask
	for _, role := range outputRoles {
		mask |= drainMaskForRole(role)
	}
	b := &consoleBridge{appGen: 1, appDrain: newAppGenerationDrain(mask), appDone: make(chan struct{})}
	b.appCond = sync.NewCond(&b.appMu)
	return b
}

// shutdownBridge marks the bridge shut so a pump parked for the next app
// generation EOFs its stream and exits (the reboot path, minus the wg wait).
func shutdownBridge(b *consoleBridge) {
	b.appMu.Lock()
	b.closeAppLocked()
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
	b := newTestBridge(roleStdout)
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

func TestAppExitedDrainsTrailingOutput(t *testing.T) {
	ss := mux.PipeStreams(false, true, false)
	guest, host := muxPair(t, ss)
	holder := newSessionHolder()
	holder.set(guest)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	b := newTestBridge(roleStdout)
	b.holder = holder
	b.stdoutR = pr

	startPump := make(chan struct{})
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		<-startPump
		pumpAppToHost(b, roleStdout, holder, mux.StreamStdout)
	}()

	want := []byte("trailing output before app exit\n")
	if _, err := pw.Write(want); err != nil {
		t.Fatalf("write app stdout: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("close app stdout: %v", err)
	}

	exited := make(chan struct{})
	go func() {
		b.appExited()
		close(exited)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		b.appMu.Lock()
		closed := b.appClosed
		b.appMu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("appExited did not mark the bridge closed")
		}
		time.Sleep(time.Millisecond)
	}
	close(startPump)

	got, err := io.ReadAll(host.Stream(mux.StreamStdout))
	if err != nil {
		t.Fatalf("read host StreamStdout: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("host got %q, want %q", got, want)
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("appExited did not finish after draining output")
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
	b := newTestBridge(roleStdout)
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
	select {
	case <-b.appDrain.done:
	case <-time.After(time.Second):
		t.Fatal("instance 1 output did not drain")
	}

	// Re-wire to a fresh fd (simulating restartApp's bridge.rewireApp) — bump
	// the generation so the parked pump resumes on the new fd.
	pr2, pw2, _ := os.Pipe()
	b.appMu.Lock()
	closeAppEnds(b.endsLocked())
	b.setEndsLocked(appEnds{stdoutR: pr2}, newAppGenerationDrain(drainStdout))
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
	b := newTestBridge(roleStdout)
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

type rewireResult struct {
	stdio childStdio
	err   error
}

type readResult struct {
	data []byte
	err  error
}

func readAllAsync(r io.Reader) <-chan readResult {
	done := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(r)
		done <- readResult{data: data, err: err}
	}()
	return done
}

func awaitRead(t *testing.T, done <-chan readResult) []byte {
	t.Helper()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("read stream: %v", res.err)
		}
		return res.data
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading stream")
		return nil
	}
}

func awaitRewire(t *testing.T, done <-chan rewireResult) childStdio {
	t.Helper()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("rewireApp: %v", res.err)
		}
		return res.stdio
	case <-time.After(2 * time.Second):
		t.Fatal("rewireApp did not finish")
		return childStdio{}
	}
}

func assertRewireBlocked(t *testing.T, done <-chan rewireResult) {
	t.Helper()
	select {
	case res := <-done:
		closeChildStdio(res.stdio)
		t.Fatalf("rewireApp returned before the previous generation drained: %v", res.err)
	case <-time.After(25 * time.Millisecond):
	}
}

func waitOutputPumps(t *testing.T, b *consoleBridge) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("app-to-host pumps did not exit")
	}
}

func TestRewireAppDrainsPipeGeneration(t *testing.T) {
	spec := proto.StdioSpec{Stdout: true, Stderr: true}
	cs1, b, err := setupAppStdio(spec)
	if err != nil {
		t.Fatal(err)
	}
	guest, host := muxPair(t, streamSetFor(spec))
	b.attach(guest)
	outRead := readAllAsync(host.Stream(mux.StreamStdout))
	errRead := readAllAsync(host.Stream(mux.StreamStderr))

	if _, err := cs1.stdout.Write([]byte("old-stdout")); err != nil {
		t.Fatalf("write old stdout: %v", err)
	}
	if _, err := cs1.stderr.Write([]byte("old-stderr")); err != nil {
		t.Fatalf("write old stderr: %v", err)
	}
	closeChildStdio(cs1)

	rewired := make(chan rewireResult, 1)
	go func() {
		cs, err := b.rewireAppWithTimeout(spec, time.Second)
		rewired <- rewireResult{stdio: cs, err: err}
	}()
	assertRewireBlocked(t, rewired)

	b.start()
	cs2 := awaitRewire(t, rewired)
	if _, err := cs2.stdout.Write([]byte("new-stdout")); err != nil {
		t.Fatalf("write new stdout: %v", err)
	}
	if _, err := cs2.stderr.Write([]byte("new-stderr")); err != nil {
		t.Fatalf("write new stderr: %v", err)
	}
	closeChildStdio(cs2)
	shutdownBridge(b)

	if got, want := string(awaitRead(t, outRead)), "old-stdoutnew-stdout"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := string(awaitRead(t, errRead)), "old-stderrnew-stderr"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	waitOutputPumps(t, b)
}

func TestRewireAppDrainsPTYGeneration(t *testing.T) {
	spec := proto.StdioSpec{TTY: true}
	cs1, b, err := setupAppStdio(spec)
	if err != nil {
		t.Fatal(err)
	}
	guest, host := muxPair(t, streamSetFor(spec))
	b.attach(guest)
	ptyRead := readAllAsync(host.Stream(mux.StreamPTY))

	if _, err := cs1.stdout.Write([]byte("old-pty")); err != nil {
		t.Fatalf("write old PTY: %v", err)
	}
	closeChildStdio(cs1)

	rewired := make(chan rewireResult, 1)
	go func() {
		cs, err := b.rewireAppWithTimeout(spec, time.Second)
		rewired <- rewireResult{stdio: cs, err: err}
	}()
	assertRewireBlocked(t, rewired)

	b.start()
	cs2 := awaitRewire(t, rewired)
	if _, err := cs2.stdout.Write([]byte("new-pty")); err != nil {
		t.Fatalf("write new PTY: %v", err)
	}
	closeChildStdio(cs2)
	shutdownBridge(b)

	if got, want := string(awaitRead(t, ptyRead)), "old-ptynew-pty"; got != want {
		t.Fatalf("PTY = %q, want %q", got, want)
	}
	waitOutputPumps(t, b)
}

func TestRewireAppForcesStuckGenerationAfterTimeout(t *testing.T) {
	spec := proto.StdioSpec{Stdout: true}
	cs1, b, err := setupAppStdio(spec)
	if err != nil {
		t.Fatal(err)
	}
	guest, host := muxPair(t, streamSetFor(spec))
	b.attach(guest)
	b.start()
	outRead := readAllAsync(host.Stream(mux.StreamStdout))

	const drainTimeout = 50 * time.Millisecond
	started := time.Now()
	rewired := make(chan rewireResult, 1)
	go func() {
		cs, err := b.rewireAppWithTimeout(spec, drainTimeout)
		rewired <- rewireResult{stdio: cs, err: err}
	}()
	cs2 := awaitRewire(t, rewired)
	if elapsed := time.Since(started); elapsed < drainTimeout {
		t.Fatalf("rewireApp returned in %s, before drain timeout %s", elapsed, drainTimeout)
	}
	closeChildStdio(cs1)

	if _, err := cs2.stdout.Write([]byte("new-after-timeout")); err != nil {
		t.Fatalf("write new stdout: %v", err)
	}
	closeChildStdio(cs2)
	shutdownBridge(b)

	if got, want := string(awaitRead(t, outRead)), "new-after-timeout"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	waitOutputPumps(t, b)
}

func TestRewireAppStopsOnBridgeShutdown(t *testing.T) {
	spec := proto.StdioSpec{Stdout: true}
	cs1, b, err := setupAppStdio(spec)
	if err != nil {
		t.Fatal(err)
	}

	rewired := make(chan rewireResult, 1)
	go func() {
		cs, err := b.rewireAppWithTimeout(spec, time.Second)
		rewired <- rewireResult{stdio: cs, err: err}
	}()
	assertRewireBlocked(t, rewired)

	started := time.Now()
	shutdownBridge(b)
	select {
	case res := <-rewired:
		closeChildStdio(res.stdio)
		if res.err == nil {
			t.Fatal("rewireApp succeeded after bridge shutdown")
		}
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("rewireApp took %s to observe bridge shutdown", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("rewireApp did not stop on bridge shutdown")
	}
	closeChildStdio(cs1)
}
