package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
)

func TestResumeEarlyUncertainCheckpointCreationAttemptsCleanup(t *testing.T) {
	made, deleted := false, false
	injected := errors.New("post-commit I/O failure")
	s := Sources{Diffs: []DiskDiff{{TemporaryCheckpoint: func(context.Context, string) error { made = true; return injected }, OpenCheckpoint: func(context.Context, string) (ExportView, error) { t.Fatal("unexpected open"); return nil, nil }, DeleteCheckpoint: func(context.Context, string) error { deleted = true; return nil }}}}
	_, err := freezeResumeEarlyInputs(context.Background(), &s)
	if !errors.Is(err, injected) || !made || !deleted {
		t.Fatalf("unexpected: made=%v deleted=%v err=%v", made, deleted, err)
	}
}
func TestResumeEarlyCommittedSnapshotPreservesReferencesOnCleanupFailure(t *testing.T) {
	testResumeEarlySinkOutcome(t, nil)
}

var errResumeEarlyReaderOpen = errors.New("injected reader open failure")

func TestResumeEarlyPersistenceFailureDoesNotReportSuccess(t *testing.T) {
	for _, err := range []error{errors.New("injected storage failure"), context.Canceled, errResumeEarlyReaderOpen} {
		t.Run(err.Error(), func(t *testing.T) { testResumeEarlySinkOutcome(t, err) })
	}
}

type resumeEarlyFailSink struct {
	*FileSink
	failure error
	cancel  context.CancelFunc
}

func (s *resumeEarlyFailSink) AbsorbSnapshot(ctx context.Context, source sparse.Source) (string, string, error) {
	if errors.Is(s.failure, context.Canceled) {
		s.cancel()
		return "", "", ctx.Err()
	}
	return "", "", s.failure
}

func testResumeEarlySinkOutcome(t *testing.T, persistenceErr error) {
	dir, err := os.MkdirTemp("/tmp", "resume-early-review-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	stage := filepath.Join(dir, "stage")
	os.Mkdir(stage, 0700)
	sock := filepath.Join(dir, "ch.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/vm.snapshot" {
			for _, n := range []string{"config.json", "state.json"} {
				os.WriteFile(filepath.Join(stage, n), []byte(`{}`), 0600)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go server.Serve(listener)
	defer server.Close()
	ram, err := memory.Create("review", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer ram.Close()
	ram.Bytes()[0] = 42
	portable := exportTestPortable(t)
	portable.Boot.Root = config.PortableRootConfig{Base: "self"}
	portable.Boot.Disks = nil
	portable.Mounts = nil
	disk, _ := sparse.NewSource(bytes.NewReader(make([]byte, 4096)), 4096, nil)
	deletionErr := errors.New("injected checkpoint delete failure")
	q := &freezeTrackingQuiescer{}
	output := filepath.Join(dir, "output")
	os.Mkdir(output, 0700)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sink ArtifactSink = NewFileSink(output, "review", nil, false, nil)
	if persistenceErr != nil {
		sink = &resumeEarlyFailSink{sink.(*FileSink), persistenceErr, cancel}
	}
	deleted, resumed := false, false
	sources := Sources{
		ResumeEarly: true, ResumeGuest: func() error { resumed = true; return nil },
		Context: ctx, SandboxID: "review", APISock: sock, StagingDir: stage,
		MemfdFD: ram.FD(), MemfdSize: 4096, CHApiDeadline: time.Second,
		Quiescer: q, PortableConfig: portable,
		Diffs: []DiskDiff{{
			TemporaryCheckpoint: func(context.Context, string) error { return nil },
			OpenCheckpoint: func(context.Context, string) (ExportView, error) {
				if !resumed {
					t.Fatal("reader opened before resume")
				}
				if persistenceErr == errResumeEarlyReaderOpen {
					return nil, persistenceErr
				}
				return borrowedResumeEarlySource{disk}, nil
			},
			DeleteCheckpoint: func(context.Context, string) error { deleted = true; return deletionErr },
			SnapshotView:     func() (io.ReadSeeker, []sparse.Extent, error) { return bytes.NewReader(make([]byte, 4096)), nil, nil },
		}},
	}
	out, err := Take(sources, sink, true)
	if !deleted || !resumed {
		t.Fatalf("cleanup=%t resumed=%t", deleted, resumed)
	}
	if persistenceErr != nil {
		if out != nil || !errors.Is(err, persistenceErr) || !errors.Is(err, deletionErr) {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		if _, err := os.Stat(filepath.Join(output, "review.snapshot")); !os.IsNotExist(err) {
			t.Fatalf("failed capture published: %v", err)
		}
		return
	}
	if err != nil || out == nil || out.CleanupWarning != deletionErr.Error() {
		t.Fatalf("expected committed snapshot and cleanup warning, out=%+v err=%v", out, err)
	}
	if _, e := os.Stat(out.SnapshotPath); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(output, "review.snapshot")); e != nil {
		t.Fatal(e)
	}
}
