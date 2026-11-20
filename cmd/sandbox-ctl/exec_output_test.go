package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
	"golang.org/x/sys/unix"
)

var execOutputErrUpdateWrite = errors.New("probe: injected WINDOW_UPDATE transport failure")

func execOutputDeadline(t *testing.T, c net.Conn) {
	t.Helper()
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
}

func execOutputWaitCode(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("exec did not finish")
		return -1
	}
}

func execOutputWaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
}

func execOutputArtifact(t *testing.T, payloadBytes int) []byte {
	t.Helper()
	payload := make([]byte, payloadBytes)
	for i := range payload {
		payload[i] = byte(1 + i%251)
	}
	src, err := sparse.NewSource(bytes.NewReader(payload), uint64(len(payload)), nil)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if _, _, err := tarstream.WriteTo(context.Background(), &wire, "image", src); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tarstream.SourceAt(bytes.NewReader(wire.Bytes()), int64(wire.Len()), ""); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	return wire.Bytes()
}

func execOutputStartExec(t *testing.T, host net.Conn, guest net.Conn, mode stdio.Mode) (*mux.Session, <-chan int) {
	t.Helper()
	execOutputDeadline(t, host)
	execOutputDeadline(t, guest)
	done := make(chan int, 1)
	go func() {
		done <- runExecOverConn(host, proto.ExecSpec{
			Argv: []string{"probe-producer"}, Stdio: mode.ProtoSpec(),
		}, mode)
	}()
	var request ctl.Request
	if err := ctl.ReadMessage(guest, &request); err != nil {
		t.Fatal(err)
	}
	if request.Type != ctl.TypeExecRequest || request.Exec == nil {
		t.Fatalf("unexpected request: %+v", request)
	}
	if err := ctl.WriteMessage(guest, &ctl.Response{Type: ctl.TypeExecAck, Stdio: &request.Exec.Stdio}); err != nil {
		t.Fatal(err)
	}
	sess := mux.NewSession(guest, mode.StreamSet(), mux.Options{})
	t.Cleanup(func() { _ = sess.Close(); _ = host.Close() })
	return sess, done
}

func execOutputSendOutput(t *testing.T, guest *mux.Session, payload []byte) {
	t.Helper()
	if n, err := guest.Stream(mux.StreamStdout).Write(payload); err != nil || n != len(payload) {
		t.Fatalf("guest write %d/%d: %v", n, len(payload), err)
	}
	if err := guest.Stream(mux.StreamStdout).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := guest.SendExitStatus(0); err != nil {
		t.Fatal(err)
	}
}

func execOutputFinishGuest(t *testing.T, guest *mux.Session) {
	t.Helper()
	if err := guest.InitMuxClose(); err != nil {
		t.Fatalf("graceful MUX close: %v", err)
	}
}

func TestRunExecLargeArtifactCompletesNormally(t *testing.T) {
	wire := execOutputArtifact(t, 2<<20)
	out := filepath.Join(t.TempDir(), "image.img")
	mode := stdio.Mode{Stdout: stdio.Stream{Kind: stdio.StreamFile, Path: out}}
	hc, gc := net.Pipe()
	guest, done := execOutputStartExec(t, hc, gc, mode)
	execOutputSendOutput(t, guest, wire)
	execOutputFinishGuest(t, guest)
	code := execOutputWaitCode(t, done)
	got, err := os.ReadFile(out)
	if err != nil || code != 0 || !bytes.Equal(got, wire) {
		t.Fatalf("exec code=%d read=%v bytes=%d/%d", code, err, len(got), len(wire))
	}
	t.Logf("normal exec artifact: bytes=%d exit=%d", len(got), code)
}

