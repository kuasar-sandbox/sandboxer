package sandbox

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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
	"golang.org/x/sys/unix"
)

func TestValidateLocalMemoryRefsUsesOutputBundleDirectory(t *testing.T) {
	out := t.TempDir()
	path, scheme, digest := writeDiskArtifact(t, out, "snapshot", []byte("artifact"), nil)
	name := filepath.Base(path)
	manifestRef := "manifest://" + strings.Repeat("a", 64)
	locatedRef := "file://located.snapshot@location:parent"
	refs := []string{
		fileRef(filepath.Join("/different/source", name), scheme, digest),
		manifestRef,
		locatedRef,
	}
	if err := validateLocalMemoryRefs(out, refs, nil, false); err != nil {
		t.Fatalf("validateLocalMemoryRefs() error = %v", err)
	}

	if err := os.Symlink(name, filepath.Join(out, "alias.snapshot")); err != nil {
		t.Fatal(err)
	}
	if err := validateLocalMemoryRefs(out, []string{"file://alias.snapshot"}, nil, false); err != nil {
		t.Fatalf("accessible sibling symlink should be accepted: %v", err)
	}

	err := validateLocalMemoryRefs(out, []string{"file://missing.snapshot"}, nil, false)
	if err == nil || !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("missing local memory ref error = %v", err)
	}
}

func TestValidateLocalMemoryRefsRejectsNonRegularFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(string) error
	}{
		{
			name: "directory",
			setup: func(path string) error {
				return os.Mkdir(path, 0o755)
			},
		},
		{
			name: "fifo",
			setup: func(path string) error {
				return syscall.Mkfifo(path, 0o600)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := t.TempDir()
			path := filepath.Join(out, "base.snapshot")
			if err := tt.setup(path); err != nil {
				t.Fatal(err)
			}

			err := validateLocalMemoryRefs(out, []string{"file://base.snapshot"}, nil, false)
			if err == nil || !strings.Contains(err.Error(), "is not a regular file") {
				t.Fatalf("non-regular local memory ref error = %v", err)
			}
		})
	}
}

func TestValidateLocalMemoryRefsEnforcesCryptoPolicy(t *testing.T) {
	out := t.TempDir()
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x51})
	wrongCodec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x52})
	plainPath, plainScheme, plainDigest := writeDiskArtifact(t, out, "snapshot", []byte("plain memory"), nil)
	otherPath, _, _ := writeDiskArtifact(t, out, "snapshot", []byte("other memory"), nil)
	encryptedPath, encryptedScheme, encryptedDigest := writeDiskArtifact(t, out, "snapshot", []byte("encrypted memory"), codec)
	plainRef := fileRef(plainPath, plainScheme, plainDigest)
	encryptedRef := fileRef(encryptedPath, encryptedScheme, encryptedDigest)

	if err := validateLocalMemoryRefs(out, []string{plainRef}, codec, false); err != nil {
		t.Fatalf("auto rejected plaintext memory dependency: %v", err)
	}
	if err := validateLocalMemoryRefs(out, []string{plainRef}, codec, true); err == nil {
		t.Fatal("required accepted plaintext memory dependency")
	}
	if err := validateLocalMemoryRefs(out, []string{encryptedRef}, codec, true); err != nil {
		t.Fatalf("required rejected encrypted memory dependency: %v", err)
	}
	if err := validateLocalMemoryRefs(out, []string{encryptedRef}, wrongCodec, true); err == nil {
		t.Fatal("wrong key accepted encrypted memory dependency")
	}
	if err := validateLocalMemoryRefs(out, []string{fileRef(otherPath, plainScheme, plainDigest)}, nil, false); err == nil {
		t.Fatal("mismatched identity accepted memory dependency")
	}
}

func TestNormalizeLocalMemoryRefsEmitsSiblingBasenames(t *testing.T) {
	digest := strings.Repeat("b", 64)
	locatedRef := "file://located.snapshot@sha256:" + digest + "@location:parent"
	manifestRef := "manifest://" + strings.Repeat("a", 64)
	got, err := normalizeLocalMemoryRefs([]string{
		"file:///legacy/path/base.snapshot",
		"file://nested/parent.snapshot",
		locatedRef,
		manifestRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"file://base.snapshot", "file://parent.snapshot", locatedRef, manifestRef}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("normalized refs = %v, want %v", got, want)
	}

	if _, err := normalizeLocalMemoryRefs([]string{"file://.."}); err == nil {
		t.Fatal("normalizeLocalMemoryRefs(file://..) succeeded")
	}
}

