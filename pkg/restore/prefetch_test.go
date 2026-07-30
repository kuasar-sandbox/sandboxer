package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestMemoryPrefetchEligibility(t *testing.T) {
	tests := []struct {
		name       string
		mode       config.PrefetchMode
		stream     fetch.Stream
		wantReason string
	}{
		{name: "default off", mode: config.PrefetchOff, stream: newPrefetchTestStream()},
		{name: "stream without capability", mode: config.PrefetchMemory, stream: plainPrefetchTestStream{}, wantReason: "reason=no_capability"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := newPrefetchTestLogs()
			task := startMemoryPrefetch(context.Background(), tt.mode, tt.stream, 2, logs.logf)
			task.Stop()
			if task != nil {
				t.Fatal("ineligible prefetch started a task")
			}
			joined := logs.joined()
			if tt.wantReason == "" && joined != "" {
				t.Fatalf("disabled prefetch logs = %q, want none", joined)
			}
			if tt.wantReason != "" && (!strings.Contains(joined, "memory prefetch skipped "+tt.wantReason) || !strings.Contains(joined, "parent_layers=2")) {
				t.Fatalf("skip logs = %q, want %q and parent layer count", joined, tt.wantReason)
			}
			if stream, ok := tt.stream.(*prefetchTestStream); ok {
				select {
				case <-stream.started:
					t.Fatal("ineligible prefetch called stream")
				default:
				}
			}
		})
	}
}

func TestRunRejectsInvalidPrefetchBeforeSideEffects(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "run")
	_, err := Run(context.Background(), Options{
		SnapshotPath: "/snapshot-must-not-be-opened",
		HostCfg: &config.SandboxConfig{
			Restore: config.RestoreConfig{Prefetch: "disk"},
		},
		RuntimeRoot: runtimeRoot,
	})
	if err == nil || !strings.Contains(err.Error(), "restore.prefetch") {
		t.Fatalf("Run error = %v, want restore.prefetch validation error", err)
	}
	if _, statErr := os.Stat(runtimeRoot); !os.IsNotExist(statErr) {
		t.Fatalf("runtime root was touched for invalid prefetch: stat error = %v", statErr)
	}
}

func TestMemoryPrefetchStartsTopStream(t *testing.T) {
	top := newPrefetchTestStream()

	task := startMemoryPrefetch(context.Background(), config.PrefetchMemory, top, 3, nil)
	<-top.started
	task.Stop()
}

func TestMemoryPrefetchStopCancelsAndJoinsBeforeClose(t *testing.T) {
	stream := newPrefetchTestStream()
	stream.holdAfterCancel = make(chan struct{})

	task := startMemoryPrefetch(context.Background(), config.PrefetchMemory, stream, 0, nil)
	<-stream.started

	stopReturned := make(chan struct{})
	go func() {
		task.Stop()
		close(stopReturned)
	}()
	<-stream.canceled
	select {
	case <-stopReturned:
		t.Fatal("Stop returned before the prefetch goroutine exited")
	default:
	}
	close(stream.holdAfterCancel)
	<-stopReturned

	if err := stream.Close(); err != nil {
		t.Fatalf("Close after Stop: %v", err)
	}
}

func TestMemoryPrefetchFailureIsFailOpen(t *testing.T) {
	wantErr := errors.New("cache unavailable")
	stream := newPrefetchTestStream()
	stream.result = wantErr
	logs := newPrefetchTestLogs()

	task := startMemoryPrefetch(context.Background(), config.PrefetchMemory, stream, 1, logs.logf)
	<-stream.started
	close(stream.complete)
	<-stream.done
	task.Stop()

	joined := logs.joined()
	if !strings.Contains(joined, wantErr.Error()) || !strings.Contains(joined, "duration=") || !strings.Contains(joined, "restore_continues=on_demand") {
		t.Fatalf("logs = %q, want fail-open diagnostic", joined)
	}
}

