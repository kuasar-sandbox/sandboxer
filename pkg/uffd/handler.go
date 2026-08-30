package uffd

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// PageSize must match the granularity uffd was set up with.
	PageSize = 4096

	// ordinaryDataFaultFillBytes is the fixed total fill unit for ordinary
	// stream Data: one urgent page followed by a best-effort 15-page tail.
	ordinaryDataFaultFillBytes    = 64 << 10
	ordinaryDataNeighborTailBytes = ordinaryDataFaultFillBytes - PageSize

	// zeroFaultFillBytes is the fixed total bound for Hole, Zero, and Released
	// runs. These paths issue no source read and use the same one-plus-fifteen
	// page wake/fill shape as ordinary Data.
	zeroFaultFillBytes    = 64 << 10
	zeroNeighborTailBytes = zeroFaultFillBytes - PageSize

	// chunkFaultFillBytes caps one buffered ChunkRun read and its guest
	// population. Current manifests use chunks no larger than 1 MiB. The
	// final-visible window is independently clipped by PageState, RAM size, and
	// the containing CH UFFD region before either directional tail is submitted.
	chunkFaultFillBytes    = 1 << 20
	chunkNeighborTailBytes = chunkFaultFillBytes - PageSize

	// MinWorkers is the minimum number of worker goroutines for processing UFFD faults.
	MinWorkers = 2
)

// Config gathers everything the handler needs from the caller. Memfd /
// BackendVA / Size come from pkg/memory.Memfd.
type Config struct {
	// Underlying memfd fd (for pread on the rare Loaded fault path).
	MemfdFD int

	// Sandbox-ctl-side mmap of the memfd. Single-uffd model: backendVA
	// is NOT registered with uffd; the handler only references it as
	// the target of process-level madvise(DONTNEED) on EVENT_REMOVE,
	// to clear sandbox-ctl's own PTE/RSS share of pages CH reclaimed
	// at the inode via fallocate(PUNCH_HOLE) in its balloon path.
	// Backend first-touch (vhost-blk DMA, snapshot copy) goes through
	// plain shmem fileops with no uffd round-trip.
	BackendVA uintptr
	Size      int

	// Source of truth for Absent-page contents. ZeroSource for cold
	// start; StreamSnapshotSource for restore.
	Source SnapshotReader

	// Number of worker goroutines for servicing UFFD page faults.
	// The vCPU count forms an upper bound on fault concurrency, serving as a
	// reasonable baseline that avoids worker over-provisioning.
	//
	// Defaults to runtime.NumCPU() when unset, with a minimum of MinWorkers.
	NumWorkers int

	// Optional logger; nil → discarded.
	Logf func(string, ...any)
}

