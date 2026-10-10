package sandbox

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRuntimeDeviceSpecsPreserveLogicalTopology(t *testing.T) {
	got, err := runtimeDeviceSpecs([]RuntimeDiskSpec{{Overlay: true, BaseCapacity: 4096, WritableCapacity: 8192}, {Name: "data", WritableCapacity: 16384}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got[0].ReadOnly || got[0].Capacity != 4096 || got[1].ReadOnly || got[1].Capacity != 8192 || got[2].Capacity != 16384 {
		t.Fatalf("unexpected device topology: %+v", got)
	}
	for _, bad := range [][]RuntimeDiskSpec{nil, {{WritableCapacity: 0}}, {{Overlay: true, WritableCapacity: 4096}}, {{BaseCapacity: 4096, WritableCapacity: 4096}}} {
		if _, err := runtimeDeviceSpecs(bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestRuntimeWaitContextDoesNotOwnLifetime(t *testing.T) {
	life, cancel := context.WithCancelCause(context.Background())
	r := &Runtime{state: runtimeRunning, lifeCtx: life, cancelLife: cancel, exitDone: make(chan struct{})}
	ctx, c := context.WithCancel(context.Background())
	c()
	if _, err := r.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait err=%v", err)
	}
	select {
	case <-life.Done():
		t.Fatal("Wait cancellation terminated runtime")
	default:
	}
	r.finishExit(ExitResult{Code: 7})
	got, err := r.Wait(context.Background())
	if err != nil || got.Code != 7 {
		t.Fatalf("Wait=%+v/%v", got, err)
	}
}

// These use a real child process and Unix management sockets, not a VM. They
// verify the owner/protocol boundary without claiming kernel/KVM coverage.
func sdkFixture(t *testing.T) (RuntimeSpec, LaunchSpec) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sdk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	kernel := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(kernel, []byte("test kernel"), 0600); err != nil {
		t.Fatal(err)
	}
	var footer bytes.Buffer
	zw := zip.NewWriter(&footer)
	if _, err := zw.CreateHeader(&zip.FileHeader{Name: tarstream.DigestMarkerPrefix + strings.Repeat("a", 64), Method: zip.Store}); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := make([]byte, 2<<20)
	copy(bundle[len(bundle)-footer.Len():], footer.Bytes())
	runtimePath := filepath.Join(dir, "runtime.bundle")
	if err := os.WriteFile(runtimePath, bundle, 0600); err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(dir, "template.ext4")
	data := make([]byte, 4096)
	data[1024+56] = 0x53
	data[1024+57] = 0xef
	if err := os.WriteFile(template, data, 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "test-ch")
	// All fixture paths are generated locally and quoted as shell literals.
	body := "#!/bin/sh\nfor arg do\n case \"$arg\" in cid=3,socket=*) export SDK_TEST_VSOCK=\"${arg#cid=3,socket=}\";; esac\ndone\nexec '" + strings.ReplaceAll(os.Args[0], "'", "'\\''") + "' -test.run='^TestSDKChild$'\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.SandboxConfig{}
	cfg.ApplyDefaults()
	cfg.Resources.Capacity.CPU = 1
	cfg.Resources.Capacity.Memory = "16MiB"
	cfg.Resources.Allocatable.CPU = 1
	cfg.Resources.Allocatable.Memory = "16MiB"
	shape := RuntimeSpec{Kernel: "file://" + kernel, Bundle: "file://" + runtimePath, Resources: cfg.Resources,
		Disks: []RuntimeDiskSpec{{Name: "root", WritableCapacity: 4096}}, CHBinary: script, RuntimeRoot: dir, BaseRoot: dir, SandboxID: "owner"}
	launch := LaunchSpec{Root: config.RootConfig{DiffTemplate: "file://" + template}, Process: config.LaunchConfig{Exec: "/bin/true", Restart: "never"}}
	return shape, launch
}

func TestSDKChild(t *testing.T) {
	sock := os.Getenv("SDK_TEST_VSOCK")
	if sock == "" {
		return
	}
	conn, err := net.Dial("unix", sock+"_5000")
	if err != nil {
		os.Exit(21)
	}
	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeHello, Phase: "runtime_ready"}); err != nil {
		os.Exit(22)
	}
	msg, err := proto.ReadMessage(conn)
	if err != nil || msg.Type != proto.TypeLaunch {
		os.Exit(23)
	}
	if os.Getenv("SDK_TEST_REJECT") == "1" {
		_ = proto.WriteMessage(conn, &proto.Message{Type: proto.TypeError, Msg: "injected launch failure"})
		select {}
	}
	if delay := os.Getenv("SDK_TEST_LAUNCH_ACK_DELAY"); delay != "" {
		d, err := time.ParseDuration(delay)
		if err != nil {
			os.Exit(28)
		}
		time.Sleep(d)
	}
	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeLaunchAck}); err != nil {
		os.Exit(24)
	}
	if _, err := proto.ReadMessage(conn); err != nil {
		os.Exit(25)
	}
	// Delay the application event independently of the per-message exchange.
	if delay := os.Getenv("SDK_TEST_APP_STARTED_DELAY"); delay != "" {
		d, err := time.ParseDuration(delay)
		if err != nil {
			os.Exit(27)
		}
		time.Sleep(d)
	}
	started, err := net.Dial("unix", sock+"_5000")
	if err != nil {
		os.Exit(26)
	}
	_ = proto.WriteMessage(started, &proto.Message{Type: proto.TypeAppStarted, PID: 42})
	_, _ = proto.ReadMessage(started)
	started.Close()
	// Keep the MUX transport alive until the owner terminates this child.
	_, _ = io.Copy(io.Discard, conn)
	os.Exit(0)
}

