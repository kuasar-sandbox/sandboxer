package vhost

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
)

// Backend is the per-device abstraction. blk0 (read-only base image)
// uses a BlockReader without a writer. blk1 (COW) uses both a base
// BlockReader (or nil) and a BlockCOW for writes.
type Backend interface {
	// ReadAt reads from the virtual disk at the given offset.
	ReadAt(buf []byte, offset int64) (int, error)
	// WriteAt writes to the virtual disk; returns ErrReadOnly if the
	// backend is read-only.
	WriteAt(buf []byte, offset int64) (int, error)
	// Flush syncs the writable layer (no-op for read-only).
	Flush() error
	// Discard hints that a range is no longer needed (may be no-op).
	Discard(offset, length int64) error
	// Size returns the visible block-device size in bytes.
	Size() int64
	// ReadOnly reports whether writes are disallowed.
	ReadOnly() bool
}

// ReadOnlyBackend wraps a BlockReader as a write-rejecting Backend.
type ReadOnlyBackend struct{ R BlockReader }

func (b *ReadOnlyBackend) ReadAt(buf []byte, offset int64) (int, error) {
	return b.R.ReadAt(buf, offset)
}
func (b *ReadOnlyBackend) WriteAt([]byte, int64) (int, error) { return 0, ErrReadOnly }
func (b *ReadOnlyBackend) Flush() error                       { return nil }
func (b *ReadOnlyBackend) Discard(offset, length int64) error { return nil }
func (b *ReadOnlyBackend) Size() int64                        { return b.R.Size() }
func (b *ReadOnlyBackend) ReadOnly() bool                     { return true }

// CowBackend wraps a BlockCOW.
type CowBackend struct{ C *BlockCOW }

func (b *CowBackend) ReadAt(buf []byte, offset int64) (int, error)  { return b.C.ReadAt(buf, offset) }
func (b *CowBackend) WriteAt(buf []byte, offset int64) (int, error) { return b.C.WriteAt(buf, offset) }
func (b *CowBackend) Flush() error                                  { return b.C.Flush() }
func (b *CowBackend) Discard(offset, length int64) error            { return b.C.Discard(offset, length) }
func (b *CowBackend) Size() int64                                   { return b.C.Size() }
func (b *CowBackend) ReadOnly() bool                                { return false }
func (b *CowBackend) BackendStats() map[string]any                  { return b.C.BackendStats() }

// ErrReadOnly is returned by WriteAt on a read-only backend.
var ErrReadOnly = fmt.Errorf("vhost: backend is read-only")

// Server runs one vhost-user-blk backend on a UDS socket. The cloud-
// hypervisor master connects exactly once; on disconnect, the server
// reaps virtq workers and stops.
type Server struct {
	socketPath string
	backend    Backend
	logf       func(format string, args ...any)
	stats      *Stats

	// unified-memfd invariant (docs/cloud-hypervisor.md §3.1): SET_MEM_TABLE must arrive with
	// fds whose inode matches memfdInode; mmapBytes are sub-slices of
	// memfdSlab (sandbox-ctl's mmap of the same memfd).
	memfdInode uint64
	memfdSlab  []byte

	// Quiesce/Resume gate for snapshot pause window (docs/sandbox.md §11.5).
	pauseMu  sync.Mutex
	inflight sync.WaitGroup

	// negotiated state (modified by master)
	mu               sync.Mutex
	features         uint64
	protocolFeatures uint64
	memTable         MemTable
	queues           []*virtq
	stopOnce         sync.Once
	stop             chan struct{}
	listener         *net.UnixListener
	// activeConn is the currently-connected master conn. Stop closes it
	// to unblock any in-flight ReadMessage. We do NOT set a read deadline
	// on the connection: vhost-user has no keepalive and the control
	// socket is silent for arbitrarily long stretches in steady state,
	// so treating idle as disconnect produces spurious teardowns
	// (5-min cycles where memTable is wiped + master reconnects). The
	// only legitimate "wake the read" trigger is explicit shutdown via
	// Stop, which closes activeConn here.
	activeConn *net.UnixConn
}

