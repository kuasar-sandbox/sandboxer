package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
)

func TestReadinessEmitterOrdersAndDeduplicatesEvents(t *testing.T) {
	var got []ReadinessEvent
	e := newReadinessEmitter(func(event ReadinessEvent) { got = append(got, event) })
	e.notifyReady() // fail closed at the emitter boundary; do not reorder.
	e.notifyControlReady()
	e.notifyControlReady()
	e.notifyReady()
	e.notifyReady() // models a later in-place app restart.
	want := []ReadinessEvent{ReadinessControlReady, ReadinessReady}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRestoreLifecycleStartsAndClosesCheckpointMaintenanceBeforeOwnerRelease(t *testing.T) {
	for _, fail := range []bool{true, false} {
		t.Run(fmt.Sprint("initialization-failure=", fail), func(t *testing.T) { testRestoreMaintenanceLifecycle(t, fail) })
	}
}

func testRestoreMaintenanceLifecycle(t *testing.T, fail bool) {
	baseDir, runDir := t.TempDir(), t.TempDir()
	directory := filepath.Join(baseDir, "checkpoint")
	owner, err := diffstore.LockDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	historyPath := filepath.Join(directory, "diffs", "history.diff")
	s, err := diffstore.Create(historyPath, diffstore.Options{DiffID: "history", LogicalSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Checkpoint(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// SDK restores inject a Runtime just like pool instances, but their disks
	// and target owner are already bound; there is no later installLaunch.
	input, release := io.Pipe()
	defer input.Close()
	defer release.Close()
	runtime, err := RunLifecycle(context.Background(), func(ctx context.Context) (int, error) {
		code, runErr := ServeAndWait(VMParams{
			Ctx: ctx, EnableCheckpoint: true, CheckpointOwner: owner, BaseDir: baseDir, RunDir: runDir,
			SandboxID: "restore-maintenance", Logf: func(string, ...any) {}, CapBytes: 4096,
			UffdSource: uffd.ZeroSource{}, LaunchSpec: &proto.LaunchSpec{}, SnapCfg: &config.SandboxConfig{},
			BuildCmd: func(CmdEnv) (*exec.Cmd, func(), error) {
				r := ctx.Value(runtimeOwnerKey{}).(*Runtime)
				refs, e := r.listCheckpoints(ctx)
				if e != nil || len(refs) != 1 || refs[0].ID != "old" {
					t.Errorf("restore manager was not initialized: %v %v", refs, e)
				}
				if fail {
					return nil, func() {}, errors.New("injected restore build failure")
				}
				cmd := exec.Command("/bin/sh", "-c", "read done")
				cmd.Stdin = input
				return cmd, func() {}, nil
			},
			PostSpawn: func(p PostSpawnCtx) error { p.NotifyReady(); return nil },
		})
		// This runs before RunLifecycle.finishExit, matching restore.Run's
		// outer active-disk/owner defers. No maintenance writer may survive.
		if e := owner.Validate(); e != nil {
			t.Errorf("borrower released target owner: %v", e)
		}
		meta, e := diffstore.OpenMetadata(historyPath)
		if e != nil {
			t.Errorf("history handle survived ServeAndWait: %v", e)
		} else {
			meta.Close()
		}
		return code, runErr
	})
	if fail {
		if err == nil || !strings.Contains(err.Error(), "injected restore build failure") {
			t.Fatal(err)
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if _, err = release.Write([]byte("done\n")); err != nil {
			t.Fatal(err)
		}
		release.Close()
		exit, err := runtime.Wait(context.Background())
		if err != nil || exit.Err != nil || exit.Code != 0 {
			t.Fatal(exit, err)
		}
	}
}

func TestPoolBeforeLaunchDoesNotAcquireCheckpointDirectory(t *testing.T) {
	baseDir := t.TempDir()
	_, err := RunLifecycle(context.Background(), func(ctx context.Context) (int, error) {
		r := ctx.Value(runtimeOwnerKey{}).(*Runtime)
		r.spec.EnableCheckpoint = true
		return ServeAndWait(VMParams{Ctx: ctx, BaseDir: baseDir, RunDir: t.TempDir(), SandboxID: "parked-pool", Logf: func(string, ...any) {},
			CapBytes: 4096, UffdSource: uffd.ZeroSource{}, LaunchSpec: &proto.LaunchSpec{}, SnapCfg: &config.SandboxConfig{},
			BuildCmd: func(CmdEnv) (*exec.Cmd, func(), error) {
				if _, e := os.Stat(filepath.Join(baseDir, "checkpoint")); !os.IsNotExist(e) {
					t.Errorf("parked pool took a business directory: %v", e)
				}
				if r.checkpointMaintenance != nil || r.checkpointOwner != nil {
					t.Error("parked pool acquired checkpoint resources")
				}
				return nil, func() {}, errors.New("parked pool test finished")
			},
		})
	})
	if err == nil || !strings.Contains(err.Error(), "parked pool test finished") {
		t.Fatal(err)
	}
}

func TestReadinessEmitterNilNotifierHasNoEffect(t *testing.T) {
	e := newReadinessEmitter(nil)
	e.notifyControlReady()
	e.notifyReady()
}

func TestReadinessEmitterConcurrentNotifications(t *testing.T) {
	var mu sync.Mutex
	var got []ReadinessEvent
	e := newReadinessEmitter(func(event ReadinessEvent) {
		mu.Lock()
		got = append(got, event)
		mu.Unlock()
	})
	e.notifyControlReady()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); e.notifyControlReady() }()
		go func() { defer wg.Done(); e.notifyReady() }()
	}
	wg.Wait()
	want := []ReadinessEvent{ReadinessControlReady, ReadinessReady}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestServeAndWaitEmitsControlReadyOnlyAfterCtlListen(t *testing.T) {
	runDir := t.TempDir()
	var got []ReadinessEvent
	_, err := ServeAndWait(VMParams{
		Ctx:        context.Background(),
		SandboxID:  "readiness-test",
		RunDir:     runDir,
		Logf:       func(string, ...any) {},
		CapBytes:   4096,
		UffdSource: uffd.ZeroSource{},
		LaunchSpec: &proto.LaunchSpec{},
		SnapCfg:    &config.SandboxConfig{},
		NotifyReadiness: func(event ReadinessEvent) {
			got = append(got, event)
			if event != ReadinessControlReady {
				t.Fatalf("unexpected event %q", event)
			}
			conn, dialErr := net.Dial("unix", filepath.Join(runDir, "ctl.sock"))
			if dialErr != nil {
				t.Fatalf("control_ready before ctl.sock became connectable: %v", dialErr)
			}
			_ = conn.Close()
		},
		BuildCmd: func(CmdEnv) (*exec.Cmd, func(), error) {
			return nil, func() {}, errors.New("injected build failure")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected build failure") {
		t.Fatalf("ServeAndWait error = %v, want injected build failure", err)
	}
	if !reflect.DeepEqual(got, []ReadinessEvent{ReadinessControlReady}) {
		t.Fatalf("events = %v, want [control_ready]", got)
	}
}

func TestServeAndWaitBackendGuardFailureCannotPublishReadiness(t *testing.T) {
	runDir := t.TempDir()
	var got []ReadinessEvent
	spawned := false
	_, err := ServeAndWait(VMParams{
		Ctx: context.Background(), SandboxID: "guard-startup-failure", RunDir: runDir,
		Logf: func(string, ...any) {},
		// memory.Create accepts this mapping size, but the required 4 KiB
		// guard cannot register it. Failure must precede every consumer.
		CapBytes:        4097,
		NotifyReadiness: func(event ReadinessEvent) { got = append(got, event) },
		BuildCmd: func(CmdEnv) (*exec.Cmd, func(), error) {
			spawned = true
			return nil, func() {}, errors.New("unexpected command construction")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "protect backend memory") {
		t.Fatalf("expected mandatory guard failure, got %v", err)
	}
	if spawned || len(got) != 0 {
		t.Fatalf("guard failure exposed consumers/readiness: spawned=%t events=%v", spawned, got)
	}
	for _, name := range []string{"ctl.sock", "uffd.sock"} {
		if _, err := os.Stat(filepath.Join(runDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("guard failure left %s: %v", name, err)
		}
	}
}