func TestValidatePortableMemoryRefsAcceptsLocatedFileRefs(t *testing.T) {
	refs := []string{
		"manifest://" + strings.Repeat("a", 64),
		"file://base.snapshot@location:parent",
	}
	if err := validatePortableMemoryRefs(refs); err != nil {
		t.Fatalf("portable refs rejected: %v", err)
	}
	if err := validatePortableMemoryRefs([]string{"file://base.snapshot"}); err == nil {
		t.Fatal("unlocated local ref accepted for direct upload")
	}
}

func TestSnapshotMemoryRefsThreeGenerationWorkingSetChain(t *testing.T) {
	portable := "manifest://" + strings.Repeat("a", 64)
	prov := config.SnapshotProvenance{
		ParentSnapshotRef: "file:///bundle/w.snapshot",
		ParentFromRefs: []string{
			"file:///bundle/b.snapshot",
			portable,
		},
	}

	merged, err := snapshotMemoryRefs(prov, true)
	if err != nil {
		t.Fatal(err)
	}
	wantMerged := []string{"file://b.snapshot", portable}
	if strings.Join(merged, ",") != strings.Join(wantMerged, ",") {
		t.Fatalf("merged memory refs = %v, want %v", merged, wantMerged)
	}

	workingSet, err := snapshotMemoryRefs(prov, false)
	if err != nil {
		t.Fatal(err)
	}
	wantWorkingSet := []string{"file://w.snapshot", "file://b.snapshot", portable}
	if strings.Join(workingSet, ",") != strings.Join(wantWorkingSet, ",") {
		t.Fatalf("working-set memory refs = %v, want %v", workingSet, wantWorkingSet)
	}

	if prov.ParentFromRefs[0] != "file:///bundle/b.snapshot" {
		t.Fatalf("snapshotMemoryRefs mutated provenance: %v", prov.ParentFromRefs)
	}
}

func TestHandleSnapshotRequestRejectsMergedLocalLowerBeforeQuiesce(t *testing.T) {
	dir := t.TempDir()
	parentPath, parentScheme, parentDigest := writeDiskArtifact(t, dir, "snapshot", make([]byte, 4096), nil)
	cfg := &config.SandboxConfig{}
	cfg.SnapshotProvenance = config.SnapshotProvenance{
		ParentSnapshotRef:  fileRef(parentPath, parentScheme, parentDigest),
		ParentSnapshotPath: parentPath,
		ParentFromRefs:     []string{"file://base.snapshot"},
	}
	mfd, err := memory.Create("portable-chain-preflight", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer mfd.Close()
	viewCalled := false
	pinger := &guestlink.Pinger{
		Client: &guestlink.HostClient{BasePath: filepath.Join(dir, "must-not-dial.sock")},
	}

	_, err = handleSnapshotRequest(ctl.Request{Upload: true}, RunOptions{
		Cfg:         cfg,
		SandboxID:   "test",
		ManifestCfg: &config.ManifestConfig{Store: manifest.StoreConfig{Endpoint: "unused"}},
	}, mfd, []SnapDiskRef{{
		DiffPath: filepath.Join(dir, "diff"),
		Size:     4096,
		SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			viewCalled = true
			return bytes.NewReader(make([]byte, 4096)), nil, nil
		},
	}}, nil, "", filepath.Join(dir, "run"), "", nil, pinger, nil, nil, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "direct upload would retain a local memory lower") {
		t.Fatalf("local lower preflight error = %v", err)
	}
	if viewCalled {
		t.Fatal("snapshot view opened before final memory-chain validation")
	}
}

func TestHandleSnapshotRequestRejectsPredictableErrorsBeforeQuiesce(t *testing.T) {
	pinger := &guestlink.Pinger{
		Client: &guestlink.HostClient{BasePath: filepath.Join(t.TempDir(), "must-not-dial.sock")},
	}
	baseCfg := &config.SandboxConfig{}
	baseOpts := RunOptions{Cfg: baseCfg, SandboxID: "test"}
	disks := []SnapDiskRef{{DiffPath: filepath.Join(t.TempDir(), "not-needed.diff")}}

	_, err := handleSnapshotRequest(ctl.Request{}, baseOpts, nil, disks, nil,
		"", t.TempDir(), "", nil, pinger, nil, nil, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "--output and --upload") {
		t.Fatalf("missing output error = %v", err)
	}

	_, err = handleSnapshotRequest(ctl.Request{Upload: true}, baseOpts, nil, disks, nil,
		"", t.TempDir(), "", nil, pinger, nil, nil, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("missing manifest config error = %v", err)
	}
}