// virtq holds per-virtq state set up by SET_VRING_*.
type virtq struct {
	num       uint32
	descAddr  uint64 // GPA
	availAddr uint64 // GPA
	usedAddr  uint64 // GPA
	baseIdx   uint16
	kickFd    int
	callFd    int
	enabled   bool

	stop chan struct{}
	done chan struct{}
}

// NumQueues is the fixed number of request queues per device. The minimal
// profile does not advertise MQ, so virtio_blk_config.num_queues is inactive.
const NumQueues = 1

// NewServer returns an unstarted server bound to socketPath. backend
// owns disk IO. logf may be nil (defaults to no-op).
func NewServer(socketPath string, backend Backend, logf func(format string, args ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{
		socketPath: socketPath,
		backend:    backend,
		logf:       logf,
		queues:     make([]*virtq, NumQueues),
		stop:       make(chan struct{}),
	}
}

// EnableStats turns on per-request counters and coverage tracking,
// labelling the backend in the eventual summary as name (e.g. "blk0")
// with origin path (file path or URI). Must be called before Serve.
// Without it, processChain skips its instrumentation hot path.
func (s *Server) EnableStats(name, path string) {
	s.stats = NewStats(name, path, s.backend.Size())
}

// SetMemfd configures the unified-memfd backing slab. Must be called
// before Serve when CH is patched to share its memfd via SET_MEM_TABLE
// (docs/cloud-hypervisor.md §3.1 invariant). slab is sandbox-ctl's mmap of the memfd; the
// backend never opens its own mmap.
func (s *Server) SetMemfd(inode uint64, slab []byte) {
	s.memfdInode = inode
	s.memfdSlab = slab
}

// Quiesce blocks until no worker is processing a chain and prevents
// new processChain calls from starting until Resume(). Used by the
// snapshot path to freeze backend DMA before reading memfd contents.
//
// The avail ring may accumulate KICKs during Quiesce; Resume() will
// process them in the next iteration of the worker loop.
func (s *Server) Quiesce() {
	s.pauseMu.Lock()
	s.inflight.Wait()
}

// Resume releases a Quiesce(); workers blocked on pauseMu are unblocked.
func (s *Server) Resume() {
	s.pauseMu.Unlock()
}

// Stats returns the live Stats handle (nil if EnableStats wasn't
// called). Callers typically use SnapshotStats to render a summary.
func (s *Server) Stats() *Stats { return s.stats }

// SnapshotStats returns an immutable view of the current stats with
// backend-specific extras merged in (when the backend implements
// StatsReporter). Returns the zero StatsSnapshot if EnableStats wasn't
// called.
func (s *Server) SnapshotStats() StatsSnapshot {
	if s.stats == nil {
		return StatsSnapshot{}
	}
	snap := s.stats.Snapshot()
	if reporter, ok := s.backend.(StatsReporter); ok {
		snap.Extra = reporter.BackendStats()
	}
	return snap
}

// WriteStatsTo formats and writes the current snapshot to w.
func (s *Server) WriteStatsTo(w io.Writer) (int64, error) {
	if s.stats == nil {
		return 0, nil
	}
	return s.SnapshotStats().WriteTo(w)
}

// Listen binds the UDS socket. Call Serve to start accepting.
func (s *Server) Listen() error {
	_ = os.Remove(s.socketPath)
	addr, err := net.ResolveUnixAddr("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("vhost: resolve %s: %w", s.socketPath, err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("vhost: listen %s: %w", s.socketPath, err)
	}
	s.listener = l
	return nil
}

// Serve accepts master connections in a loop and processes messages
// for each. When a master disconnects, per-connection state is reset
// (memtable cleared, virtq workers stopped) and the listener accepts
// the next master. This removes the "single connection per lifetime"
// invariant that previously forced sandbox-init to use POWER_OFF (vs
// RESTART) and that left CH unable to reattach after any reset.
//
// Serve returns nil on Stop() or context cancel. Returns error only on
// fatal accept failures (other than the close-driven case).
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		return fmt.Errorf("vhost: Listen not called")
	}
	defer s.cleanup()

	go func() {
		<-ctx.Done()
		s.Stop()
	}()

	for {
		select {
		case <-s.stop:
			return nil
		default:
		}
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			select {
			case <-s.stop:
				return nil
			default:
				return fmt.Errorf("vhost: accept: %w", err)
			}
		}
		s.logf("vhost: master connected on %s", s.socketPath)
		s.mu.Lock()
		s.activeConn = conn
		s.mu.Unlock()
		s.serveOneMaster(conn)
		s.mu.Lock()
		s.activeConn = nil
		s.mu.Unlock()
		s.resetConnectionState()
	}
}

