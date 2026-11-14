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

	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
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
	// Flush checks health; COW uses the documented non-durable no-op.
	Flush() error
	// Discard removes complete pages and masks the lower layer with zero.
	Discard(offset, length int64) error
	// SupportsDiscard is immutable for the backend's lifetime.
	SupportsDiscard() bool
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
func (b *ReadOnlyBackend) SupportsDiscard() bool              { return false }

// CowBackend wraps a BlockCOW.
type CowBackend struct{ C *diffstore.Disk }

func (b *CowBackend) ReadAt(buf []byte, offset int64) (int, error)  { return b.C.ReadAt(buf, offset) }
func (b *CowBackend) WriteAt(buf []byte, offset int64) (int, error) { return b.C.WriteAt(buf, offset) }
func (b *CowBackend) Flush() error                                  { return b.C.Flush() }
func (b *CowBackend) Discard(offset, length int64) error            { return b.C.Discard(offset, length) }
func (b *CowBackend) Size() int64                                   { return b.C.Size() }
func (b *CowBackend) ReadOnly() bool                                { return false }
func (b *CowBackend) SupportsDiscard() bool                         { return true }
func (b *CowBackend) BackendStats() map[string]any                  { return b.C.BackendStats() }

// ErrReadOnly is returned by WriteAt on a read-only backend.
var ErrReadOnly = fmt.Errorf("vhost: backend is read-only")

// Server runs one vhost-user-blk backend on a UDS socket. The cloud-
// hypervisor master connects exactly once; on disconnect, the server
// reaps virtq workers and stops.
type Server struct {
	capacityNotify   bool     // advertise CONFIG_CHANGE_MSG for dynamic slots
	frontendReq      *os.File // guarded by mu
	socketPath       string
	backend          Backend
	logf             func(format string, args ...any)
	stats            *Stats
	ctx              context.Context
	onReadFatal      func(error)
	discardSupported bool // fixed at device construction, including pooled devices

	// unified-memfd invariant (docs/vmm.md#patch-0001): SET_MEM_TABLE must arrive with
	// fds whose inode matches memfdInode; mmapBytes are sub-slices of
	// memfdSlab (sandbox-ctl's mmap of the same memfd).
	memfdInode uint64
	memfdSlab  []byte

	// Quiesce/Resume gate for snapshot pause window (docs/block-storage.md#quiesce-and-close).
	pauseMu  pauseGate
	inflight sync.WaitGroup

	// negotiated state (modified by master)
	mu               sync.Mutex
	features         uint64
	featuresSet      bool
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
	num         uint32
	descAddr    uint64 // GPA
	availAddr   uint64 // GPA
	usedAddr    uint64 // GPA
	baseIdx     uint16
	kickFd      int
	callFd      int
	enabled     bool
	kickArmed   bool   // GET_VRING_BASE disarms implicit start until the next KICK
	readScratch []byte // private to the queue worker

	stop     chan struct{}
	done     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
	drainMu  sync.Mutex
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
		socketPath:       socketPath,
		backend:          backend,
		discardSupported: !backend.ReadOnly() && backend.SupportsDiscard(),
		logf:             logf,
		queues:           make([]*virtq, NumQueues),
		stop:             make(chan struct{}),
	}
}

// SetReadFatal installs the runtime owner's non-blocking termination report.
// It must be set before Serve; it must not synchronously Stop or join workers.
func (s *Server) SetReadFatal(report func(error)) { s.onReadFatal = report }

func (q *virtq) readContext() context.Context {
	if q.ctx != nil {
		return q.ctx
	}
	return context.Background()
}

func (q *virtq) stopped() error {
	select {
	case <-q.stop:
		return readretry.Terminal(context.Canceled)
	default:
	}
	return readretry.Terminal(q.readContext().Err())
}

func (q *virtq) stopReading() {
	if q.cancel != nil {
		q.cancel()
	}
	if q.stop != nil {
		q.stopOnce.Do(func() {
			select {
			case <-q.stop:
			default:
				close(q.stop)
			}
		})
	}
}

