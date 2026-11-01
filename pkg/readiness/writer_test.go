package readiness_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/readiness"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
)

func TestWriterExactSequenceAndDuplicates(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	n := readiness.NewWriter(w, nil)
	n.Notify(sandbox.ReadinessRuntimeReady)
	n.Notify(sandbox.ReadinessControlReady)
	n.Notify(sandbox.ReadinessRuntimeReady)
	n.Notify(sandbox.ReadinessControlReady)
	n.Notify(sandbox.ReadinessReady)
	n.Notify(sandbox.ReadinessReady)
	if err := n.Close(); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "control_ready\nready\n"; got != want {
		t.Fatalf("readiness wire = %q, want %q", got, want)
	}
}

func TestWriterInvalidEventFailsClosed(t *testing.T) {
	for _, event := range []sandbox.ReadinessEvent{sandbox.ReadinessReady, "unknown"} {
		t.Run(string(event), func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			var logs bytes.Buffer
			n := readiness.NewWriter(w, func(format string, args ...any) {
				fmt.Fprintf(&logs, format+"\n", args...)
			})
			n.Notify(event)
			n.Notify(sandbox.ReadinessControlReady)
			body, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if len(body) != 0 {
				t.Fatalf("readiness wire = %q, want empty EOF", body)
			}
			if got := strings.Count(logs.String(), "invariant violation"); got != 1 {
				t.Fatalf("invariant log count = %d, logs=%q", got, logs.String())
			}
		})
	}
}

func TestWriterEPIPEDisablesWithoutFailure(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	n := readiness.NewWriter(w, func(format string, args ...any) {
		fmt.Fprintf(&logs, format+"\n", args...)
	})
	n.Notify(sandbox.ReadinessControlReady)
	n.Notify(sandbox.ReadinessReady)
	if got := strings.Count(logs.String(), "reader closed early"); got != 1 {
		t.Fatalf("EPIPE log count = %d, logs=%q", got, logs.String())
	}
	if err := n.Close(); err != nil {
		t.Fatalf("Close after EPIPE: %v", err)
	}
}

func TestWriterConcurrentNotifyAndClose(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	n := readiness.NewWriter(w, nil)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); n.Notify(sandbox.ReadinessControlReady) }()
		go func() { defer wg.Done(); n.Notify(sandbox.ReadinessReady) }()
		go func() { defer wg.Done(); _ = n.Close() }()
	}
	wg.Wait()
	_ = n.Close()
	body, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "" && got != "control_ready\n" && got != "control_ready\nready\n" {
		t.Fatalf("invalid concurrent readiness wire %q", got)
	}
}

type shortWriteCloser struct {
	bytes.Buffer
	max    int
	closes int
}

func (w *shortWriteCloser) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.Buffer.Write(p)
}

func (w *shortWriteCloser) Close() error { w.closes++; return nil }

func TestWriterUsesFullWriteLoop(t *testing.T) {
	w := &shortWriteCloser{max: 2}
	n := readiness.NewWriter(w, nil)
	n.Notify(sandbox.ReadinessControlReady)
	n.Notify(sandbox.ReadinessReady)
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := w.String(), "control_ready\nready\n"; got != want {
		t.Fatalf("readiness wire = %q, want %q", got, want)
	}
	if w.closes != 1 {
		t.Fatalf("output closed %d times, want exactly once", w.closes)
	}
}

func TestWriterDisabled(t *testing.T) {
	for _, w := range []*readiness.Writer{nil, {}, readiness.NewWriter(nil, nil)} {
		w.Notify(sandbox.ReadinessControlReady)
		w.Notify(sandbox.ReadinessReady)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		w.Notify("unknown")
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
