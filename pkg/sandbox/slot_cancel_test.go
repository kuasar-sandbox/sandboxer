package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

type retrySlotTemplateFetcher struct {
	readStarted chan struct{}
	readOnce    sync.Once
	opens       atomic.Int32
	closes      atomic.Int32
	recovered   atomic.Bool
	afterOpen   func()
}

func (f *retrySlotTemplateFetcher) OpenManifest(ctx context.Context, key store.ContentKey) (fetch.Stream, error) {
	return f.OpenVerifiedManifest(ctx, key)
}

func (f *retrySlotTemplateFetcher) OpenVerifiedManifest(context.Context, store.ContentKey) (fetch.Stream, error) {
	opened := f.opens.Add(1)
	data := bytes.Repeat([]byte{0x5a}, 16<<10)
	source, err := sparse.NewSource(bytes.NewReader(data), uint64(len(data)), nil)
	if err != nil {
		return nil, err
	}
	return &retrySlotTemplateStream{Source: source, owner: f, initializer: opened > 1}, nil
}

type retrySlotTemplateStream struct {
	sparse.Source
	owner       *retrySlotTemplateFetcher
	initializer bool
	sizeOnce    sync.Once
}

func (s *retrySlotTemplateStream) Size() uint64 {
	if s.initializer && s.owner.afterOpen != nil {
		s.sizeOnce.Do(s.owner.afterOpen)
	}
	return s.Source.Size()
}

func (s *retrySlotTemplateStream) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	if s.owner.recovered.Load() {
		return s.Source.ReadAt(ctx, p, off)
	}
	s.owner.readOnce.Do(func() { close(s.owner.readStarted) })
	return 0, readerr.Mark(errors.New("temporary slot template source failure"), true)
}

func (s *retrySlotTemplateStream) Close() error {
	s.owner.closes.Add(1)
	return nil
}

func TestCurrentRunSlotBindCancellationDiscardsUncommittedWriter(t *testing.T) {
	testSlotBindCancellation(t, true)
}

func TestCheckpointSlotBindFailureDiscardsOnlyUncommittedWriter(t *testing.T) {
	testSlotBindCancellation(t, false)
}

