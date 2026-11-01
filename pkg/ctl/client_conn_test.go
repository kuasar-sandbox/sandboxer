package ctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestCheckpointReplySurvivesCancellationAfterFinalRead(t *testing.T) {
	for _, kind := range []string{"list", "list-error", "delete-ack"} {
		t.Run(kind, func(t *testing.T) {
			var wire bytes.Buffer
			want := []DiffCheckpointRef{{DiffID: "disk", ID: "version"}}
			switch kind {
			case "list":
				if err := WriteCheckpointList(&wire, want); err != nil {
					t.Fatal(err)
				}
			case "list-error":
				if err := WriteMessage(&wire, Response{Type: TypeError, Code: CheckpointUnsupportedCode, Msg: "disabled"}); err != nil {
					t.Fatal(err)
				}
			case "delete-ack":
				if err := WriteMessage(&wire, Response{Type: TypeCheckpointDeleteDone}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := &contextConn{ctx: ctx, Conn: &cancelOnFinalReadConn{reader: bytes.NewReader(wire.Bytes()), cancel: cancel}}
			if kind == "delete-ack" {
				var reply Response
				if err := ReadMessage(conn, &reply); err != nil || reply.Type != TypeCheckpointDeleteDone {
					t.Fatalf("complete delete ACK lost: %+v, %v", reply, err)
				}
			} else {
				refs, err := ReadCheckpointList(conn)
				if kind == "list-error" {
					if !errors.Is(err, ErrCheckpointUnsupported) {
						t.Fatal("server error replaced by cancellation", err)
					}
				} else if err != nil || !reflect.DeepEqual(refs, want) {
					t.Fatalf("complete list lost: %+v, %v", refs, err)
				}
			}
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("fixture did not cancel before returning final bytes")
			}
		})
	}
}

type cancelOnFinalReadConn struct {
	net.Conn
	reader *bytes.Reader
	cancel context.CancelFunc
}

func (c *cancelOnFinalReadConn) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if c.reader.Len() == 0 {
		c.cancel()
	}
	return n, err
}

func TestContextTransportErrorPreservesCompletedResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	business := errors.New("disk read failed")
	for _, err := range []error{nil, business, ErrCheckpointUnsupported, io.EOF, io.ErrUnexpectedEOF} {
		if got := contextTransportError(ctx, err); got != err {
			t.Fatalf("completed/unrelated result %v replaced by %v", err, got)
		}
	}
	closed := fmt.Errorf("checkpoint list incomplete: %w", &net.OpError{Op: "read", Net: "unix", Err: net.ErrClosed})
	if got := contextTransportError(ctx, closed); !errors.Is(got, context.Canceled) {
		t.Fatal("context-triggered close was not classified", got)
	}
	if got := contextTransportError(context.Background(), closed); got != closed {
		t.Fatal("unrelated connection close was replaced", got)
	}
}

func TestContextTransportErrorHandlesSocketDeadlineBeforeContextTimer(t *testing.T) {
	// A socket can observe the shared deadline before the context timer runs.
	ctx := deadlineOnlyContext{Context: context.Background(), deadline: time.Now().Add(-time.Second)}
	timeout := &net.OpError{Op: "read", Net: "unix", Err: os.ErrDeadlineExceeded}
	if got := contextTransportError(ctx, timeout); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatal("socket deadline was not classified", got)
	}
	if got := contextTransportError(context.Background(), timeout); got != timeout {
		t.Fatal("unrelated timeout was replaced", got)
	}
}

type deadlineOnlyContext struct {
	context.Context
	deadline time.Time
}

func (c deadlineOnlyContext) Deadline() (time.Time, bool) { return c.deadline, true }