func TestHandleSnapshotRequestValidatesDiskMergeBaseBeforeQuiesce(t *testing.T) {
	dir := t.TempDir()
	diff := filepath.Join(dir, "must-not-be-opened.diff")
	digest := strings.Repeat("0", 64)
	cfg := &config.SandboxConfig{}
	cfg.SnapshotProvenance = config.SnapshotProvenance{
		ParentSnapshotRef: "file://base.snapshot@sha256:" + digest,
		ParentOverlayBase: "file://base.overlay@sha256:" + digest,
		ParentOverlayPath: filepath.Join(dir, "missing.overlay"),
	}
	pinger := &guestlink.Pinger{
		Client: &guestlink.HostClient{BasePath: filepath.Join(dir, "must-not-dial.sock")},
	}
	mergeRef := false
	req := ctl.Request{OutDir: filepath.Join(dir, "out"), MergeRef: &mergeRef}
	viewCalled := false

	_, err := handleSnapshotRequest(req, RunOptions{Cfg: cfg, SandboxID: "test"}, nil,
		[]SnapDiskRef{{
			DiffPath: diff,
			Size:     4096,
			SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
				viewCalled = true
				return bytes.NewReader(make([]byte, 4096)), nil, nil
			},
		}}, nil, "", filepath.Join(dir, "run"),
		"", nil, pinger, nil, nil, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "disk 0 merge base") {
		t.Fatalf("disk merge preflight error = %v", err)
	}
	if viewCalled {
		t.Fatal("snapshot view provider called during size preflight")
	}
}

func TestHandleSnapshotRequestResolvesUploadKeyBeforeSnapshotView(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.SandboxConfig{}
	viewCalled := false
	keyCalls := 0
	_, err := handleSnapshotRequest(ctl.Request{Upload: true}, RunOptions{
		Cfg:         cfg,
		SandboxID:   "test",
		ManifestCfg: &config.ManifestConfig{Store: manifest.StoreConfig{Endpoint: "unused"}},
		CustomerKeyFn: func() ([32]byte, error) {
			keyCalls++
			return [32]byte{}, errors.New("invalid customer key")
		},
	}, nil, []SnapDiskRef{{
		DiffPath: filepath.Join(dir, "diff"),
		Size:     4096,
		SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			viewCalled = true
			return bytes.NewReader(make([]byte, 4096)), nil, nil
		},
	}}, nil, "", filepath.Join(dir, "run"), "", nil, nil, nil, nil, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "customer key") {
		t.Fatalf("upload key error = %v", err)
	}
	if keyCalls != 1 {
		t.Fatalf("customer key calls=%d, want 1", keyCalls)
	}
	if viewCalled {
		t.Fatal("snapshot view was opened before customer-key validation")
	}
}