func TestSDKBaseLaunchCloseAndRetainedWait(t *testing.T) {
	shape, launch := sdkFixture(t)
	startCtx, cancelStart := context.WithCancel(context.Background())
	r, err := StartRuntime(startCtx, shape)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	cancelStart()
	if r.State() != "runtime_ready" {
		t.Fatal(r.State())
	}
	if _, err := r.Stats(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Snapshot(context.Background(), ctl.Request{}); err == nil {
		t.Fatal("base snapshot accepted")
	}
	if _, err := os.Stat(filepath.Join(r.runDir, "sandbox.runtime.cfg")); !os.IsNotExist(err) {
		t.Fatalf("base C0: %v", err)
	}
	if _, err := r.deviceSet.DeviceBackends()[0].ReadAt(make([]byte, 512), 0); !errors.Is(err, vhost.ErrBackendUnbound) {
		t.Fatal(err)
	}
	invalid := launch
	invalid.Process.PIDNamespace = "invalid"
	if err := r.Launch(context.Background(), invalid); err == nil || r.State() != "runtime_ready" {
		t.Fatalf("validation consumed runtime: %v/%s", err, r.State())
	}
	launchCtx, cancelLaunch := context.WithCancel(context.Background())
	if err := r.Launch(launchCtx, launch); err != nil {
		t.Fatal(err)
	}
	cancelLaunch()
	if r.State() != "running" {
		t.Fatal(r.State())
	}
	if err := r.Launch(context.Background(), launch); err == nil {
		t.Fatal("second workload accepted")
	}
	if _, err := r.deviceSet.DeviceBackends()[0].ReadAt(make([]byte, 512), 0); err != nil {
		t.Fatalf("read after operation cancel: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Wait(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _ = r.Close() })
	}
	wg.Wait()
	first, err := r.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		next, err := r.Wait(canceled)
		if err != nil || !reflect.DeepEqual(next, first) {
			t.Fatalf("retained exit changed: %+v/%v vs %+v", next, err, first)
		}
	}
	if _, err := os.Stat(r.runDir); !os.IsNotExist(err) {
		t.Fatalf("run directory leaked: %v", err)
	}
	if _, err := os.Stat(strings.TrimPrefix(launch.Root.DiffTemplate, "file://")); err != nil {
		t.Fatalf("caller template removed: %v", err)
	}
}

func TestSDKLaunchFailureTerminatesRuntime(t *testing.T) {
	t.Setenv("SDK_TEST_REJECT", "1")
	shape, launch := sdkFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := StartRuntime(ctx, shape)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Launch(ctx, launch); err == nil {
		t.Fatal("failed guest launch succeeded")
	}
	if r.State() != "closed" {
		t.Fatal(r.State())
	}
	if err := r.Launch(context.Background(), launch); err == nil {
		t.Fatal("replacement launch accepted")
	}
}

func TestSDKStartFailureCleansInternalRuntime(t *testing.T) {
	shape, launch := sdkFixture(t)
	shape.Disks[0].WritableCapacity = 8192
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch}); err == nil || r != nil {
		t.Fatalf("Start=%v/%v", r, err)
	}
	if _, err := os.Stat(filepath.Join(shape.RuntimeRoot, shape.SandboxID)); !os.IsNotExist(err) {
		t.Fatalf("internal runtime leaked: %v", err)
	}
}

