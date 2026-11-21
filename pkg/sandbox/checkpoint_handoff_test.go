package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"golang.org/x/sys/unix"
)

func coldCheckpointHandoffFixture(t *testing.T) (RuntimeSpec, LaunchSpec, []string, [][]byte) {
	t.Helper()
	shape, launch := sdkFixture(t)
	shape.PathID = shape.SandboxID
	shape.RuntimeRoot = filepath.Join(shape.RuntimeRoot, "runs")
	shape.EnableCheckpoint = true
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	identities, err := ResolvePortableProjection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	portable, err := config.ProjectPortableCold(cfg, identities)
	if err != nil {
		t.Fatal(err)
	}
	portable, err = portable.Checkpointed("")
	if err != nil {
		t.Fatal(err)
	}
	launch.Root = config.RootConfig{}
	for _, name := range []string{"data-a", "data-b"} {
		shape.Disks = append(shape.Disks, RuntimeDiskSpec{Name: name, WritableCapacity: 4096})
		launch.Disks = append(launch.Disks, config.DiskConfig{Name: name})
		portable.Boot.Disks = append(portable.Boot.Disks, config.PortableDiskConfig{Name: name})
		mount := config.MountConfig{Type: "disk", Source: name, Target: "/" + name}
		launch.Mounts = append(launch.Mounts, mount)
		portable.Mounts = append(portable.Mounts, mount)
	}
	directory := filepath.Join(DefaultBaseDir(shape.BaseRoot, shape.PathID), "checkpoint")
	if err := os.MkdirAll(filepath.Join(directory, "diffs"), 0700); err != nil {
		t.Fatal(err)
	}
	cp := &sandboxfile.Checkpoint{Version: 1, CheckpointID: "source", Root: sandboxfile.CheckpointDisk{DiffID: "root"}}
	var paths []string
	var payloads [][]byte
	for i, id := range []string{"root", "data-a", "data-b"} {
		base := portable.Boot.Root
		if i > 0 {
			base = portable.Boot.Disks[i-1].PortableRootConfig
			cp.Disks = append(cp.Disks, sandboxfile.CheckpointNamedDisk{Name: id, DiffID: id})
		}
		path := filepath.Join(directory, "diffs", id+".diff")
		s, err := diffstore.Create(path, diffstore.Options{DiffID: id, BaseID: artifact.DiffCheckpointBaseID(base), LogicalSize: 4096})
		if err != nil {
			t.Fatal(err)
		}
		payload := bytes.Repeat([]byte{byte(i + 1)}, 4096)
		payload[1024+56], payload[1024+57] = 0x53, 0xef
		if _, err := s.WriteAt(payload, 0); err != nil {
			t.Fatal(err)
		}
		if err := s.Checkpoint(context.Background(), cp.CheckpointID); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
		payloads = append(payloads, payload)
	}
	source, err := sandboxfile.BuildCheckpointSource(context.Background(), portable, cp)
	if err != nil {
		t.Fatal(err)
	}
	ref, path, err := snapshot.NewFileSink(directory, "source", nil, false, nil).AbsorbSandbox(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	launch.PortableConfig = portable
	launch.SourceBinding = &RunSourceBinding{SandboxRef: ref, RuntimeRef: "file://" + path, RelativeDir: directory, Checkpoint: cp}
	return shape, launch, paths, payloads
}

// The reader byte is the actual OFD lifetime lock shared by diffstore Views;
// probe it independently of visible checkpoint names and GC scheduling.
func checkpointHasReaders(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path+".lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 2, Len: 1}
	if err := unix.FcntlFlock(f.Fd(), unix.F_OFD_GETLK, &lock); err != nil {
		t.Fatal(err)
	}
	return lock.Type != unix.F_UNLCK
}

func assertCheckpointWriters(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		s, err := diffstore.OpenMetadata(path)
		if s != nil {
			s.Close()
		}
		if !errors.Is(err, diffstore.ErrBusy) {
			t.Fatalf("runtime did not retain writer %s: %v", path, err)
		}
	}
}