// Handler manages a single userfaultfd:
//   - uffdC: created in CH's process, registered MISSING on chVA,
//     handed to sandbox-ctl via SCM_RIGHTS in the va_report handshake.
//     Catches first-touch from vCPU. The backendVA mmap that
//     sandbox-ctl holds for the same memfd is left WITHOUT any uffd
//     registration; kernel handles backend-mm faults via plain shmem
//     fileops.
//
// Single-uffd correctness rests on EEXIST + UFFDIO_WAKE recovery, not
// on a vhost-side touch-before-publish invariant:
//
//   - vhost-blk write path: Linux virtio_blk routinely places newly
//     allocated, never-touched page-cache pages into the request's
//     writable descriptors. Their GPAs translate to backendVA pages
//     that are still in state Absent; the backend memcpy creates a
//     folio on the shared shmem inode through plain shmem fileops.
//
//   - vCPU read on the same page later faults via uffdC. handleFault
//     attempts UFFDIO_ZEROPAGE / UFFDIO_COPY; the kernel returns
//     EEXIST because the folio already exists. The handler issues
//     UFFDIO_WAKE; on retry the kernel installs the chVA PTE pointing
//     at the backend-written folio (MISSING-only registration, the
//     folio-found path bypasses uffd). Guest sees the disk data.
//
//   - Page state is conditionally set to Loaded after the EEXIST → WAKE
//     recovery so subsequent faults short-circuit. pages_copied/pages_zeroed
//     still count only bytes explicitly reported complete by the ioctl; the
//     pre-existing backend folio is not attributed to either counter.
//
// Fault workers resolve exactly one urgent page. EAGAIN/ENOENT wake that page
// without marking an uncompleted page Loaded, allowing the next fault to retry.
// The one serial best-effort tail worker may issue one multi-page ioctl for a
// forward tail and, for a buffered ChunkRun, one more for its prefix. It
// commits only the page-aligned completed prefix and stops on a conflict.
//
// 5.10+ kernel compatible — no MINOR_SHMEM / UFFDIO_CONTINUE required.
type Handler struct {
	cfg Config

	// uffds holds the CH-mm uffd fds; on x86_64 a zone larger than
	// 3 GiB is split into low + high regions and CH sends one uffd
	// per region via successive va_report messages. Each fd is
	// MISSING-registered on its own chVA range. Fault events from
	// any of them feed the same handleFault logic; the addrMap
	// translates the per-region chVA to the unified memfd offset.
	uffds   []*os.File
	uffdsMu sync.Mutex

	addrMap *AddressMap
	state   *PageStateMap

	epfd int

	// removeQ holds in-flight EVENT_REMOVE / EVENT_UNMAP requests.
	// dispatch() pushes onto this channel non-blocking, and a single
	// flusher goroutine merges contiguous ranges into one
	// madvise(DONTNEED) on backendVA per merged run instead of issuing
	// a syscall per event on the reader path. Decouples the reader
	// from the (potentially thousands per second) free_page_reporting
	// storm CH delivers during cold-start — without this, reader gets
	// stuck issuing madvise back-to-back and page-fault events queue
	// up behind it, stalling the vCPU.
	removeQ chan removeReq

	logf       func(string, ...any)
	stop       chan struct{}
	readerDone chan struct{} // closed when runReader exits — Close() waits on this before signalling stop, so the flusher's drain pass sees no further pushes
	readerStop atomic.Bool
	closeOnce  sync.Once
	wg         sync.WaitGroup
	queue      []chan faultEvent

	// ctx scopes all Run.ReadAt calls. Tail shutdown is separate from the
	// mandatory remove flusher: Close first rejects reservations, cancels
	// this context, waits for the one tail slot to become idle, and joins the
	// tail worker before stopping the reader/workers/remove flusher.
	ctx        context.Context
	cancel     context.CancelFunc
	closing    atomic.Bool
	tailBusy   atomic.Bool
	tailQ      chan tailTask
	tailBuf    []byte
	tailIdle   chan struct{}
	tailSubmit sync.Mutex
	tailWG     sync.WaitGroup

	ops uffdOps

	stats handlerStats
}

// removeReq carries a coalesce-able EVENT_REMOVE notification. The
// flusher sorts by memfd offset and merges contiguous ranges before
// calling fallocate(PUNCH_HOLE).
type removeReq struct {
	memfdOffset uint64
	length      uint64
}