func TestSDKConfigConversionPreservesEphemeralAndCapability(t *testing.T) {
	cfg := &config.SandboxConfig{Resources: config.ResourcesConfig{Control: config.ControlConfig{CgroupFD: 99}},
		Launch:         config.LaunchConfig{Exec: "/app", EphemeralEnv: map[string]string{"SECRET": "scoped"}, CgroupControl: true, PIDNamespace: "shared", User: "1000", Plugin: []config.PluginConfig{{Exec: "/sidecar"}}},
		EphemeralFiles: []config.FileConfig{{Path: "/token", Content: "scoped"}}, Metadata: map[string]string{"name": "workload"}}
	launch := LaunchSpecFromConfig(cfg)
	copy, err := launch.config(runtimeSpecFromConfig(cfg, nil).config())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(copy, cfg) {
		t.Fatalf("config lost fields:\n%+v\n%+v", copy, cfg)
	}
	copy.Launch.EphemeralEnv["SECRET"] = "changed"
	if cfg.Launch.EphemeralEnv["SECRET"] != "scoped" {
		t.Fatal("retained caller map")
	}
}

func TestSDKRetainedBaseReadSurvivesLaunchContext(t *testing.T) {
	shape, launch := sdkFixture(t)
	payload := make([]byte, 8192)
	payload[1080] = 0x53
	payload[1081] = 0xef
	copy(payload[4096:], "retained base")
	path, scheme, digest := writeDiskArtifact(t, t.TempDir(), "overlay", payload, nil)
	launch.Root = config.RootConfig{Base: fileRef(path, scheme, digest)}
	shape.Disks[0].WritableCapacity = int64(len(payload))
	ctx, cancel := context.WithCancel(context.Background())
	r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	cancel()
	got := make([]byte, 13)
	if _, err := r.disks[0].base.ReadAt(got, 4096); err != nil || string(got) != "retained base" {
		t.Fatalf("lazy read=%q/%v", got, err)
	}
}

func TestSDKLaunchCloseCancelsPreparationAndJoins(t *testing.T) {
	shape, launch := sdkFixture(t)
	// Block a real launch in artifact open, which must receive owner cancellation.
	blocked := &sdkBlockingFetcher{started: make(chan struct{})}
	launch.Root = config.RootConfig{Base: "manifest://" + strings.Repeat("b", 64)}
	launch.Fetcher = blocked
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := StartRuntime(ctx, shape)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Launch(ctx, launch) }()
	<-blocked.started
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled launch succeeded")
		}
	case <-ctx.Done():
		t.Fatal("launch preparation did not cancel")
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("Close did not join preparation")
	}
	if r.State() != "closed" {
		t.Fatal(r.State())
	}
}

type sdkBlockingFetcher struct {
	started chan struct{}
	once    sync.Once
}

