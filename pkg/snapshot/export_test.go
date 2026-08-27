package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

type captureSink struct {
	order       []string
	sandboxBody []byte
	committed   bool
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

func TestCaptureSandboxAtFreezeDataFirstRootOnceAndC0Unchanged(t *testing.T) {
	c0 := exportTestPortable(t)
	original, err := config.MarshalPortableSandboxConfig(c0)
	if err != nil {
		t.Fatal(err)
	}
	rootCalls, dataCalls := 0, 0
	sources := ExportSources{
		SandboxID: "sid", PortableConfig: c0,
		ParentSandboxRef: "file://parent.sandbox@sha256:" + strings.Repeat("a", 64),
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
			Kernel: "file://kernel@sha256:" + key, Runtime: "file://runtime@sha256:" + key,
			Root:  config.PortableRootConfig{Base: "file://base@sha256:" + key, Overlay: &config.PortableOverlayConfig{Base: "self"}},
			Disks: []config.PortableDiskConfig{{Name: "data", PortableRootConfig: config.PortableRootConfig{Base: "file://old.overlay@sha256:" + key}}},
		},
		Launch: config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"},
		Mounts: []config.MountConfig{{Target: "/data", Type: "disk", Source: "data"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}