type handlerStats struct {
	faultsAbsent        atomic.Uint64
	faultsReleased      atomic.Uint64
	faultsLoaded        atomic.Uint64 // MISSING faults on StateLoaded pages (folio reclaimed by post-settled balloon PUNCH; must re-fill)
	zeropages           atomic.Uint64 // count of UFFDIO_ZEROPAGE calls
	copies              atomic.Uint64 // count of UFFDIO_COPY calls
	pagesZeroed         atomic.Uint64 // total pages installed via ZEROPAGE
	pagesCopied         atomic.Uint64 // total pages installed via COPY
	wakes               atomic.Uint64
	removeEvents        atomic.Uint64
	errors              atomic.Uint64
	madviseCalls        atomic.Uint64 // madvise(DONTNEED) syscalls issued on backendVA
	madviseBytes        atomic.Uint64 // total bytes advised DONTNEED
	removeQDropped      atomic.Uint64 // events that fell back to sync flush (queue full)
	removeEventsBatched atomic.Uint64 // events that went through the batch path (avg events/syscall = removeEventsBatched / madviseCalls)
	backendLookupMiss   atomic.Uint64 // EVENT_REMOVE with no backend VMA covering the offset (registration bug)

	inflight        atomic.Int64 // faults currently being serviced (gauge; ≤ NumWorkers)
	inflightHWM     atomic.Uint64
	queueDepthHWM   atomic.Uint64
	faultQueueWait  latHist
	pageIn          latHist // source-read latency for the periodic lazy logger
	sourceReadCalls atomic.Uint64
	sourceReadBytes atomic.Uint64
	sourceReadNs    atomic.Uint64
	urgentCopyCalls atomic.Uint64
	urgentCopyNs    atomic.Uint64
	urgentZeroCalls atomic.Uint64
	urgentZeroNs    atomic.Uint64
	tailSubmitted   atomic.Uint64
	tailDroppedBusy atomic.Uint64
	tailCanceled    atomic.Uint64
	tailBuffered    atomic.Uint64
	tailDeferred    atomic.Uint64
	tailZero        atomic.Uint64
	tailPlanned     atomic.Uint64
	tailCompleted   atomic.Uint64
	tailCopyNs      atomic.Uint64
	tailZeroNs      atomic.Uint64
	tailConflicts   atomic.Uint64
	tailPartial     atomic.Uint64
}

type faultEvent struct {
	address uint64
	flags   uint64
	uffdFD  int // which uffd this came from — determines ioctl target fd
	queued  time.Time
}

// NewWithBackendUffd constructs the single-uffd handler. The CH-side
// uffd (uffdCFromCH) is provided by the va_report OnReady callback;
// only this fd is registered MISSING on chVA. The backendVA mmap that
// sandbox-ctl holds for the same memfd has no uffd attached — see the
// Handler comment block above for the protocol-level safety argument.
//
// Steps:
//  1. epoll_create1 → add uffdC
//  2. AddressMap registers ProcessCH (caller did before calling us);
//     ProcessBackend is registered separately by the caller for the
//     mmap whose VA range AssertLoaded validates against.
//  3. Page state initialized to all-Absent
//
// Start the goroutines via Start(); stop via Close().
func NewWithBackendUffd(uffdCFromCH int, addrMap *AddressMap, cfg Config) (*Handler, error) {
	if uffdCFromCH <= 0 {
		return nil, fmt.Errorf("uffd: uffdCFromCH invalid")
	}
	if cfg.MemfdFD <= 0 {
		return nil, fmt.Errorf("uffd: MemfdFD invalid")
	}
	if cfg.Size <= 0 || cfg.Size%PageSize != 0 {
		return nil, fmt.Errorf("uffd: Size %d not page-aligned", cfg.Size)
	}
	if cfg.Source == nil {
		return nil, fmt.Errorf("uffd: Source is nil")
	}
	if addrMap == nil {
		return nil, fmt.Errorf("uffd: AddressMap is nil")
	}

	cfg.NumWorkers = workerCount(cfg.NumWorkers)
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	// epoll for the single uffd (uffdC, owned by CH, sent over SCM_RIGHTS).
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("epoll_create1: %w", err)
	}
	ev := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(uffdCFromCH)}
	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, uffdCFromCH, &ev); err != nil {
		_ = unix.Close(epfd)
		return nil, fmt.Errorf("epoll_ctl ADD uffdC fd=%d: %w", uffdCFromCH, err)
	}

	numPages := cfg.Size / PageSize
	state := NewPageStateMap(numPages)

	queues := make([]chan faultEvent, cfg.NumWorkers)
	for i := range queues {
		queues[i] = make(chan faultEvent, 256)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var tailBuf []byte
	if !isZeroSource(cfg.Source) {
		bufferBytes := ordinaryDataFaultFillBytes
		if _, ok := cfg.Source.(chunkWindowSource); ok {
			bufferBytes = chunkFaultFillBytes
		}
		// Allocate the source-specific maximum before workers start. Fault and
		// tail processing never grow or replace it, avoiding allocation and GC
		// assist on the UFFD hot path. Cold ZeroSource handlers need no Data
		// buffer, ordinary Data needs 64 KiB, and a chunk-window source needs
		// the current 1 MiB manifest bound.
		tailBuf = make([]byte, bufferBytes)
	}
	h := &Handler{
		cfg:        cfg,
		uffds:      []*os.File{os.NewFile(uintptr(uffdCFromCH), "uffd_C_chVA_region0")},
		addrMap:    addrMap,
		state:      state,
		epfd:       epfd,
		removeQ:    make(chan removeReq, 4096),
		logf:       logf,
		stop:       make(chan struct{}),
		readerDone: make(chan struct{}),
		queue:      queues,
		ctx:        ctx,
		cancel:     cancel,
		tailQ:      make(chan tailTask, 1),
		tailBuf:    tailBuf,
		tailIdle:   make(chan struct{}, 1),
		ops:        realUffdOps,
	}
	return h, nil
}