// serveOneMaster runs the vhost-user protocol loop on a single master
// connection until the master disconnects (EOF / socket close) or Stop
// fires (which closes activeConn to break out of ReadMessage). Errors
// are logged; the loop never returns them up to Serve, since a wedged
// master should not kill the listener.
//
// No read deadline is set: the vhost-user control socket has no
// keepalive semantics — it's silent throughout normal steady-state
// operation (kick/call eventfds carry all the traffic). A per-read
// deadline misclassifies that silence as disconnect.
func (s *Server) serveOneMaster(conn *net.UnixConn) {
	defer conn.Close()
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		msg, err := ReadMessage(conn)
		if err != nil {
			select {
			case <-s.stop:
				return
			default:
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logf("vhost: read: %v (master disconnected)", err)
			return
		}
		if err := s.handle(conn, msg); err != nil {
			s.logf("vhost: handle %s: %v", MsgName(msg.Header.Request), err)
			closeFds(msg.Fds)
			return
		}
	}
}

// resetConnectionState clears per-master state so the next master
// connection starts from a clean slate. virtq workers from the prior
// connection are stopped; memtable, features, and queue addrs are
// zeroed. The backend itself is preserved across reconnects.
//
// Workers block in syscall.Read on the kick eventfd; closing q.stop
// alone is not enough because the Read won't observe the channel
// closure. We close the kickFd which makes the read return EBADF and
// the worker exits. The callFd is also closed for symmetry.
//
// Ordering: queues are detached then fully drained BEFORE the memTable
// is cleared. The opposite order races — a worker that has just popped
// a desc and is mid-translation would observe an empty memTable and
// log "UVA … not in any region" on a desc that the next reconnect's
// worker will re-process anyway. By waiting for stopAndDrainQueues to
// join every worker first, we guarantee no Translate() call is in
// flight when memTable.SetRegions(nil) lands.
func (s *Server) resetConnectionState() {
	s.mu.Lock()
	queues := s.queues
	s.queues = make([]*virtq, NumQueues)
	s.features = 0
	s.protocolFeatures = 0
	s.mu.Unlock()
	// Drain workers first (joins on q.done). No s.mu held — the workers
	// don't need it to exit, and holding it across the join could
	// deadlock if a worker ever takes s.mu mid-iteration.
	stopAndDrainQueues(queues)
	s.mu.Lock()
	s.memTable.SetRegions(nil)
	s.mu.Unlock()
}

