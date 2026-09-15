package readretry

import (
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
)

func TestReadSequence(t *testing.T) {
	for _, accessErr := range []error{
		errors.New("unmarked access failure"),
		readerr.Mark(io.ErrUnexpectedEOF, true),
		context.DeadlineExceeded, context.Canceled,
	} {
		t.Run(accessErr.Error(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			attempts := 0
			buf := []byte("original")
			err := Do(ctx, func() error {
				attempts++
				if attempts <= 2 {
					copy(buf, "partial!")
					return accessErr
				}
				copy(buf, "complete")
				return nil
			})
			if err != nil || attempts != 3 || string(buf) != "complete" {
				t.Fatalf("attempts=%d buffer=%q err=%v", attempts, buf, err)
			}
		})
	}
}

func TestTerminalCauses(t *testing.T) {
	for _, cause := range []error{io.EOF, syscall.EAGAIN, errors.New("corrupt index")} {
		attempts := 0
		err := Do(context.Background(), func() error {
			attempts++
			return errors.Join(context.Canceled, readerr.Mark(cause, false))
		})
		if attempts != 1 || !IsTerminal(err) || !errors.Is(err, cause) {
			t.Fatalf("attempts=%d, lost terminal cause: %v", attempts, err)
		}
	}
}

func TestOwnCancellationStopsBackoffAndHasPriority(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("operation stopped")
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Do(ctx, func() error {
			close(entered)
			return errors.New("temporary failure")
		})
	}()
	<-entered
	cancel(cause)
	select {
	case err := <-done:
		if !IsTerminal(err) || !errors.Is(err, cause) {
			t.Fatalf("lost operation cause: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("backoff did not stop")
	}
	if err := Do(ctx, func() error { t.Fatal("read after cancellation"); return nil }); !errors.Is(err, cause) {
		t.Fatalf("lost initial cancellation: %v", err)
	}
}

func TestCancellationDuringSuccessfulAttemptDoesNotComplete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	err := Do(ctx, func() error { cancel(); return nil })
	if !IsTerminal(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("completed stopped operation: %v", err)
	}
}

func BenchmarkHealthyRead(b *testing.B) {
	ctx := context.Background()
	for _, retry := range []bool{false, true} {
		name := "direct"
		if retry {
			name = "retry"
		}
		b.Run(name, func(b *testing.B) {
			buf := make([]byte, 4096)
			read := func() error { clear(buf); return nil }
			b.ReportAllocs()
			b.SetBytes(int64(len(buf)))
			for b.Loop() {
				if retry {
					_ = Do(ctx, read)
				} else {
					_ = read()
				}
			}
		})
	}
}

func TestCompleteReadDoesNotSwallowMarkedOrJoinedEOF(t *testing.T) {
	for _, cause := range []error{readerr.Mark(io.EOF, true), errors.Join(io.EOF, io.ErrClosedPipe)} {
		calls := 0
		n, err := ReadAt(context.Background(), 4, func() (int, error) {
			calls++
			if calls == 1 {
				return 4, cause
			}
			return 4, io.EOF
		})
		if n != 4 || err != nil || calls != 2 {
			t.Fatalf("full failure swallowed: n=%d err=%v calls=%d", n, err, calls)
		}
	}
}
