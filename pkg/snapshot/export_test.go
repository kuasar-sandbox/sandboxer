package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

type captureSink struct {
	order       []string
	sandboxBody []byte
	committed   bool
	closeErr    error
	closes      int
}

func (s *captureSink) AbsorbOverlay(_ context.Context, diff io.ReadSeeker, _ []sparse.Extent) (string, string, error) {
	s.order = append(s.order, "data")
	_, _ = diff.Seek(0, io.SeekStart)
	_, _ = io.ReadAll(diff)
	return "manifest://" + strings.Repeat("d", 64), "", nil
}
func (s *captureSink) AbsorbOverlaySource(context.Context, sparse.Source) (string, string, error) {
	return "manifest://" + strings.Repeat("d", 64), "", nil
}
func (s *captureSink) AbsorbSandbox(ctx context.Context, source sparse.Source) (string, string, error) {
	s.order = append(s.order, "sandbox")
	s.sandboxBody = make([]byte, source.Size())
	n, err := source.ReadAt(ctx, s.sandboxBody, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	if n != len(s.sandboxBody) {
		return "", "", io.ErrUnexpectedEOF
	}
	return "manifest://" + strings.Repeat("e", 64), "", nil
}
func (s *captureSink) AbsorbSnapshot(context.Context, sparse.Source) (string, string, error) {
	return "", "", errors.New("unexpected snapshot")
}
func (s *captureSink) CommitSandbox(context.Context, string, string) error {
	s.order = append(s.order, "commit")
	s.committed = true
	return nil
}
func (s *captureSink) CommitSnapshot(context.Context, string, string) error {
	return errors.New("unexpected snapshot commit")
}
func (s *captureSink) Close() error {
	s.order = append(s.order, "close")
	s.closes++
	return s.closeErr
}

func TestCaptureSandboxAtFreezeDataFirstRootOnceAndC0Unchanged(t *testing.T) {
	c0 := exportTestPortable(t)
	original, err := config.MarshalPortableSandboxConfig(c0)
	if err != nil {
		t.Fatal(err)
	}
	rootCalls, dataCalls := 0, 0
	sources := ExportSources{
		SandboxID: "sid", PortableConfig: c0,
		ParentSandboxRef: "file://parent.sandbox@digest:" + strings.Repeat("a", 64),
		Diffs: []DiskDiff{
			{SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
				rootCalls++
				return bytes.NewReader(bytes.Repeat([]byte{0x11}, 8192)), []sparse.Extent{{Offset: 4096, Size: 4096}}, nil
			}},
			{SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
				dataCalls++
				return bytes.NewReader(bytes.Repeat([]byte{0x22}, 4096)), nil, nil
			}},
		},
	}
	sink := &captureSink{}
	out, err := captureSandboxAtFreeze(context.Background(), sources, sink, true)
	if err != nil {
		t.Fatal(err)
	}
	if rootCalls != 1 || dataCalls != 1 {
		t.Fatalf("SnapshotView calls root/data = %d/%d", rootCalls, dataCalls)
	}
	if !reflect.DeepEqual(sink.order, []string{"data", "sandbox", "commit"}) {
		t.Fatalf("order = %v", sink.order)
	}
	after, err := config.MarshalPortableSandboxConfig(c0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("capture mutated C0")
	}
	openedSource, err := sparse.NewSource(bytes.NewReader(sink.sandboxBody), uint64(len(sink.sandboxBody)), nil)
	if err != nil {
		t.Fatal(err)
	}
	root, err := sandboxfile.Open(context.Background(), &testExportStream{Source: openedSource})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if root.Payload.Size() != 8192 || root.Portable.Boot.Disks[0].Base != out.DataRefs[0] {
		t.Fatalf("captured E payload/config = %d %#v", root.Payload.Size(), root.Portable.Boot.Disks)
	}
}

func TestExportSinkCloseFailureResumesBackendsAndCH(t *testing.T) {
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
	portable.Boot.Disks = nil
	portable.Mounts = nil
	quiescer := &recordingQuiescer{}
	sink := &captureSink{closeErr: errors.New("injected close failure")}
	_, err = Export(context.Background(), ExportSources{
		SandboxID:        "close-failure",
		APISock:          sock,
		CHApiDeadline:    time.Second,
		PortableConfig:   portable,
		ParentSandboxRef: "file://parent.sandbox@digest:" + strings.Repeat("a", 64),
		Diffs: []DiskDiff{{SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			return bytes.NewReader(make([]byte, 4096)), nil, nil
		}}},
		Quiescer: quiescer,
	}, sink, false)
	if err == nil || !strings.Contains(err.Error(), "close artifact sink") || !strings.Contains(err.Error(), "injected close failure") {
		t.Fatalf("Export error = %v, want sink close failure", err)
	}
	if sink.closes != 1 {
		t.Fatalf("artifact sink closes = %d, want 1", sink.closes)
	}
	if quiescer.quiesce != 1 || quiescer.resume != 1 {
		t.Fatalf("quiescer calls = quiesce:%d resume:%d (Export error: %v)", quiescer.quiesce, quiescer.resume, err)
	}
	mu.Lock()
	got := strings.Join(requests, ",")
	mu.Unlock()
	if want := "/api/v1/vm.pause,/api/v1/vm.resume"; got != want {
		t.Fatalf("CH requests = %q, want %q", got, want)
	}
}