// stopAndDrainQueues signals each queue to stop and unblocks any
// worker blocked in syscall.Read on the kick eventfd. We use two
// mechanisms because either alone is racy:
//  1. eventfd_write to the kickFd: the worker's blocking Read returns
//     with the count we wrote; then it observes q.stop on the next
//     loop iteration and exits cleanly.
//  2. close(kickFd) as a backstop: if the worker missed the wakeup
//     for any reason, the next read returns EBADF and the worker
//     exits. close() alone is racy because close on a fd that another
//     thread is already inside a syscall on does not abort the syscall
//     on Linux.
func stopAndDrainQueues(queues []*virtq) {
	for _, q := range queues {
		if q == nil {
			continue
		}
		if q.stop != nil {
			select {
			case <-q.stop:
			default:
				close(q.stop)
			}
		}
		if q.kickFd >= 0 {
			// Wake the blocking Read.
			one := []byte{1, 0, 0, 0, 0, 0, 0, 0}
			_, _ = syscall.Write(q.kickFd, one)
			_ = syscall.Close(q.kickFd)
			q.kickFd = -1
		}
		if q.callFd >= 0 {
			_ = syscall.Close(q.callFd)
			q.callFd = -1
		}
		if q.done != nil {
			<-q.done
		}
	}
}

// Stop terminates the serve loop and stops virtq workers. Closes the
// active master connection (so a serveOneMaster blocked in ReadMessage
// returns immediately) and the kick eventfds (so virtq workers blocked
// in syscall.Read on the kick fd return immediately with EBADF).
// Without these explicit closes, sandbox-ctl shutdown would block
// indefinitely: serveOneMaster has no read deadline (vhost-user is
// silent in steady state, so a deadline would falsely trigger), and
// the virtq worker only checks q.stop between iterations not during
// the syscall.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.mu.Lock()
		queues := s.queues
		conn := s.activeConn
		s.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		stopAndDrainQueues(queues)
	})
}

func (s *Server) cleanup() {
	s.mu.Lock()
	s.memTable.SetRegions(nil)
	s.mu.Unlock()
	_ = os.Remove(s.socketPath)
}

// handle dispatches one received message.
func (s *Server) handle(conn *net.UnixConn, m *Message) error {
	switch m.Header.Request {
	case MsgGetFeatures:
		return s.handleGetFeatures(conn, m)
	case MsgSetFeatures:
		return s.handleSetFeatures(conn, m)
	case MsgGetProtocolFeatures:
		return s.handleGetProtocolFeatures(conn, m)
	case MsgSetProtocolFeatures:
		return s.handleSetProtocolFeatures(conn, m)
	case MsgSetOwner, MsgResetOwner:
		// No payload, no reply required.
		return nil
	case MsgGetQueueNum:
		return SendU64Reply(conn, m.Header.Request, NumQueues)
	case MsgGetConfig:
		return s.handleGetConfig(conn, m)
	case MsgSetConfig:
		// We don't honor SET_CONFIG; reply with empty payload if needed.
		if m.NeedsReply() {
			return SendReply(conn, m.Header.Request, nil)
		}
		return nil
	case MsgSetMemTable:
		return s.handleSetMemTable(conn, m)
	case MsgSetVringNum:
		return s.handleSetVringNum(m)
	case MsgSetVringAddr:
		return s.handleSetVringAddr(m)
	case MsgSetVringBase:
		return s.handleSetVringBase(m)
	case MsgGetVringBase:
		return s.handleGetVringBase(conn, m)
	case MsgSetVringKick:
		return s.handleSetVringKick(m)
	case MsgSetVringCall:
		return s.handleSetVringCall(m)
	case MsgSetVringEnable:
		return s.handleSetVringEnable(m)
	case MsgGetMaxMemSlots:
		return SendU64Reply(conn, m.Header.Request, MaxFds)
	default:
		s.logf("vhost: unhandled %s (size=%d fds=%d)",
			MsgName(m.Header.Request), m.Header.Size, len(m.Fds))
		closeFds(m.Fds)
		if m.NeedsReply() {
			return SendReply(conn, m.Header.Request, nil)
		}
		return nil
	}
}

