package uffd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"golang.org/x/sys/unix"
)

type releaseCaptureQuiescer struct{ frozen bool }

func (q *releaseCaptureQuiescer) Quiesce() { q.frozen = true }
func (q *releaseCaptureQuiescer) Resume()  { q.frozen = false }

// Real UFFD/Go handler -> Take -> actual S carrier -> layered restore -> real
// UFFD/Go handler. CH's HTTP lifecycle is simulated here; the CH patch separately
// tests its production inflate/reporting helpers against real UFFD.
func TestReleaseCaptureRestoreWithoutRefault(t *testing.T) {
	ctx := context.Background()
	const pages = 4
	size := pages * PageSize
	source := newRecordingSnapshot(uint64(size), sparse.Data, 0x77)
	h, backend, cpu := realShmemHandler(t, pages, source)
	if err := h.EnsureLoaded(ctx, 3*PageSize, PageSize); err != nil {
		t.Fatal(err)
	}
	copy(backend[3*PageSize:], bytes.Repeat([]byte{0xa5}, PageSize))
	if h.state.Get(0) != StateAbsent {
		t.Fatal("target was materialized before release")
	}
	if err := unix.Madvise(cpu[:PageSize], unix.MADV_DONTNEED); err != nil {
		t.Fatal(err)
	}
	waitRemove(t, h)
	if h.state.Get(0) != StateReleased {
		t.Fatal("release was not recorded")
	}
	// Do not read or refault the released range before any capture below.

	parentBytes := bytes.Repeat([]byte{0x77}, size)
	parentMem, err := sparse.NewSource(bytes.NewReader(parentBytes), uint64(size), nil)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: "manifest://" + key})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := snapshotfile.BuildSource(parentMem, []byte("{}"), []byte("{}"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	parentSink := snapshot.NewFileSink(t.TempDir(), "parent", nil, false, nil)
	parentRef, parentPath, err := parentSink.AbsorbSnapshot(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := parentSink.CommitSnapshot(ctx, parentRef, parentPath); err != nil {
		t.Fatal(err)
	}
	if err := parentSink.Close(); err != nil {
		t.Fatal(err)
	}
	portable := &config.PortableSandboxConfig{
		Version:   1,
		Resources: config.PortableResourcesConfig{Capacity: config.CapacityConfig{CPU: 1, Memory: "1GiB"}, Allocatable: config.AllocatableConfig{CPU: 1, Memory: "1GiB"}},
		Boot:      config.PortableBootConfig{Kernel: "file://kernel@digest:" + key, Runtime: "file://runtime.bundle@digest:" + key, Root: config.PortableRootConfig{Base: "self"}},
		Launch:    config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"},
	}
	if err := portable.Validate(); err != nil {
		t.Fatal(err)
	}
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
				if err := os.WriteFile(filepath.Join(staging, name), []byte("{}"), 0600); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	q := &releaseCaptureQuiescer{}
	failLayout := true
	inputs := snapshot.Sources{
		SandboxID: "released", Context: ctx, APISock: sock, MemfdFD: h.cfg.MemfdFD, MemfdSize: int64(size),
		StagingDir: staging, PortableConfig: portable, CHApiDeadline: time.Second, Quiescer: q,
		MemoryLayout: func() ([]sparse.Extent, []sparse.Extent, error) {
			if !q.frozen {
				return nil, nil, errors.New("layout outside backend freeze")
			}
			if failLayout {
				return nil, nil, errors.New("injected layout failure")
			}
			return h.CaptureMemoryLayout(func() ([]sparse.Extent, error) { return snapshot.WalkHoles(h.cfg.MemfdFD, int64(size)) })
		},
		Diffs: []snapshot.DiskDiff{{SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			return bytes.NewReader(make([]byte, PageSize)), nil, nil
		}}},
	}
	if _, err := snapshot.Take(inputs, snapshot.NewFileSink(t.TempDir(), "released", nil, false, nil), true); err == nil || !strings.Contains(err.Error(), "injected layout failure") {
		t.Fatalf("failure injection: %v", err)
	}
	if q.frozen || h.state.Get(0) != StateReleased {
		t.Fatal("failed capture changed release state or retained freeze")
	}
	failLayout = false
	for i, merge := range []bool{false, true, false} {
		t.Run(fmt.Sprintf("capture_%d_merge_%t", i, merge), func(t *testing.T) {
			inputs.MemoryFromRefs = []string{parentRef}
			inputs.MergeBaseSnapshot = ""
			if merge {
				inputs.MergeBaseSnapshot = strings.Replace(parentRef, filepath.Base(parentPath), parentPath, 1)
				inputs.MemoryFromRefs = nil
			}
			out, err := snapshot.Take(inputs, snapshot.NewFileSink(t.TempDir(), "released", nil, false, nil), true)
			if err != nil {
				t.Fatal(err)
			}
			if h.state.Get(0) != StateReleased {
				t.Fatal("capture consumed release state or refaulted page")
			}
			stream, err := fetch.OpenTarStream(out.SnapshotPath)
			if err != nil {
				t.Fatal(err)
			}
			root, err := snapshotfile.Open(ctx, stream)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			run, err := root.Memory.RunAt(0, PageSize)
			if err != nil || run.Kind() == sparse.Hole {
				t.Fatalf("release became inheritance: run=%v err=%v", run, err)
			}
			var restored fetch.Stream = root.Memory
			if !merge {
				p, err := fetch.OpenTarStream(parentPath)
				if err != nil {
					t.Fatal(err)
				}
				parentRoot, err := snapshotfile.Open(ctx, p)
				if err != nil {
					t.Fatal(err)
				}
				defer parentRoot.Close()
				restored = fetch.NewLayered(root.Memory, parentRoot.Memory)
			}
			reader, err := NewStreamSnapshotSource(restored, uint64(size))
			if err != nil {
				t.Fatal(err)
			}
			recovered, bytesOut, _ := realShmemHandler(t, pages, reader)
			if err := recovered.EnsureLoaded(ctx, 0, uint64(size)); err != nil {
				t.Fatal(err)
			}
			want := bytes.Repeat([]byte{0x77}, size)
			clear(want[:PageSize])
			copy(want[3*PageSize:], bytes.Repeat([]byte{0xa5}, PageSize))
			if !bytes.Equal(bytesOut, want) {
				t.Fatal("complete restored memory differs after release without refault")
			}
			if err := recovered.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCaptureMemoryLayoutLoadedHoleAndResidentReuse(t *testing.T) {
	h := &Handler{cfg: Config{Size: 4 * PageSize}, state: NewPageStateMap(4)}
	h.state.Set(1, StateReleased)
	h.state.Set(2, StateLoaded)
	h.state.Set(3, StateReleased) // Resident bytes override stale release metadata.
	holes, zeros, err := h.CaptureMemoryLayout(func() ([]sparse.Extent, error) { return []sparse.Extent{{Offset: 0, Size: 3 * PageSize}}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(holes, []sparse.Extent{{Offset: 0, Size: PageSize}}) || !reflect.DeepEqual(zeros, []sparse.Extent{{Offset: PageSize, Size: 2 * PageSize}}) {
		t.Fatalf("holes=%v zeros=%v", holes, zeros)
	}
	if h.state.Get(1) != StateReleased || h.state.Get(2) != StateLoaded {
		t.Fatal("capture reset existing state")
	}
}