// execOutputPipeMode makes the real Mode.Bridge open an owned pipe descriptor as its
// ordinary file output. The small pipe holds the copy goroutine until the test
// starts a reader; no global os.Stdout mutation or guest drain timer is used.
func execOutputPipeMode(t *testing.T) (stdio.Mode, *os.File, *os.File, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
	capacity, err := unix.FcntlInt(w.Fd(), unix.F_SETPIPE_SZ, 4096)
	if err != nil {
		t.Fatal(err)
	}
	mode := stdio.Mode{Stdout: stdio.Stream{Kind: stdio.StreamFile, Path: fmt.Sprintf("/proc/self/fd/%d", w.Fd())}}
	// Linux rounds pipe capacity to its page size, which can exceed 4 KiB on
	// ARM. Full copy chunks can enter the pipe before the next Write blocks.
	return mode, r, w, capacity / mux.DataChunk * mux.DataChunk
}

func execOutputReadPipe(r *os.File) <-chan []byte {
	done := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(r); done <- b }()
	return done
}

func execOutputCollectPipe(t *testing.T, done <-chan []byte) []byte {
	t.Helper()
	select {
	case b := <-done:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("output pipe did not close")
		return nil
	}
}

func TestRunExecGracefulCloseWithPausedDestinationDrainsAll(t *testing.T) {
	mode, r, w, buffered := execOutputPipeMode(t)
	wire := execOutputArtifact(t, buffered+90*1024)
	if len(wire) > buffered+mux.DefaultWindow+mux.DataChunk {
		t.Fatalf("fixture %d exceeds guaranteed credit while the destination is paused", len(wire))
	}
	hc, gc := net.Pipe()
	guest, done := execOutputStartExec(t, hc, gc, mode)
	execOutputSendOutput(t, guest, wire)
	execOutputFinishGuest(t, guest)
	select {
	case code := <-done:
		t.Fatalf("exec returned %d before its destination was drained", code)
	default:
	}
	gotCh := execOutputReadPipe(r)
	code := execOutputWaitCode(t, done)
	_ = w.Close()
	got := execOutputCollectPipe(t, gotCh)
	if code != 0 || !bytes.Equal(got, wire) {
		t.Fatalf("graceful paused drain code=%d bytes=%d/%d", code, len(got), len(wire))
	}
	t.Logf("actual exec, destination paused until after graceful close: bytes=%d exit=%d", len(got), code)
}

func TestRunExecRejectsDestinationWriteFailure(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	n, writeErr := full.Write([]byte("probe"))
	_ = full.Close()
	if n != 0 || !errors.Is(writeErr, unix.ENOSPC) {
		t.Fatalf("/dev/full precondition: n=%d err=%v", n, writeErr)
	}
	mode := stdio.Mode{Stdout: stdio.Stream{Kind: stdio.StreamFile, Path: "/dev/full"}}
	hc, gc := net.Pipe()
	guest, done := execOutputStartExec(t, hc, gc, mode)
	// A failed destination must end the session even when the guest has more
	// output than the available stream credit and cannot reach EXIT_STATUS.
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		_, _ = guest.Stream(mux.StreamStdout).Write(bytes.Repeat([]byte{0xab}, 4*mux.DefaultWindow))
	}()
	if code := execOutputWaitCode(t, done); code != 1 {
		t.Fatalf("exec accepted failed destination: exit=%d", code)
	}
	execOutputWaitSignal(t, producerDone, "blocked guest output to stop")
}

type execOutputFailWindowConn struct {
	net.Conn
	armed  atomic.Bool
	failed atomic.Int32
}

func (c *execOutputFailWindowConn) Write(p []byte) (int, error) {
	if c.armed.Load() && len(p) == mux.FrameHeaderLen && p[0] == mux.StreamStdout && p[1] == mux.FrameWindowUpdate {
		c.failed.Add(1)
		return 0, execOutputErrUpdateWrite
	}
	return c.Conn.Write(p)
}

// Observe the real ctl ACK followed by MUX frames. The channel closes at the
// start of the Read after a complete EXIT_STATUS frame: dispatch of that frame
// has finished, so the real exec path already owns exit 0 before fault injection.
type execOutputExitObservedConn struct {
	*execOutputFailWindowConn
	ackDone           bool
	buf               []byte
	exitFrameComplete bool
	afterExitErr      error
	exitProcessed     chan struct{}
	once              sync.Once
}

