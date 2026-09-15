package vhost

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"golang.org/x/sys/unix"
)

type recoveryStream struct {
	fetch.Stream
	read func(context.Context, []byte) (int, error)
}

func (s recoveryStream) ReadAt(ctx context.Context, p []byte, _ uint64) (int, error) {
	return s.read(ctx, p)
}
func (recoveryStream) Close() error { return nil }

// Exercise the real chain and ring path, including COW's implicit base read.
func recoveryQueue(t *testing.T, stream fetch.Stream, write bool) (*Server, *virtq, []byte, *BlockCOW) {
	t.Helper()
	base := NewStreamReader(context.Background(), stream, 4096)
	var backend Backend = &ReadOnlyBackend{R: base}
	var cow *BlockCOW
	if write {
		var err error
		cow, err = OpenBlockCOW(filepath.Join(t.TempDir(), "diff"), base, DiffInit{CreateSize: 4096})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cow.Close() })
		backend = &CowBackend{C: cow}
	}
	s := NewServer("unused", backend, nil)
	mem := make([]byte, 4096)
	const uva = uint64(0x1000)
	s.memTable.SetRegions([]MemRegion{{GuestPhysAddr: 0, UserspaceAddr: uva, MemorySize: uint64(len(mem)), mmapBytes: mem}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	q := &virtq{num: 8, descAddr: uva, availAddr: uva + 128, usedAddr: uva + 192, kickFd: -1, callFd: -1, ctx: ctx, cancel: cancel, stop: make(chan struct{})}
	desc := func(i int, addr uint64, n uint32, flags, next uint16) {
		d := mem[i*16 : (i+1)*16]
		binary.LittleEndian.PutUint64(d, addr)
		binary.LittleEndian.PutUint32(d[8:], n)
		binary.LittleEndian.PutUint16(d[12:], flags)
		binary.LittleEndian.PutUint16(d[14:], next)
	}
	kind := uint32(BlkTypeIn)
	flags := uint16(descFlagNext | descFlagWrite)
	if write {
		kind = BlkTypeOut
		flags = descFlagNext
	}
	binary.LittleEndian.PutUint32(mem[256:], kind)
	desc(0, 256, 16, descFlagNext, 1)
	desc(1, 512, 512, flags, 2)
	desc(2, 1536, 1, descFlagWrite, 0)
	binary.LittleEndian.PutUint16(mem[130:], 1)
	mem[1536] = 0xff
	copy(mem[512:1024], bytes.Repeat([]byte{0x9a}, 512))
	call, err := unix.Eventfd(0, unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	q.callFd = call
	t.Cleanup(func() {
		if q.callFd >= 0 {
			unix.Close(q.callFd)
		}
	})
	return s, q, mem, cow
}
func assertPending(t *testing.T, q *virtq, mem []byte) {
	t.Helper()
	if mem[1536] != 0xff || q.baseIdx != 0 || binary.LittleEndian.Uint16(mem[194:]) != 0 {
		t.Fatalf("failed read published completion: status=%d base=%d used=%d", mem[1536], q.baseIdx, binary.LittleEndian.Uint16(mem[194:]))
	}
}
func TestReadRecoveryRetainsRequestAndCOWMaterialization(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "cow-write"}[write], func(t *testing.T) {
			var calls atomic.Int32
			blocked, release := make(chan struct{}), make(chan struct{})
			stream := recoveryStream{read: func(ctx context.Context, p []byte) (int, error) {
				n := calls.Add(1)
				if n <= 3 {
					for i := range p {
						p[i] = byte(n)
					}
					return len(p), []error{io.ErrClosedPipe, context.DeadlineExceeded, context.Canceled}[n-1]
				}
				close(blocked)
				select {
				case <-release:
				case <-ctx.Done():
					return 0, ctx.Err()
				}
				for i := range p {
					p[i] = 0x42
				}
				return len(p), io.EOF
			}}
			s, q, mem, cow := recoveryQueue(t, stream, write)
			done := make(chan error, 1)
			go func() { done <- s.processQueue(q) }()
			select {
			case <-blocked:
			case <-time.After(time.Second):
				t.Fatal("did not retry")
			}
			assertPending(t, q, mem)
			if cow != nil && cow.DirtyCount() != 0 {
				t.Fatal("failed materialization marked dirty")
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 4 || q.baseIdx != 1 || mem[1536] != BlkStatusOK || binary.LittleEndian.Uint16(mem[194:]) != 1 {
				t.Fatal("request did not complete exactly once")
			}
			if cow != nil {
				got := make([]byte, 4096)
				if _, err := cow.ReadAt(got, 0); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got[:512], bytes.Repeat([]byte{0x9a}, 512)) || !bytes.Equal(got[512:], bytes.Repeat([]byte{0x42}, 3584)) {
					t.Fatal("COW content lost or partially replayed")
				}
			} else if !bytes.Equal(mem[512:1024], bytes.Repeat([]byte{0x42}, 512)) {
				t.Fatal("old partial response escaped")
			}
		})
	}
}
func TestTerminalEOFAndEAGAINNeverComplete(t *testing.T) {
	for _, write := range []bool{false, true} {
		for _, cause := range []error{io.EOF, syscall.EAGAIN} {
			for _, full := range []bool{false, true} {
				t.Run(cause.Error()+map[bool]string{false: "-read", true: "-write"}[write]+map[bool]string{false: "-short", true: "-full"}[full], func(t *testing.T) {
					stream := recoveryStream{read: func(_ context.Context, p []byte) (int, error) {
						n := 0
						if full {
							n = len(p)
						}
						return n, readerr.Mark(cause, false)
					}}
					s, q, mem, cow := recoveryQueue(t, stream, write)
					err := s.processQueue(q)
					if !readretry.IsTerminal(err) || !errors.Is(err, cause) {
						t.Fatalf("terminal chain lost: %v", err)
					}
					assertPending(t, q, mem)
					if cow != nil && cow.DirtyCount() != 0 {
						t.Fatal("fatal write marked dirty")
					}
				})
			}
		}
	}
}
func TestCanceledQueueRetainsCompletion(t *testing.T) {
	entered := make(chan struct{}, 1)
	s, q, mem, _ := recoveryQueue(t, recoveryStream{read: func(_ context.Context, p []byte) (int, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		return 0, io.ErrClosedPipe
	}}, false)
	done := make(chan error, 1)
	go func() { done <- s.processQueue(q) }()
	<-entered
	q.stopReading()
	select {
	case err := <-done:
		if !readretry.IsTerminal(err) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry did not observe stop")
	}
	assertPending(t, q, mem)
}
func TestFatalPrecedesQuiesceDrain(t *testing.T) {
	s, q, mem, _ := recoveryQueue(t, recoveryStream{read: func(_ context.Context, p []byte) (int, error) { return 0, readerr.Mark(io.EOF, false) }}, false)
	fd, err := unix.Eventfd(1, unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	q.kickFd = fd
	q.done = make(chan struct{})
	entered, release := make(chan struct{}), make(chan struct{})
	s.SetReadFatal(func(error) { close(entered); <-release })
	go s.runWorker(0, q)
	<-entered
	drained := make(chan struct{})
	go func() { s.Quiesce(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("freeze passed before fatal was published")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("freeze did not drain")
	}
	s.Resume()
	<-q.done
	assertPending(t, q, mem)
}
func TestQueueStopWhileSnapshotGateHeld(t *testing.T) {
	s, q, _, _ := recoveryQueue(t, recoveryStream{}, false)
	fd, err := unix.Eventfd(1, unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	q.kickFd = fd
	q.done = make(chan struct{})
	s.queues[0] = q
	s.Quiesce()
	defer s.Resume()
	go s.runWorker(0, q)
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop waited for snapshot gate")
	}
}

func TestGetVringBaseAndStopJoinSameWorker(t *testing.T) {
	for range 20 {
		entered := make(chan struct{})
		s, q, mem, _ := recoveryQueue(t, recoveryStream{read: func(ctx context.Context, p []byte) (int, error) { close(entered); <-ctx.Done(); return 0, ctx.Err() }}, false)
		fd, err := unix.Eventfd(1, unix.EFD_CLOEXEC)
		if err != nil {
			t.Fatal(err)
		}
		q.kickFd = fd
		q.done = make(chan struct{})
		s.queues[0] = q
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		conns := make([]*net.UnixConn, 2)
		for i, fd := range pair {
			f := os.NewFile(uintptr(fd), "vring-test")
			c, err := net.FileConn(f)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			conns[i] = c.(*net.UnixConn)
		}
		go s.runWorker(0, q)
		<-entered
		readDone := make(chan struct{})
		go func() { io.Copy(io.Discard, conns[1]); close(readDone) }()
		start := make(chan struct{})
		getDone, stopDone := make(chan struct{}), make(chan struct{})
		go func() {
			<-start
			_ = s.handleGetVringBase(conns[0], &Message{Header: Header{Request: MsgGetVringBase}, Payload: make([]byte, 8)})
			close(getDone)
		}()
		go func() { <-start; s.Stop(); close(stopDone) }()
		close(start)
		for _, done := range []chan struct{}{getDone, stopDone, q.done} {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("GET_VRING_BASE/Stop deadlocked")
			}
		}
		assertPending(t, q, mem)
		s.resetConnectionState()
		conns[0].Close()
		conns[1].Close()
		<-readDone
	}
}