func workerCount(numWorkers int) int {
	if numWorkers <= 0 {
		numWorkers = runtime.NumCPU()
	}
	if numWorkers < MinWorkers {
		return MinWorkers
	}
	return numWorkers
}

// AddUffd attaches an additional CH-side uffd fd to an already-running
// handler. Used when CH issues per-region va_reports for a zone split
// across the x86 PCI hole: the first va_report drove NewWithBackendUffd
// and Start(); subsequent va_reports call this. Takes ownership of the
// fd (closes it on handler shutdown).
//
// Safe to call concurrently with the reader goroutine — the new fd is
// added to the same epoll set, and runReader picks events off any
// ready fd without caring how many fds exist.
func (h *Handler) AddUffd(uffdFD int) error {
	if uffdFD <= 0 {
		return fmt.Errorf("uffd: AddUffd fd=%d invalid", uffdFD)
	}
	h.uffdsMu.Lock()
	defer h.uffdsMu.Unlock()
	idx := len(h.uffds)
	ev := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(uffdFD)}
	if err := unix.EpollCtl(h.epfd, unix.EPOLL_CTL_ADD, uffdFD, &ev); err != nil {
		return fmt.Errorf("uffd: epoll_ctl ADD fd=%d: %w", uffdFD, err)
	}
	name := fmt.Sprintf("uffd_C_chVA_region%d", idx)
	h.uffds = append(h.uffds, os.NewFile(uintptr(uffdFD), name))
	h.logf("uffd: attached additional region uffd fd=%d (region #%d)", uffdFD, idx)
	return nil
}

// AddressMap exposes the map for the va_report server to register
// ProcessCH on handshake (must happen BEFORE the first uffdC fault,
// so the handler can translate ev.address → memfd offset).
func (h *Handler) AddressMap() *AddressMap { return h.addrMap }

// Start spawns the reader and worker goroutines.
func (h *Handler) Start() {
	h.tailWG.Add(1)
	go h.runTailWorker()
	h.wg.Add(1)
	go h.runReader()
	for i := range h.queue {
		h.wg.Add(1)
		go h.runWorker(i)
	}
	h.wg.Add(1)
	go h.runRemoveFlusher()
}