// VHOST_USER_F_PROTOCOL_FEATURES = bit 30 in vhost-user features.
// VIRTIO_F_VERSION_1            = bit 32 in virtio features.
// VIRTIO_BLK_F_RO               = bit 5
// VIRTIO_BLK_F_FLUSH            = bit 9
// VIRTIO_BLK_F_DISCARD          = bit 13
const (
	bitVhostProtocolFeatures = uint64(1) << 30
	bitVirtioVersion1        = uint64(1) << 32
	bitVirtioBlkRO           = uint64(1) << 5
	bitVirtioBlkFlush        = uint64(1) << 9
	bitVirtioBlkDiscard      = uint64(1) << 13
)

// VHOST_USER_PROTOCOL_F_MQ        = 0
// VHOST_USER_PROTOCOL_F_CONFIG    = 9
const (
	bitProtocolMq     = uint64(1) << 0
	bitProtocolConfig = uint64(1) << 9
)

// advertisedFeatures is the fixed minimal device profile. Do not advertise
// optional features merely because their config fields exist on the wire.
// Existing snapshots keep this same feature set; inactive config bytes do
// not require a new snapshot version or renegotiation.
func (s *Server) advertisedFeatures() uint64 {
	feats := bitVirtioVersion1 | bitVhostProtocolFeatures | bitVirtioBlkFlush
	if s.backend.ReadOnly() {
		feats |= bitVirtioBlkRO
	}
	return feats
}

func (s *Server) handleGetFeatures(conn *net.UnixConn, m *Message) error {
	return SendU64Reply(conn, m.Header.Request, s.advertisedFeatures())
}

func (s *Server) handleSetFeatures(conn *net.UnixConn, m *Message) error {
	if len(m.Payload) != 8 {
		return fmt.Errorf("vhost: SET_FEATURES payload size %d, want 8", len(m.Payload))
	}
	v, err := ParseU64(m.Payload)
	if err != nil {
		return err
	}
	supported := s.advertisedFeatures()
	if unsupported := v &^ supported; unsupported != 0 {
		return fmt.Errorf("vhost: unsupported virtio features: requested=%#x supported=%#x unsupported=%#x",
			v, supported, unsupported)
	}
	s.mu.Lock()
	s.features = v
	s.mu.Unlock()
	s.logf("vhost: SET_FEATURES = 0x%x", v)
	return nil
}

func (s *Server) handleGetProtocolFeatures(conn *net.UnixConn, m *Message) error {
	return SendU64Reply(conn, m.Header.Request, bitProtocolConfig)
}

func (s *Server) handleSetProtocolFeatures(conn *net.UnixConn, m *Message) error {
	if len(m.Payload) != 8 {
		return fmt.Errorf("vhost: SET_PROTOCOL_FEATURES payload size %d, want 8", len(m.Payload))
	}
	v, err := ParseU64(m.Payload)
	if err != nil {
		return err
	}
	if unsupported := v &^ bitProtocolConfig; unsupported != 0 {
		return fmt.Errorf("vhost: unsupported protocol features: requested=%#x supported=%#x unsupported=%#x",
			v, bitProtocolConfig, unsupported)
	}
	s.mu.Lock()
	s.protocolFeatures = v
	s.mu.Unlock()
	return nil
}

func (s *Server) handleGetConfig(conn *net.UnixConn, m *Message) error {
	// GET_CONFIG carries [offset:u32, size:u32, flags:u32] and size bytes.
	// On error the protocol requires an empty reply payload, not a
	// successful truncated config or an allocation from an unchecked size.
	const configHeaderSize = 12
	if len(m.Payload) < configHeaderSize {
		return SendReply(conn, m.Header.Request, nil)
	}
	offset := binary.LittleEndian.Uint32(m.Payload[0:4])
	size := binary.LittleEndian.Uint32(m.Payload[4:8])
	end := uint64(offset) + uint64(size)
	if size == 0 || end > BlkConfigSize ||
		uint64(len(m.Payload)) != configHeaderSize+uint64(size) {
		return SendReply(conn, m.Header.Request, nil)
	}

	// The pinned CH frontend reads the full 60-byte wire layout. Only
	// capacity is active in our profile; all feature-gated bytes are zero.
	// GET flags are echoed, including CH's normal WRITABLE request flag.
	cfg := BlkConfig{Capacity: uint64(s.backend.Size()) / SectorSize}
	full := cfg.Marshal()
	out := make([]byte, configHeaderSize+int(size))
	copy(out[:configHeaderSize], m.Payload[:configHeaderSize])
	copy(out[configHeaderSize:], full[int(offset):int(end)])
	return SendReply(conn, m.Header.Request, out)
}

