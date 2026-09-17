package vhost_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
	"golang.org/x/sys/unix"
)

type captureQuiescer struct{}

func (captureQuiescer) Quiesce() {}
func (captureQuiescer) Resume()  {}

func captureAPI(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "ch.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/vm.snapshot" {
			for _, name := range []string{"config.json", "state.json"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"test":true}`), 0600); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			}
		}
		w.WriteHeader(204)
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	return sock, dir
}
func captureConfig() *config.PortableSandboxConfig {
	return &config.PortableSandboxConfig{Version: config.PortableSandboxConfigVersion, Resources: config.PortableResourcesConfig{Capacity: config.CapacityConfig{CPU: 1, Memory: "4KiB"}, Allocatable: config.AllocatableConfig{CPU: 1, Memory: "4KiB"}}, Boot: config.PortableBootConfig{Kernel: "file://vmlinux@digest:" + strings.Repeat("a", 64), Runtime: "file://runtime.bundle@digest:" + strings.Repeat("b", 64), Root: config.PortableRootConfig{Base: "self"}}, Launch: config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"}}
}

type failAfterCaptureSink struct {
	snapshot.ArtifactSink
	fail    func()
	commits int
}

func (s *failAfterCaptureSink) AbsorbSandbox(ctx context.Context, src sparse.Source) (string, string, error) {
	a, b, e := s.ArtifactSink.AbsorbSandbox(ctx, src)
	s.fail()
	return a, b, e
}
func (s *failAfterCaptureSink) CommitSandbox(ctx context.Context, a, b string) error {
	s.commits++
	return s.ArtifactSink.CommitSandbox(ctx, a, b)
}
func (s *failAfterCaptureSink) CommitSnapshot(ctx context.Context, a, b string) error {
	s.commits++
	return s.ArtifactSink.CommitSnapshot(ctx, a, b)
}

// This uses real artifact sinks/readers and a restored BlockCOW over the
// captured payload. CH's API is simulated; it is not claimed as a KVM E2E.
func TestCacheDirtyWritebackSnapshotExportRestore(t *testing.T) {
	testCacheDirtyWritebackSnapshotExportRestore(t, (*testing.T).TempDir)
}

func TestTmpfsCacheDirtyWritebackSnapshotExportRestore(t *testing.T) {
	testCacheDirtyWritebackSnapshotExportRestore(t, tmpfsCaptureDir)
}

func tmpfsCaptureDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/dev/shm", "diff-cow-238-capture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != unix.TMPFS_MAGIC {
		t.Fatalf("capture fixture fd is not tmpfs: %x", fs.Type)
	}
	return dir
}

func testCacheDirtyWritebackSnapshotExportRestore(t *testing.T, diffDir func(*testing.T) string) {
	for _, captureMemory := range []bool{false, true} {
		for _, writeback := range []bool{false, true} {
			for _, encrypted := range []bool{false, true} {
				t.Run(fmt.Sprintf("snapshot=%t/writeback=%t/encrypted=%t", captureMemory, writeback, encrypted), func(t *testing.T) {
					entered, release := make(chan struct{}), make(chan struct{})
					var once sync.Once
					open := func() { once.Do(func() { close(release) }) }
					defer open()
					gate := func() { close(entered); <-release }
					var beforeSelect func()
					var beforeWrite func() error
					if writeback {
						beforeWrite = func() error { gate(); return nil }
					} else {
						beforeSelect = gate
					}
					cache, err := vhost.NewCOWCacheForTest(8192, 8192, beforeSelect, beforeWrite)
					if err != nil {
						t.Fatal(err)
					}
					defer cache.Close()
					// Make the gate one-shot because healthy Close may start a later batch.
					// This test accepts only one page before releasing the worker.
					var key [32]byte
					key[0] = 71
					var options []vhost.BlockCOWOption
					var codec tarstream.Codec
					if encrypted {
						options = append(options, vhost.WithDiffEncryption(key, true))
						codec, err = manifestcrypto.NewTarStreamCodec(key)
						if err != nil {
							t.Fatal(err)
						}
					}
					cow, err := vhost.OpenBlockCOW(filepath.Join(diffDir(t), "diff"), nil, vhost.DiffInit{CreateSize: 8192}, append(options, vhost.WithCOWCache(cache))...)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { open(); _ = cow.Close() }()
					want := make([]byte, 8192)
					copy(want[4096+123:], bytes.Repeat([]byte{0x74}, 512))
					if _, err := cow.WriteAt(want[4096+123:4096+635], 4096+123); err != nil {
						t.Fatal(err)
					}
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("worker gate")
					}
					sock, staging := captureAPI(t)
					outdir := t.TempDir()
					diffs := []snapshot.DiskDiff{{SnapshotView: cow.SnapshotView, CheckError: cow.Err}}
					sink := snapshot.NewFileSink(outdir, "cached", codec, encrypted, nil)
					var artifact string
					if captureMemory {
						mfd, err := memory.Create("cached-capture", 4096)
						if err != nil {
							t.Fatal(err)
						}
						defer mfd.Close()
						out, err := snapshot.Take(snapshot.Sources{Context: context.Background(), SandboxID: "cached", APISock: sock, StagingDir: staging, MemfdFD: mfd.FD(), MemfdSize: 4096, PortableConfig: captureConfig(), Diffs: diffs, Quiescer: captureQuiescer{}}, sink, false)
						if err != nil {
							t.Fatal(err)
						}
						artifact = out.SandboxPath
					} else {
						out, err := snapshot.Export(context.Background(), snapshot.ExportSources{SandboxID: "cached", APISock: sock, PortableConfig: captureConfig(), Diffs: diffs, Quiescer: captureQuiescer{}}, sink, false)
						if err != nil {
							t.Fatal(err)
						}
						artifact = out.SandboxPath
					}
					if cache.Stats().DirtyUsed != 4096 {
						t.Fatal("capture drained the dirty/writeback page")
					}
					var stream fetch.Stream
					var openErr error
					if encrypted {
						stream, openErr = fetch.OpenTarStream(artifact, tarstream.WithCodec(codec, true))
					} else {
						stream, openErr = fetch.OpenTarStream(artifact)
					}
					err = openErr
					if err != nil {
						t.Fatal(err)
					}
					captured, err := sandboxfile.Open(context.Background(), stream)
					if err != nil {
						_ = stream.Close()
						t.Fatal(err)
					}
					defer captured.Close()
					base := vhost.NewStreamReader(context.Background(), captured.Payload, int64(captured.Payload.Size()))
					restored, err := vhost.OpenBlockCOW(filepath.Join(diffDir(t), "restored.diff"), base, vhost.DiffInit{CreateSize: 8192}, options...)
					if err != nil {
						t.Fatal(err)
					}
					defer restored.Close()
					got := make([]byte, len(want))
					if _, err := restored.ReadAt(got, 0); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, want) {
						t.Fatal("captured/restored unflushed plaintext differs")
					}
				})
			}
		}
	}
}

func TestCacheBackgroundFatalAfterLastCaptureReadPreventsCommit(t *testing.T) {
	for _, captureMemory := range []bool{false, true} {
		t.Run(fmt.Sprint(captureMemory), func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			open := func() { once.Do(func() { close(release) }) }
			defer open()
			fatal := errors.New("injected background storage failure")
			cache, err := vhost.NewCOWCacheForTest(4096, 4096, nil, func() error { <-release; return fatal })
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			cow, err := vhost.OpenBlockCOW(filepath.Join(t.TempDir(), "diff"), nil, vhost.DiffInit{CreateSize: 8192}, vhost.WithCOWCache(cache))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { open(); _ = cow.Close() }()
			notified := make(chan struct{})
			cow.SetFatalHandler(func(error) { close(notified) })
			if _, err := cow.WriteAt([]byte("latest"), 4096); err != nil {
				t.Fatal(err)
			}
			sock, staging := captureAPI(t)
			sink := &failAfterCaptureSink{ArtifactSink: snapshot.NewFileSink(t.TempDir(), "failed", nil, false, nil), fail: func() { open(); <-notified }}
			diffs := []snapshot.DiskDiff{{SnapshotView: cow.SnapshotView, CheckError: cow.Err}}
			// No canceled context: publication must also inspect independent cache health.
			if captureMemory {
				mfd, err := memory.Create("failed-capture", 4096)
				if err != nil {
					t.Fatal(err)
				}
				defer mfd.Close()
				out, err := snapshot.Take(snapshot.Sources{Context: context.Background(), SandboxID: "failed", APISock: sock, StagingDir: staging, MemfdFD: mfd.FD(), MemfdSize: 4096, PortableConfig: captureConfig(), Diffs: diffs, Quiescer: captureQuiescer{}}, sink, false)
				if out != nil || !errors.Is(err, fatal) {
					t.Fatalf("snapshot published: %v %v", out, err)
				}
			} else {
				out, err := snapshot.Export(context.Background(), snapshot.ExportSources{SandboxID: "failed", APISock: sock, PortableConfig: captureConfig(), Diffs: diffs, Quiescer: captureQuiescer{}}, sink, false)
				if out != nil || !errors.Is(err, fatal) {
					t.Fatalf("export published: %v %v", out, err)
				}
			}
			if sink.commits != 0 {
				t.Fatal("failed capture committed root")
			}
		})
	}
}

func TestCacheFatalReachesRuntimeOwnerWithoutGuestRequest(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	fatal := errors.New("injected idle writeback failure")
	cache, err := vhost.NewCOWCacheForTest(4096, 4096, nil, func() error { <-release; return fatal })
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	cow, err := vhost.OpenBlockCOW(filepath.Join(t.TempDir(), "diff"), nil, vhost.DiffInit{CreateSize: 4096}, vhost.WithCOWCache(cache))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { open(); _ = cow.Close() }()
	if _, err := cow.WriteAt([]byte("accepted"), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	code, err := sandbox.ServeAndWait(sandbox.VMParams{Ctx: ctx, SandboxID: "cache-fatal", RunDir: t.TempDir(), BaseDir: t.TempDir(), CapBytes: 4096, Logf: t.Logf, LaunchSpec: &proto.LaunchSpec{}, SnapCfg: &config.SandboxConfig{}, Disks: []sandbox.DiskBackend{{Cow: cow}}, BuildCmd: func(sandbox.CmdEnv) (*exec.Cmd, func(), error) {
		cmd = exec.Command("sleep", "60")
		return cmd, func() {}, nil
	}, PostSpawn: func(pc sandbox.PostSpawnCtx) error { open(); <-pc.Ctx.Done(); return pc.Ctx.Err() }})
	if code != -1 || !errors.Is(err, fatal) {
		t.Fatalf("runtime lost background cause %d %v", code, err)
	}
	if cmd == nil || cmd.ProcessState == nil {
		t.Fatal("owner did not join child")
	}
	var status unix.WaitStatus
	if _, err := unix.Wait4(cmd.Process.Pid, &status, unix.WNOHANG, nil); !errors.Is(err, unix.ECHILD) {
		t.Fatalf("child not reaped exactly once: %v", err)
	}
}