// Close stops the reader+workers+flusher and closes all uffd fds +
// epfd. Idempotent.
//
// Ordering matters for the flusher's stop-time drain pass: we want
// the flusher to see all in-flight EVENT_REMOVE pushes from the
// reader before it drains-and-exits, otherwise residual events
// stranded in removeQ would never be madvise'd.
//
//  1. reject tail reservations, cancel Run.ReadAt, wait for the unique
//     reserved/queued/running tail task, and join the tail worker.
//  2. close uffds + epfd → epoll_wait in reader returns or observes
//     readerStop, then runs deferred close(readerDone). Reader cannot push
//     more events after this.
//  3. wait on readerDone — guarantees reader exited and flushed any
//     final EVENT_REMOVE into removeQ.
//  4. close(stop) → flusher's <-h.stop case fires, drains everything
//     remaining in removeQ, issues final madvise, exits. Workers
//     also unblock and exit.
//  5. wg.Wait — collect reader+workers+flusher.
//  6. close worker queues — safe now that workers have exited.
func (h *Handler) Close() error {
	h.closeOnce.Do(func() {
		// 1. Reject new reservations and cancel all Run.ReadAt calls. The
		// submit mutex closes the enqueue-vs-cancel race.
		h.closing.Store(true)
		h.tailSubmit.Lock()
		h.cancel()
		h.tailSubmit.Unlock()
		h.waitTailIdle()
		h.tailWG.Wait()

		// 2. break reader out of epoll_wait
		h.readerStop.Store(true)
		h.uffdsMu.Lock()
		for _, f := range h.uffds {
			if f != nil {
				_ = f.Close()
			}
		}
		h.uffds = nil
		epfd := h.epfd
		if h.epfd >= 0 {
			_ = unix.Close(epfd)
		}
		h.uffdsMu.Unlock()
		// 3. wait until reader has exited (no more pushes to removeQ)
		<-h.readerDone
		h.uffdsMu.Lock()
		if h.epfd == epfd {
			h.epfd = -1
		}
		h.uffdsMu.Unlock()
		// 4. signal flusher + workers to exit; flusher drains removeQ
		close(h.stop)
		// 5. all goroutines should be finishing up now
		h.wg.Wait()
		// 6. close worker queues (workers already exited)
		for _, q := range h.queue {
			close(q)
		}
	})
	return nil
}

// runReader pulls events from both uffds via epoll. Per-uffd reads
// are non-blocking (O_NONBLOCK on each fd); we drain whichever fd
// epoll reports ready, until EAGAIN, then wait again.
func (h *Handler) runReader() {
	defer h.wg.Done()
	defer close(h.readerDone)
	const msgSize = int(unsafe.Sizeof(uffdMsg{}))
	buf := make([]byte, msgSize*16)
	events := make([]unix.EpollEvent, 4)
	epfd := h.epfd
	for {
		if h.readerStop.Load() {
			return
		}
		select {
		case <-h.stop:
			return
		default:
		}
		n, err := unix.EpollWait(epfd, events, 200)
		if h.readerStop.Load() {
			return
		}
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.EBADF) {
				return
			}
			h.logf("uffd: epoll_wait: %v", err)
			h.stats.errors.Add(1)
			return
		}
		if n == 0 {
			continue
		}
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			h.drain(fd, buf, msgSize)
		}
	}
}

func (h *Handler) drain(fd int, buf []byte, msgSize int) {
	for {
		n, err := unix.Read(fd, buf)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) {
				return
			}
			if errors.Is(err, unix.EBADF) {
				return
			}
			h.logf("uffd: read fd=%d: %v", fd, err)
			h.stats.errors.Add(1)
			return
		}
		if n == 0 {
			return
		}
		if n%msgSize != 0 {
			h.logf("uffd: short read fd=%d %d not multiple of %d", fd, n, msgSize)
			h.stats.errors.Add(1)
			continue
		}
		for off := 0; off < n; off += msgSize {
			msg := (*uffdMsg)(unsafe.Pointer(&buf[off]))
			h.dispatch(msg, fd)
		}
	}
}

func (h *Handler) dispatch(msg *uffdMsg, fromFD int) {
	switch msg.Event {
	case uffdEventPagefault:
		pf := (*uffdMsgPagefault)(unsafe.Pointer(&msg.Arg[0]))
		offset, ok := h.addrMap.Locate(pf.Address)
		if !ok {
			h.logf("uffd: fault at unknown va 0x%x (fd=%d)", pf.Address, fromFD)
			h.stats.errors.Add(1)
			_ = ioctlUffdWake(fromFD, pf.Address&^(PageSize-1), PageSize)
			return
		}
		idx := offset / PageSize
		hashed := pageIdxHash(idx) % uint64(len(h.queue))
		select {
		case h.queue[hashed] <- faultEvent{address: pf.Address, flags: pf.Flags, uffdFD: fromFD, queued: time.Now()}:
			recordAtomicMax(&h.stats.queueDepthHWM, uint64(h.QueueDepth()))
		case <-h.stop:
		}
	case uffdEventRemove, uffdEventUnmap:
		rm := (*uffdMsgRemove)(unsafe.Pointer(&msg.Arg[0]))
		h.handleRemove(rm.Start, rm.End, fromFD)
		h.stats.removeEvents.Add(1)
	default:
		h.logf("uffd: unexpected event 0x%x on fd=%d", msg.Event, fromFD)
	}
}