func TestMemoryPrefetchNaturalCompletionThenStop(t *testing.T) {
	stream := newPrefetchTestStream()
	logs := newPrefetchTestLogs()
	task := startMemoryPrefetch(context.Background(), config.PrefetchMemory, stream, 4, logs.logf)
	<-stream.started
	close(stream.complete)
	<-stream.done
	task.Stop()

	joined := logs.joined()
	if !strings.Contains(joined, "started mode=memory parent_layers=4") || !strings.Contains(joined, "completed mode=memory parent_layers=4") || !strings.Contains(joined, "duration=") {
		t.Fatalf("logs = %q, want started/completed observability", joined)
	}
	if strings.Contains(joined, "canceled") {
		t.Fatalf("natural completion was misreported as canceled: %q", joined)
	}
}

func TestMemoryPrefetchCancellationLog(t *testing.T) {
	stream := newPrefetchTestStream()
	logs := newPrefetchTestLogs()
	task := startMemoryPrefetch(context.Background(), config.PrefetchMemory, stream, 2, logs.logf)
	<-stream.started
	task.Stop()

	joined := logs.joined()
	if !strings.Contains(joined, "canceled mode=memory parent_layers=2") || !strings.Contains(joined, "duration=") {
		t.Fatalf("logs = %q, want canceled observability", joined)
	}
}

func TestPrefetchTaskStopIsIdempotent(t *testing.T) {
	done := make(chan struct{})
	close(done)
	var cancelCalls atomic.Int32
	task := &prefetchTask{
		cancel: func() { cancelCalls.Add(1) },
		done:   done,
	}

	const callers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			task.Stop()
		}()
	}
	close(start)
	wg.Wait()
	task.Stop()
	if got := cancelCalls.Load(); got != 1 {
		t.Fatalf("cancel calls = %d, want 1", got)
	}
}

type plainPrefetchTestStream struct{}

func (plainPrefetchTestStream) Size() uint64 { return 1 }
func (plainPrefetchTestStream) RunAt(uint64, uint64) (sparse.RunKind, uint64, error) {
	return sparse.Data, 1, nil
}
func (plainPrefetchTestStream) ReadAt(context.Context, []byte, uint64) (int, error) {
	return 0, io.EOF
}
func (plainPrefetchTestStream) Close() error { return nil }

type prefetchTestStream struct {
	started         chan struct{}
	canceled        chan struct{}
	done            chan struct{}
	complete        chan struct{}
	holdAfterCancel chan struct{}
	result          error
	cancelOnce      sync.Once
	doneOnce        sync.Once
}

func newPrefetchTestStream() *prefetchTestStream {
	return &prefetchTestStream{
		started:  make(chan struct{}, 1),
		canceled: make(chan struct{}),
		done:     make(chan struct{}),
		complete: make(chan struct{}),
	}
}

func (s *prefetchTestStream) Size() uint64 { return 1 }
func (s *prefetchTestStream) RunAt(uint64, uint64) (sparse.RunKind, uint64, error) {
	return sparse.Data, 1, nil
}
func (s *prefetchTestStream) ReadAt(context.Context, []byte, uint64) (int, error) {
	return 0, io.EOF
}
func (s *prefetchTestStream) Close() error {
	select {
	case <-s.done:
		return nil
	default:
		return errors.New("stream closed before prefetch exited")
	}
}
func (s *prefetchTestStream) Prefetch(ctx context.Context) error {
	s.started <- struct{}{}
	defer s.doneOnce.Do(func() { close(s.done) })
	select {
	case <-s.complete:
		return s.result
	case <-ctx.Done():
		s.cancelOnce.Do(func() { close(s.canceled) })
		if s.holdAfterCancel != nil {
			<-s.holdAfterCancel
		}
		return ctx.Err()
	}
}

type prefetchTestLogs struct {
	mu    sync.Mutex
	lines []string
}

func newPrefetchTestLogs() *prefetchTestLogs { return &prefetchTestLogs{} }

func (l *prefetchTestLogs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *prefetchTestLogs) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

var _ fetch.Prefetcher = (*prefetchTestStream)(nil)
