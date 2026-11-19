package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
)

func memorySaveRecord(t *testing.T, directory string) *memorycheckpoint.RecordWriter {
	t.Helper()
	start, err := memorycheckpoint.ProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	r, err := memorycheckpoint.CreateRecord(directory, memorycheckpoint.CaptureRecord{
		Version: 1, Producer: memorycheckpoint.RunIdentity{SandboxID: "sid"},
		CheckpointID: "previous", MemoryID: "memory", Epoch: 1,
		Committed: true, SnapshotRef: "snapshot", SandboxRef: "sandbox",
		Holder: memorycheckpoint.HolderIdentity{PID: os.Getpid(), StartTime: start, SocketPath: filepath.Join(directory, "run", "memory-holders", "m.sock")},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestPreviousMemoryWaitDoesNotEnterCapture(t *testing.T) {
	directory := t.TempDir()
	record := memorySaveRecord(t, directory)
	p, err := newPreviousMemorySave(directory, "sid")
	if err != nil {
		t.Fatal(err)
	}
	h := &SnapshotHandler{EnableCheckpoint: true, previousMemory: p}
	if _, err := h.handleContext(context.Background(), ctl.Request{CheckpointID: "next", NoWaitForPreviousMemorySave: true}, "", nil); !errors.Is(err, ctl.ErrMemorySavePending) {
		t.Fatalf("no-wait=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.handleContext(ctx, ctl.Request{CheckpointID: "next"}, "", nil)
		done <- err
	}()
	// The source record itself proves pending; cancellation must not allow
	// the handler to reach its deliberately absent VMM/capture dependencies.
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled pause wait retained request")
	}
	if h.capture.active || h.capture.terminal {
		t.Fatal("waiting changed the capture state")
	}
	if err := record.Update(func(r *memorycheckpoint.CaptureRecord) {
		r.RuntimeRetired, r.FileReady, r.RAMReleased, r.PayloadRef = true, true, true, "payload"
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.wait(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeFrozenCaptureLeavesOperationBeforeRetirement(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "committed-error"}[failed], func(t *testing.T) {
			r := &Runtime{state: runtimeRunning}
			called := false
			captureErr := errors.New("committed publication error")
			r.snapshot = func(context.Context, ctl.Request) (ctl.Response, error) {
				var err error
				if failed {
					err = captureErr
				}
				return ctl.Response{BeforeWrite: func() error {
					done := make(chan struct{})
					go func() { r.opsWG.Wait(); close(done) }()
					select {
					case <-done:
						called = true
						return nil
					case <-time.After(time.Second):
						return errors.New("capture retained its own exit barrier")
					}
				}}, err
			}
			response, err := r.Snapshot(context.Background(), ctl.Request{CheckpointID: "next"})
			if !called || response.BeforeWrite != nil || failed != errors.Is(err, captureErr) || !failed && err != nil {
				t.Fatalf("retirement called=%v response=%+v err=%v", called, response, err)
			}
		})
	}
}

func TestRetirementPublicationFailureReturnsAndRetries(t *testing.T) {
	directory := t.TempDir()
	record := memorySaveRecord(t, directory)
	m, err := memory.Create("retirement-failure", 4096)
	if err != nil {
		t.Fatal(err)
	}
	m.Bytes()[0] = 0x73
	fd, err := memorycheckpoint.ReadOnlyMemfd(m.FD())
	if err != nil {
		t.Fatal(err)
	}
	holder, err := memorycheckpoint.NewHolder(record, fd, 4096, t.Logf)
	if err != nil {
		fd.Close()
		m.Close()
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	pin, err := memorycheckpoint.PinDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	save := &checkpointMemorySave{holder: holder, pin: pin, logf: t.Logf,
		dump: func(_ context.Context, reader io.ReaderAt) (string, error) {
			var b [1]byte
			if _, err := reader.ReadAt(b[:], 0); err != nil || b[0] != 0x73 {
				return "", errors.New("retained memory changed")
			}
			return "payload", nil
		},
	}
	// Fail the real atomic record publication after the VMM has retired. A
	// fixed filesystem error must not indefinitely retain the pause caller.
	metadata, hidden := filepath.Join(directory, "memory"), filepath.Join(directory, "memory-hidden")
	if err := os.Rename(metadata, hidden); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, nil, 0600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := save.retire(); err == nil || time.Since(started) >= memoryRetirementResponseTimeout {
		t.Fatalf("publication failure was not returned promptly: %v", err)
	}
	if record.Read().RuntimeRetired || record.Read().RAMReleased {
		t.Fatal("failed publication fabricated completed facts")
	}
	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(hidden, metadata); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := holder.Wait(ctx); err != nil {
		t.Fatalf("retirement did not finish after storage recovery: %v", err)
	}
	got, err := memorycheckpoint.ReadCurrent(directory)
	if err != nil || !got.RuntimeRetired || !got.FileReady || !got.RAMReleased {
		t.Fatalf("completed record=%+v err=%v", got, err)
	}
}