func TestHandleSnapshotRequestReattachesGuestAfterTakeFailure(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")
	listener, err := net.Listen("unix", base)
	if err != nil {
		t.Fatal(err)
	}
	memoryHighPath := filepath.Join(dir, "memory.high")
	if err := os.WriteFile(memoryHighPath, []byte("234881024\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("123456789\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.events.local"), []byte("low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var quiesceSawLiftedMemoryHigh atomic.Bool
	guestDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			guestDone <- acceptErr
			return
		}
		defer conn.Close()
		line := make([]byte, len(proto.HostConnectLine))
		if _, readErr := io.ReadFull(conn, line); readErr != nil {
			guestDone <- readErr
			return
		}
		if string(line) != string(proto.HostConnectLine) {
			guestDone <- fmt.Errorf("CONNECT line = %q", line)
			return
		}
		if _, writeErr := conn.Write([]byte("OK 1\n")); writeErr != nil {
			guestDone <- writeErr
			return
		}
		request, readErr := proto.ReadMessage(conn)
		if readErr != nil {
			guestDone <- readErr
			return
		}
		if request.Type != proto.TypeQuiesce {
			guestDone <- fmt.Errorf("request type = %q", request.Type)
			return
		}
		value, readErr := os.ReadFile(memoryHighPath)
		quiesceSawLiftedMemoryHigh.Store(readErr == nil && strings.TrimSpace(string(value)) == "max")
		guestDone <- proto.WriteMessage(conn, &proto.Message{
			Type:             proto.TypeQuiesced,
			DropCachesResult: proto.DropCachesSkipped,
		})
	}()
	t.Cleanup(func() { _ = listener.Close() })
	chSock := filepath.Join(dir, "ch.sock")
	chListener, err := net.Listen("unix", chSock)
	if err != nil {
		t.Fatal(err)
	}
	var resumed atomic.Bool
	var pauseSawLiftedMemoryHigh atomic.Bool
	chServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/vm.pause" {
			value, readErr := os.ReadFile(memoryHighPath)
			pauseSawLiftedMemoryHigh.Store(readErr == nil && strings.TrimSpace(string(value)) == "max")
		}
		if r.URL.Path == "/api/v1/vm.snapshot" {
			http.Error(w, "injected snapshot failure", http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/api/v1/vm.resume" {
			resumed.Store(true)
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	chDone := make(chan struct{})
	go func() {
		_ = chServer.Serve(chListener)
		close(chDone)
	}()
	t.Cleanup(func() {
		_ = chServer.Close()
		<-chDone
	})

	mfd, err := memory.Create("snapshot-failure-reattach", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer mfd.Close()
	reattachCalls := 0
	reattachedAfterResume := false
	_, err = handleSnapshotRequest(
		ctl.Request{OutDir: filepath.Join(dir, "out")},
		RunOptions{Cfg: &config.SandboxConfig{}, SandboxID: "test"},
		mfd,
		[]SnapDiskRef{{
			DiffPath: filepath.Join(dir, "diff"),
			Size:     4096,
			SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
				return bytes.NewReader(make([]byte, 4096)), nil, nil
			},
		}},
		nil,
		chSock,
		filepath.Join(dir, "run"),
		dir,
		nil,
		&guestlink.Pinger{Client: &guestlink.HostClient{BasePath: base}},
		nil,
		func() error {
			reattachCalls++
			if !resumed.Load() {
				return errors.New("reattach ran before VM resume")
			}
			reattachedAfterResume = true
			return nil
		},
		nil,
		discardLogf,
	)
	if err == nil || !strings.Contains(err.Error(), "CH snapshot") {
		t.Fatalf("snapshot error = %v, want CH snapshot failure", err)
	}
	if guestErr := <-guestDone; guestErr != nil {
		t.Fatal(guestErr)
	}
	if reattachCalls != 1 {
		t.Fatalf("reattach calls = %d, want 1", reattachCalls)
	}
	if !resumed.Load() {
		t.Fatal("VM was not resumed before failed snapshot returned")
	}
	if !reattachedAfterResume {
		t.Fatal("guest was not reattached after VM resume")
	}
	if !quiesceSawLiftedMemoryHigh.Load() {
		t.Fatal("guest quiesce did not run with memory.high lifted")
	}
	if !pauseSawLiftedMemoryHigh.Load() {
		t.Fatal("VM pause did not run with memory.high lifted")
	}
	if value, readErr := os.ReadFile(memoryHighPath); readErr != nil {
		t.Fatal(readErr)
	} else if string(value) != "234881024\n" {
		t.Fatalf("memory.high = %q after failed snapshot, want original value", value)
	}
}

// fakeSignaler records signals sent to it; never blocks. Goroutine-safe:
// waitForCHWithSignalEscalation calls Signal from the test goroutine while
// watcher goroutines poll the recorded list.
type fakeSignaler struct {
	mu   sync.Mutex
	sent []os.Signal
}

func (f *fakeSignaler) Signal(sig os.Signal) error {
	f.mu.Lock()
	f.sent = append(f.sent, sig)
	f.mu.Unlock()
	return nil
}

func (f *fakeSignaler) sentCount(sig os.Signal) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sent {
		if s == sig {
			n++
		}
	}
	return n
}

func discardLogf(string, ...any) {}