func (s *Server) handleSetMemTable(_ *net.UnixConn, m *Message) error {
	regs, err := ParseSetMemTable(m.Payload, m.Fds)
	if err != nil {
		closeFds(m.Fds)
		return err
	}
	if s.memfdInode == 0 || len(s.memfdSlab) == 0 {
		closeFds(m.Fds)
		return fmt.Errorf("vhost: SET_MEM_TABLE arrived but SetMemfd was not called " +
			"(unified-memfd invariant requires sandbox-ctl to pre-allocate the slab)")
	}
	if err := BindRegions(regs, m.Fds, s.memfdInode, s.memfdSlab); err != nil {
		closeFds(m.Fds)
		return err
	}
	// fds can be closed now; the backend never opens its own mmap.
	closeFds(m.Fds)

	s.mu.Lock()
	s.memTable.SetRegions(regs)
	s.mu.Unlock()
	s.logf("vhost: SET_MEM_TABLE accepted %d regions (inode-match)", len(regs))
	return nil
}

// queueIdx parses the 4-byte payload prefix used by all SET_VRING_*
// messages: [u32 idx | flags-or-fd-marker | ... data].
func queueIdx(payload []byte) (int, error) {
	if len(payload) < 4 {
		return 0, fmt.Errorf("vhost: vring payload too short: %d", len(payload))
	}
	idx := binary.LittleEndian.Uint32(payload[0:4])
	if idx >= NumQueues {
		return 0, fmt.Errorf("vhost: queue idx %d out of range (max %d)", idx, NumQueues)
	}
	return int(idx), nil
}

func (s *Server) ensureQueue(idx int) *virtq {
	if s.queues[idx] == nil {
		s.queues[idx] = &virtq{kickFd: -1, callFd: -1}
	}
	return s.queues[idx]
}

func (s *Server) handleSetVringNum(m *Message) error {
	if len(m.Payload) < 8 {
		return fmt.Errorf("SET_VRING_NUM payload too short")
	}
	idx, err := queueIdx(m.Payload)
	if err != nil {
		return err
	}
	num := binary.LittleEndian.Uint32(m.Payload[4:8])
	s.mu.Lock()
	q := s.ensureQueue(idx)
	q.num = num
	s.mu.Unlock()
	return nil
}

func (s *Server) handleSetVringAddr(m *Message) error {
	// Payload: u32 idx, u32 flags, u64 desc, u64 used, u64 avail, u64 log
	if len(m.Payload) < 40 {
		return fmt.Errorf("SET_VRING_ADDR payload too short: %d", len(m.Payload))
	}
	idx := binary.LittleEndian.Uint32(m.Payload[0:4])
	desc := binary.LittleEndian.Uint64(m.Payload[8:16])
	used := binary.LittleEndian.Uint64(m.Payload[16:24])
	avail := binary.LittleEndian.Uint64(m.Payload[24:32])
	if idx >= NumQueues {
		return fmt.Errorf("vhost: SET_VRING_ADDR idx %d", idx)
	}
	s.mu.Lock()
	q := s.ensureQueue(int(idx))
	q.descAddr = desc
	q.availAddr = avail
	q.usedAddr = used
	s.mu.Unlock()
	return nil
}

func (s *Server) handleSetVringBase(m *Message) error {
	if len(m.Payload) < 8 {
		return fmt.Errorf("SET_VRING_BASE payload too short")
	}
	idx, err := queueIdx(m.Payload)
	if err != nil {
		return err
	}
	base := binary.LittleEndian.Uint16(m.Payload[4:6])
	s.mu.Lock()
	q := s.ensureQueue(idx)
	q.baseIdx = base
	s.mu.Unlock()
	return nil
}