func assertCheckpointHandoff(t *testing.T, r *Runtime, paths []string, payloads [][]byte) {
	t.Helper()
	assertCheckpointWriters(t, paths)
	if owner, err := diffstore.LockDirectory(r.spec.checkpointPlan.Directory); !errors.Is(err, diffstore.ErrBusy) {
		if owner != nil {
			owner.Close()
		}
		t.Fatal("handoff released directory owner", err)
	}
	for i, path := range paths {
		if checkpointHasReaders(t, path) {
			t.Fatalf("restore reader survived successful Launch: %s", path)
		}
		ids, err := r.disks[i].cow.ListCheckpoints(context.Background())
		if err != nil || len(ids) != 1 || ids[0] != "source" {
			t.Fatal("source handoff changed checkpoint visibility", ids, err)
		}
		got := make([]byte, len(payloads[i]))
		if _, err := r.disks[i].cow.ReadAt(got, 0); err != nil || !bytes.Equal(got, payloads[i]) {
			t.Fatalf("disk %d content after handoff: %v", i, err)
		}
		update := bytes.Repeat([]byte{byte(10 + i)}, 512)
		if _, err := r.disks[i].cow.WriteAt(update, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := r.disks[i].cow.ReadAt(got[:512], 0); err != nil || !bytes.Equal(got[:512], update) {
			t.Fatalf("disk %d write after handoff: %v", i, err)
		}
	}
}

func TestColdCheckpointSDKHandoffReleasesOnlySourceReaders(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("split=%v", split), func(t *testing.T) {
			shape, launch, paths, payloads := coldCheckpointHandoffFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var r *Runtime
			var err error
			if split {
				r, err = StartRuntime(ctx, shape)
				if err == nil {
					err = r.Launch(ctx, launch)
				}
			} else {
				r, err = Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
			}
			if r != nil {
				defer r.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			assertCheckpointHandoff(t, r, paths, payloads)
		})
	}
}

func TestColdCheckpointRunHandoff(t *testing.T) {
	shape, launch, paths, _ := coldCheckpointHandoffFixture(t)
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, RunOptions{EnableCheckpoint: true, Cfg: cfg, PortableConfig: launch.PortableConfig, SourceBinding: launch.SourceBinding,
			SandboxID: shape.SandboxID, PathID: shape.PathID, CHBinary: shape.CHBinary, RuntimeRoot: shape.RuntimeRoot, BaseRoot: shape.BaseRoot,
			NotifyReadiness: func(event ReadinessEvent) {
				if event == ReadinessReady {
					close(ready)
				}
			}})
		done <- err
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run ended before ready: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assertCheckpointWriters(t, paths)
	for _, path := range paths {
		if checkpointHasReaders(t, path) {
			t.Fatalf("CLI Run retained restore readers after ready: %s", path)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not finish cancellation")
	}
}

func checkpointHandoffOptions(t *testing.T, shape RuntimeSpec, launch LaunchSpec) RunOptions {
	t.Helper()
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	return RunOptions{EnableCheckpoint: shape.EnableCheckpoint, Cfg: cfg, PortableConfig: launch.PortableConfig, SourceBinding: launch.SourceBinding,
		SandboxID: shape.SandboxID, PathID: shape.PathID, CHBinary: shape.CHBinary, RuntimeRoot: shape.RuntimeRoot, BaseRoot: shape.BaseRoot}
}