func TestExportRejectsOverlongProspectiveLayerChainBeforePause(t *testing.T) {
	portable := exportTestPortable(t)
	portable.Boot.Disks = nil
	portable.Mounts = nil
	portable.Boot.Root = config.PortableRootConfig{Base: "self"}
	for i := 0; i < config.MaxPortableLayerRefs; i++ {
		portable.Boot.Root.BaseFromRefs = append(portable.Boot.Root.BaseFromRefs,
			"manifest://"+fmt.Sprintf("%064x", i+1))
	}
	if err := portable.Validate(); err != nil {
		t.Fatal(err)
	}

	viewCalls := 0
	quiescer := &recordingQuiescer{}
	_, err := Export(context.Background(), ExportSources{
		SandboxID:        "layer-limit",
		APISock:          filepath.Join(t.TempDir(), "must-not-call-ch.sock"),
		PortableConfig:   portable,
		ParentSandboxRef: "manifest://" + strings.Repeat("f", 64),
		Diffs: []DiskDiff{{SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			viewCalls++
			return bytes.NewReader(make([]byte, 4096)), nil, nil
		}}},
		Quiescer: quiescer,
	}, &captureSink{}, true)
	if err == nil || !strings.Contains(err.Error(), "exceeds 64 entries") {
		t.Fatalf("Export error = %v, want prospective C1 layer-limit rejection", err)
	}
	if quiescer.quiesce != 0 || quiescer.resume != 0 {
		t.Fatalf("prospective C1 error touched backends: quiesce=%d resume=%d", quiescer.quiesce, quiescer.resume)
	}
	if viewCalls != 0 {
		t.Fatalf("prospective C1 error opened SnapshotView %d times", viewCalls)
	}
}

func TestValidateExportGraphRejectsProspectiveConfigSize(t *testing.T) {
	portable := exportTestPortable(t)
	portable.Metadata = make(map[string]string)
	for i := 0; i < 15; i++ {
		portable.Metadata[fmt.Sprintf("padding-%02d", i)] = strings.Repeat("x", config.MaxPortableScalarBytes)
	}
	portable.Metadata["edge"] = "x"
	raw, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	remaining := config.MaxPortableConfigBytes - len(raw)
	if remaining <= 0 || remaining >= config.MaxPortableScalarBytes {
		t.Fatalf("portable config calibration has invalid remaining size %d", remaining)
	}
	portable.Metadata["edge"] += strings.Repeat("x", remaining)
	raw, err = config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != config.MaxPortableConfigBytes {
		t.Fatalf("calibrated C0 size = %d, want %d", len(raw), config.MaxPortableConfigBytes)
	}

	err = ValidateExportGraph(portable, "", []bool{true, true})
	if err == nil || !strings.Contains(err.Error(), "portable config exceeds") {
		t.Fatalf("ValidateExportGraph error = %v, want prospective config-size rejection", err)
	}
}

type testExportStream struct{ sparse.Source }

func (s *testExportStream) Close() error { return nil }

var _ fetch.Stream = (*testExportStream)(nil)

func exportTestPortable(t *testing.T) *config.PortableSandboxConfig {
	t.Helper()
	key := strings.Repeat("a", 64)
	cfg := &config.PortableSandboxConfig{
		Version: 1,
		Resources: config.PortableResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 1, Memory: "1GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1, Memory: "1GiB"},
		},
		Boot: config.PortableBootConfig{
			Kernel: "file://kernel@digest:" + key, Runtime: "file://runtime@digest:" + key,
			Root:  config.PortableRootConfig{Base: "file://base@digest:" + key, Overlay: &config.PortableOverlayConfig{Base: "self"}},
			Disks: []config.PortableDiskConfig{{Name: "data", PortableRootConfig: config.PortableRootConfig{Base: "file://old.overlay@digest:" + key}}},
		},
		Launch: config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"},
		Mounts: []config.MountConfig{{Target: "/data", Type: "disk", Source: "data"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}
