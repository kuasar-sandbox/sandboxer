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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
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
	for _, failure := range []string{"none", "postcommit-close", "lost-commit-reply"} {
		t.Run(failure, func(t *testing.T) {
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
			if failure == "postcommit-close" {
				sink.closeErr = errors.New("injected postcommit close")
			}
			portable := exportTestPortable(t)
			portable.Boot.Runtime = "file://captured-v1.bundle@digest:" + strings.Repeat("b", 64)
			portable.Boot.Disks = nil
			portable.Mounts = nil
			mem, err := memory.Create("runtime-capture", 4096)
			if err != nil {
				t.Fatal(err)
			}
			defer mem.Close()
			fd, err := memorycheckpoint.ReadOnlyMemfd(mem.FD())
			if err != nil {
				t.Fatal(err)
			}
			recordDir := filepath.Join(dir, "checkpoint")
			if err := os.MkdirAll(recordDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, "run"), 0700); err != nil {
				t.Fatal(err)
			}
			holder, err := memorycheckpoint.PrepareHolder(recordDir, filepath.Join(dir, "run"), memorycheckpoint.CaptureRecord{
				Version: memorycheckpoint.Version, Producer: memorycheckpoint.RunIdentity{SandboxID: "frozen"},
				CheckpointID: "cp", MemoryID: "memory", Epoch: 1,
			}, fd, 4096, t.Logf)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if !holder.Record().Read().Committed {
					_ = holder.Abort()
					return
				}
				if err := holder.Record().Update(func(r *memorycheckpoint.CaptureRecord) { r.RuntimeRetired = true }); err != nil {
					t.Fatal(err)
				}
				if err := holder.Start(func(context.Context, io.ReaderAt) (string, error) { return "payload", nil }, nil); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := holder.Wait(ctx); err != nil {
					t.Fatal(err)
				}
				// Reopen from disk after the response path and holder have finished.
				durable, err := memorycheckpoint.ReadRecord(recordDir, "cp", "memory")
				if err != nil || durable.SnapshotRef != sink.snapshotRef+"@runtime:captured-v1.bundle:digest:"+strings.Repeat("b", 64) {
					t.Fatalf("durable capture runtime lost: %+v: %v", durable, err)
				}
			}()
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
					ref, err := manifest.ParseRef(out.SnapshotRef)
					if err != nil || ref.Runtime != (manifest.RuntimeIdentity{Basename: "captured-v1.bundle", Digest: strings.Repeat("b", 64)}) {
						t.Fatalf("holder received incomplete runtime: %s: %v", out.SnapshotRef, err)
					}
					if err := holder.Record().Update(func(r *memorycheckpoint.CaptureRecord) {
						r.SnapshotRef, r.SandboxRef, r.Committed = out.SnapshotRef, out.SandboxRef, true
					}); err != nil {
						return err
					}
					committed = true
					if failure == "lost-commit-reply" {
						return errors.Join(ErrFrozenMemoryCommitted, errors.New("injected lost commit reply"))
					}
					return nil
				}},
			}, sink, false)
			if failure != "none" && !errors.Is(err, ErrFrozenMemoryCommitted) || failure == "none" && err != nil {
				t.Fatalf("capture error=%v", err)
			}
			if !committed || resumes.Load() != 0 || q.resume.Load() != 0 || !q.frozen.Load() {
				t.Fatalf("committed=%v resumes=%d backend=%d frozen=%v", committed, resumes.Load(), q.resume.Load(), q.frozen.Load())
			}
		})
	}
}
