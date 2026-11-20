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
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
)

func TestValidateFrozenMemoryPayloadAcceptsSparseSource(t *testing.T) {
	s, err := sparse.NewSource(bytes.NewReader(make([]byte, 8192)), 8192, []sparse.Extent{{Offset: 4096, Size: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateMemoryPayload(context.Background(), s); err != nil {
		t.Fatalf("encodable top forced synchronous fallback: %v", err)
	}
}

func TestFrozenCaptureNeverResumesAfterCommit(t *testing.T) {
	for _, closeFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "postcommit-close-error"}[closeFails], func(t *testing.T) {
			dir := t.TempDir()
			sock := filepath.Join(dir, "ch.sock")
			l, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			var resumes atomic.Int32
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/vm.resume" {
					resumes.Add(1)
				}
				if r.URL.Path == "/api/v1/vm.snapshot" {
					for _, n := range []string{"config.json", "state.json"} {
						if err := os.WriteFile(filepath.Join(dir, n), []byte(`{}`), 0600); err != nil {
							t.Error(err)
						}
					}
				}
				w.WriteHeader(http.StatusNoContent)
			})}
			go srv.Serve(l)
			defer srv.Close()
			q := &freezeTrackingQuiescer{}
			sink := &takeCaptureSink{events: &takeEvents{}, frozen: &q.frozen}
			if closeFails {
				sink.closeErr = errors.New("injected postcommit close")
			}
			portable := exportTestPortable(t)
			portable.Boot.Disks = nil
			portable.Mounts = nil
			committed := false
			_, err = Take(Sources{Context: context.Background(), SandboxID: "frozen", APISock: sock, StagingDir: dir, MemfdFD: 0, MemfdSize: 4096, CHApiDeadline: time.Second, PortableConfig: portable, Quiescer: q,
				Diffs: []DiskDiff{{SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) { return bytes.NewReader(make([]byte, 4096)), nil, nil }}},
				CaptureMemory: func(context.Context) (*memory.CaptureView, error) {
					return &memory.CaptureView{Size: 4096, Materialized: []byte{1}, Dirty: []byte{1}, Released: []byte{0}}, nil
				},
				FrozenMemory: &FrozenMemoryCapture{Build: func(context.Context, ArtifactSink, []byte, []byte, []byte, *memory.CaptureView) (sparse.Source, uint64, error) {
					s, err := sparse.NewSource(bytes.NewReader([]byte("descriptor")), 10, nil)
					return s, 4096, err
				}, Commit: func(out *Outputs) error {
					if out.SnapshotRef == "" || out.SandboxRef == "" {
						t.Error("missing fixed refs at commit")
					}
					committed = true
					return nil
				}},
			}, sink, false)
			if closeFails && !errors.Is(err, ErrFrozenMemoryCommitted) || !closeFails && err != nil {
				t.Fatalf("capture error=%v", err)
			}
			if !committed || resumes.Load() != 0 || q.resume.Load() != 0 || !q.frozen.Load() {
				t.Fatalf("committed=%v resumes=%d backend=%d frozen=%v", committed, resumes.Load(), q.resume.Load(), q.frozen.Load())
			}
		})
	}
}