// handleRemove records a single EVENT_REMOVE: marks pages Released in
// the state map (so subsequent faults see the right phase) and queues
// a backendVA reclaim request on the flusher channel. The reader
// returns immediately — the actual madvise(DONTNEED) on backendVA is
// amortized across coalesced ranges by runRemoveFlusher.
//
// CH already does fallocate(PUNCH_HOLE) on the memfd inode in its
// balloon free_page_reporting path; that drops the file-level pages.
// We are responsible for the *process-level* reclaim on sandbox-ctl's
// own backendVA mapping — without it, sandbox-ctl's PTE/RSS share of
// each released page leaks for the lifetime of the sandbox.
//
// Queue full (rare; coalescer falls behind the reader): synchronously
// madvise as a fallback so we don't lose the reclaim signal.
func (h *Handler) handleRemove(startVA, endVA uint64, fromFD int) {
	_ = fromFD
	startOff, ok := h.addrMap.Locate(startVA)
	if !ok {
		h.logf("uffd: EVENT_REMOVE start 0x%x unknown", startVA)
		return
	}
	length := endVA - startVA
	endOff := startOff + length
	startPage := startOff / PageSize
	endPage := (endOff + PageSize - 1) / PageSize
	h.state.SetRange(startPage, endPage, StateReleased)

	select {
	case h.removeQ <- removeReq{memfdOffset: startOff, length: length}:
	default:
		h.stats.removeQDropped.Add(1)
		h.madviseBackend(startOff, length)
	}
}

// madviseBackend resolves backendVA for [memfdOffset, +length) and
// issues madvise(DONTNEED). Loops over BackendVAFor in case the range
// straddles backend VMA boundaries (today backend is registered as a
// single full-memfd VMA so the loop runs once; future multi-region
// backend mappings would otherwise lose tail bytes silently — that's
// the failure mode this loop guards against).
//
// On lookup miss (no backend VMA covers the offset, which means the
// AddressMap is missing a registration the design requires) the call
// is dropped with a log line and a counter bump so the regression is
// visible in stats. Failure to madvise increments the error counter.
func (h *Handler) madviseBackend(memfdOffset, length uint64) {
	for length > 0 {
		backendVA, mapped, ok := h.addrMap.BackendVAFor(memfdOffset, length)
		if !ok {
			h.logf("uffd: EVENT_REMOVE backendVA miss off=0x%x remaining=%d",
				memfdOffset, length)
			h.stats.backendLookupMiss.Add(1)
			return
		}
		if mapped == 0 {
			// Defensive: BackendVAFor returned ok with zero length
			// (would be a bug). Avoid an infinite loop.
			h.logf("uffd: BackendVAFor returned zero mapped for off=0x%x len=%d",
				memfdOffset, length)
			return
		}
		if err := madviseDontneedRange(uintptr(backendVA), uintptr(mapped)); err != nil {
			h.logf("uffd: madvise(DONTNEED) backendVA=0x%x len=%d: %v",
				backendVA, mapped, err)
			h.stats.errors.Add(1)
			return
		}
		h.stats.madviseCalls.Add(1)
		h.stats.madviseBytes.Add(mapped)
		memfdOffset += mapped
		length -= mapped
	}
}