func testSlotBindCancellation(t *testing.T, currentRun bool) {
	t.Helper()
	ctx := t.Context()
	fetcher := &retrySlotTemplateFetcher{readStarted: make(chan struct{})}
	storage, err := artifact.NewProcessStorageWithLocalAccess(nil, nil, nil, false, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	directory := t.TempDir()
	owner, err := diffstore.LockDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	slot := &config.DiskSlotConfig{}
	cfg := &config.SandboxConfig{Boot: config.BootConfig{Disks: []config.DiskConfig{{Name: "data", Slot: slot}}}}
	portable := runFromDirectPortable(t)
	portable.Version = 2
	portable.Boot.Disks = []config.PortableDiskConfig{{Name: "data", Slot: slot}}
	portable.Mounts = []config.MountConfig{{Type: "disk", Source: "data", Target: "/data"}}
	bases, err := portable.Checkpointed("")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareCheckpointDiskPlan(cfg, bases, directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := plan.Disks[0]
	rootWriter, err := diffstore.Create(root.Path, diffstore.Options{DiffID: root.DiffID, BaseID: root.BaseID, LogicalSize: 8192})
	if err != nil {
		t.Fatal(err)
	}
	if err := rootWriter.Close(); err != nil {
		t.Fatal(err)
	}
	run := &artifact.CurrentRun{SandboxID: "slot", InstanceID: "instance", RunID: "run"}
	metadataPath := filepath.Join(directory, "current-disks.json")
	var before []byte
	if currentRun {
		if err := RetainCurrentDiskPlan(ctx, storage, owner, run, cfg, portable, "", plan, []int64{8192, 0}, nil); err != nil {
			t.Fatal(err)
		}
		before, err = os.ReadFile(metadataPath)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		run = nil
	}
	cache, err := diffstore.NewCache(64<<10, 32<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	set, err := vhost.NewBackendSet([]vhost.BlockDeviceSpec{{Slot: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := set.Bind([]vhost.Backend{nil}); err != nil {
		t.Fatal(err)
	}
	server := vhost.NewServer("", set.DeviceBackends()[0], func(string, ...any) {})
	handler := &SnapshotHandler{Cfg: cfg, PortableConfig: portable, Disks: []SnapDiskRef{{Size: 8192}, {Slot: true}}, Servers: []*vhost.Server{server}}
	runtime := &Runtime{
		state: runtimeRunning, lifeCtx: ctx, baseDir: directory, diskStorage: storage, diskCache: cache,
		diskBackends: set, diskSnapshot: handler, checkpointOwner: owner,
		spec:          RuntimeSpec{EnableCheckpoint: true, CurrentRun: run, checkpointPlan: plan},
		diskInventory: []diskEntry{{reserved: true, dynamic: true}},
	}
	defer runtime.closeBoundDisks()
	// Recover before deferred retained-writer cleanup if an assertion fails.
	defer fetcher.recovered.Store(true)
	runtime.diskInventory[0].Name = "data"
	runtime.diskInventory[0].State = diskUnbound
	request := BindDiskRequest{SlotName: "data", Disk: config.DiskV1{Kind: "disk", Version: 1, DiffTemplate: "manifest://" + strings.Repeat("a", 64)}}
	bindCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	attemptStarted := fetcher.readStarted
	if !currentRun {
		// Cancel after the lazy factory has transferred source ownership to
		// OpenDisk, before BindDisk's next context check. This deterministically
		// covers the non-CurrentRun checkpoint transaction's failure window.
		attemptStarted = make(chan struct{})
		fetcher.afterOpen = func() { cancel(); close(attemptStarted) }
	}
	result := make(chan error, 1)
	go func() { result <- runtime.BindDisk(bindCtx, request) }()
	select {
	case <-attemptStarted:
	case err := <-result:
		// The post-open cancellation branch can finish before this goroutine
		// is scheduled; its source callback must still have run.
		select {
		case <-attemptStarted:
			result <- err
		default:
			t.Fatalf("bind returned before template initialization: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bind never started template initialization")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled bind returned %v", err)
		}
	case <-time.After(3 * time.Second):
		// Recover the test source before reporting a retained-Close regression,
		// so an erroneous materializer can leave the test process cleanly.
		fetcher.recovered.Store(true)
		select {
		case <-result:
		case <-time.After(5 * time.Second):
		}
		t.Fatal("canceled bind waited for its uncommitted template to recover")
	}
	if fetcher.opens.Load() < 2 || fetcher.opens.Load() != fetcher.closes.Load() {
		t.Fatalf("opened %d template probes/readers, closed %d", fetcher.opens.Load(), fetcher.closes.Load())
	}
	if runtime.diskInventory[0].State != diskUnbound || len(runtime.diskOwned) != 0 || handler.PortableConfig.Boot.Disks[0].Binding != nil {
		t.Fatal("canceled bind published a disk or capture topology")
	}
	after, err := os.ReadFile(metadataPath)
	if currentRun && (err != nil || !bytes.Equal(before, after)) {
		t.Fatalf("canceled bind changed retained topology: %v", err)
	} else if !currentRun && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("standalone checkpoint bind published CurrentRun metadata: %v", err)
	}
	files, err := os.ReadDir(filepath.Join(directory, "diffs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		// Stable lock files are retained to coordinate later creators.
		if file.Name() != filepath.Base(root.Path) && !strings.HasSuffix(file.Name(), ".lock") {
			t.Fatalf("canceled bind left writer staging content: %s", file.Name())
		}
	}
	if err := handler.capture.begin(); err != nil {
		t.Fatalf("canceled bind retained the capture gate: %v", err)
	}
	handler.capture.finish(false)

	// The same slot remains usable. A successful CurrentRun bind must finish
	// verification and durable materialization before returning, and Close
	// must retain that now-published disk independently of the template.
	fetcher.afterOpen = nil
	if currentRun {
		fetcher.recovered.Store(true)
	}
	if err := runtime.BindDisk(ctx, request); err != nil {
		t.Fatal(err)
	}
	if len(runtime.diskOwned) != 1 || runtime.diskOwned[0].ownedDiff {
		t.Fatal("successful checkpoint bind did not retain its writer")
	}
	if currentRun && fetcher.opens.Load() != fetcher.closes.Load() {
		t.Fatal("successful CurrentRun bind returned before releasing its template")
	}
	bound := runtime.spec.checkpointPlan.Disks[1]
	if _, err := os.Stat(bound.Path); currentRun && err != nil {
		t.Fatalf("successful bind did not publish its writer: %v", err)
	}
	if currentRun {
		if _, err := set.DeviceBackends()[0].WriteAt([]byte("saved"), 0); err != nil {
			t.Fatal(err)
		}
		if err := runtime.closeBoundDisks(); err != nil {
			t.Fatal(err)
		}
	} else {
		// Successful standalone checkpoint binding remains asynchronous, and
		// its ordinary Close must finish the retained initializer, not abort it.
		closed := make(chan error, 1)
		go func() { closed <- runtime.closeBoundDisks() }()
		select {
		case err := <-closed:
			t.Fatalf("successful checkpoint writer discarded its pending template: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		fetcher.recovered.Store(true)
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("retained Close did not finish after source recovery")
		}
	}
	reopened, err := diffstore.OpenDisk(bound.Path, nil, diffstore.DiffInit{Existing: true}, diffstore.WithCheckpoint(bound.DiffID, bound.BaseID))
	if err != nil {
		t.Fatalf("successful bind lost its retained writer on Close: %v", err)
	}
	defer reopened.Close()
	got := make([]byte, 8)
	want := "ZZZZZZZZ"
	if currentRun {
		want = "savedZZZ"
	}
	if _, err := reopened.ReadAt(got, 0); err != nil || string(got) != want {
		t.Fatalf("reopened retained content %q: %v", got, err)
	}
}
