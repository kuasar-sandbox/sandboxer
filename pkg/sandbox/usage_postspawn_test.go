package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
	"github.com/kuasar-sandbox/sandboxer/pkg/usage"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
	"golang.org/x/sys/unix"
)

// A real subprocess exercises the existing wait owner without starting a VM.
func TestUsagePostSpawnChild(t *testing.T) {
	marker := os.Getenv("KUASAR_TEST_POSTSPAWN_MARKER")
	if marker == "" {
		return
	}
	if _, err := os.Stdout.WriteString(strings.Repeat("drain-output", 8192)); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(marker, []byte("running"), 0600); err != nil {
		os.Exit(3)
	}
	time.Sleep(30 * time.Second)
	os.Exit(4)
}

func readPostSpawnUsage(runDir string) (usage.View, error) {
	conn, err := net.DialTimeout("unix", filepath.Join(runDir, "ctl.sock"), time.Second)
	if err != nil {
		return usage.View{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if err := ctl.WriteMessage(conn, ctl.Request{Type: ctl.TypeUsageRequest}); err != nil {
		return usage.View{}, err
	}
	r, err := ctl.ReadUsageResponse(conn)
	if err != nil {
		return usage.View{}, err
	}
	var view usage.View
	if err := json.Unmarshal(r.Usage, &view); err != nil {
		return usage.View{}, err
	}
	return view, nil
}

func TestUsagePostSpawnFailure(t *testing.T) {
	for _, mode := range []string{"empty", "saved", "tail"} {
		t.Run(mode, func(t *testing.T) {
			base, runDir := t.TempDir(), t.TempDir()
			path := filepath.Join(base, "test.usage")
			var previous []byte
			if mode != "empty" {
				m, err := usage.Open(base, "test", "old", time.Now(), time.Second, 5*time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if err := m.Counter("guest.cpu", "old-source", 25, 100, true); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				m.Close(ctx, time.Now())
				cancel()
				previous, err = os.ReadFile(path)
				if err != nil || m.View().Saved == nil {
					t.Fatalf("seed: %+v %v", m.View(), err)
				}
				if mode == "tail" {
					if err := os.WriteFile(path, append(append([]byte(nil), previous...), []byte("KUUSAGE1")...), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			cow, err := vhost.OpenBlockCOW(filepath.Join(t.TempDir(), "disk"), nil, vhost.DiffInit{CreateSize: 4096})
			if err != nil {
				t.Fatal(err)
			}
			defer cow.Close()
			marker := filepath.Join(runDir, "child-ready")
			var command *exec.Cmd
			var output bytes.Buffer
			var active usage.View
			var failureAt time.Time
			businessErr := errors.New("injected restore PostSpawn failure")
			readyCount := 0
			exitCode, gotErr := ServeAndWait(VMParams{
				Ctx: context.Background(), SandboxID: "test", BaseDir: base, RunDir: runDir,
				Logf: t.Logf, CapBytes: 4096, UffdSource: uffd.ZeroSource{},
				LaunchSpec: &proto.LaunchSpec{}, Disks: []DiskBackend{{Cow: cow}},
				SnapCfg: &config.SandboxConfig{Usage: config.UsageConfig{Enabled: true},
					Resources: config.ResourcesConfig{Capacity: config.CapacityConfig{CPU: 1}}},
				NotifyReadiness: func(event ReadinessEvent) {
					if event == ReadinessReady {
						readyCount++
					}
				},
				BuildCmd: func(CmdEnv) (*exec.Cmd, func(), error) {
					command = exec.Command(os.Args[0], "-test.run=^TestUsagePostSpawnChild$")
					command.Env = append(os.Environ(), "KUASAR_TEST_POSTSPAWN_MARKER="+marker)
					command.Stdout = &output // real os/exec copy goroutine, joined by its sole Wait
					command.Stderr = &output
					return command, func() {}, nil
				},
				PostSpawn: func(pc PostSpawnCtx) error {
					deadline := time.Now().Add(3 * time.Second)
					for {
						view, err := readPostSpawnUsage(runDir)
						if err != nil {
							return fmt.Errorf("live query: %w", err)
						}
						observed := false
						if view.Enabled && view.ReadError == "" && view.Live != nil {
							for _, c := range view.Live.Counters {
								if c.Name == "ch.cpu" && c.SourceKnown && strings.Contains(c.Source, fmt.Sprintf("/%d/", pc.Cmd.Process.Pid)) {
									observed = true
								}
							}
						}
						_, markerErr := os.Stat(marker)
						if observed && markerErr == nil {
							active = view
							break
						}
						if time.Now().After(deadline) {
							return fmt.Errorf("sampler/real child did not become active: %+v, %v", view, markerErr)
						}
						time.Sleep(time.Millisecond)
					}
					f, err := os.Open(path)
					if err != nil {
						return err
					}
					err = unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
					_ = f.Close()
					if !errors.Is(err, unix.EWOULDBLOCK) {
						return fmt.Errorf("live manager was not holding flock: %v", err)
					}
					failureAt = time.Now()
					return businessErr
				},
			})
			if !errors.Is(gotErr, businessErr) || exitCode != -1 {
				t.Fatalf("PostSpawn result changed: code=%d err=%v", exitCode, gotErr)
			}
			if time.Since(failureAt) > 3*time.Second {
				t.Fatal("failure cleanup exceeded bounded budget")
			}
			if readyCount != 0 {
				t.Fatalf("failed restore emitted readiness: %d", readyCount)
			}
			if command.ProcessState == nil {
				t.Fatal("CH child was not reaped before returning")
			}
			status := command.ProcessState.Sys().(syscall.WaitStatus)
			if !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("wrong failed-PostSpawn child exit: %v", command.ProcessState)
			}
			var waitStatus unix.WaitStatus
			if _, err := unix.Wait4(command.Process.Pid, &waitStatus, unix.WNOHANG, nil); !errors.Is(err, unix.ECHILD) {
				t.Fatalf("sole cmd.Wait did not already reap child: %v", err)
			}
			if output.String() != strings.Repeat("drain-output", 8192) {
				t.Fatalf("os/exec output was not drained: bytes=%d", output.Len())
			}
			after, err := os.ReadFile(path)
			if err != nil || len(after) <= len(previous) || !bytes.HasPrefix(after, previous) {
				t.Fatalf("old saved history changed: before=%d after=%d err=%v", len(previous), len(after), err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatalf("stopped sampler retained manager flock: %v", err)
			}
			recovered, err := usage.Recover(f, int64(len(after)), "test")
			if err != nil || recovered.Record == nil || recovered.IncompleteTail || !recovered.Record.Snapshot.Closed {
				t.Fatalf("failure did not close/save usage: %+v %v", recovered, err)
			}
			final := recovered.Record.Snapshot
			if final.RunEpoch != active.Live.RunEpoch {
				t.Fatal("failure changed active run identity")
			}
			seenCH, seenSelf, seenGuest := false, false, false
			for _, c := range final.Counters {
				switch c.Name {
				case "ch.cpu":
					seenCH = c.SourceKnown && strings.Contains(c.Source, fmt.Sprintf("/%d/", command.Process.Pid))
				case "sandbox_ctl.cpu":
					seenSelf = c.SourceKnown && strings.Contains(c.Source, fmt.Sprintf("/%d/", os.Getpid()))
				case "guest.cpu":
					seenGuest = true
					want := usage.Uint128{}
					if mode != "empty" {
						want.Lo = 250_000_000
					}
					if c.KnownTotal != want {
						t.Fatalf("failed restore changed old Guest total: %+v", c)
					}
				}
			}
			if !seenCH || !seenSelf || !seenGuest {
				t.Fatalf("source accounting lost on failed restore: %+v", final.Counters)
			}
			for _, g := range final.Gauges {
				if g.Name == "guest.memory" || strings.HasPrefix(g.Name, "filesystem.") {
					t.Fatalf("unready restore fabricated Guest gauge: %+v", g)
				}
			}
			t.Logf("postspawn evidence: pid=%d elapsed=%v final-bytes=%d previous-bytes=%d closed=%v output=%d", command.Process.Pid, time.Since(failureAt), len(after), len(previous), final.Closed, output.Len())
		})
	}
}
