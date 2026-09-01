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
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
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
	portable := exportTestPortable(t)
	portable.Boot.Disks = nil
	portable.Mounts = nil
	_, err = Take(Sources{
		SandboxID:      "resume-on-error",
		APISock:        sock,
		MemfdSize:      4096,
		StagingDir:     t.TempDir(),
		CHApiDeadline:  time.Second,
		PortableConfig: portable,
		Diffs: []DiskDiff{{SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			return bytes.NewReader(make([]byte, 4096)), nil, nil
		}}},
		Quiescer: quiescer,
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

func TestTakeSinkCloseFailureResumesBackendsAndCH(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	mfd, err := memory.Create("take-close-failure", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer mfd.Close()

	sock := filepath.Join(dir, "ch.sock")
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
			if err := os.WriteFile(filepath.Join(staging, "config.json"), []byte(`{"vm":"config"}`), 0o600); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := os.WriteFile(filepath.Join(staging, "state.json"), []byte(`{"vm":"state"}`), 0o600); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
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

	portable := exportTestPortable(t)
	portable.Boot.Root = config.PortableRootConfig{Base: "self"}
	portable.Boot.Disks = nil
	portable.Mounts = nil
	events := &takeEvents{}
	quiescer := &freezeTrackingQuiescer{}
	sink := &takeCaptureSink{
		events: events, frozen: &quiescer.frozen,
		closeErr: errors.New("injected close failure"),
	}
	_, err = Take(Sources{
		Context: context.Background(), SandboxID: "close-failure", APISock: sock,
		MemfdFD: mfd.FD(), MemfdSize: int64(mfd.Size()), StagingDir: staging,
		PortableConfig: portable, CHApiDeadline: time.Second, Quiescer: quiescer,
		Diffs: []DiskDiff{{SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			return bytes.NewReader(make([]byte, 4096)), nil, nil
		}}},
	}, sink, false)
	if err == nil || !strings.Contains(err.Error(), "close artifact sink") || !strings.Contains(err.Error(), "injected close failure") {
		t.Fatalf("Take error = %v, want sink close failure", err)
	}
	if sink.closes != 1 {
		t.Fatalf("artifact sink closes = %d, want 1", sink.closes)
	}
	if quiescer.quiesce.Load() != 1 || quiescer.resume.Load() != 1 || quiescer.frozen.Load() {
		t.Fatalf("backend lifecycle quiesce=%d resume=%d frozen=%v",
			quiescer.quiesce.Load(), quiescer.resume.Load(), quiescer.frozen.Load())
	}
	mu.Lock()
	got := strings.Join(requests, ",")
	mu.Unlock()
	if want := "/api/v1/vm.pause,/api/v1/vm.snapshot,/api/v1/vm.resume"; got != want {
		t.Fatalf("CH requests = %q, want %q", got, want)
	}
}

func TestTakeBuildsSandboxAndMemorySnapshotAtOneFreezePoint(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	mfd, err := memory.Create("take-es-integration", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer mfd.Close()
	memoryBytes := bytes.Repeat([]byte{0x7a}, mfd.Size())
	copy(mfd.Bytes(), memoryBytes)

	events := &takeEvents{}
	quiescer := &freezeTrackingQuiescer{}
	chSock := filepath.Join(dir, "ch.sock")
	listener, err := net.Listen("unix", chSock)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		events.add("ch:" + request.URL.Path)
		if request.URL.Path == "/api/v1/vm.snapshot" {
			if !quiescer.frozen.Load() {
				http.Error(w, "snapshot outside backend freeze", http.StatusConflict)
				return
			}
			if writeErr := os.WriteFile(filepath.Join(staging, "config.json"), []byte(`{"vm":"config"}`), 0o600); writeErr != nil {
				http.Error(w, writeErr.Error(), http.StatusInternalServerError)
				return
			}
			if writeErr := os.WriteFile(filepath.Join(staging, "state.json"), []byte(`{"vm":"state"}`), 0o600); writeErr != nil {
				http.Error(w, writeErr.Error(), http.StatusInternalServerError)
				return
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

	portable := exportTestPortable(t)
	portable.Boot.Root = config.PortableRootConfig{Base: "self"}
	portable.Boot.Disks = nil
	portable.Mounts = nil
	if err := portable.Validate(); err != nil {
		t.Fatal(err)
	}
	diskCalls := 0
	parentMemoryRef := "file://parent.snapshot@digest:" + strings.Repeat("a", 64)
	sink := &takeCaptureSink{events: events, frozen: &quiescer.frozen}
	out, err := Take(Sources{
		Context: context.Background(), SandboxID: "test", APISock: chSock,
		MemfdFD: mfd.FD(), MemfdSize: int64(mfd.Size()), StagingDir: staging,
		PortableConfig: portable, MemoryFromRefs: []string{parentMemoryRef},
		CHApiDeadline: time.Second, Quiescer: quiescer,
		Diffs: []DiskDiff{{SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			diskCalls++
			if !quiescer.frozen.Load() {
				return nil, nil, errors.New("disk captured outside backend freeze")
			}
			events.add("disk-view")
			return bytes.NewReader(bytes.Repeat([]byte{0x3c}, 4096)), nil, nil
		}}},
	}, sink, true)
	if err != nil {
		t.Fatal(err)
	}
	if diskCalls != 1 {
		t.Fatalf("disk SnapshotView calls = %d, want 1", diskCalls)
	}
	if quiescer.quiesce.Load() != 1 || quiescer.resume.Load() != 1 || quiescer.frozen.Load() {
		t.Fatalf("backend lifecycle quiesce=%d resume=%d frozen=%v",
			quiescer.quiesce.Load(), quiescer.resume.Load(), quiescer.frozen.Load())
	}
	wantOrder := []string{
		"ch:/api/v1/vm.pause", "disk-view", "sink:sandbox",
		"ch:/api/v1/vm.snapshot", "sink:snapshot", "sink:commit-snapshot",
		"sink:close", "ch:/api/v1/vm.resume",
	}
	if got := events.snapshot(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("snapshot E/S order = %v, want %v", got, wantOrder)
	}
	if out.SandboxRef != sink.sandboxRef || out.SnapshotRef != sink.snapshotRef {
		t.Fatalf("Take refs = E:%q S:%q", out.SandboxRef, out.SnapshotRef)
	}
	if sink.closes != 1 {
		t.Fatalf("artifact sink closes = %d, want 1", sink.closes)
	}

	eSource, err := sparse.NewSource(bytes.NewReader(sink.sandboxBody), uint64(len(sink.sandboxBody)), nil)
	if err != nil {
		t.Fatal(err)
	}
	eRoot, err := sandboxfile.Open(context.Background(), &testExportStream{Source: eSource})
	if err != nil {
		t.Fatal(err)
	}
	if eRoot.Portable.Boot.Root.Base != "self" {
		t.Fatalf("Sandbox E root = %+v", eRoot.Portable.Boot.Root)
	}
	if err := eRoot.Close(); err != nil {
		t.Fatal(err)
	}

	sSource, err := sparse.NewSource(bytes.NewReader(sink.snapshotBody), uint64(len(sink.snapshotBody)), nil)
	if err != nil {
		t.Fatal(err)
	}
	sRoot, err := snapshotfile.Open(context.Background(), &testExportStream{Source: sSource})
	if err != nil {
		t.Fatal(err)
	}
	snapshotCfg, err := ParseConfig(sRoot.SnapshotConfig)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotCfg.SandboxRef != out.SandboxRef || !reflect.DeepEqual(snapshotCfg.FromRefs, []string{parentMemoryRef}) {
		t.Fatalf("snapshot.cfg = %+v", snapshotCfg)
	}
	if strings.Contains(string(sRoot.SnapshotConfig), "boot:") || strings.Contains(string(sRoot.SnapshotConfig), "launch:") || strings.Contains(string(sRoot.SnapshotConfig), "disks:") {
		t.Fatalf("snapshot.cfg retained disk or launch provenance: %s", sRoot.SnapshotConfig)
	}
	gotMemory := make([]byte, sRoot.Memory.Size())
	if n, readErr := sRoot.Memory.ReadAt(context.Background(), gotMemory, 0); readErr != nil && !errors.Is(readErr, io.EOF) {
		t.Fatal(readErr)
	} else if n != len(gotMemory) {
		t.Fatalf("memory read = %d, want %d", n, len(gotMemory))
	}
	if !bytes.Equal(gotMemory, memoryBytes) {
		t.Fatal("Snapshot S memory payload differs from memfd")
	}
	if err := sRoot.Close(); err != nil {
		t.Fatal(err)
	}
}

type takeEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *takeEvents) add(event string) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

func (e *takeEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

type freezeTrackingQuiescer struct {
	frozen  atomic.Bool
	quiesce atomic.Int32
	resume  atomic.Int32
}

func (q *freezeTrackingQuiescer) Quiesce() {
	q.quiesce.Add(1)
	q.frozen.Store(true)
}

func (q *freezeTrackingQuiescer) Resume() {
	q.frozen.Store(false)
	q.resume.Add(1)
}

type takeCaptureSink struct {
	events       *takeEvents
	frozen       *atomic.Bool
	sandboxBody  []byte
	snapshotBody []byte
	sandboxRef   string
	snapshotRef  string
	closeErr     error
	closes       int
}

func (s *takeCaptureSink) AbsorbOverlay(context.Context, io.ReadSeeker, []sparse.Extent) (string, string, error) {
	return "", "", errors.New("unexpected data overlay")
}

func (s *takeCaptureSink) AbsorbOverlaySource(context.Context, sparse.Source) (string, string, error) {
	return "", "", errors.New("unexpected overlay source")
}

func (s *takeCaptureSink) AbsorbSandbox(ctx context.Context, source sparse.Source) (string, string, error) {
	if !s.frozen.Load() {
		return "", "", errors.New("Sandbox E written outside backend freeze")
	}
	s.events.add("sink:sandbox")
	body, err := readTakeSource(ctx, source)
	if err != nil {
		return "", "", err
	}
	s.sandboxBody = body
	s.sandboxRef = "file://sandbox.sandbox@digest:" + strings.Repeat("e", 64)
	return s.sandboxRef, "", nil
}

func (s *takeCaptureSink) AbsorbSnapshot(ctx context.Context, source sparse.Source) (string, string, error) {
	if !s.frozen.Load() {
		return "", "", errors.New("Snapshot S written outside backend freeze")
	}
	s.events.add("sink:snapshot")
	body, err := readTakeSource(ctx, source)
	if err != nil {
		return "", "", err
	}
	s.snapshotBody = body
	s.snapshotRef = "file://memory.snapshot@digest:" + strings.Repeat("f", 64)
	return s.snapshotRef, "", nil
}

func (s *takeCaptureSink) CommitSandbox(context.Context, string, string) error {
	return errors.New("Sandbox E must not be the snapshot operation root")
}

func (s *takeCaptureSink) CommitSnapshot(context.Context, string, string) error {
	if !s.frozen.Load() {
		return errors.New("Snapshot S committed outside backend freeze")
	}
	s.events.add("sink:commit-snapshot")
	return nil
}

func (s *takeCaptureSink) Close() error {
	if !s.frozen.Load() {
		return errors.New("artifact sink closed outside backend freeze")
	}
	s.closes++
	s.events.add("sink:close")
	return s.closeErr
}

func readTakeSource(ctx context.Context, source sparse.Source) ([]byte, error) {
	body := make([]byte, source.Size())
	n, err := source.ReadAt(ctx, body, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if n != len(body) {
		return nil, io.ErrUnexpectedEOF
	}
	return body, nil
}

type recordingQuiescer struct {
	quiesce int
	resume  int
}

func (q *recordingQuiescer) Quiesce() { q.quiesce++ }
func (q *recordingQuiescer) Resume()  { q.resume++ }
