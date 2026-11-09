package journalio_test

import (
	"bytes"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/journalio"
)

func TestOpenLeavesLoggerAndFallbackOwnershipWithCaller(t *testing.T) {
	previous := log.Writer()
	fallback := &fallbackSink{}
	writer, err := journalio.Open("journald=runtime,WORKLOAD_ID=w-1", fallback)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if log.Writer() != previous {
		t.Fatal("Open changed the process logger")
	}
	// No payload is written, so native journal availability is irrelevant.
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if log.Writer() != previous || fallback.closed {
		t.Fatal("Close changed the process logger or closed the borrowed fallback")
	}
	if _, err := writer.Write([]byte("after close")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("closed output accepted bytes", err)
	}
	if _, err := fallback.Write([]byte("still owned by caller")); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRequiresExplicitJournalTarget(t *testing.T) {
	for _, target := range []string{"", "default", "file=diagnostics.log"} {
		if writer, err := journalio.Open(target, nil); err == nil || writer != nil {
			t.Fatalf("Open(%q) = %v, %v; want nil writer and invalid target", target, writer, err)
		}
	}
}

func TestOpenDiagnosticOwnsOnlyItsOutput(t *testing.T) {
	previous := log.Writer()
	writer, err := journalio.OpenDiagnostic("journald=runtime,WORKLOAD_ID=w-1")
	if err != nil {
		t.Fatal(err)
	}
	if log.Writer() != previous {
		t.Fatal("OpenDiagnostic changed the process logger")
	}
	// Construction and an empty Close do not need a running journal.
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("after close")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("closed diagnostic output accepted bytes", err)
	}
	for _, target := range []string{"", "default", "file=diagnostics.log", "journald=ctl,MESSAGE=x"} {
		if writer, err := journalio.OpenDiagnostic(target); err == nil || writer != nil {
			t.Fatalf("OpenDiagnostic(%q) = %v, %v; want invalid target", target, writer, err)
		}
	}
}

type fallbackSink struct {
	bytes.Buffer
	closed bool
}

func (s *fallbackSink) Close() error { s.closed = true; return nil }