func (b *ReadOnlyBackend) readAt(ctx context.Context, buf []byte, off int64) (int, error) {
	return readBlock(ctx, b.R, buf, off)
}
func (b *CowBackend) readAt(ctx context.Context, buf []byte, off int64) (int, error) {
	return b.C.ReadAtContext(ctx, buf, off)
}
func (b *CowBackend) writeAt(ctx context.Context, buf []byte, off int64) (int, error) {
	return b.C.WriteAtContext(ctx, buf, off)
}
func (s *Server) readAt(ctx context.Context, buf []byte, off int64) (int, error) {
	return readBackendAt(ctx, s.backend, buf, off)
}

func readBackendAt(ctx context.Context, backend Backend, buf []byte, off int64) (int, error) {
	if b, ok := backend.(interface {
		readAt(context.Context, []byte, int64) (int, error)
	}); ok {
		return b.readAt(ctx, buf, off)
	}
	return backend.ReadAt(buf, off)
}
func (s *Server) writeAt(ctx context.Context, buf []byte, off int64) (int, error) {
	return writeBackendAt(ctx, s.backend, buf, off)
}

func writeBackendAt(ctx context.Context, backend Backend, buf []byte, off int64) (int, error) {
	if b, ok := backend.(interface {
		writeAt(context.Context, []byte, int64) (int, error)
	}); ok {
		return b.writeAt(ctx, buf, off)
	}
	return backend.WriteAt(buf, off)
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
// (docs/vmm.md#patch-0001 invariant). slab is sandbox-ctl's mmap of the memfd; the
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
	s.ctx = ctx
	defer func() { s.Stop(); s.cleanup() }()

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
	s.featuresSet = false
	s.protocolFeatures = 0
	if s.frontendReq != nil {
		_ = s.frontendReq.Close()
		s.frontendReq = nil
	}
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
		q.drainMu.Lock()
		q.stopReading()
		if q.kickFd >= 0 {
			// Wake the blocking Read.
			one := []byte{1, 0, 0, 0, 0, 0, 0, 0}
			_, _ = syscall.Write(q.kickFd, one)
			_ = syscall.Close(q.kickFd)
		}
		if q.done != nil {
			<-q.done
		}
		q.kickFd = -1
		if q.callFd >= 0 {
			_ = syscall.Close(q.callFd)
			q.callFd = -1
		}
		q.drainMu.Unlock()
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
	case MsgSetBackendReqFd:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.protocolFeatures&bitBackendRequest == 0 || len(m.Payload) != 0 || len(m.Fds) != 1 {
			return errors.New("vhost: invalid or unnegotiated SET_BACKEND_REQ_FD")
		}
		if s.frontendReq != nil {
			_ = s.frontendReq.Close()
		}
		s.frontendReq = os.NewFile(uintptr(m.Fds[0]), "vhost-user frontend request")
		return nil
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
	bitVirtioBlkSegMax       = uint64(1) << 2
	blkSegMax                = 64
	bitVirtioBlkRO           = uint64(1) << 5
	bitVirtioBlkFlush        = uint64(1) << 9
	bitVirtioBlkDiscard      = uint64(1) << 13
)

// VHOST_USER_PROTOCOL_F_MQ        = 0
// VHOST_USER_PROTOCOL_F_CONFIG    = 9
const (
	bitBackendRequest = uint64(1) << 5
	bitProtocolMq     = uint64(1) << 0
	bitProtocolConfig = uint64(1) << 9
)

