package stdio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"golang.org/x/sys/unix"
)

type outputFailureSink struct {
	writeErr error
	closeErr error
	closed   int
}

func (s *outputFailureSink) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return len(p), nil
}

func (s *outputFailureSink) Close() error {
	s.closed++
	return s.closeErr
}

func TestOutputCompletionRetainsWriteAndCloseErrors(t *testing.T) {
	for _, writeFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("write-fails-%t", writeFails), func(t *testing.T) {
			hc, gc := net.Pipe()
			ss := mux.PipeStreams(false, true, false)
			host := mux.NewSession(hc, ss, mux.Options{})
			guest := mux.NewSession(gc, ss, mux.Options{})
			defer host.Close()
			defer guest.Close()
			closeErr := errors.New("output close failed")
			sink := &outputFailureSink{closeErr: closeErr}
			if writeFails {
				sink.writeErr = errors.New("output write failed")
			}
			finish := copyOutput(host, host.Stream(mux.StreamStdout), sink, sink, "stdout")
			producerDone := make(chan error, 1)
			go func() {
				_, err := io.WriteString(guest.Stream(mux.StreamStdout), "complete output")
				if err == nil {
					err = guest.Stream(mux.StreamStdout).CloseWrite()
				}
				producerDone <- err
			}()
			completed := make(chan error, 1)
			go func() { completed <- finish() }()
			select {
			case err := <-completed:
				if !errors.Is(err, closeErr) || (writeFails && !errors.Is(err, sink.writeErr)) {
					t.Fatalf("completion error = %v", err)
				}
				// Completion is idempotent and retains the first Close error.
				if again := finish(); !errors.Is(again, closeErr) || sink.closed != 1 {
					t.Fatalf("second completion error=%v closes=%d", again, sink.closed)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("output completion did not finish")
			}
			if err := <-producerDone; err != nil && !writeFails {
				t.Fatal(err)
			}
		})
	}
}

func TestBridgePartialOutputSetupFailureDoesNotStartBlockedOutput(t *testing.T) {
	hc, gc := net.Pipe()
	ss := mux.PipeStreams(false, true, true)
	host := mux.NewSession(hc, ss, mux.Options{})
	guest := mux.NewSession(gc, ss, mux.Options{})
	defer host.Close()
	defer guest.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	capacity, err := unix.FcntlInt(w.Fd(), unix.F_GETPIPE_SZ, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Fill the destination and buffer guest output before setup. Starting an
	// output pump before the failing stderr open would block in the sink Write,
	// which closing the MUX cannot interrupt.
	if _, err := w.Write(bytes.Repeat([]byte{'x'}, capacity)); err != nil {
		t.Fatal(err)
	}
	payload := []byte("buffered before bridge setup")
	if _, err := guest.Stream(mux.StreamStdout).Write(payload); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mode := Mode{
		Stdout: Stream{Kind: StreamFile, Path: fmt.Sprintf("/proc/self/fd/%d", w.Fd())},
		Stderr: Stream{Kind: StreamFile, Path: filepath.Join(dir, "missing", "stderr")},
	}
	done := make(chan error, 1)
	go func() {
		_, err := mode.BridgeWithResult(context.Background(), host, ss)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("setup error=%v, want missing stderr directory", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stderr setup failure left the stdout pump blocked")
	}
	buffered := make([]byte, len(payload))
	if n, _ := host.Stream(mux.StreamStdout).Read(buffered); n != len(payload) || !bytes.Equal(buffered, payload) {
		t.Fatalf("output was consumed before setup completed: %q", buffered[:n])
	}
}

func TestBridgeInputCleanupIsNotOutputFailure(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	hc, gc := net.Pipe()
	ss := mux.PipeStreams(true, true, false)
	host := mux.NewSession(hc, ss, mux.Options{})
	guest := mux.NewSession(gc, ss, mux.Options{})
	defer host.Close()
	defer guest.Close()
	out := filepath.Join(t.TempDir(), "stdout")
	mode := Mode{
		Stdin:  Stream{Kind: StreamFile, Path: fmt.Sprintf("/proc/self/fd/%d", r.Fd())},
		Stdout: Stream{Kind: StreamFile, Path: out},
	}
	finish, err := mode.BridgeWithResult(context.Background(), host, ss)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(guest.Stream(mux.StreamStdout), "output"); err != nil {
		t.Fatal(err)
	}
	if err := guest.Stream(mux.StreamStdout).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- finish() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("normal input cancellation became an output failure: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("input cleanup did not join")
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != "output" {
		t.Fatalf("output=%q err=%v", got, err)
	}
	if err := host.Err(); err != nil {
		t.Fatalf("normal stdin reset failed the session: %v", err)
	}
}