func (c *execOutputExitObservedConn) Read(p []byte) (int, error) {
	if c.exitFrameComplete {
		c.once.Do(func() { close(c.exitProcessed) })
		if c.afterExitErr != nil {
			return 0, c.afterExitErr
		}
	}
	n, err := c.Conn.Read(p)
	c.buf = append(c.buf, p[:n]...)
	if !c.ackDone {
		if len(c.buf) < 4 {
			return n, err
		}
		size := int(binary.LittleEndian.Uint32(c.buf[:4]))
		if len(c.buf) < 4+size {
			return n, err
		}
		c.buf = c.buf[4+size:]
		c.ackDone = true
	}
	for len(c.buf) >= mux.FrameHeaderLen {
		size := int(binary.BigEndian.Uint16(c.buf[2:4]))
		if len(c.buf) < mux.FrameHeaderLen+size {
			break
		}
		if c.buf[0] == mux.StreamControl && c.buf[1] == mux.FrameExitStatus {
			c.exitFrameComplete = true
		}
		c.buf = c.buf[mux.FrameHeaderLen+size:]
	}
	return n, err
}

func TestRunExecRejectsTruncatedOutputAfterWindowUpdateFailure(t *testing.T) {
	mode, r, w, buffered := execOutputPipeMode(t)
	wire := execOutputArtifact(t, buffered+90*1024)
	if len(wire) > buffered+mux.DefaultWindow+mux.DataChunk {
		t.Fatal("fixture exceeds paused credit")
	}
	hc, gc := net.Pipe()
	fault := &execOutputFailWindowConn{Conn: hc}
	observed := &execOutputExitObservedConn{execOutputFailWindowConn: fault, exitProcessed: make(chan struct{})}
	guest, done := execOutputStartExec(t, observed, gc, mode)
	execOutputSendOutput(t, guest, wire)
	execOutputWaitSignal(t, observed.exitProcessed, "processed EXIT_STATUS")
	// Deliberately do not ask the guest to close: only the next WINDOW_UPDATE
	// fails. There is no drain timer, early guest Close, or destination error.
	fault.armed.Store(true)
	gotCh := execOutputReadPipe(r)
	code := execOutputWaitCode(t, done)
	_ = w.Close()
	got := execOutputCollectPipe(t, gotCh)
	_, _, parseErr := tarstream.SourceAt(bytes.NewReader(got), int64(len(got)), "")
	if fault.failed.Load() != 1 || code != 1 || len(got) >= len(wire) || parseErr == nil || !strings.Contains(parseErr.Error(), "invalid payload bounds") {
		t.Fatalf("failed output was not rejected: failures=%d code=%d bytes=%d/%d parseErr=%v", fault.failed.Load(), code, len(got), len(wire), parseErr)
	}
	t.Logf("exec rejected WINDOW_UPDATE error with exit=%d; emitted=%d received=%d lost=%d; exact artifact error=%v", code, len(wire), len(got), len(wire)-len(got), parseErr)
}

// An error after EXIT_STATUS can be unrelated to any output pump (for
// example, waiting for the final MUX_CLOSE frame). Exec must inspect Session.Err.
func TestRunExecRejectsSessionFailureAfterExitStatus(t *testing.T) {
	for _, guestCode := range []int{0, 37} {
		t.Run(fmt.Sprintf("guest-exit-%d", guestCode), func(t *testing.T) {
			hc, gc := net.Pipe()
			observed := &execOutputExitObservedConn{
				execOutputFailWindowConn: &execOutputFailWindowConn{Conn: hc},
				exitProcessed:            make(chan struct{}),
				afterExitErr:             errors.New("injected failure waiting for MUX_CLOSE"),
			}
			guest, done := execOutputStartExec(t, observed, gc, stdio.Mode{})
			if err := guest.SendExitStatus(guestCode); err != nil {
				t.Fatal(err)
			}
			execOutputWaitSignal(t, observed.exitProcessed, "processed EXIT_STATUS")
			want := guestCode
			if want == 0 {
				want = 1
			}
			if code := execOutputWaitCode(t, done); code != want {
				t.Fatalf("exec exit=%d, want %d after session error", code, want)
			}
		})
	}
}