// runRemoveFlusher drains removeQ and amortizes madvise(DONTNEED) on
// backendVA across coalesced ranges. Triggers on either: (1) queue
// depth ≥ maxBatch (default 64 events), or (2) flushInterval since
// first pending event (default 10 ms). On stop, flushes remaining
// pending events and exits.
//
// Per-syscall cost of madvise(DONTNEED) on MAP_SHARED is small (just
// zap_pte_range over the calling process's PTEs), but reader contention
// at 5k+ events/s would still starve PAGEFAULT dispatch — the channel
// indirection keeps reader's hot path in O(decode + chan-send).
func (h *Handler) runRemoveFlusher() {
	defer h.wg.Done()
	const flushInterval = 10 * time.Millisecond
	const maxBatch = 64

	pending := make([]removeReq, 0, maxBatch)
	timer := time.NewTimer(flushInterval)
	if !timer.Stop() {
		<-timer.C
	}
	timerArmed := false

	flush := func() {
		if len(pending) == 0 {
			return
		}
		// Sort by memfdOffset and merge contiguous ranges so we issue
		// one madvise per merged run instead of one per event.
		sort.Slice(pending, func(i, j int) bool {
			return pending[i].memfdOffset < pending[j].memfdOffset
		})
		merged := pending[:0:cap(pending)]
		cur := pending[0]
		for _, r := range pending[1:] {
			if r.memfdOffset == cur.memfdOffset+cur.length {
				cur.length += r.length
			} else if r.memfdOffset < cur.memfdOffset+cur.length {
				// overlap — extend cur to cover both
				end := r.memfdOffset + r.length
				if end > cur.memfdOffset+cur.length {
					cur.length = end - cur.memfdOffset
				}
			} else {
				merged = append(merged, cur)
				cur = r
			}
		}
		merged = append(merged, cur)

		flushedEvents := uint64(len(pending))
		for _, r := range merged {
			h.madviseBackend(r.memfdOffset, r.length)
		}
		h.stats.removeEventsBatched.Add(flushedEvents)
		pending = pending[:0]
	}

	for {
		select {
		case <-h.stop:
			// Drain anything left in the channel before exiting so the
			// reclaim signal isn't lost on shutdown.
			for {
				select {
				case req := <-h.removeQ:
					pending = append(pending, req)
				default:
					flush()
					return
				}
			}
		case req, ok := <-h.removeQ:
			if !ok {
				flush()
				return
			}
			pending = append(pending, req)
			if len(pending) >= maxBatch {
				if timerArmed {
					if !timer.Stop() {
						<-timer.C
					}
					timerArmed = false
				}
				flush()
			} else if !timerArmed {
				timer.Reset(flushInterval)
				timerArmed = true
			}
		case <-timer.C:
			timerArmed = false
			flush()
		}
	}
}

func (h *Handler) runWorker(idx int) {
	defer h.wg.Done()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Fault workers own only the urgent page. Speculative data uses the
	// single Handler-wide tailBuf under tailBusy reservation.
	pageBuf := make([]byte, PageSize)
	q := h.queue[idx]
	for {
		select {
		case <-h.stop:
			return
		case ev, ok := <-q:
			if !ok {
				return
			}
			if !ev.queued.IsZero() {
				h.stats.faultQueueWait.record(uint64(time.Since(ev.queued).Nanoseconds()))
			}
			h.handleFault(ev, pageBuf)
		}
	}
}

func pageIdxHash(idx uint64) uint64 {
	h := fnv.New64a()
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(idx >> (i * 8))
	}
	_, _ = h.Write(b[:])
	return h.Sum64()
}

