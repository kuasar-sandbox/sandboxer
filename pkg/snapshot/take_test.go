package snapshot

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

func TestAbsorbOverlayUsesSnapshotView(t *testing.T) {
	const size = 3 * 4096
	logical := make([]byte, size)
	copy(logical[4096:8192], bytes.Repeat([]byte{0x6d}, 4096))
	holes := []sparse.Extent{
		{Offset: 0, Size: 4096},
		{Offset: 8192, Size: 4096},
	}
	called := false
	diff := DiskDiff{
		Path: filepath.Join(t.TempDir(), "must-not-be-opened.diff"),
		SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			called = true
			return bytes.NewReader(logical), holes, nil
		},
	}
	sink := NewFileSink(t.TempDir(), "sid", nil, false, nil)
	ref, path, err := absorbOverlay(context.Background(), sink, diff, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("snapshot view provider was not called")
	}
	if ref == "" || path == "" {
		t.Fatalf("absorbOverlay returned ref=%q path=%q", ref, path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	view, err := tarstream.ReadSeekFrom(bytes.NewReader(raw), "overlay")
	if err != nil {
		t.Fatal(err)
	}
	if got := view.Holes(); len(got) != len(holes) || got[0] != holes[0] || got[1] != holes[1] {
		t.Fatalf("artifact holes=%v want %v", got, holes)
	}
	got, err := io.ReadAll(view)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, logical) {
		t.Fatal("artifact content differs from snapshot view")
	}
}

func TestAbsorbOverlayRequiresSnapshotView(t *testing.T) {
	sink := NewFileSink(t.TempDir(), "sid", nil, false, nil)
	if _, _, err := absorbOverlay(context.Background(), sink, DiskDiff{Path: "raw.diff"}, false, nil, false); err == nil {
		t.Fatal("absorbOverlay accepted a raw-path-only diff")
	}
}

// A disk-only Take must never touch the memfd (an invalid fd would fail the
// hole walk if it did) and must produce a bundle whose payload is the bare
// config ZIP.
func TestTakeDiskOnlyProducesMemorylessBundle(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ch.sock")
	stagingDir := t.TempDir()
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/vm.snapshot" {
			// Real CH drops config.json + state.json into the staging dir.
			for _, name := range []string{"config.json", "state.json"} {
				if err := os.WriteFile(filepath.Join(stagingDir, name), []byte("{}"), 0o644); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serveDone)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-serveDone
	})

	cfgCalls := 0
	outDir := t.TempDir()
	const diffSize = 2 * 4096
	out, err := Take(Sources{
		SandboxID:     "disk-only",
		APISock:       sock,
		StagingDir:    stagingDir,
		CHApiDeadline: time.Second,
		MemfdFD:       -1, // any memory access fails the whole Take
		MemfdSize:     1 << 20,
		Diffs: []DiskDiff{{
			Path: "root.diff",
			SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
				return bytes.NewReader(make([]byte, diffSize)), nil, nil
			},
		}},
		SnapshotCfg: func([]string) ([]byte, error) {
			cfgCalls++
			return []byte("memory: false\n"), nil
		},
		Quiescer:   &recordingQuiescer{},
		SkipMemory: true,
	}, NewFileSink(outDir, "disk-only", nil, false, nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfgCalls != 1 {
		t.Fatalf("snapshot.cfg builder calls = %d, want 1", cfgCalls)
	}
	if out.MemorySize != 0 || out.MemoryResident != 0 {
		t.Fatalf("disk-only outputs report memory size=%d resident=%d, want 0/0", out.MemorySize, out.MemoryResident)
	}

	raw, err := os.ReadFile(out.SnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	view, err := tarstream.ReadSeekFrom(bytes.NewReader(raw), "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	logical := make([]byte, view.Size())
	if _, err := io.ReadFull(view, logical); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(logical), int64(len(logical)))
	if err != nil {
		t.Fatalf("disk-only bundle payload is not a bare ZIP: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{"config.json", "state.json", "snapshot.cfg"} {
		if !names[want] {
			t.Fatalf("disk-only bundle ZIP missing %s: %v", want, names)
		}
	}
	cfgEntry, err := zr.Open("snapshot.cfg")
	if err != nil {
		t.Fatal(err)
	}
	defer cfgEntry.Close()
	cfgBody, err := io.ReadAll(cfgEntry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfgBody), "memory: false") {
		t.Fatalf("disk-only snapshot.cfg does not carry the memory=false marker:\n%s", cfgBody)
	}
}

func TestTakeResumesAfterDestroyModeFailure(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ch.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []string
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/api/v1/vm.snapshot" {
			http.Error(w, "injected snapshot failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serveDone)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-serveDone
	})

	quiescer := &recordingQuiescer{}
	_, err = Take(Sources{
		SandboxID:     "resume-on-error",
		APISock:       sock,
		StagingDir:    t.TempDir(),
		CHApiDeadline: time.Second,
		SnapshotCfg:   func([]string) ([]byte, error) { return nil, nil },
		Quiescer:      quiescer,
	}, NewFileSink(t.TempDir(), "resume-on-error", nil, false, nil), false)
	if err == nil || !strings.Contains(err.Error(), "CH snapshot") {
		t.Fatalf("Take error = %v, want injected CH snapshot failure", err)
	}
	mu.Lock()
	got := strings.Join(requests, ",")
	mu.Unlock()
	if want := "/api/v1/vm.pause,/api/v1/vm.snapshot,/api/v1/vm.resume"; got != want {
		t.Fatalf("CH requests = %q, want %q", got, want)
	}
	if quiescer.quiesce != 1 || quiescer.resume != 1 {
		t.Fatalf("quiescer calls = quiesce:%d resume:%d", quiescer.quiesce, quiescer.resume)
	}
}

type recordingQuiescer struct {
	quiesce int
	resume  int
}

func (q *recordingQuiescer) Quiesce() { q.quiesce++ }
func (q *recordingQuiescer) Resume()  { q.resume++ }
