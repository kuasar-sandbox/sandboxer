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

type stoppedCaptureSink struct {
	ArtifactSink
	cancel       func()
	beforeCommit bool
	commits      int
}

func (s *stoppedCaptureSink) AbsorbSandbox(ctx context.Context, source sparse.Source) (string, string, error) {
	a, b, e := s.ArtifactSink.AbsorbSandbox(ctx, source)
	if s.beforeCommit {
		s.cancel()
	}
	return a, b, e
}
func (s *stoppedCaptureSink) AbsorbSnapshot(ctx context.Context, source sparse.Source) (string, string, error) {
	a, b, e := s.ArtifactSink.AbsorbSnapshot(ctx, source)
	if s.beforeCommit {
		s.cancel()
	}
	return a, b, e
}
func (s *stoppedCaptureSink) CommitSandbox(ctx context.Context, a, b string) error {
	s.commits++
	return s.ArtifactSink.CommitSandbox(ctx, a, b)
}
func (s *stoppedCaptureSink) CommitSnapshot(ctx context.Context, a, b string) error {
	s.commits++
	return s.ArtifactSink.CommitSnapshot(ctx, a, b)
}
func (s *stoppedCaptureSink) Close() error { err := s.ArtifactSink.Close(); s.cancel(); return err }

func TestStoppedCaptureCannotCommitOrReportSuccess(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		for _, beforeCommit := range []bool{false, true} {
			t.Run(map[bool]string{false: "export", true: "snapshot"}[snapshot]+map[bool]string{false: "-close", true: "-before-commit"}[beforeCommit], func(t *testing.T) {
				dir := t.TempDir()
				staging := filepath.Join(dir, "staging")
				if err := os.Mkdir(staging, 0700); err != nil {
					t.Fatal(err)
				}
				sock := filepath.Join(dir, "ch.sock")
				listener, err := net.Listen("unix", sock)
				if err != nil {
					t.Fatal(err)
				}
				server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/vm.snapshot" {
						for _, name := range []string{"config.json", "state.json"} {
							if err := os.WriteFile(filepath.Join(staging, name), []byte(`{"test":true}`), 0600); err != nil {
								http.Error(w, err.Error(), 500)
								return
							}
						}
					}
					w.WriteHeader(204)
				})}
				done := make(chan struct{})
				go func() { defer close(done); server.Serve(listener) }()
				defer func() { server.Close(); <-done }()
				ctx, cancel := context.WithCancelCause(context.Background())
				cause := errors.New("mandatory read ended runtime")
				sink := &stoppedCaptureSink{cancel: func() { cancel(cause) }, beforeCommit: beforeCommit}
				portable := exportTestPortable(t)
				portable.Boot.Root = config.PortableRootConfig{Base: "self"}
				portable.Boot.Disks = nil
				portable.Mounts = nil
				diffs := []DiskDiff{{SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) { return bytes.NewReader(make([]byte, 4096)), nil, nil }}}
				if snapshot {
					mfd, err := memory.Create("capture-stop", 4096)
					if err != nil {
						t.Fatal(err)
					}
					defer mfd.Close()
					q := &freezeTrackingQuiescer{}
					sink.ArtifactSink = &takeCaptureSink{events: &takeEvents{}, frozen: &q.frozen}
					out, err := Take(Sources{Context: ctx, SandboxID: "stopped", APISock: sock, MemfdFD: mfd.FD(), MemfdSize: int64(mfd.Size()), StagingDir: staging, PortableConfig: portable, CHApiDeadline: time.Second, Quiescer: q, Diffs: diffs}, sink, false)
					if out != nil || !errors.Is(err, cause) {
						t.Fatalf("capture reported success/lost cause: out=%v err=%v", out, err)
					}
				} else {
					sink.ArtifactSink = &captureSink{}
					out, err := Export(ctx, ExportSources{SandboxID: "stopped", APISock: sock, PortableConfig: portable, CHApiDeadline: time.Second, Quiescer: &recordingQuiescer{}, Diffs: diffs}, sink, false)
					if out != nil || !errors.Is(err, cause) {
						t.Fatalf("export reported success/lost cause: out=%v err=%v", out, err)
					}
				}
				if beforeCommit && sink.commits != 0 {
					t.Fatalf("stopped capture committed %d times", sink.commits)
				}
			})
		}
	}
}
