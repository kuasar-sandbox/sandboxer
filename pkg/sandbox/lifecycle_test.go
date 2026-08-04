package sandbox

import (
	"bytes"
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

func TestHandleSnapshotRequestRejectsPredictableErrorsBeforeQuiesce(t *testing.T) {
	pinger := &guestlink.Pinger{
		Client: &guestlink.HostClient{BasePath: filepath.Join(t.TempDir(), "must-not-dial.sock")},
	}
	baseCfg := &config.SandboxConfig{}
	baseOpts := RunOptions{Cfg: baseCfg, SandboxID: "test"}
	disks := []SnapDiskRef{{DiffPath: filepath.Join(t.TempDir(), "not-needed.diff")}}

	_, err := handleSnapshotRequest(ctl.Request{}, baseOpts, nil, disks, nil,
		"", t.TempDir(), pinger, nil, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "--output and --upload") {
		t.Fatalf("missing output error = %v", err)
	}

	_, err = handleSnapshotRequest(ctl.Request{Upload: true}, baseOpts, nil, disks, nil,
		"", t.TempDir(), pinger, nil, nil, discardLogf)
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
		pinger, nil, nil, discardLogf)
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
	}}, nil, "", filepath.Join(dir, "run"), nil, nil, nil, discardLogf)
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
	chServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// TestWaitForCH_NoSignals: clean-exit path — doneCh fires before any
// signal, helper returns immediately with the wait error.
func TestWaitForCH_NoSignals(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}

	doneCh <- nil
	err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", 0, time.Second, discardLogf)
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

	err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", 0, 5*time.Second, discardLogf)
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
	err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", 0, grace, discardLogf)
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
	_ = waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", 0, grace, discardLogf)
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
