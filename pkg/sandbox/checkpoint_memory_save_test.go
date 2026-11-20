package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"golang.org/x/sys/unix"
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
		Holder: fixtureMemoryHolder(t, directory, "previous", "memory", start),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestPreviousMemoryUnavailableDoesNotPoisonLaterPause(t *testing.T) {
	directory := t.TempDir()
	w := memorySaveRecord(t, directory)
	if err := w.Update(func(r *memorycheckpoint.CaptureRecord) { r.RuntimeRetired = true; r.SaveError = "writer exited" }); err != nil {
		t.Fatal(err)
	}
	r := w.Read()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate exact PID reuse while a real, separately opened reader pin
	// remains. Physical proof is covered by the subprocess recovery suite.
	r.Holder.StartTime++
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.Path(), b, 0600); err != nil {
		t.Fatal(err)
	}
	pin, err := os.OpenFile(w.Path()+".ram.lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	if err := unix.Flock(int(pin.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	observed, err := memorycheckpoint.ObservePreviousSave(context.Background(), directory, r)
	if !errors.Is(err, memorycheckpoint.ErrMemoryUnavailable) || observed.Record.RAMReleased {
		t.Fatalf("live pin lost its charge: %+v %v", observed, err)
	}
	h := &SnapshotHandler{SandboxID: "sid", CheckpointDir: directory, EnableCheckpoint: true, runtime: &Runtime{}}
	state, err := h.captureState(context.Background(), ctl.CaptureStateQuery{SandboxID: "sid", ForCapture: true})
	if err != nil || state.Pending || state.RetentionSupported == nil || !*state.RetentionSupported {
		t.Fatalf("old loss poisoned new admission: %+v %v", state, err)
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
	observed, err = memorycheckpoint.ObservePreviousSave(context.Background(), directory, r)
	if err != nil || !observed.Record.RAMReleased || !observed.Record.Lost() {
		t.Fatalf("exact release recovery: %+v %v", observed, err)
	}

}

func prepareMemorySaveHolder(t *testing.T, directory string, fd *os.File) *memorycheckpoint.Holder {
	t.Helper()
	run := filepath.Join(directory, "run")
	if err := os.MkdirAll(run, 0700); err != nil {
		t.Fatal(err)
	}
	h, err := memorycheckpoint.PrepareHolder(directory, run, memorycheckpoint.CaptureRecord{Version: 1, Producer: memorycheckpoint.RunIdentity{SandboxID: "sid"}, CheckpointID: "previous", MemoryID: "memory", Epoch: 1}, fd, 4096, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Record().Update(func(r *memorycheckpoint.CaptureRecord) {
		r.Committed = true
		r.SnapshotRef = "snapshot"
		r.SandboxRef = "sandbox"
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPreviousMemoryDoesNotBlockNewCaptureAdmission(t *testing.T) {
	directory := t.TempDir()
	record := memorySaveRecord(t, directory)
	if err := record.Update(func(r *memorycheckpoint.CaptureRecord) { r.RuntimeRetired = true }); err != nil {
		t.Fatal(err)
	}
	h := &SnapshotHandler{SandboxID: "sid", CheckpointDir: directory, EnableCheckpoint: true, runtime: &Runtime{}, CHProcess: func() processSignaler { return nil }}
	for _, noWait := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := h.handleContext(ctx, ctl.Request{CheckpointID: "next", NoWaitForPreviousMemorySave: noWait}, "", nil)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "process is not started") {
			t.Fatalf("prior holder gated preflight: %v", err)
		}
		if record.Read().RAMReleased {
			t.Fatal("admission fabricated RAM release")
		}
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
	m, err := memory.Create("retirement-failure", 4096)
	if err != nil {
		t.Fatal(err)
	}
	m.Bytes()[0] = 0x73
	fd, err := memorycheckpoint.ReadOnlyMemfd(m.FD())
	if err != nil {
		t.Fatal(err)
	}
	holder := prepareMemorySaveHolder(t, directory, fd)
	record := holder.Record()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	save := &checkpointMemorySave{holder: holder, logf: t.Logf,
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