func TestVMMMemoryHighLifecycleGuard(t *testing.T) {
	t.Run("restore original value", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "memory.high")
		if err := os.WriteFile(path, []byte("234881024\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		previous, memoryHighLock, err := liftVMMMemoryHigh(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if memoryHighLock != nil {
				_ = memoryHighLock.Close()
			}
		}()
		if value, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		} else if string(value) != "max" {
			t.Fatalf("lifted memory.high = %q, want max", value)
		}
		restored, err := restoreVMMMemoryHigh(dir, previous)
		if err != nil {
			t.Fatal(err)
		}
		if !restored {
			t.Fatal("original memory.high was not restored")
		}
		if err := memoryHighLock.Close(); err != nil {
			t.Fatal(err)
		}
		memoryHighLock = nil
		if value, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		} else if string(value) != "234881024\n" {
			t.Fatalf("restored memory.high = %q, want original value", value)
		}
	})

	t.Run("serialize local Budget high update", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "memory.high")
		if err := os.WriteFile(path, []byte("234881024"), 0o644); err != nil {
			t.Fatal(err)
		}
		// A local high update must read the host VMM charge as a safety lower
		// bound. Model the cgroup input explicitly; an absent memory.current is
		// intentionally fail-closed and would keep the controller retrying.
		if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("0"), 0o644); err != nil {
			t.Fatal(err)
		}
		previous, memoryHighLock, err := liftVMMMemoryHigh(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if memoryHighLock != nil {
				_ = memoryHighLock.Close()
			}
		}()
		cfg := &config.SandboxConfig{Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "8GiB"},
		}}
		cfg.ApplyDefaults()
		memoryCtl, err := resctl.NewMemoryController(resctl.MemoryControllerOptions{
			Config: cfg, CgroupPath: dir, InitialBudget: 8 << 30, Logf: discardLogf,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		memoryCtl.StartCold(ctx)
		defer memoryCtl.Stop()
		if !memoryCtl.SubmitGuestReport(proto.MemReport{Epoch: 1, Seq: 1, MemTotalBytes: 8 << 30, MemAvailableBytes: 8 << 30}) {
			t.Fatal("report rejected")
		}
		time.Sleep(25 * time.Millisecond)
		if value, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		} else if string(value) != "max" {
			t.Fatalf("memory.high changed during lifecycle operation: %q", value)
		}

		restored, err := restoreVMMMemoryHigh(dir, previous)
		if err != nil {
			t.Fatal(err)
		}
		if !restored {
			t.Fatal("original memory.high was not restored before releasing lifecycle lock")
		}
		if err := memoryHighLock.Close(); err != nil {
			t.Fatal(err)
		}
		memoryHighLock = nil
		deadline := time.Now().Add(time.Second)
		for {
			value, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(value) != "234881024" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("local Budget update remained blocked after lifecycle lock release")
			}
			time.Sleep(time.Millisecond)
		}
	})
}

func TestDestroyAfterSnapshotRetainsBarrierWhenShutdownFails(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "ch.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	requestSeen := make(chan struct{}, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/vmm.shutdown" {
			t.Errorf("shutdown request = %s %s", r.Method, r.URL.Path)
		}
		requestSeen <- struct{}{}
		w.WriteHeader(http.StatusInternalServerError)
	})}
	serverDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serverDone)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-serverDone
	})

	chExited := make(chan struct{})
	barrierReleased := make(chan struct{})
	go destroyAfterSnapshot(sock, nil, func() { close(barrierReleased) }, chExited, time.Second, discardLogf)

	select {
	case <-requestSeen:
	case <-time.After(time.Second):
		t.Fatal("destroy shutdown request was not sent")
	}
	select {
	case <-barrierReleased:
		t.Fatal("destroy barrier released after failed shutdown while VMM was still live")
	case <-time.After(25 * time.Millisecond):
	}
	close(chExited)
	select {
	case <-barrierReleased:
	case <-time.After(time.Second):
		t.Fatal("destroy barrier was not released after VMM exit")
	}
}

// TestWaitForCH_NoSignals: clean-exit path — doneCh fires before any
// signal, helper returns immediately with the wait error.
func TestWaitForCH_NoSignals(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}

	doneCh <- nil
	err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", "", 0, time.Second, discardLogf)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(proc.sent) != 0 {
		t.Fatalf("expected no signals sent, got %v", proc.sent)
	}
}

// TestWaitForCH_SIGTERM_GracefulExit: SIGTERM arrives, CH exits within
// grace — only SIGTERM forwarded, no SIGKILL.
func TestWaitForCH_SIGTERM_GracefulExit(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}

	go func() {
		sigCh <- syscall.SIGTERM
		time.Sleep(50 * time.Millisecond)
		doneCh <- &exitErrStub{code: 0}
	}()

	err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", "", 0, 5*time.Second, discardLogf)
	if err == nil {
		t.Fatal("expected non-nil exit err stub")
	}
	if proc.sentCount(syscall.SIGTERM) != 1 {
		t.Fatalf("expected 1 SIGTERM, got %v", proc.sent)
	}
	if proc.sentCount(syscall.SIGKILL) != 0 {
		t.Fatalf("expected no SIGKILL, got %v", proc.sent)
	}
}

