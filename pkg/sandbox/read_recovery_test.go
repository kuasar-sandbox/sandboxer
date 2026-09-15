package sandbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
	"golang.org/x/sys/unix"
)

type fatalSnapshotSource struct{ cause error }

func (s fatalSnapshotSource) RunAt(uint64, uint64) (sparse.Run, error) { return nil, s.cause }

func TestMandatoryReadFatalDuringPostSpawnReapsCHOnce(t *testing.T) {
	runDir := t.TempDir()
	marker := filepath.Join(runDir, "child-ready")
	cow, err := vhost.OpenBlockCOW(filepath.Join(t.TempDir(), "diff"), nil, vhost.DiffInit{CreateSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer cow.Close()
	var cmd *exec.Cmd
	var environment CmdEnv
	var ready atomic.Int32
	cause := readerr.Mark(errors.New("injected mandatory source failure"), false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := ServeAndWait(VMParams{
		Ctx: ctx, SandboxID: "read-fatal", BaseDir: t.TempDir(), RunDir: runDir, Logf: t.Logf, CapBytes: 4096,
		UffdSource: fatalSnapshotSource{cause}, LaunchSpec: &proto.LaunchSpec{}, Disks: []DiskBackend{{Cow: cow}}, SnapCfg: &config.SandboxConfig{},
		NotifyReadiness: func(e ReadinessEvent) {
			if e == ReadinessReady {
				ready.Add(1)
			}
		},
		BuildCmd: func(env CmdEnv) (*exec.Cmd, func(), error) {
			environment = env
			cmd = exec.Command(os.Args[0], "-test.run=^TestUsagePostSpawnChild$")
			cmd.Env = append(os.Environ(), "KUASAR_TEST_POSTSPAWN_MARKER="+marker)
			return cmd, func() {}, nil
		},
		PostSpawn: func(pc PostSpawnCtx) error {
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				select {
				case <-pc.Ctx.Done():
					return pc.Ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
			var fds [2]int
			if err := unix.Pipe2(fds[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
				return err
			}
			defer unix.Close(fds[0])
			defer unix.Close(fds[1])
			conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: environment.UffdSock, Net: "unix"})
			if err != nil {
				return err
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			body, _ := json.Marshal(map[string]any{"type": "va_report", "va_start": uint64(0x100000), "size": uint64(4096)})
			frame := make([]byte, 4+len(body))
			binary.LittleEndian.PutUint32(frame, uint32(len(body)))
			copy(frame[4:], body)
			if _, _, err = conn.WriteMsgUnix(frame, unix.UnixRights(fds[0]), nil); err != nil {
				return err
			}
			var size [4]byte
			if _, err = io.ReadFull(conn, size[:]); err != nil {
				return err
			}
			ack := make([]byte, binary.LittleEndian.Uint32(size[:]))
			if _, err = io.ReadFull(conn, ack); err != nil {
				return err
			}
			if !strings.Contains(string(ack), "ack") {
				return errors.New(string(ack))
			}
			var fault [32]byte
			fault[0] = 0x12
			binary.LittleEndian.PutUint64(fault[16:], 0x100000)
			if _, err = unix.Write(fds[1], fault[:]); err != nil {
				return err
			}
			<-pc.Ctx.Done()
			pc.NotifyReady() // a late handshake cannot announce a failed VM
			return pc.Ctx.Err()
		},
	})
	if code != -1 || !errors.Is(err, cause) {
		t.Fatalf("first fatal cause lost: code=%d err=%v", code, err)
	}
	if ready.Load() != 0 || cmd.ProcessState == nil {
		t.Fatalf("ready=%d process state=%v", ready.Load(), cmd.ProcessState)
	}
	var status unix.WaitStatus
	if _, err := unix.Wait4(cmd.Process.Pid, &status, unix.WNOHANG, nil); !errors.Is(err, unix.ECHILD) {
		t.Fatalf("sole Wait did not reap CH: %v", err)
	}
}