func (f *sdkBlockingFetcher) OpenManifest(ctx context.Context, _ store.ContentKey) (fetch.Stream, error) {
	f.once.Do(func() { close(f.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestLaunchOutcomeLinearization(t *testing.T) {
	for _, first := range []string{"ack", "cancel", "close", "exit"} {
		t.Run(first, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &Runtime{state: runtimeLaunching, launchContext: ctx}
			switch first {
			case "cancel":
				cancel()
			case "close":
				r.state = runtimeClosing
			case "exit":
				r.stopForExit()
			}
			succeeded := r.commitLaunch()
			if succeeded != (first == "ack") {
				t.Fatalf("commit=%v", succeeded)
			}
			cancel()
			r.stopForExit()
			if r.commitLaunch() != succeeded {
				t.Fatal("terminal event rewrote retained launch outcome")
			}
		})
	}
}

func TestReconstructionOwnerWaitsForReadinessAndRetainsExit(t *testing.T) {
	entered, ready, exit := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		r   *Runtime
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		r, err := RunLifecycle(ctx, func(life context.Context) (int, error) {
			r := life.Value(runtimeOwnerKey{}).(*Runtime)
			close(entered)
			<-ready
			r.mu.Lock()
			r.state = runtimeRunning
			close(r.readyDone)
			r.mu.Unlock()
			select {
			case <-exit:
				return 7, nil
			case <-life.Done():
				return -1, life.Err()
			}
		})
		resultCh <- result{r, err}
	}()
	<-entered
	select {
	case <-resultCh:
		t.Fatal("restore returned before reconstruction ready")
	default:
	}
	close(ready)
	resultValue := <-resultCh
	if resultValue.err != nil {
		t.Fatal(resultValue.err)
	}
	cancel()
	if resultValue.r.lifeCtx.Err() != nil {
		t.Fatal("operation owns returned runtime")
	}
	close(exit)
	got, err := resultValue.r.Wait(context.Background())
	if err != nil || got.Code != 7 {
		t.Fatalf("exit=%+v/%v", got, err)
	}
}

func TestSDKDataOverlayExcludesImageMetadata(t *testing.T) {
	shape, launch := sdkFixture(t)
	var footer bytes.Buffer
	zw := zip.NewWriter(&footer)
	file, err := zw.CreateHeader(&zip.FileHeader{Name: "config.json", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte(`{"Cmd":["/bin/true"]}`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path, scheme, digest := writeDiskArtifact(t, t.TempDir(), "image", append(lifecycleEROFSFixture(), footer.Bytes()...), nil)
	shape.Disks = append(shape.Disks, RuntimeDiskSpec{Name: "data", Overlay: true, BaseCapacity: 4096, WritableCapacity: 4096})
	launch.Disks = []config.DiskConfig{{Name: "data", RootConfig: config.RootConfig{Base: fileRef(path, scheme, digest), Overlay: &config.OverlayConfig{DiffTemplate: launch.Root.DiffTemplate}}}}
	launch.Mounts = []config.MountConfig{{Type: "disk", Source: "data", Target: "/data"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := r.disks[1].reader.Size(); got != 4096 {
		t.Fatalf("metadata visible as device bytes: %d", got)
	}
	if len(r.deviceSet.DeviceBackends()) != 3 {
		t.Fatal("logical data disk topology changed")
	}
}

func TestSDKStdioSetupFailureTerminatesLaunch(t *testing.T) {
	shape, launch := sdkFixture(t)
	launch.Stdio.Stdout = stdio.Stream{Kind: stdio.StreamFile, Path: filepath.Join(shape.BaseRoot, "missing", "output")}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch}); r != nil || err == nil {
		t.Fatalf("stdio failure accepted: %v/%v", r, err)
	}
}

func TestSDKLocalPortableC0DoesNotRequireSourceBinding(t *testing.T) {
	shape, launch := sdkFixture(t)
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	ids, err := ResolvePortableProjection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	launch.PortableConfig, err = config.ProjectPortableCold(cfg, ids)
	if err != nil {
		t.Fatal(err)
	}
	if launch.SourceBinding != nil {
		t.Fatal("local cold config unexpectedly has source provenance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
}

// A launch may apply its workload for longer than one app-notify exchange.
// Both the default and an explicit socket deadline must leave that preparation
// to the operation context, while the eventual notification still uses the
// normal acknowledged protocol.
func TestSDKLaunchWaitIsNotAppNotifyDeadline(t *testing.T) {
	for _, deadline := range []string{"", "200ms"} {
		t.Run("app_notify="+deadline, func(t *testing.T) {
			t.Setenv("SDK_TEST_LAUNCH_ACK_DELAY", "600ms")
			shape, launch := sdkFixture(t)
			shape.Timeouts.AppNotify = deadline
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := StartRuntime(ctx, shape)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if err := r.Launch(ctx, launch); err != nil {
				t.Fatalf("valid delayed app_started rejected: %v", err)
			}
			if r.State() != "running" {
				t.Fatalf("state=%s", r.State())
			}
		})
	}
}

func TestSDKLaunchContextBoundsGuestPreparation(t *testing.T) {
	t.Setenv("SDK_TEST_LAUNCH_ACK_DELAY", "10s")
	shape, launch := sdkFixture(t)
	startCtx, cancelStart := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStart()
	r, err := StartRuntime(startCtx, shape)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := r.Launch(ctx, launch); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("guest preparation did not honor operation deadline: %v", err)
	}
	if r.State() != "closed" {
		t.Fatalf("failed Launch retained state=%s", r.State())
	}
}

func TestSDKMissingAppStartedRemainsBounded(t *testing.T) {
	t.Setenv("SDK_TEST_APP_STARTED_DELAY", "10s")
	shape, launch := sdkFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := StartRuntime(ctx, shape)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Launch(ctx, launch); err == nil || !strings.Contains(err.Error(), "app_started notification timed out after 2s") {
		t.Fatalf("lost notification no longer bounded: %v", err)
	}
	if r.State() != "closed" {
		t.Fatalf("state=%s", r.State())
	}
}
func TestSDKColdBudgetLogUsesAdmittedReservation(t *testing.T) {
	shape, _ := sdkFixture(t)
	observed := make(chan string, 1)
	shape.logf = func(f string, a ...any) {
		if f == "initial cold Budget reserved=%d" {
			observed <- fmt.Sprintf(f, a...)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := StartRuntime(ctx, shape)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	select {
	case got := <-observed:
		if got != "initial cold Budget reserved=16777216" {
			t.Fatal(got)
		}
	default:
		t.Fatal("admitted cold Budget was not recorded")
	}
}

func TestSDKLaunchStartTimeoutStillBoundsGuestPreparation(t *testing.T) {
	t.Setenv("SDK_TEST_LAUNCH_ACK_DELAY", "10s")
	shape, launch := sdkFixture(t)
	launch.Process.StartTimeout = "100ms"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := StartRuntime(ctx, shape)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	err = r.Launch(ctx, launch)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("launch.start_timeout did not fail before operation deadline: %v, context=%v", err, ctx.Err())
	}
	if r.State() != "closed" {
		t.Fatalf("state=%s", r.State())
	}
}