func TestWaitForCH_OrderedShutdownLiftsMemoryHigh(t *testing.T) {
	dir := t.TempDir()
	memoryHighPath := filepath.Join(dir, "memory.high")
	if err := os.WriteFile(memoryHighPath, []byte("234881024"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.events.local"), []byte("low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "ch.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	requestSeen := make(chan bool, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, readErr := os.ReadFile(memoryHighPath)
		requestSeen <- r.Method == http.MethodPut && r.URL.Path == "/api/v1/vmm.shutdown" &&
			readErr == nil && strings.TrimSpace(string(value)) == "max"
		w.WriteHeader(http.StatusNoContent)
	})}
	serverDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serverDone)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-serverDone
	})

	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}
	var liftedAtRequest atomic.Bool
	go func() {
		select {
		case liftedAtRequestValue := <-requestSeen:
			liftedAtRequest.Store(liftedAtRequestValue)
		case <-time.After(time.Second):
		}
		doneCh <- nil
	}()
	sigCh <- syscall.SIGTERM
	if err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, sock, dir, time.Second, time.Second, discardLogf); err != nil {
		t.Fatal(err)
	}
	if !liftedAtRequest.Load() {
		t.Fatal("ordered shutdown request did not observe memory.high=max")
	}
	if len(proc.sent) != 0 {
		t.Fatalf("ordered shutdown unexpectedly used process signals: %v", proc.sent)
	}
	if value, err := os.ReadFile(memoryHighPath); err != nil {
		t.Fatal(err)
	} else if string(value) != "max" {
		t.Fatalf("memory.high = %q after ordered shutdown request, want max until VMM exit", value)
	}
}

func TestVMMMemoryHighThrottleDrainDelay(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "memory.current")
	eventsPath := filepath.Join(dir, "memory.events.local")
	if err := os.WriteFile(eventsPath, []byte("low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currentPath, []byte("280408064\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := vmmMemoryHighThrottleDrainDelay(dir, []byte("234881024\n")); got != memoryHighThrottleDrain {
		t.Fatalf("over-high drain delay = %s, want %s", got, memoryHighThrottleDrain)
	}
	if err := os.WriteFile(currentPath, []byte("200000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsPath, []byte("low 0\nhigh 1\nmax 0\noom 0\noom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := vmmMemoryHighThrottleDrainDelay(dir, []byte("234881024\n")); got != memoryHighThrottleDrain {
		t.Fatalf("prior-high-event drain delay = %s, want %s", got, memoryHighThrottleDrain)
	}
	if err := os.WriteFile(eventsPath, []byte("low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := vmmMemoryHighThrottleDrainDelay(dir, []byte("234881024\n")); got != 0 {
		t.Fatalf("never-throttled drain delay = %s, want 0", got)
	}
	if got := vmmMemoryHighThrottleDrainDelay(dir, []byte("max\n")); got != 0 {
		t.Fatalf("unlimited-high drain delay = %s, want 0", got)
	}
}

func TestWaitForCH_BriefMemoryHighLockContentionRetriesOrderedShutdown(t *testing.T) {
	dir := t.TempDir()
	memoryHighPath := filepath.Join(dir, "memory.high")
	for name, value := range map[string]string{
		"memory.high":         "234881024",
		"memory.current":      "123456789",
		"memory.events.local": "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	memoryHighLock, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer memoryHighLock.Close()
	if err := unix.Flock(int(memoryHighLock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			_ = unix.Flock(int(memoryHighLock.Fd()), unix.LOCK_UN)
		}
	}()

	sock := filepath.Join(dir, "ch.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	requestSeen := make(chan bool, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, readErr := os.ReadFile(memoryHighPath)
		requestSeen <- r.Method == http.MethodPut && r.URL.Path == "/api/v1/vmm.shutdown" &&
			readErr == nil && strings.TrimSpace(string(value)) == "max"
		w.WriteHeader(http.StatusNoContent)
	})}
	serverDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serverDone)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-serverDone
	})

	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}
	retryStarted := make(chan struct{}, 1)
	logf := func(format string, _ ...any) {
		if strings.Contains(format, "retrying for up to %s before SIGTERM fallback") {
			select {
			case retryStarted <- struct{}{}:
			default:
			}
		}
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- waitForCHWithSignalEscalation(
			doneCh,
			sigCh,
			proc,
			1234,
			sock,
			dir,
			time.Second,
			time.Second,
			logf,
		)
	}()

	sigCh <- syscall.SIGTERM
	select {
	case <-retryStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not retry the memory.high lifecycle lock")
	}
	if proc.sentCount(syscall.SIGTERM) != 0 {
		t.Fatalf("brief memory.high lifecycle contention caused fallback signal: %v", proc.sent)
	}
	if err := unix.Flock(int(memoryHighLock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	lockHeld = false
	select {
	case lifted := <-requestSeen:
		if !lifted {
			t.Fatal("retried ordered shutdown did not observe memory.high=max")
		}
	case <-time.After(time.Second):
		t.Fatal("ordered shutdown was not attempted after memory.high lifecycle lock release")
	}
	doneCh <- nil
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait helper did not return after VMM exit")
	}
	if len(proc.sent) != 0 {
		t.Fatalf("retried ordered shutdown unexpectedly used process signals: %v", proc.sent)
	}
}

