package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

type usageRawResult struct {
	index      int
	memory     proto.UsageMemory
	filesystem proto.UsageFilesystem
}

// A slot belongs to a source for the Guest lifetime, including reconnect,
// quiesce rollback and memory restore. Its mutex never spans a syscall.
type usageSource struct {
	mu                sync.Mutex
	busy, closed      bool
	disk, incarnation string
	read              func() usageRawResult
	close             func()
}

func (s *usageSource) start(index int, out chan<- usageRawResult) bool {
	s.mu.Lock()
	if s.busy || s.closed {
		s.mu.Unlock()
		return false
	}
	s.busy = true
	s.mu.Unlock()
	go func() {
		start := time.Now()
		r := s.read()
		r.index = index
		elapsed := time.Since(start).Nanoseconds()
		if index == 0 {
			r.memory.DurationNS = elapsed
		} else {
			r.filesystem.DurationNS = elapsed
		}
		// The result channel belongs to one request and has a slot per source.
		// A late result can neither block here nor become a newer observation.
		s.mu.Lock()
		s.busy = false
		closed := s.closed
		s.mu.Unlock()
		out <- r
		if closed && s.close != nil {
			s.close()
		}
	}()
	return true
}

func (s *usageSource) stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	busy := s.busy
	s.mu.Unlock()
	if !busy && s.close != nil {
		s.close()
	}
}

type usageService struct {
	mu          sync.Mutex
	paused      bool
	resuming    bool // admission reopened, but ACK/MUX/thaw not yet committed
	generation  uint64
	epoch       string
	lastRequest uint64
	conn        *vsockConn
	connDone    chan struct{}
	cancel      chan struct{}
	sources     []*usageSource
}

func newUsageService() *usageService {
	memory := &usageSource{read: func() usageRawResult {
		m, err := readUsageMemory()
		if err != nil && m.Status == "" {
			m.Status = proto.UsageInvalid
		}
		return usageRawResult{memory: m}
	}}
	return &usageService{sources: []*usageSource{memory}}
}

var guestUsage = newUsageService()

// Register runs only at controlled disk assembly, before switch-root hides the
// original upper. CLOEXEC handles never enter application ExtraFiles. Keeping
// the handles even with Host usage disabled allows enablement after restore;
// no proc/statfs observation or worker starts until a usage request arrives.
func (s *usageService) register(disk, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, source := range s.sources {
		if source.disk == disk {
			return nil
		}
	}
	if len(s.sources) > proto.MaxUsageFilesystems {
		return errors.New("too many managed usage filesystems")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return err
	}
	incarnation := fmt.Sprintf("%s/%d", disk, st.Dev)
	source := &usageSource{disk: disk, incarnation: incarnation, close: func() { _ = f.Close() }}
	source.read = func() usageRawResult {
		r := proto.UsageFilesystem{Disk: disk, Incarnation: incarnation, UsageReadState: proto.UsageReadState{Status: proto.UsageOK}}
		var stat unix.Statfs_t
		if err := unix.Fstatfs(fd, &stat); err != nil {
			r.Status = proto.UsageError
		} else if stat.Bsize <= 0 || stat.Frsize < 0 || uint64(stat.Type) != unix.EXT4_SUPER_MAGIC {
			r.Status = proto.UsageUnsupported
		} else {
			r.Blocks, r.BFree = usageUint(stat.Blocks), usageUint(stat.Bfree)
			r.BlockSize, r.FragmentSize, r.Type = usageUint(uint64(stat.Bsize)), usageUint(uint64(stat.Frsize)), usageUint(uint64(stat.Type))
		}
		return usageRawResult{filesystem: r}
	}
	s.sources = append(s.sources, source)
	return nil
}

func (s *usageService) collect(req proto.UsageRequest, cancel <-chan struct{}) proto.UsageResponse {
	r := proto.UsageResponse{RunEpoch: req.RunEpoch, RequestID: req.RequestID, Memory: proto.UsageMemory{UsageReadState: proto.UsageReadState{Status: proto.UsageTimeout}}}
	s.mu.Lock()
	sources := append([]*usageSource(nil), s.sources...)
	s.mu.Unlock()
	r.Filesystems = make([]proto.UsageFilesystem, len(sources)-1)
	for i, source := range sources[1:] {
		r.Filesystems[i] = proto.UsageFilesystem{Disk: source.disk, Incarnation: source.incarnation, UsageReadState: proto.UsageReadState{Status: proto.UsageTimeout}}
	}
	out := make(chan usageRawResult, len(sources))
	deadline := time.Now().Add(time.Duration(req.ReadBudgetNS))
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	pending := 0
	for i, source := range sources {
		if source.start(i, out) {
			pending++
		} else if i == 0 {
			r.Memory.Status = proto.UsageBusy
		} else {
			r.Filesystems[i-1].Status = proto.UsageBusy
		}
	}
	for pending > 0 {
		select {
		case value := <-out:
			if !time.Now().Before(deadline) {
				return r
			}
			pending--
			if value.index == 0 {
				r.Memory = value.memory
			} else {
				r.Filesystems[value.index-1] = value.filesystem
			}
		case <-timer.C:
			return r
		case <-cancel:
			return r
		}
	}
	return r
}