// Existing snapshots may restore any supported subset. In particular, an old
// GuestSnapshot keeps its saved features without gaining DISCARD on restore.
func (s *Server) advertisedFeatures() uint64 {
	feats := bitVirtioVersion1 | bitVhostProtocolFeatures | bitVirtioBlkFlush | bitVirtioBlkSegMax
	if s.backend.ReadOnly() {
		feats |= bitVirtioBlkRO
	}
	if s.discardSupported {
		feats |= bitVirtioBlkDiscard
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
	for _, q := range s.queues {
		if q != nil {
			if err := validateQueueSize(v, q.num); err != nil {
				s.mu.Unlock()
				return err
			}
		}
	}
	s.features = v
	s.featuresSet = true
	s.mu.Unlock()
	for idx := 0; idx < NumQueues; idx++ {
		s.maybeStartWorker(idx)
	}
	s.logf("vhost: SET_FEATURES = 0x%x", v)
	return nil
}

func (s *Server) handleGetProtocolFeatures(conn *net.UnixConn, m *Message) error {
	return SendU64Reply(conn, m.Header.Request, s.supportedProtocolFeatures())
}

func (s *Server) handleSetProtocolFeatures(conn *net.UnixConn, m *Message) error {
	if len(m.Payload) != 8 {
		return fmt.Errorf("vhost: SET_PROTOCOL_FEATURES payload size %d, want 8", len(m.Payload))
	}
	v, err := ParseU64(m.Payload)
	if err != nil {
		return err
	}
	if unsupported := v &^ s.supportedProtocolFeatures(); unsupported != 0 {
		return fmt.Errorf("vhost: unsupported protocol features: requested=%#x supported=%#x unsupported=%#x",
			v, s.supportedProtocolFeatures(), unsupported)
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

	// The pinned CH frontend reads the full 60-byte wire layout.
	// GET flags are echoed, including CH's normal WRITABLE request flag.
	cfg := BlkConfig{Capacity: uint64(s.backend.Size()) / SectorSize, SegMax: blkSegMax}
	if s.discardSupported {
		cfg.MaxDiscSec = maxDiscardSectors
		cfg.MaxDiscSeg = 1
		cfg.DiscSecAln = 8
	}
	full := cfg.Marshal()
	out := make([]byte, configHeaderSize+int(size))
	copy(out[:configHeaderSize], m.Payload[:configHeaderSize])
	copy(out[configHeaderSize:], full[int(offset):int(end)])
	return SendReply(conn, m.Header.Request, out)
}

// SetMemoryLoader binds the shared-memory barrier before Serve starts.
func (s *Server) SetMemoryLoader(load func(context.Context, uint64, uint64) error) {
	s.memTable.EnsureLoaded = load
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
	var queues []*virtq
	if len(s.memTable.regions) != 0 {
		queues = s.queues
	}
	implicitEnable := s.features&bitVhostProtocolFeatures == 0
	s.mu.Unlock()
	// SET_MEM_TABLE replaces translations, not SET_VRING state. Keep the
	// queues attached so Stop can still find them, and serialize worker
	// cancellation/reinitialization with shutdown via drainMu. Never join
	// a worker under s.mu, and do not replace translations until all join.
	restart := make([]bool, len(queues))
	for idx, q := range queues {
		if q == nil {
			continue
		}
		q.drainMu.Lock()
		if q.stop != nil {
			restart[idx] = q.enabled || implicitEnable
			q.stopReading()
			_, _ = syscall.Write(q.kickFd, []byte{1, 0, 0, 0, 0, 0, 0, 0})
			<-q.done
		}
	}
	s.mu.Lock()
	s.memTable.SetRegions(regs)
	s.mu.Unlock()
	for idx, q := range queues {
		if q == nil {
			continue
		}
		select {
		case <-s.stop:
			restart[idx] = false
		default:
			q.stop = nil
			q.done = nil
			q.ctx = nil
			q.cancel = nil
			q.stopOnce = sync.Once{}
			if restart[idx] {
				// The old worker may have consumed a kick before cancellation.
				// Rescan pending descriptors using the new translations.
				_, _ = syscall.Write(q.kickFd, []byte{1, 0, 0, 0, 0, 0, 0, 0})
			}
		}
		q.drainMu.Unlock()
	}
	for idx := 0; idx < NumQueues; idx++ {
		s.maybeStartWorker(idx)
	}
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

// num is zero until the master supplies the ring geometry. A direct
// chain needs separate descriptors for both the header and the status.
func validateQueueSize(features uint64, num uint32) error {
	if features&bitVirtioBlkSegMax != 0 && num != 0 && num < blkSegMax+2 {
		return fmt.Errorf("vhost: vring size %d cannot hold %d data segments plus header and status", num, blkSegMax)
	}
	return nil
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
	// Split ring indices are 16 bit and wrap at twice the largest queue.
	if num == 0 || num > 32768 {
		return fmt.Errorf("vhost: invalid direct vring size %d", num)
	}
	s.mu.Lock()
	q := s.ensureQueue(idx)
	if err := validateQueueSize(s.features, num); err != nil {
		s.mu.Unlock()
		return err
	}
	q.num = num
	s.mu.Unlock()
	s.maybeStartWorker(idx)
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
	s.maybeStartWorker(int(idx))
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
	s.mu.Unlock()
	q.drainMu.Lock()
	defer q.drainMu.Unlock()
	// Stop the worker if running.
	if q.stop != nil {
		q.stopReading()
		// Reuse the existing eventfd wake mechanism, retaining the descriptor
		// for a subsequent SET_VRING_BASE/ENABLE on this same master.
		_, _ = syscall.Write(q.kickFd, []byte{1, 0, 0, 0, 0, 0, 0, 0})
		<-q.done
		select {
		case <-s.stop:
		default:
			q.stop = nil
			q.stopOnce = sync.Once{}
		}
	}
	base := q.baseIdx
	q.enabled = false
	q.kickArmed = false
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
	s.mu.Unlock()
	q.drainMu.Lock()
	select {
	case <-s.stop:
		q.drainMu.Unlock()
		return net.ErrClosed
	default:
	}
	if q.kickFd >= 0 {
		_ = syscall.Close(q.kickFd)
	}
	q.kickFd = m.Fds[0]
	q.kickArmed = true
	q.drainMu.Unlock()
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
	s.mu.Unlock()
	q.drainMu.Lock()
	select {
	case <-s.stop:
		q.drainMu.Unlock()
		return net.ErrClosed
	default:
	}
	if q.callFd >= 0 {
		_ = syscall.Close(q.callFd)
	}
	q.callFd = m.Fds[0]
	q.drainMu.Unlock()
	s.maybeStartWorker(idx)
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
	configured := s.featuresSet && len(s.memTable.regions) != 0 && q != nil && q.num != 0
	implicit := s.features&bitVhostProtocolFeatures == 0
	s.mu.Unlock()
	if !configured {
		return
	}
	q.drainMu.Lock()
	defer q.drainMu.Unlock()
	select {
	case <-s.stop:
		return
	default:
	}
	if !q.enabled && !(implicit && q.kickArmed) {
		return
	}
	if q.kickFd < 0 || q.callFd < 0 || q.descAddr == 0 || q.stop != nil {
		return
	}
	q.stop = make(chan struct{})
	q.done = make(chan struct{})
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	q.ctx, q.cancel = context.WithCancel(ctx)
	go s.runWorker(idx, q)
	s.logf("vhost: started worker for queue %d", idx)
}

// SetDirtyTracker binds the instance accumulator before serving guest memory.
func (s *Server) SetDirtyTracker(mark func(uint64, uint64) error) { s.memTable.MarkDirty = mark }

// EnableCapacityChange must be called before serving a zero-geometry slot.
func (s *Server) EnableCapacityChange() { s.capacityNotify = true }

func (s *Server) supportedProtocolFeatures() uint64 {
	if s.capacityNotify {
		return bitProtocolConfig | bitBackendRequest
	}
	return bitProtocolConfig
}

func (s *Server) SetSlotOrigin(path string, size int64) {
	if s.stats != nil {
		if s.capacityNotify {
			s.stats.setCapacity(size)
		}
		s.stats.SetOrigin(path)
	}
}

// CanNotifyCapacity is checked before the first backend publication so a VMM
// lacking the required protocol cannot acquire a disk that it cannot expose.
func (s *Server) CanNotifyCapacity() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frontendReq != nil
}

// NotifyCapacity requests a new GET_CONFIG followed by a guest configuration
// interrupt. Retrying after a reconnect is safe. Only the Guest confirms size.
func (s *Server) NotifyCapacity() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frontendReq == nil {
		return errors.New("vhost: VMM capacity change unavailable")
	}
	var header [12]byte
	binary.LittleEndian.PutUint32(header[:], 2) // VHOST_USER_BACKEND_CONFIG_CHANGE_MSG
	binary.LittleEndian.PutUint32(header[4:], FlagVersion1)
	// A stalled VMM must not hold the owner capture gate or prevent teardown.
	n, err := syscall.SendmsgN(int(s.frontendReq.Fd()), header[:], nil, nil, syscall.MSG_DONTWAIT|syscall.MSG_NOSIGNAL)
	if err == nil && n != len(header) {
		return io.ErrShortWrite
	}
	return err
}
