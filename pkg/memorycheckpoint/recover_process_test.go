package memorycheckpoint

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"golang.org/x/sys/unix"
)

type recoveryProcess struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  *json.Decoder
	dir     string
	runDir  string
	path    string
	record  CaptureRecord
	killed  bool
}

func startRecoveryProcess(t *testing.T, stage ...string) *recoveryProcess {
	t.Helper()
	dir := t.TempDir()
	checkpoint := filepath.Join(dir, "checkpoint")
	runDir := filepath.Join(dir, "runs", "runtime")
	for _, p := range []string{checkpoint, runDir} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRecoveryHolderProcess$", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), "KUASAR_MEMORY_RECOVERY_ROOT="+dir)
	if len(stage) != 0 {
		cmd.Env = append(cmd.Env, "KUASAR_MEMORY_RECOVERY_STAGE="+stage[0])
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &recoveryProcess{command: cmd, input: stdin, output: json.NewDecoder(stdout), dir: checkpoint, runDir: runDir}
	t.Cleanup(func() {
		_ = stdin.Close()
		if !p.killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	if err := p.output.Decode(&p.record); err != nil {
		t.Fatalf("holder startup: %v; %s", err, stderr.String())
	}
	p.path, err = RecordPath(checkpoint, p.record.CheckpointID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *recoveryProcess) request(t *testing.T, command string) {
	t.Helper()
	if _, err := fmt.Fprintln(p.input, command); err != nil {
		t.Fatal(err)
	}
	if err := p.output.Decode(&p.record); err != nil {
		t.Fatal(err)
	}
}

func (p *recoveryProcess) kill(t *testing.T) {
	t.Helper()
	if err := p.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err := p.command.Wait()
	var exited *exec.ExitError
	if !errors.As(err, &exited) {
		t.Fatalf("producer was not killed: %v", err)
	}
	p.killed = true
}

// This is an actual producer process, with the real constructor and protocol.
// Commands provide ordering barriers, not alternative production behavior.
func TestRecoveryHolderProcess(t *testing.T) {
	root := os.Getenv("KUASAR_MEMORY_RECOVERY_ROOT")
	if root == "" {
		return
	}
	fail := func(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(2) }
	m, err := memory.Create("recovery-process", 8192)
	if err != nil {
		fail(err)
	}
	copy(m.Bytes(), bytes.Repeat([]byte{0x35}, 8192))
	fd, err := ReadOnlyMemfd(m.FD())
	if err != nil {
		fail(err)
	}
	h, err := PrepareHolder(filepath.Join(root, "checkpoint"), filepath.Join(root, "runs", "runtime"), CaptureRecord{Version: Version, Producer: RunIdentity{SandboxID: "sid", InstanceID: "instance", RunID: "run"}, CheckpointID: "checkpoint", MemoryID: "memory", Epoch: 1}, fd, 8192, nil)
	if err != nil {
		fail(err)
	}
	if err := m.Close(); err != nil {
		fail(err)
	}
	stage := os.Getenv("KUASAR_MEMORY_RECOVERY_STAGE")
	if stage != "uncommitted" {
		if err := h.Record().Update(func(r *CaptureRecord) {
			r.Committed = true
			r.RuntimeRetired = stage != "committed"
			r.SnapshotRef = "snapshot"
			r.SandboxRef = "sandbox"
		}); err != nil {
			fail(err)
		}
	}
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(h.Record().Read()); err != nil {
		fail(err)
	}
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		switch scan.Text() {
		case "published":
			if err := h.Record().Update(func(r *CaptureRecord) { r.FileReady = true; r.PayloadRef = "payload" }); err != nil {
				fail(err)
			}
		case "closed":
			h.mu.Lock()
			err := h.closeRAMLocked()
			h.mu.Unlock()
			if err != nil {
				fail(err)
			}
		case "unlock":
			if err := h.Record().Close(); err != nil {
				fail(err)
			}
		case "barrier":
			// The header has reached the client. serveConn holds this mutex
			// through the subsequent SCM_RIGHTS send, so its pair is queued.
			h.mu.Lock()
			h.mu.Unlock()
		default:
			fail(errors.New("unknown process test command"))
		}
		if err := enc.Encode(h.Record().Read()); err != nil {
			fail(err)
		}
	}
	os.Exit(0)
}

func TestRecoverKilledHolderQueuedRAMPair(t *testing.T) {
	for _, finish := range []string{"received", "discarded", "truncated"} {
		t.Run(finish, func(t *testing.T) {
			p := startRecoveryProcess(t)
			c, err := dialHolder(context.Background(), p.record.Holder)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := writeHolderMessage(c, holderMessage{Type: "open", MemoryID: p.record.MemoryID, Epoch: p.record.Epoch, CheckpointID: p.record.CheckpointID, Token: "queued"}); err != nil {
				t.Fatal(err)
			}
			var reply holderMessage
			if err := readHolderMessage(c, &reply); err != nil || reply.Type != "ram" {
				t.Fatalf("open=%+v %v", reply, err)
			}
			p.request(t, "barrier")
			p.kill(t)
			got, err := RecoverMemoryRelease(context.Background(), p.path, p.record)
			if err != nil || !got.ProducerExited || got.Record.RAMReleased {
				t.Fatalf("queued pair was not retained: %+v %v", got, err)
			}
			if _, err := DiscardCapture(context.Background(), p.dir, p.record.Producer, p.record.CheckpointID); !errors.Is(err, unix.EWOULDBLOCK) {
				t.Fatalf("discard bypassed queued RAM: %v", err)
			}
			if finish == "received" {
				ram, pin, err := receiveRAMPair(c)
				if err != nil {
					t.Fatal(err)
				}
				defer ram.Close()
				defer pin.Close()
				for _, fd := range []*os.File{ram, pin} {
					flags, err := unix.FcntlInt(fd.Fd(), unix.F_GETFD, 0)
					if err != nil || flags&unix.FD_CLOEXEC == 0 {
						t.Fatalf("received descriptor is inheritable: %d %v", flags, err)
					}
				}
				if err := validateRAMPin(pin, p.record.Holder.RAMLock); err != nil {
					t.Fatal(err)
				}
				mapped, err := unix.Mmap(int(ram.Fd()), 0, 8192, unix.PROT_READ, unix.MAP_SHARED)
				if err != nil {
					t.Fatal(err)
				}
				if err := ram.Close(); err != nil {
					t.Fatal(err)
				}
				if mapped[0] != 0x35 {
					t.Fatal("queued RAM changed after producer exit")
				}
				got, err = RecoverMemoryRelease(context.Background(), p.path, p.record)
				if err != nil || got.Record.RAMReleased {
					t.Fatalf("mapped RAM prematurely released: %+v %v", got, err)
				}
				if err := unix.Munmap(mapped); err != nil {
					t.Fatal(err)
				}
				if err := pin.Close(); err != nil {
					t.Fatal(err)
				}
			} else if finish == "truncated" {
				var one [1]byte
				_, _, flags, _, err := c.ReadMsgUnix(one[:], nil)
				if err != nil || flags&unix.MSG_CTRUNC == 0 {
					t.Fatalf("expected rejected truncated rights: flags=%d %v", flags, err)
				}
			} else {
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}
			got, err = RecoverMemoryRelease(context.Background(), p.path, p.record)
			if err != nil || !got.Record.Lost() {
				t.Fatalf("last pair close did not settle lost capture: %+v %v", got, err)
			}
			if got.Record.Discarded || got.Record.FileReady {
				t.Fatal("automatic recovery invented file/discard")
			}
			if _, err := WaitCurrent(context.Background(), p.dir, "sid", true); err != nil {
				t.Fatalf("lost historical source kept SID pending: %v", err)
			}
		})
	}
}

func TestReceiveRAMPairRejectsAndClosesEveryDescriptor(t *testing.T) {
	for _, count := range []int{1, 3, 8} {
		t.Run(fmt.Sprintf("fds-%d", count), func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "capture.json")
			pin, id, err := createRAMPin(path)
			if err != nil {
				t.Fatal(err)
			}
			defer pin.Close()
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			connections := make([]*net.UnixConn, 0, 2)
			for _, fd := range pair {
				f := os.NewFile(uintptr(fd), "ram-pair-test")
				conn, err := net.FileConn(f)
				_ = f.Close()
				if err != nil {
					t.Fatal(err)
				}
				c := conn.(*net.UnixConn)
				connections = append(connections, c)
				defer c.Close()
			}
			fds := make([]int, count)
			for i := range fds {
				fds[i] = int(pin.Fd())
			}
			if _, _, err := connections[0].WriteMsgUnix([]byte{1}, unix.UnixRights(fds...), nil); err != nil {
				t.Fatal(err)
			}
			if err := pin.Close(); err != nil {
				t.Fatal(err)
			}
			if proof, err := openRAMPin(path, id, unix.LOCK_EX); !errors.Is(err, unix.EWOULDBLOCK) {
				if proof != nil {
					proof.Close()
				}
				t.Fatalf("queued rights did not hold the proof: %v", err)
			}
			if ram, pin, err := receiveRAMPair(connections[1]); err == nil || ram != nil || pin != nil {
				t.Fatal("invalid descriptor count accepted")
			}
			proof, err := openRAMPin(path, id, unix.LOCK_EX)
			if err != nil {
				t.Fatalf("rejected transfer leaked a received pin: %v", err)
			}
			proof.Close()
		})
	}
}

func recoveryOpenOptions(t *testing.T, p *recoveryProcess, allowFile <-chan struct{}) (Descriptor, OpenOptions) {
	t.Helper()
	view := &memory.CaptureView{Size: 8192, Materialized: []byte{3}, Dirty: []byte{3}, Released: []byte{0}}
	encoded, err := EncodeMap(view)
	if err != nil {
		t.Fatal(err)
	}
	mapRef := "file://map.image@digest:" + strings.Repeat("1", 64)
	opts := OpenOptions{Directory: p.dir, OpenFile: func(ctx context.Context, ref string) (fetch.Stream, error) {
		var b []byte
		if ref == mapRef {
			b = encoded
		} else if ref == "payload" {
			if allowFile != nil {
				select {
				case <-allowFile:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			b = bytes.Repeat([]byte{0x35}, 8192)
		} else {
			return nil, errors.New("unexpected reference")
		}
		s, err := sparse.NewSource(bytes.NewReader(b), uint64(len(b)), nil)
		return &testStream{Source: s}, err
	}}
	return Descriptor{Version: Version, MemoryID: p.record.MemoryID, CheckpointID: p.record.CheckpointID, Size: 8192, MapRef: mapRef}, opts
}

func TestRecoverPublishedFileSwitchesAfterProducerDeath(t *testing.T) {
	p := startRecoveryProcess(t)
	allowFile := make(chan struct{})
	d, opts := recoveryOpenOptions(t, p, allowFile)
	top, err := OpenTop(context.Background(), d, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer top.Close()
	p.request(t, "published")
	p.kill(t)
	got, err := RecoverMemoryRelease(context.Background(), p.path, p.record)
	if err != nil || got.Record.RAMReleased {
		t.Fatalf("live borrowed RAM released: %+v %v", got, err)
	}
	// A disk reader retains the GC pin. It must not retain the independent RAM
	// proof or need an ACK from the dead producer to complete its active switch.
	close(allowFile)
	select {
	case <-top.done:
	case <-time.After(3 * time.Second):
		t.Fatal("dead producer prevented file switch")
	}
	got, err = RecoverMemoryRelease(context.Background(), p.path, p.record)
	if err != nil || !got.Record.RAMReleased || !got.Record.FileReady || got.Record.Lost() {
		t.Fatalf("file recovery=%+v %v", got, err)
	}
	var b [1]byte
	if _, err := top.Reader.ReadAt(b[:], 4096); err != nil || b[0] != 0x35 {
		t.Fatalf("file data=%x %v", b[0], err)
	}
	if lock, err := ExclusiveDirectory(p.dir); err == nil {
		lock.Close()
		t.Fatal("disk reader lost its GC protection")
	}
}

func TestRecoverClosedRAMBeforeReleasePublication(t *testing.T) {
	for _, fileReady := range []bool{false, true} {
		t.Run(fmt.Sprintf("file-%v", fileReady), func(t *testing.T) {
			p := startRecoveryProcess(t)
			if fileReady {
				p.request(t, "published")
			}
			p.request(t, "closed")
			p.kill(t)
			got, err := RecoverMemoryRelease(context.Background(), p.path, p.record)
			if err != nil || !got.Record.RAMReleased || got.Record.FileReady != fileReady || got.Record.Lost() == fileReady {
				t.Fatalf("closed RAM recovery=%+v %v", got, err)
			}
			if !fileReady {
				d, opts := recoveryOpenOptions(t, p, nil)
				if top, err := OpenTop(context.Background(), d, opts); !errors.Is(err, ErrMemoryLost) || top != nil {
					t.Fatalf("lost source was not rejected: %v", err)
				}
				w := &RecordWriter{path: p.path, record: got.Record}
				if err := w.Update(func(r *CaptureRecord) { r.FileReady = true; r.PayloadRef = "invented" }); err == nil {
					t.Fatal("lost source was promoted to a file")
				}
			}
			if err := RemoveHolderDirectory(got.Record); err != nil {
				t.Fatal(err)
			}
			if err := RemoveHolderDirectory(got.Record); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(p.runDir); err != nil {
				t.Fatalf("holder cleanup touched runtime directory: %v", err)
			}
		})
	}
}

func TestRecoverKilledProducerBeforeParentReaps(t *testing.T) {
	p := startRecoveryProcess(t)
	p.request(t, "published")
	if err := p.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	p.killed = true
	defer p.command.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for {
		start, state, err := processIdentityState(p.record.Holder.PID)
		if err != nil {
			t.Fatalf("unreaped producer identity disappeared: %v", err)
		}
		if start != p.record.Holder.StartTime {
			t.Fatal("unreaped producer PID reused")
		}
		if state == 'Z' || state == 'X' || state == 'x' {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("killed child did not enter exited state")
		}
		time.Sleep(time.Millisecond)
	}
	// The leader can become a zombie just before the final task releases
	// shared files. Nonblocking recovery is retried without reaping the child.
	for {
		got, err := RecoverMemoryRelease(context.Background(), p.path, p.record)
		if err != nil {
			t.Fatal(err)
		}
		if got.ProducerExited && got.Record.RAMReleased && got.Record.FileReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unreaped producer retained released RAM: %+v", got)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRecoveryProofFailuresNeverPublishRelease(t *testing.T) {
	for _, failure := range []string{"missing-pin", "replaced-pin", "changed-identity", "writer-busy", "runtime-not-retired"} {
		t.Run(failure, func(t *testing.T) {
			p := startRecoveryProcess(t)
			p.kill(t)
			expected := p.record
			var lock *os.File
			switch failure {
			case "missing-pin", "replaced-pin":
				if err := os.Remove(p.path + ".ram.lock"); err != nil {
					t.Fatal(err)
				}
				if failure == "replaced-pin" {
					if err := os.WriteFile(p.path+".ram.lock", nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "changed-identity":
				expected.MemoryID = "wrong"
			case "writer-busy":
				var err error
				lock, err = os.OpenFile(func() string {
					path, _ := preparationLockPath(filepath.Dir(filepath.Dir(filepath.Dir(p.path))), p.record.CheckpointID)
					return path
				}(), os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			case "runtime-not-retired":
				expected.RuntimeRetired = false
				if err := writeJSON(p.path, expected); err != nil {
					t.Fatal(err)
				}
			}
			got, err := RecoverMemoryRelease(context.Background(), p.path, expected)
			if got.Record.RAMReleased {
				t.Fatalf("failed proof published RAMReleased: %+v %v", got, err)
			}
			if failure != "writer-busy" && failure != "runtime-not-retired" && err == nil {
				t.Fatal("invalid proof was hidden")
			}
			actual, err := ReadCaptureRecord(p.path)
			if err != nil || actual.RAMReleased {
				t.Fatalf("invalid durable release: %+v %v", actual, err)
			}
			if failure == "missing-pin" || failure == "replaced-pin" {
				if _, err := DiscardCapture(context.Background(), p.dir, expected.Producer, expected.CheckpointID); err == nil {
					t.Fatal("discard bypassed missing/replaced pin")
				}
			}
		})
	}
}