// usageIO enforces an absolute deadline across partial reads/writes and EINTR.
// SO_RCVTIMEO/SO_SNDTIMEO alone renew their budget on each raw syscall. Idle
// usage connections have no timer; the single connection slot and lifecycle
// shutdown bound their ownership. Each complete request supplies a new budget.
type usageIO struct {
	conn *vsockConn
	end  time.Time
}

func (c usageIO) io(op fdIOFunc, b []byte) (int, error) {
	for {
		if !c.end.IsZero() && !time.Now().Before(c.end) {
			return 0, context.DeadlineExceeded
		}
		if err := c.conn.SetDeadline(c.end); err != nil {
			return 0, err
		}
		n, err := op(c.conn.fd, b)
		if n > 0 {
			return n, err
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return n, err
	}
}
func (c usageIO) Read(b []byte) (int, error) {
	n, err := c.io(syscall.Read, b)
	if n == 0 && err == nil {
		err = io.EOF
	}
	return n, err
}
func (c usageIO) Write(b []byte) (int, error) { return c.io(syscall.Write, b) }

func (s *usageService) serve(c *vsockConn, first *proto.Message) {
	s.mu.Lock()
	if s.paused || s.conn != nil {
		s.mu.Unlock()
		_ = proto.WriteMessage(usageIO{c, time.Now().Add(100 * time.Millisecond)}, &proto.Message{Type: proto.TypeError, Msg: "usage busy"})
		return
	}
	s.conn, s.connDone, s.cancel = c, make(chan struct{}), make(chan struct{})
	done, cancel := s.connDone, s.cancel
	s.mu.Unlock()
	defer func() {
		_ = c.SetLinger(muxCloseLingerSec)
		_ = c.Close()
		s.mu.Lock()
		if s.conn == c {
			s.conn = nil
		}
		close(done)
		s.mu.Unlock()
	}()
	for msg := first; msg != nil; {
		if msg.Type != proto.TypeUsageRequest || msg.UsageRequest == nil {
			return
		}
		req := *msg.UsageRequest
		if req.RunEpoch == "" || len(req.RunEpoch) > 128 || req.RequestID == 0 || req.ReadBudgetNS <= 0 || req.ReadBudgetNS > int64(time.Second) {
			return
		}
		s.mu.Lock()
		valid := !s.paused && (s.epoch == "" || s.epoch == req.RunEpoch) && req.RequestID > s.lastRequest
		// pause permanently cancels this connection, even if resume has
		// already reopened admission. Plain attach may advance the lifecycle
		// generation without canceling this still-live usage connection.
		select {
		case <-cancel:
			valid = false
		default:
		}
		if valid {
			s.epoch, s.lastRequest = req.RunEpoch, req.RequestID
		}
		s.mu.Unlock()
		if !valid {
			return
		}
		// Encoding/transmission owns a separate small remainder. Host still
		// validates its own original end-to-end deadline and rejects late data.
		end := time.Now().Add(time.Duration(req.ReadBudgetNS) + time.Duration(req.ReadBudgetNS)/4)
		response := s.collect(req, cancel)
		select {
		case <-cancel:
			return
		default:
		}
		if err := proto.WriteMessage(usageIO{c, end}, &proto.Message{Type: proto.TypeUsageResponse, UsageResponse: &response}); err != nil {
			return
		}
		var err error
		msg, err = proto.ReadMessage(usageIO{conn: c})
		if err != nil {
			return
		}
	}
}

func (s *usageService) pause(ctx context.Context) error {
	s.mu.Lock()
	return s.pauseLocked(ctx)
}

// A failed reattach closes only the gate it reopened, never a later lifecycle
// generation or an already-live usage connection from a plain MUX reconnect.
func (s *usageService) rollbackResume(ctx context.Context, generation uint64) error {
	s.mu.Lock()
	if s.generation != generation {
		s.mu.Unlock()
		return nil
	}
	return s.pauseLocked(ctx)
}

// pauseLocked consumes mu; no lock is held while joining the network owner.
func (s *usageService) pauseLocked(ctx context.Context) error {
	s.paused = true
	s.resuming = false
	s.generation++
	c, done := s.conn, s.connDone
	if c != nil {
		select {
		case <-s.cancel:
		default:
			close(s.cancel)
		}
	}
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	// shutdown interrupts the network syscall. It does not pretend to cancel
	// a source's fstatfs/proc syscall, whose original slot remains occupied.
	_ = c.shutdown()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *usageService) resume(newHost bool) (needsRollback bool, generation uint64) {
	s.mu.Lock()
	// A retry owns its own rollback boundary even without an intervening
	// pause. An older failed ACK/thaw must not close a newer admitted gate.
	s.generation++
	// A retry inherits an uncommitted reopening, not the already-live state
	// of an ordinary MUX reconnect. If both attempts fail, admission closes.
	needsRollback, generation = s.paused || s.resuming, s.generation
	s.resuming = needsRollback
	if newHost {
		s.epoch = ""
		s.lastRequest = 0
	}
	s.paused = false
	s.mu.Unlock()
	return needsRollback, generation
}

func (s *usageService) commitResume(generation uint64) {
	s.mu.Lock()
	if s.generation == generation {
		s.resuming = false
	}
	s.mu.Unlock()
}
func (s *usageService) close() {
	for _, source := range s.sources {
		source.stop()
	}
}