// Stats returns a snapshot of counter values for diagnostics.
func (h *Handler) Stats() map[string]uint64 {
	queueWait := h.stats.faultQueueWait.snapshot()
	return map[string]uint64{
		"faults_absent":         h.stats.faultsAbsent.Load(),
		"faults_released":       h.stats.faultsReleased.Load(),
		"faults_loaded":         h.stats.faultsLoaded.Load(),
		"zeropage_calls":        h.stats.zeropages.Load(),
		"copy_calls":            h.stats.copies.Load(),
		"pages_zeroed":          h.stats.pagesZeroed.Load(),
		"pages_copied":          h.stats.pagesCopied.Load(),
		"wakes":                 h.stats.wakes.Load(),
		"remove_events":         h.stats.removeEvents.Load(),
		"remove_q_dropped":      h.stats.removeQDropped.Load(),
		"remove_events_batched": h.stats.removeEventsBatched.Load(),
		"madvise_calls":         h.stats.madviseCalls.Load(),
		"madvise_bytes":         h.stats.madviseBytes.Load(),
		"backend_lookup_miss":   h.stats.backendLookupMiss.Load(),
		"errors":                h.stats.errors.Load(),
		"fault_queue_wait_ns":   queueWait.SumNs,
		"fault_queue_wait_p50":  queueWait.P50(),
		"fault_queue_wait_p95":  queueWait.P95(),
		"fault_queue_wait_p99":  queueWait.P99(),
		"fault_queue_depth":     uint64(h.QueueDepth()),
		"fault_queue_depth_hwm": h.stats.queueDepthHWM.Load(),
		"fault_inflight":        uint64(max(h.stats.inflight.Load(), 0)),
		"fault_inflight_hwm":    h.stats.inflightHWM.Load(),
		"source_read_calls":     h.stats.sourceReadCalls.Load(),
		"source_read_bytes":     h.stats.sourceReadBytes.Load(),
		"source_read_ns":        h.stats.sourceReadNs.Load(),
		"urgent_copy_calls":     h.stats.urgentCopyCalls.Load(),
		"urgent_copy_ns":        h.stats.urgentCopyNs.Load(),
		"urgent_zero_calls":     h.stats.urgentZeroCalls.Load(),
		"urgent_zero_ns":        h.stats.urgentZeroNs.Load(),
		"tail_submitted":        h.stats.tailSubmitted.Load(),
		"tail_dropped_busy":     h.stats.tailDroppedBusy.Load(),
		"tail_canceled":         h.stats.tailCanceled.Load(),
		"tail_buffered_data":    h.stats.tailBuffered.Load(),
		"tail_deferred_data":    h.stats.tailDeferred.Load(),
		"tail_zero":             h.stats.tailZero.Load(),
		"tail_pages_planned":    h.stats.tailPlanned.Load(),
		"tail_pages_completed":  h.stats.tailCompleted.Load(),
		"tail_copy_ns":          h.stats.tailCopyNs.Load(),
		"tail_zero_ns":          h.stats.tailZeroNs.Load(),
		"tail_conflicts":        h.stats.tailConflicts.Load(),
		"tail_partial":          h.stats.tailPartial.Load(),
	}
}

// Inflight is the number of faults currently being serviced (≤ NumWorkers).
func (h *Handler) Inflight() int64 { return h.stats.inflight.Load() }

// QueueDepth is the number of faults enqueued but not yet picked up by a
// worker, summed across all per-worker queues.
func (h *Handler) QueueDepth() int {
	n := 0
	for _, q := range h.queue {
		n += len(q)
	}
	return n
}

// LazyStats is a point-in-time view of the lazy-load counters + live gauges +
// page-in latency, for the periodic stats logger (and rate computation).
type LazyStats struct {
	FaultsAbsent   uint64
	FaultsReleased uint64
	FaultsLoaded   uint64
	PagesCopied    uint64
	PagesZeroed    uint64
	Errors         uint64
	Inflight       int64
	QueueDepth     int
	PageIn         LatSnapshot // data-fetch latency histogram (cumulative; .Sub for a window)
}

// LazyStats snapshots the counters + gauges in one call.
func (h *Handler) LazyStats() LazyStats {
	return LazyStats{
		FaultsAbsent:   h.stats.faultsAbsent.Load(),
		FaultsReleased: h.stats.faultsReleased.Load(),
		FaultsLoaded:   h.stats.faultsLoaded.Load(),
		PagesCopied:    h.stats.pagesCopied.Load(),
		PagesZeroed:    h.stats.pagesZeroed.Load(),
		Errors:         h.stats.errors.Load(),
		Inflight:       h.stats.inflight.Load(),
		QueueDepth:     h.QueueDepth(),
		PageIn:         h.stats.pageIn.snapshot(),
	}
}
