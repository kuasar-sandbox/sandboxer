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

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestMemoryPrefetchDisabledIsSilent(t *testing.T) {
	manifestKey := strings.Repeat("a", 64)
	for _, rawMode := range []string{"", "off"} {
		mode, err := config.ParsePrefetchMode(rawMode)
		if err != nil {
			t.Fatal(err)
		}
		for _, backend := range []struct {
			name string
			key  string
		}{
			{name: "file"},
			{name: "manifest", key: manifestKey},
		} {
			t.Run(rawMode+"/"+backend.name, func(t *testing.T) {
				stream := newPrefetchTestStream()
				logs := newPrefetchTestLogs()
				task := startMemoryPrefetch(context.Background(), mode, backend.key, stream, 2, logs.logf)
				task.Stop()
				if task != nil {
					t.Fatal("disabled prefetch started a task")
				}
				if got := stream.calls.Load(); got != 0 {
					t.Fatalf("disabled prefetch calls = %d; want 0", got)
				}
				if joined := logs.joined(); joined != "" {
					t.Fatalf("disabled prefetch logs = %q; want none", joined)
				}
			})
		}
	}
}

func TestMemoryPrefetchNoCapabilityIsFailOpen(t *testing.T) {
	manifestKey := strings.Repeat("b", 64)
	for _, backend := range []struct {
		name string
		key  string
	}{
		{name: "file"},
		{name: "manifest", key: manifestKey},
	} {
		t.Run(backend.name, func(t *testing.T) {
			logs := newPrefetchTestLogs()
			task := startMemoryPrefetch(
				context.Background(),
				config.PrefetchMemory,
				backend.key,
				plainPrefetchTestStream{},
				2,
				logs.logf,
			)
			task.Stop()
			if task != nil {
				t.Fatal("stream without Prefetcher capability started a task")
			}
			joined := logs.joined()
			if !strings.Contains(joined, "memory prefetch skipped reason=no_capability backend="+backend.name+" parent_layers=2") {
				t.Fatalf("skip logs = %q; want backend and parent layer count", joined)
			}
			if backend.key != "" && !strings.Contains(joined, "key="+backend.key) {
				t.Fatalf("manifest skip logs = %q; want key", joined)
			}
		})
	}
}

func TestCompositeManifestRefRejectedBeforePrefetch(t *testing.T) {
	key := strings.Repeat("c", 64)
	if _, err := manifest.ParseRef("manifest://" + key + ":" + key); err == nil {
		t.Fatal("canonical parser accepted a composite manifest ref")
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

func TestMemoryPrefetchUsesCurrentSelfWithoutSelector(t *testing.T) {
	manifestKey := strings.Repeat("d", 64)
	for _, backend := range []struct {
		name       string
		key        string
		completion string
	}{
		{name: "file", completion: "advised"},
		{name: "manifest", key: manifestKey, completion: "completed"},
	} {
		t.Run(backend.name, func(t *testing.T) {
			self := newPrefetchTestStream()
			parents := []*prefetchTestStream{newPrefetchTestStream(), newPrefetchTestStream()}
			disks := []*prefetchTestStream{newPrefetchTestStream(), newPrefetchTestStream(), newPrefetchTestStream()}
			logs := newPrefetchTestLogs()

			task := startMemoryPrefetch(
				context.Background(),
				config.PrefetchMemory,
				backend.key,
				self,
				len(parents),
				logs.logf,
			)
			<-self.started
			close(self.complete)
			<-self.done
			task.Stop()

			if got := self.calls.Load(); got != 1 {
				t.Fatalf("current self Prefetch calls = %d; want 1", got)
			}
			for i, stream := range append(parents, disks...) {
				if got := stream.calls.Load(); got != 0 {
					t.Fatalf("non-self stream %d Prefetch calls = %d; want 0", i, got)
				}
			}

			identity := "backend=" + backend.name + " parent_layers=2"
			if backend.key != "" {
				identity += " key=" + backend.key
			}
			joined := logs.joined()
			if !strings.Contains(joined, "memory prefetch started "+identity) ||
				!strings.Contains(joined, "memory prefetch "+backend.completion+" "+identity+" duration=") {
				t.Fatalf("logs = %q; want started/%s for current %s self", joined, backend.completion, backend.name)
			}
		})
	}
}

func TestMemoryPrefetchStopCancelsAndJoinsBeforeClose(t *testing.T) {
	stream := newPrefetchTestStream()
	stream.holdAfterCancel = make(chan struct{})

	task := startMemoryPrefetch(context.Background(), config.PrefetchMemory, "", stream, 0, nil)
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
	manifestKey := strings.Repeat("e", 64)
	for _, backend := range []struct {
		name string
		key  string
	}{
		{name: "file"},
		{name: "manifest", key: manifestKey},
	} {
		t.Run(backend.name, func(t *testing.T) {
			stream := newPrefetchTestStream()
			stream.result = wantErr
			logs := newPrefetchTestLogs()

			task := startMemoryPrefetch(
				context.Background(),
				config.PrefetchMemory,
				backend.key,
				stream,
				1,
				logs.logf,
			)
			<-stream.started
			close(stream.complete)
			<-stream.done
			task.Stop()

			joined := logs.joined()
			if !strings.Contains(joined, "memory prefetch failed backend="+backend.name+" parent_layers=1") ||
				!strings.Contains(joined, wantErr.Error()) ||
				!strings.Contains(joined, "duration=") ||
				!strings.Contains(joined, "restore_continues=on_demand") {
				t.Fatalf("logs = %q; want fail-open %s diagnostic", joined, backend.name)
			}
			if backend.key != "" && !strings.Contains(joined, "key="+backend.key) {
				t.Fatalf("manifest failure logs = %q; want key", joined)
			}
		})
	}
}

func TestMemoryPrefetchNaturalCompletionThenStop(t *testing.T) {
	stream := newPrefetchTestStream()
	logs := newPrefetchTestLogs()
	task := startMemoryPrefetch(context.Background(), config.PrefetchMemory, "", stream, 4, logs.logf)
	<-stream.started
	close(stream.complete)
	<-stream.done
	task.Stop()

	joined := logs.joined()
	if !strings.Contains(joined, "started backend=file parent_layers=4") ||
		!strings.Contains(joined, "advised backend=file parent_layers=4") ||
		!strings.Contains(joined, "duration=") {
		t.Fatalf("logs = %q, want started/advised observability", joined)
	}
	if strings.Contains(joined, "canceled") {
		t.Fatalf("natural completion was misreported as canceled: %q", joined)
	}
}

func TestMemoryPrefetchCancellationLog(t *testing.T) {
	stream := newPrefetchTestStream()
	logs := newPrefetchTestLogs()
	task := startMemoryPrefetch(context.Background(), config.PrefetchMemory, "", stream, 2, logs.logf)
	<-stream.started
	task.Stop()

	joined := logs.joined()
	if !strings.Contains(joined, "canceled backend=file parent_layers=2") || !strings.Contains(joined, "duration=") {
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
	calls           atomic.Int32
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
	s.calls.Add(1)
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
