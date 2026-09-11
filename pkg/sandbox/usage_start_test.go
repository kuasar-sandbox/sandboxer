package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestUsageInitFailure(t *testing.T) {
	for _, mode := range []string{"empty", "saved", "tail"} {
		t.Run(mode, func(t *testing.T) {
			base, runDir := t.TempDir(), t.TempDir()
			path := filepath.Join(base, "test.usage")
			var before []byte
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
				before, err = os.ReadFile(path)
				if err != nil || m.View().Saved == nil {
					t.Fatalf("seed: %v %+v", err, m.View())
				}
				if mode == "tail" {
					before = append(before, []byte("KUUSAGE1")...)
					if err := os.WriteFile(path, before, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			businessErr := errors.New("injected build failure")
			_, err := ServeAndWait(VMParams{
				Ctx: context.Background(), SandboxID: "test", BaseDir: base, RunDir: runDir,
				Logf: func(string, ...any) {}, CapBytes: 4096, UffdSource: uffd.ZeroSource{},
				LaunchSpec: &proto.LaunchSpec{},
				// No disks/CPU: fail usage construction, not business setup.
				SnapCfg: &config.SandboxConfig{Usage: config.UsageConfig{Enabled: true}},
				BuildCmd: func(CmdEnv) (*exec.Cmd, func(), error) {
					for _, history := range []bool{false, true} {
						conn, err := net.DialTimeout("unix", filepath.Join(runDir, "ctl.sock"), time.Second)
						if err != nil {
							t.Fatal(err)
						}
						_ = conn.SetDeadline(time.Now().Add(time.Second))
						err = ctl.WriteMessage(conn, ctl.Request{Type: ctl.TypeUsageRequest, UsageHistory: history, UsageLimit: 10})
						if err != nil {
							_ = conn.Close()
							t.Fatal(err)
						}
						response, err := ctl.ReadUsageResponse(conn)
						_ = conn.Close()
						if err != nil {
							t.Fatal(err)
						}
						if history {
							if response.Type != ctl.TypeError || !strings.Contains(response.Msg, "offline") {
								t.Errorf("uninitialized history exposed: %+v", response)
							}
						} else {
							var view usage.View
							if err := json.Unmarshal(response.Usage, &view); err != nil {
								t.Fatal(err)
							}
							if !view.Enabled || view.ReadError == "" || view.Live != nil || view.Saved != nil {
								t.Errorf("uninitialized manager exposed: %+v", view)
							}
						}
					}
					// Offline readers can already acquire the file, before the VM
					// lifecycle exits. This does not create a second usage owner.
					f, err := os.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
						t.Errorf("failed constructor retains writer lock: %v", err)
					}
					info, err := f.Stat()
					if err != nil {
						_ = f.Close()
						t.Fatal(err)
					}
					recovered, err := usage.Recover(f, info.Size(), "test")
					_ = f.Close()
					if err != nil || (recovered.Record != nil) != (mode != "empty") || recovered.IncompleteTail != (mode == "tail") {
						t.Errorf("offline recovery changed: %+v %v", recovered, err)
					}
					return nil, func() {}, businessErr
				},
			})
			if !errors.Is(err, businessErr) {
				t.Fatalf("business result changed: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("failed sampler wrote/truncated history: before=%d after=%d err=%v", len(before), len(after), err)
			}
		})
	}
}

// A constructed sampler accounts for the Host process even when later setup
// fails before CH exists. This differs from an uninitialized sampler above.
func TestUsagePreSpawnFailurePreservesHistoryAndAccountsForHost(t *testing.T) {
	for _, failure := range []string{"build", "spawn", "cancel", "forward"} {
		for _, mode := range []string{"empty", "saved", "tail"} {
			t.Run(failure+"/"+mode, func(t *testing.T) {
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
						t.Fatalf("seed: %v %+v", err, m.View())
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
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				buildErr := errors.New("injected pre-spawn build failure")
				params := VMParams{
					Ctx: ctx, SandboxID: "test", BaseDir: base, RunDir: runDir,
					Logf: func(string, ...any) {}, CapBytes: 4096, UffdSource: uffd.ZeroSource{},
					LaunchSpec: &proto.LaunchSpec{}, Disks: []DiskBackend{{Cow: cow}},
					SnapCfg: &config.SandboxConfig{Usage: config.UsageConfig{Enabled: true},
						Resources: config.ResourcesConfig{Capacity: config.CapacityConfig{CPU: 1}}},
					BuildCmd: func(CmdEnv) (*exec.Cmd, func(), error) {
						if failure == "build" {
							return nil, func() {}, buildErr
						}
						if failure == "cancel" {
							cancel()
						}
						return exec.Command(filepath.Join(runDir, "absent-CH")), func() {}, nil
					},
				}
				if failure == "forward" {
					params.Forwards = []ForwardSpec{{UDSPath: filepath.Join(runDir, "absent", "forward.sock"),
						Network: "tcp", Address: "127.0.0.1:1"}}
				}
				_, businessErr := ServeAndWait(params)
				if businessErr == nil || (failure == "build" && !errors.Is(businessErr, buildErr)) {
					t.Fatalf("business failure changed: %v", businessErr)
				}
				after, err := os.ReadFile(path)
				if err != nil || len(after) <= len(previous) || !bytes.HasPrefix(after, previous) {
					t.Fatalf("previous complete record bytes changed: %v", err)
				}
				f, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
					t.Fatalf("failure retained writer ownership: %v", err)
				}
				recovered, err := usage.Recover(f, int64(len(after)), "test")
				if err != nil || recovered.Record == nil || recovered.IncompleteTail || !recovered.Record.Snapshot.Closed {
					t.Fatalf("final Host observation was not saved: %+v %v", recovered, err)
				}
				host, guest := false, false
				for _, c := range recovered.Record.Snapshot.Counters {
					switch c.Name {
					case "sandbox_ctl.cpu":
						host = c.SourceKnown && c.Source != "" && c.Hertz != 0
					case "guest.cpu":
						guest = true
						if c.KnownTotal != (usage.Uint128{Lo: 250_000_000}) || c.Source != "old-source" {
							t.Fatalf("failed new run changed previous Guest consumption: %+v", c)
						}
					default:
						t.Fatalf("invented unstarted process observation: %+v", c)
					}
				}
				if !host || guest != (mode != "empty") {
					t.Fatalf("wrong source accounting: %+v", recovered.Record.Snapshot.Counters)
				}
			})
		}
	}
}