func (s *Server) handleGetVringBase(conn *net.UnixConn, m *Message) error {
	idx, err := queueIdx(m.Payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	q := s.ensureQueue(idx)
	base := q.baseIdx
	q.enabled = false
	s.mu.Unlock()
	// Stop the worker if running.
	if q.stop != nil {
		close(q.stop)
		<-q.done
		q.stop = nil
	}
	out := make([]byte, 8)
	binary.LittleEndian.PutUint32(out[0:4], uint32(idx))
	binary.LittleEndian.PutUint32(out[4:8], uint32(base))
	return SendReply(conn, m.Header.Request, out)
}

// fdMarker says "idx implicit, no fd" if bit 8 set; otherwise an fd is attached.
const noFdMarker = 1 << 8

func (s *Server) handleSetVringKick(m *Message) error {
	if len(m.Payload) < 4 {
		return fmt.Errorf("SET_VRING_KICK payload too short")
	}
	idx := int(binary.LittleEndian.Uint32(m.Payload[0:4]) & 0xff)
	if idx >= NumQueues {
		closeFds(m.Fds)
		return fmt.Errorf("vhost: SET_VRING_KICK idx %d", idx)
	}
	if len(m.Fds) != 1 {
		closeFds(m.Fds)
		return fmt.Errorf("SET_VRING_KICK expects 1 fd, got %d", len(m.Fds))
	}
	s.mu.Lock()
	q := s.ensureQueue(idx)
	if q.kickFd >= 0 {
		_ = syscall.Close(q.kickFd)
	}
	q.kickFd = m.Fds[0]
	s.mu.Unlock()
	s.maybeStartWorker(idx)
	return nil
}

func (s *Server) handleSetVringCall(m *Message) error {
	if len(m.Payload) < 4 {
		return fmt.Errorf("SET_VRING_CALL payload too short")
	}
	idx := int(binary.LittleEndian.Uint32(m.Payload[0:4]) & 0xff)
	if idx >= NumQueues {
		closeFds(m.Fds)
		return fmt.Errorf("vhost: queue idx %d", idx)
	}
	if len(m.Fds) != 1 {
		closeFds(m.Fds)
		return fmt.Errorf("SET_VRING_CALL expects 1 fd, got %d", len(m.Fds))
	}
	s.mu.Lock()
	q := s.ensureQueue(idx)
	if q.callFd >= 0 {
		_ = syscall.Close(q.callFd)
	}
	q.callFd = m.Fds[0]
	s.mu.Unlock()
	return nil
}

func (s *Server) handleSetVringEnable(m *Message) error {
	if len(m.Payload) < 8 {
		return fmt.Errorf("SET_VRING_ENABLE payload too short")
	}
	idx, err := queueIdx(m.Payload)
	if err != nil {
		return err
	}
	enabled := binary.LittleEndian.Uint32(m.Payload[4:8]) == 1
	s.mu.Lock()
	q := s.ensureQueue(idx)
	q.enabled = enabled
	s.mu.Unlock()
	if enabled {
		s.maybeStartWorker(idx)
	}
	return nil
}

// maybeStartWorker spawns the virtq worker once kick + call + addr are
// all set. Idempotent: only starts once per queue (guarded by stop chan).
func (s *Server) maybeStartWorker(idx int) {
	s.mu.Lock()
	q := s.queues[idx]
	if q == nil || q.kickFd < 0 || q.callFd < 0 || q.descAddr == 0 || q.stop != nil {
		s.mu.Unlock()
		return
	}
	q.stop = make(chan struct{})
	q.done = make(chan struct{})
	s.mu.Unlock()
	go s.runWorker(idx, q)
	s.logf("vhost: started worker for queue %d", idx)
}