func TestWaitForCH_PersistentLifecycleLockBusyFallsBackToSIGTERM(t *testing.T) {
	dir := t.TempDir()
	memoryHighPath := filepath.Join(dir, "memory.high")
	if err := os.WriteFile(memoryHighPath, []byte("234881024"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous, memoryHighLock, err := liftVMMMemoryHigh(dir)
	if err != nil {
		t.Fatal(err)
	}
	doneCh := make(chan error, 1)
	defer func() {
		select {
		case doneCh <- nil:
		default:
		}
		_, _ = restoreVMMMemoryHigh(dir, previous)
		_ = memoryHighLock.Close()
	}()

	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- waitForCHWithSignalEscalation(
			doneCh,
			sigCh,
			proc,
			1234,
			filepath.Join(dir, "unused.sock"),
			dir,
			time.Second,
			time.Second,
			discardLogf,
		)
	}()

	start := time.Now()
	sigCh <- syscall.SIGTERM
	for proc.sentCount(syscall.SIGTERM) == 0 && time.Since(start) <= 500*time.Millisecond {
		time.Sleep(time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown signal blocked on lifecycle lock for %s", elapsed)
	} else if elapsed < memoryHighLockRetryWindow {
		t.Fatalf("shutdown fell back before the controller retry window elapsed: %s", elapsed)
	}
	if proc.sentCount(syscall.SIGTERM) != 1 {
		t.Fatalf("expected 1 fallback SIGTERM, got %v", proc.sent)
	}
	doneCh <- nil
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait helper did not return after VMM exit")
	}
}

func TestWaitForCH_LockRetryRemainsSignalResponsive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.high"), []byte("234881024"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous, memoryHighLock, err := liftVMMMemoryHigh(dir)
	if err != nil {
		t.Fatal(err)
	}
	doneCh := make(chan error, 1)
	defer func() {
		select {
		case doneCh <- nil:
		default:
		}
		_, _ = restoreVMMMemoryHigh(dir, previous)
		_ = memoryHighLock.Close()
	}()

	sigCh := make(chan os.Signal, 2)
	proc := &fakeSignaler{}
	retryStarted := make(chan struct{}, 1)
	logf := func(format string, _ ...any) {
		if strings.Contains(format, "retrying for up to %s before SIGTERM fallback") {
			select {
			case retryStarted <- struct{}{}:
			default:
			}
		}
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- waitForCHWithSignalEscalation(
			doneCh,
			sigCh,
			proc,
			1234,
			filepath.Join(dir, "unused.sock"),
			dir,
			time.Second,
			time.Second,
			logf,
		)
	}()

	sigCh <- syscall.SIGTERM
	select {
	case <-retryStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not enter the memory.high lifecycle lock retry window")
	}
	start := time.Now()
	sigCh <- syscall.SIGINT
	for proc.sentCount(syscall.SIGKILL) == 0 && time.Since(start) <= 500*time.Millisecond {
		time.Sleep(time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("second signal was blocked by memory.high lifecycle lock retry for %s", elapsed)
	}
	if proc.sentCount(syscall.SIGKILL) != 1 {
		t.Fatalf("expected immediate SIGKILL during memory.high lifecycle lock retry, got %v", proc.sent)
	}
	if proc.sentCount(syscall.SIGTERM) != 0 {
		t.Fatalf("memory.high lifecycle lock retry unexpectedly fell back before escalation: %v", proc.sent)
	}
	doneCh <- nil
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait helper did not reap VMM after retry escalation")
	}
}

