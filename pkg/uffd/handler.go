package uffd

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
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

	// MaxBatchPages caps the number of contiguous pages a single fault
	// event resolves in one UFFDIO_COPY / UFFDIO_ZEROPAGE call. 256 pages
	// = 1 MiB. Hits two goals:
	//  1. Amortize ioctl + per-fault wake overhead across many pages
	//     (typical first-touch workloads page in adjacent VA ranges)
	//  2. Keep the per-worker source buffer at 1 MiB (sync.Pool friendly,
	//     no surprise multi-MiB allocations under fault burst)
	MaxBatchPages = 256

	// MaxBatchBytes is MaxBatchPages × PageSize.
	MaxBatchBytes = MaxBatchPages * PageSize

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
//   - Page state is set to Loaded after the EEXIST → WAKE recovery so
//     subsequent faults short-circuit; ZEROPAGE counters reflect the
//     attempted install, not the actual content (folio carries the
//     backend bytes).
//
// EAGAIN on UFFDIO_ZEROPAGE/COPY signals partial-folio overlap within
// a multi-page batch (kernel can't install some pages because folios
// already exist for them). Treated as a soft error: counter bump,
// return without state.Set; the next fault on the same VA re-enters
// with a smaller batch and converges.
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
	closeOnce  sync.Once
	wg         sync.WaitGroup
	queue      []chan faultEvent

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
	batchPagesSum       atomic.Uint64 // sum of run lengths (for avg calc)
	batchCalls          atomic.Uint64 // count of batches (avg = sum/calls)
	batchMaxPages       atomic.Uint64 // largest single batch observed
	madviseCalls        atomic.Uint64 // madvise(DONTNEED) syscalls issued on backendVA
	madviseBytes        atomic.Uint64 // total bytes advised DONTNEED
	removeQDropped      atomic.Uint64 // events that fell back to sync flush (queue full)
	removeEventsBatched atomic.Uint64 // events that went through the batch path (avg events/syscall = removeEventsBatched / madviseCalls)
	backendLookupMiss   atomic.Uint64 // EVENT_REMOVE with no backend VMA covering the offset (registration bug)

	inflight atomic.Int64 // faults currently being serviced (gauge; ≤ NumWorkers)
	pageIn   latHist      // page-in FETCH latency (data Source.ReadAt; the slow-remote/cache signal)
}