func TestColdCheckpointHandoffWaitsForEveryWriter(t *testing.T) {
	for _, mode := range []string{"success", "last-writer-failure", "close-error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			shape, launch, paths, _ := coldCheckpointHandoffFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			opts, release, err := prepareColdCheckpoint(ctx, checkpointHandoffOptions(t, shape, launch))
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			for _, path := range paths {
				if !checkpointHasReaders(t, path) {
					t.Fatal("reopen validation prematurely released source readers", path)
				}
			}
			var blocker *diffstore.Store
			if mode == "last-writer-failure" {
				blocker, err = diffstore.Open(paths[2], diffstore.Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer blocker.Close()
			}
			shape.checkpointPlan, shape.checkpointOwner = opts.checkpointPlan, opts.checkpointOwner
			shape.checkpointReleaseSources = opts.checkpointReleaseSources
			r, err := StartRuntime(ctx, shape)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			called := false
			closeErr := errors.New("injected source close error")
			r.spec.checkpointReleaseSources = func() error {
				called = true
				if len(r.disks) != 3 {
					t.Fatalf("released before every writer was registered: %d", len(r.disks))
				}
				assertCheckpointWriters(t, paths)
				for _, path := range paths {
					if !checkpointHasReaders(t, path) {
						t.Fatal("partial preparation released source reader", path)
					}
				}
				if err := opts.checkpointReleaseSources(); err != nil {
					return err
				}
				if mode == "close-error" {
					return closeErr
				}
				if mode == "cancel" {
					cancel()
				}
				return nil
			}
			// This is the same already-prepared ownership transfer used by Run;
			// native Start/Launch separately exercise preparation in Launch above.
			launch = LaunchSpecFromConfig(opts.Cfg)
			launch.legacyAccess = &opts
			launch.SourceBinding, launch.PortableConfig = opts.SourceBinding, opts.PortableConfig
			err = r.Launch(ctx, launch)
			if mode == "success" {
				if err != nil || !called {
					t.Fatal("successful handoff", called, err)
				}
			} else if err == nil {
				t.Fatal("startup failure was ignored")
			}
			if mode == "last-writer-failure" {
				if called || !errors.Is(err, diffstore.ErrBusy) {
					t.Fatal("incomplete writer set released source readers", called, err)
				}
				for _, path := range paths {
					if !checkpointHasReaders(t, path) {
						t.Fatal("failed prepare lost sources before failure cleanup", path)
					}
				}
			}
			if mode == "close-error" && !errors.Is(err, closeErr) {
				t.Fatal("source close failure did not propagate", err)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("handoff cancellation did not fail startup", err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
			if err := release(); err != nil {
				t.Fatal("repeated cleanup", err)
			}
			for _, path := range paths {
				if checkpointHasReaders(t, path) {
					t.Fatal("reader leaked after cleanup", path)
				}
			}
		})
	}
}

func TestColdCheckpointMaterializationEndsSourceReadLifetime(t *testing.T) {
	for _, mode := range []string{"cross-directory", "historical", "backend-disabled"} {
		t.Run(mode, func(t *testing.T) {
			shape, launch, paths, payloads := coldCheckpointHandoffFixture(t)
			opts := checkpointHandoffOptions(t, shape, launch)
			if mode == "cross-directory" {
				opts.BaseRoot = t.TempDir()
			} else if mode == "backend-disabled" {
				opts.EnableCheckpoint = false
			} else {
				s, err := diffstore.Open(paths[0], diffstore.Options{})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.WriteAt(bytes.Repeat([]byte{9}, 4096), 0); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			opts, release, err := prepareColdCheckpoint(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if opts.SourceBinding.Checkpoint != nil || opts.checkpointReleaseSources != nil {
				t.Fatal("materialization did not complete source handoff")
			}
			for _, path := range paths {
				if checkpointHasReaders(t, path) {
					t.Fatal("materialization retained source version", path)
				}
			}
			ref, err := manifest.ParseRef(opts.SourceBinding.RuntimeRef)
			if err != nil {
				t.Fatal(err)
			}
			opened, err := artifact.OpenFile(context.Background(), ref.Path, ref, nil, nil, nil, nil, false)
			if err != nil {
				t.Fatal("materialized file removed with source pins", err)
			}
			root, err := sandboxfile.Open(context.Background(), opened)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, 4096)
			if _, err := root.Payload.ReadAt(context.Background(), got, 0); err != nil || !bytes.Equal(got, payloads[0]) {
				t.Fatal("materialized source no longer readable", err)
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			if opts.EnableCheckpoint {
				if owner, err := diffstore.LockDirectory(opts.checkpointPlan.Directory); !errors.Is(err, diffstore.ErrBusy) {
					if owner != nil {
						owner.Close()
					}
					t.Fatal("source release closed target owner", err)
				}
			}
			materialDir := opts.SourceBinding.RelativeDir
			if err := release(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(materialDir); !os.IsNotExist(err) {
				t.Fatal("final cleanup retained materialization", err)
			}
		})
	}
}

func TestColdCheckpointPartialPinFailureReleasesEarlierDisks(t *testing.T) {
	shape, launch, paths, _ := coldCheckpointHandoffFixture(t)
	if err := os.Remove(paths[2]); err != nil {
		t.Fatal(err)
	}
	_, _, err := prepareColdCheckpoint(context.Background(), checkpointHandoffOptions(t, shape, launch))
	if err == nil {
		t.Fatal("missing final source was accepted")
	}
	for _, path := range paths[:2] {
		if checkpointHasReaders(t, path) {
			t.Fatal("partial pin set leaked after failure", path)
		}
	}
}

func TestColdCheckpointHandoffPreservesIndependentExporters(t *testing.T) {
	shape, launch, paths, payloads := coldCheckpointHandoffFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before, err := diffstore.OpenExport(ctx, paths[0], "source", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	after, err := diffstore.OpenExport(ctx, paths[0], "source", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	if err := r.disks[0].cow.DeleteCheckpoint(ctx, "source"); err != nil {
		t.Fatal(err)
	}
	if ids, err := r.disks[0].cow.ListCheckpoints(ctx); err != nil || len(ids) != 0 {
		t.Fatal("source name remained visible", ids, err)
	}
	if _, err := r.disks[0].cow.WriteAt(bytes.Repeat([]byte{9}, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if err := r.disks[0].cow.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	for _, view := range []*diffstore.View{before, after} {
		got := make([]byte, 4096)
		if _, err := view.ReadAt(ctx, got, 0); err != nil || !bytes.Equal(got, payloads[0]) {
			t.Fatal("independent exporter lost fixed contents", err)
		}
	}
	if err := before.Close(); err != nil || !checkpointHasReaders(t, paths[0]) {
		t.Fatal("closing earlier exporter released later exporter", err)
	}
	if err := after.Close(); err != nil || checkpointHasReaders(t, paths[0]) {
		t.Fatal("restore reader retained deleted version after last exporter closed", err)
	}
}

func TestRestoredDiskSourceFenceDoesNotRequireColdLayoutMarker(t *testing.T) {
	shape, launch, paths, _ := coldCheckpointHandoffFixture(t)
	ctx := context.Background()
	binding := *launch.SourceBinding
	ref, err := manifest.ParseRef(binding.SandboxRef)
	if err != nil {
		t.Fatal(err)
	}
	ref.Path = filepath.Join(binding.RelativeDir, ref.Path)
	binding.RuntimeRef = ref.String()
	// The memory restore entry supplies resolved E provenance without the
	// cold-launch layout marker. Its actual immutable graph still owns DIFFs.
	binding.Checkpoint = nil
	opts := RunOptions{checkpointDir: filepath.Join(t.TempDir(), "unused", "checkpoint"), SandboxID: shape.SandboxID, SourceBinding: &binding}
	guard, err := protectCaptureParents(ctx, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	storage, err := artifact.NewProcessStorage(&config.ManifestConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	parent := artifact.Checkpoint{Directory: binding.RelativeDir, SandboxID: shape.SandboxID, Retain: []artifact.CheckpointSource{}}
	version := artifact.CheckpointRef{ID: "source", DiffID: "root"}
	short, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err = storage.DeleteCheckpoints(short, parent, []artifact.CheckpointRef{version}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("restored source was not fenced: %v", err)
	}
	if err = guard.Close(); err != nil {
		t.Fatal(err)
	}
	// Restoring the latest E can keep its DIFF open as the active writer.
	live, err := diffstore.Open(paths[0], diffstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	liveGuard, err := protectCaptureParents(ctx, opts, false)
	if err != nil {
		t.Fatal("parent validation conflicted with the restored writer", err)
	}
	if err := liveGuard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	view, err := diffstore.OpenExport(ctx, paths[0], "source", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if err = storage.DeleteCheckpoints(ctx, parent, []artifact.CheckpointRef{version}, nil); err != nil {
		t.Fatal(err)
	}
	if guard, err = protectCaptureParents(ctx, opts, false); err == nil {
		guard.Close()
		t.Fatal("existing restored reader authorized new edge after revocation")
	}
}

func TestCaptureParentProtectionWithoutCheckpointDirectory(t *testing.T) {
	for _, memory := range []bool{false, true} {
		opts := RunOptions{SandboxID: "ordinary", checkpointDir: filepath.Join(t.TempDir(), "checkpoint")}
		if memory {
			opts.SourceBinding = &RunSourceBinding{SandboxRef: "manifest://" + strings.Repeat("0", 64)}
		}
		guard, err := protectCaptureParents(context.Background(), opts, memory)
		if err != nil {
			t.Fatalf("ordinary capture memory=%t: %v", memory, err)
		}
		if guard != nil {
			if err := guard.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(opts.checkpointDir); !os.IsNotExist(err) {
			t.Fatalf("ordinary capture created unused checkpoint directory: %v", err)
		}
	}
}