func TestWaitForCH_ThrottleDrainRemainsSignalResponsive(t *testing.T) {
	dir := t.TempDir()
	for name, value := range map[string]string{
		"memory.high":         "234881024",
		"memory.current":      "280408064",
		"memory.events.local": "low 0\nhigh 1\nmax 0\noom 0\noom_kill 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 2)
	proc := &fakeSignaler{}
	drainStarted := make(chan struct{}, 1)
	logf := func(format string, _ ...any) {
		if strings.Contains(format, "waiting %s for existing VMM memory.high throttles") {
			select {
			case drainStarted <- struct{}{}:
			default:
			}
		}
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- waitForCHWithSignalEscalation(
			doneCh,
			sigCh,
			proc,
			1234,
			filepath.Join(dir, "unused.sock"),
			dir,
			time.Second,
			10*time.Second,
			logf,
		)
	}()

	sigCh <- syscall.SIGTERM
	select {
	case <-drainStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not enter memory.high drain")
	}
	start := time.Now()
	sigCh <- syscall.SIGINT
	for proc.sentCount(syscall.SIGKILL) == 0 && time.Since(start) <= 500*time.Millisecond {
		time.Sleep(time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("second signal was blocked by throttle drain for %s", elapsed)
	}
	if proc.sentCount(syscall.SIGKILL) != 1 {
		t.Fatalf("expected immediate SIGKILL during throttle drain, got %v", proc.sent)
	}
	doneCh <- nil
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait helper did not reap VMM after drain escalation")
	}
}

// TestWaitForCH_SIGTERM_EscalatesToSIGKILL: CH ignores SIGTERM, helper
// must escalate to SIGKILL after grace expires. This is the regression
// test for the >50min hang observed in cold-target.
func TestWaitForCH_SIGTERM_EscalatesToSIGKILL(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}
	grace := 100 * time.Millisecond

	// Simulate stuck CH: doneCh never fires until we manually signal it.
	// SIGKILL handling: once helper sends SIGKILL we treat as "process
	// died" and unblock doneCh.
	var killSeen atomic.Bool
	go func() {
		for {
			if killSeen.Load() {
				doneCh <- &exitErrStub{code: -1}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	// Watch for SIGKILL appearance (via the goroutine-safe accessor).
	go func() {
		for {
			time.Sleep(5 * time.Millisecond)
			if proc.sentCount(syscall.SIGKILL) > 0 {
				killSeen.Store(true)
				return
			}
		}
	}()

	t0 := time.Now()
	sigCh <- syscall.SIGTERM
	err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", "", 0, grace, discardLogf)
	elapsed := time.Since(t0)
	if err == nil {
		t.Fatal("expected exit err stub")
	}

	if proc.sentCount(syscall.SIGTERM) != 1 {
		t.Fatalf("expected 1 SIGTERM, got %v", proc.sent)
	}
	if proc.sentCount(syscall.SIGKILL) != 1 {
		t.Fatalf("expected 1 SIGKILL escalation, got sent=%v", proc.sent)
	}
	// Must have waited at least the grace period before SIGKILL.
	if elapsed < grace {
		t.Fatalf("escalation fired too early: %v < %v", elapsed, grace)
	}
	// And not waited far longer (would indicate hang).
	if elapsed > grace+500*time.Millisecond {
		t.Fatalf("escalation took too long: %v (grace=%v)", elapsed, grace)
	}
}

// TestWaitForCH_DoubleSIGTERM_EscalatesImmediately: second SIGTERM
// during shutdown grace must skip the timer and SIGKILL right away.
func TestWaitForCH_DoubleSIGTERM_EscalatesImmediately(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 2)
	proc := &fakeSignaler{}
	grace := 10 * time.Second // long — would mask immediate escalation if buggy

	var killSeen atomic.Bool
	go func() {
		for !killSeen.Load() {
			time.Sleep(5 * time.Millisecond)
		}
		doneCh <- &exitErrStub{code: -1}
	}()
	go func() {
		for {
			time.Sleep(2 * time.Millisecond)
			if proc.sentCount(syscall.SIGKILL) > 0 {
				killSeen.Store(true)
				return
			}
		}
	}()

	t0 := time.Now()
	sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond) // first SIGTERM arms timer
	sigCh <- syscall.SIGINT           // second signal escalates
	_ = waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", "", 0, grace, discardLogf)
	elapsed := time.Since(t0)

	if proc.sentCount(syscall.SIGKILL) != 1 {
		t.Fatalf("expected 1 SIGKILL via immediate-escalate path, got sent=%v", proc.sent)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("immediate escalation took too long: %v", elapsed)
	}
}

// exitErrStub matches the *exec.ExitError shape just enough for callers
// that check via type assertion; ours doesn't, but we use it to be
// explicit about "process exited unsuccessfully".
type exitErrStub struct{ code int }

func (e *exitErrStub) Error() string { return fmt.Sprintf("exit %d", e.code) }

func init() {
	// Silence "imported and not used" if the file is included in builds
	// where errors is not referenced elsewhere.
	_ = errors.New
}