type faultEvent struct {
	address uint64
	flags   uint64
	uffdFD  int // which uffd this came from — determines ioctl target fd
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

	return &Handler{
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
	}, nil
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
//  1. close uffds + epfd → epoll_wait in reader returns error
//     (EBADF) → reader breaks out of its loop and runs deferred
//     close(readerDone). Reader cannot push more events after this.
//  2. wait on readerDone — guarantees reader exited and flushed any
//     final EVENT_REMOVE into removeQ.
//  3. close(stop) → flusher's <-h.stop case fires, drains everything
//     remaining in removeQ, issues final madvise, exits. Workers
//     also unblock and exit.
//  4. wg.Wait — collect reader+workers+flusher.
//  5. close worker queues — safe now that workers have exited.
func (h *Handler) Close() error {
	h.closeOnce.Do(func() {
		// 1. break reader out of epoll_wait
		h.uffdsMu.Lock()
		for _, f := range h.uffds {
			if f != nil {
				_ = f.Close()
			}
		}
		h.uffds = nil
		h.uffdsMu.Unlock()
		if h.epfd >= 0 {
			_ = unix.Close(h.epfd)
			h.epfd = -1
		}
		// 2. wait until reader has exited (no more pushes to removeQ)
		<-h.readerDone
		// 3. signal flusher + workers to exit; flusher drains removeQ
		close(h.stop)
		// 4. all goroutines should be finishing up now
		h.wg.Wait()
		// 5. close worker queues (workers already exited)
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
	for {
		select {
		case <-h.stop:
			return
		default:
		}
		n, err := unix.EpollWait(h.epfd, events, 200)
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
		case h.queue[hashed] <- faultEvent{address: pf.Address, flags: pf.Flags, uffdFD: fromFD}:
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
	// Per-worker buffer sized for the maximum batch (1 MiB). Sticking
	// to a fixed buffer avoids per-fault allocation in the hot path.
	pageBuf := make([]byte, MaxBatchBytes)
	q := h.queue[idx]
	for {
		select {
		case <-h.stop:
			return
		case ev, ok := <-q:
			if !ok {
				return
			}
			h.handleFault(ev, pageBuf)
		}
	}
}

// absentRunFrom returns the number of contiguous StateAbsent pages
// starting at pageIdx, capped at MaxBatchPages and at the end of the
// page-state table. State-only walk — no Source involvement; the
// Source then caps further within this bound based on its internal
// boundaries.
func (h *Handler) absentRunFrom(pageIdx uint64) uint64 {
	stateLen := uint64(h.state.Len())
	n := uint64(1)
	for n < MaxBatchPages && pageIdx+n < stateLen {
		if h.state.Get(pageIdx+n) != StateAbsent {
			break
		}
		n++
	}
	return n
}

// extendReleasedBatch returns the longest run of Released pages at
// pageIdx (capped at MaxBatchPages). Released runs all resolve via
// UFFDIO_ZEROPAGE; classification is purely state-based.
func (h *Handler) extendReleasedBatch(pageIdx uint64) uint64 {
	stateLen := uint64(h.state.Len())
	n := uint64(1)
	for n < MaxBatchPages && pageIdx+n < stateLen {
		if h.state.Get(pageIdx+n) != StateReleased {
			break
		}
		n++
	}
	return n
}

func (h *Handler) recordBatch(pages uint64) {
	h.stats.batchCalls.Add(1)
	h.stats.batchPagesSum.Add(pages)
	for {
		cur := h.stats.batchMaxPages.Load()
		if pages <= cur || h.stats.batchMaxPages.CompareAndSwap(cur, pages) {
			break
		}
	}
}

func (h *Handler) handleFault(ev faultEvent, pageBuf []byte) {
	h.stats.inflight.Add(1)
	defer h.stats.inflight.Add(-1)
	memfdOffset, ok := h.addrMap.Locate(ev.address)
	if !ok {
		h.logf("uffd: handleFault unknown va 0x%x", ev.address)
		h.stats.errors.Add(1)
		_ = ioctlUffdWake(ev.uffdFD, ev.address&^(PageSize-1), PageSize)
		return
	}
	pageVA := ev.address &^ (PageSize - 1)
	pageOffset := memfdOffset &^ (PageSize - 1)
	pageIdx := pageOffset / PageSize

	state := h.state.Get(pageIdx)
	switch state {
	case StateAbsent:
		h.stats.faultsAbsent.Add(1)

		// State-side cap: how many consecutive Absent pages from
		// pageIdx. Source then picks any n ≤ this, aligned to its
		// own internal boundaries (chunk for manifest, IsZero
		// transition for sparse).
		capPages := h.absentRunFrom(pageIdx)
		capBytes := capPages * PageSize
		// Clamp to the CH region containing pageOffset: a fill ioctl
		// runs on one region's uffd fd and must not cross into the next
		// region's non-contiguous VA (x86 PCI-hole split), or the
		// kernel returns ENOENT for the out-of-region tail. The
		// PageStateMap is contiguous over the whole memfd, so an Absent
		// run can otherwise straddle the region boundary.
		if rem, ok := h.addrMap.CHRegionRemaining(pageOffset, capBytes); ok {
			capBytes = rem
		}

		tFetch := time.Now()
		n, isZero, err := h.cfg.Source.ReadAt(pageBuf[:capBytes], pageOffset)
		// Record only DATA fetches (the RPC/IO path): zero-region serves are
		// near-instant and would mask the slow-remote/cache tail. p99 here is
		// the cold-fetch latency that reveals a degraded store.
		if err == nil && !isZero {
			h.stats.pageIn.record(uint64(time.Since(tFetch).Nanoseconds()))
		}
		if err != nil && !errors.Is(err, io.EOF) {
			h.logf("uffd: source.ReadAt off=0x%x cap=%d: %v", pageOffset, capBytes, err)
			h.stats.errors.Add(1)
			_ = ioctlUffdWake(ev.uffdFD, pageVA, PageSize)
			return
		}
		runBytes := uint64(n)
		// Source returns 0 only at EOF — fall back to one zero page so
		// the faulting thread makes progress instead of looping.
		if runBytes == 0 {
			runBytes = PageSize
			isZero = true
		}
		if runBytes%PageSize != 0 {
			// Source returned a partial last page near EOF. Round up
			// and zero-pad; UFFDIO_* ioctls require page-aligned len.
			pad := PageSize - runBytes%PageSize
			for i := uint64(n); i < runBytes+pad; i++ {
				pageBuf[i] = 0
			}
			runBytes += pad
		}
		runPages := runBytes / PageSize

		if isZero {
			err := ioctlUffdZeropage(ev.uffdFD, pageVA, runBytes)
			// EAGAIN or EEXIST on a multi-page batch means the kernel did
			// only a partial install: some page in the run already has a
			// folio (vhost backend memcpy, a prior fault, or — common
			// while the balloon is concurrently PUNCH_HOLE/EVENT_REMOVE-
			// churning these offsets — a neighbour the balloon left
			// resident). The wrapper does not surface which prefix
			// succeeded, and crucially that page need NOT be the faulting
			// page `pageVA`. Drop the batch and retry the single faulting
			// page so we never WAKE/mark-Loaded a page we did not resolve
			// (doing so makes the vCPU re-fault forever — a fault↔WAKE
			// livelock). ENOENT (a batch that ran off the region — should
			// not happen post region-clamp, kept as a safety net) narrows
			// the same way: the faulting page is in-region and resolves.
			if (errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOENT)) && runBytes > PageSize {
				err = ioctlUffdZeropage(ev.uffdFD, pageVA, PageSize)
				runBytes = PageSize
				runPages = 1
			}
			if err != nil {
				if errors.Is(err, unix.EEXIST) {
					// Folio already present — wake the faulting thread;
					// kernel re-fault path finds the folio (MISSING-only
					// registration) and installs the PTE without a uffd
					// round-trip. Mark Loaded below.
					_ = ioctlUffdWake(ev.uffdFD, pageVA, runBytes)
					h.stats.wakes.Add(1)
				} else if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.ENOENT) {
					// Single-page EAGAIN (transient) or ENOENT (no
					// compatible VMA — should not occur post region-clamp).
					// Wake the faulting page so vCPU retries; do NOT
					// mark Loaded (folio not installed). Next fault on
					// this page re-enters the handler.
					_ = ioctlUffdWake(ev.uffdFD, pageVA, PageSize)
					h.stats.wakes.Add(1)
					return
				} else {
					h.logf("uffd: ZEROPAGE va=0x%x len=%d: %v", pageVA, runBytes, err)
					h.stats.errors.Add(1)
					return
				}
			}
			h.stats.zeropages.Add(1)
			h.stats.pagesZeroed.Add(runPages)
		} else {
			src := uint64(uintptr(unsafe.Pointer(&pageBuf[0])))
			err := ioctlUffdCopy(ev.uffdFD, pageVA, src, runBytes)
			if (errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOENT)) && runBytes > PageSize {
				// Same partial-install fallback as ZEROPAGE: a batched
				// EAGAIN/EEXIST/ENOENT means some page in the run already
				// has a folio (or ran off the region — safety net post
				// clamp) and need not be the faulting page. Retry the
				// faulting page only (pageBuf[0:PageSize] already holds
				// its source bytes) so we never mark a page Loaded that
				// we did not resolve — otherwise the vCPU re-faults
				// forever (fault↔WAKE livelock).
				err = ioctlUffdCopy(ev.uffdFD, pageVA, src, PageSize)
				runBytes = PageSize
				runPages = 1
			}
			if err != nil {
				if errors.Is(err, unix.EEXIST) {
					_ = ioctlUffdWake(ev.uffdFD, pageVA, runBytes)
					h.stats.wakes.Add(1)
				} else if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.ENOENT) {
					_ = ioctlUffdWake(ev.uffdFD, pageVA, PageSize)
					h.stats.wakes.Add(1)
					return
				} else {
					h.logf("uffd: COPY va=0x%x len=%d: %v", pageVA, runBytes, err)
					h.stats.errors.Add(1)
					return
				}
			}
			h.stats.copies.Add(1)
			h.stats.pagesCopied.Add(runPages)
		}
		// Mark the run Loaded so subsequent faults short-circuit.
		for i := uint64(0); i < runPages; i++ {
			h.state.Set(pageIdx+i, StateLoaded)
		}
		h.recordBatch(runPages)

	case StateReleased:
		h.stats.faultsReleased.Add(1)
		runPages := h.extendReleasedBatch(pageIdx)
		runBytes := runPages * PageSize
		// Clamp to the CH region (see StateAbsent): a Released run is
		// also state-map contiguous and can straddle the PCI-hole split,
		// which would ENOENT on the single region fd.
		if rem, ok := h.addrMap.CHRegionRemaining(pageOffset, runBytes); ok && rem < runBytes {
			runBytes = rem
			runPages = runBytes / PageSize
		}
		err := ioctlUffdZeropage(ev.uffdFD, pageVA, runBytes)
		// Batched EAGAIN/EEXIST: some page in the run already has a
		// folio and need not be the faulting page. ENOENT: the batch
		// ran off the region (safety net post region-clamp). Narrow to
		// pageVA so we never mark a page Loaded we did not resolve —
		// otherwise the vCPU re-faults forever (fault↔WAKE livelock).
		if (errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOENT)) && runBytes > PageSize {
			err = ioctlUffdZeropage(ev.uffdFD, pageVA, PageSize)
			runBytes = PageSize
			runPages = 1
		}
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				// Faulting page itself already has a folio: WAKE; the
				// kernel re-fault (MISSING-only registration) installs
				// its PTE. Mark Loaded below.
				_ = ioctlUffdWake(ev.uffdFD, pageVA, PageSize)
				h.stats.wakes.Add(1)
			} else if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.ENOENT) {
				// Single-page EAGAIN (transient) or ENOENT (no compatible
				// VMA — should not occur post region-clamp): WAKE so the
				// vCPU retries; do NOT mark Loaded — next fault re-enters
				// and resolves this page.
				_ = ioctlUffdWake(ev.uffdFD, pageVA, PageSize)
				h.stats.wakes.Add(1)
				return
			} else {
				h.logf("uffd: ZEROPAGE(released) va=0x%x len=%d: %v",
					pageVA, runBytes, err)
				h.stats.errors.Add(1)
				return
			}
		}
		h.stats.zeropages.Add(1)
		h.stats.pagesZeroed.Add(runPages)
		for i := uint64(0); i < runPages; i++ {
			h.state.Set(pageIdx+i, StateLoaded)
		}
		h.recordBatch(runPages)

	case StateLoaded:
		h.stats.faultsLoaded.Add(1)
		// uffd MISSING fires only when NO folio backs the page. Reaching
		// here with StateLoaded therefore proves the folio was reclaimed
		// out from under us: the post-settled aggressive balloon inflate
		// PUNCH_HOLE'd this resident page and the synthetic EVENT_REMOVE
		// has not (yet) been reflected in state. It is the StateReleased
		// situation under a stale label — WAKE-only cannot make progress
		// (no folio for the kernel re-fault to install), so the page MUST
		// be re-filled or the vCPU re-faults forever (fault↔WAKE
		// livelock). A punched page is guest-freed by the balloon
		// contract, so the guest expects a fresh zero page; replaying
		// Source content would reincarnate stale data into a reused page.
		// Single page only — neighbouring Loaded pages may still be
		// resident, so there is no safe run to batch.
		err := ioctlUffdZeropage(ev.uffdFD, pageVA, PageSize)
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				// Folio actually present: a genuine transient race (it
				// got installed between EVENT_REMOVE generation and now).
				// WAKE; the kernel re-fault installs the PTE. Page stays
				// Loaded.
				_ = ioctlUffdWake(ev.uffdFD, pageVA, PageSize)
				h.stats.wakes.Add(1)
				return
			} else if errors.Is(err, unix.EAGAIN) {
				// Transient kernel state: WAKE so the vCPU retries; the
				// next fault re-enters this path and re-fills.
				_ = ioctlUffdWake(ev.uffdFD, pageVA, PageSize)
				h.stats.wakes.Add(1)
				return
			}
			h.logf("uffd: ZEROPAGE(loaded) va=0x%x: %v", pageVA, err)
			h.stats.errors.Add(1)
			return
		}
		h.stats.zeropages.Add(1)
		h.stats.pagesZeroed.Add(1)
		h.recordBatch(1)
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
	calls := h.stats.batchCalls.Load()
	pagesSum := h.stats.batchPagesSum.Load()
	avgBatch := uint64(0)
	if calls > 0 {
		avgBatch = pagesSum / calls
	}
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
		"batch_calls":           calls,
		"batch_pages_total":     pagesSum,
		"batch_avg_pages":       avgBatch,
		"batch_max_pages":       h.stats.batchMaxPages.Load(),
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
