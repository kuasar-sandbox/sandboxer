package uffd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"golang.org/x/sys/unix"
)

type uffdOps struct {
	copy     func(fd int, dst uint64, src []byte) (int64, error)
	zeropage func(fd int, dst, length uint64) (int64, error)
	wake     func(fd int, start, length uint64) error
}

var realUffdOps = uffdOps{
	copy: func(fd int, dst uint64, src []byte) (int64, error) {
		return ioctlUffdCopy(fd, dst, uint64(uintptr(unsafe.Pointer(&src[0]))), uint64(len(src)))
	},
	zeropage: ioctlUffdZeropage,
	wake:     ioctlUffdWake,
}

type tailKind uint8

const (
	tailBufferedData tailKind = iota
	tailDeferredData
	tailZero
)

type tailTask struct {
	kind tailKind

	run sparse.Run

	uffdFD      int
	dstVA       uint64
	pageIdx     uint64
	start       uint64
	end         uint64
	bufferStart uint64
	prefixStart uint64
	expected    PageState
}

type urgentOutcome struct {
	tailEligible bool
}

func (h *Handler) handleFault(ev faultEvent, pageBuf []byte) {
	inflight := h.stats.inflight.Add(1)
	recordAtomicMax(&h.stats.inflightHWM, uint64(inflight))
	defer h.stats.inflight.Add(-1)

	memfdOffset, ok := h.addrMap.Locate(ev.address)
	if !ok {
		h.logf("uffd: handleFault unknown va 0x%x", ev.address)
		h.stats.errors.Add(1)
		h.wake(ev.uffdFD, ev.address&^(PageSize-1), PageSize)
		return
	}
	pageVA := ev.address &^ (PageSize - 1)
	pageOffset := memfdOffset &^ (PageSize - 1)
	pageIdx := pageOffset / PageSize

	switch state := h.state.Get(pageIdx); state {
	case StateAbsent:
		h.stats.faultsAbsent.Add(1)
		h.handleAbsentFault(ev.uffdFD, pageVA, pageOffset, pageIdx, pageBuf)
	case StateReleased:
		h.stats.faultsReleased.Add(1)
		h.handleZeroFault(ev.uffdFD, pageVA, pageOffset, pageIdx, StateReleased)
	case StateLoaded:
		h.stats.faultsLoaded.Add(1)
		// A MISSING fault on StateLoaded means the balloon reclaimed the
		// folio before its remove event was observed. Preserve the existing
		// safety rule: refill only this page with zero and do not speculate.
		if _, err := h.urgentZero(ev.uffdFD, pageVA, pageIdx, StateLoaded); err != nil {
			h.logf("uffd: ZEROPAGE(loaded) va=0x%x: %v", pageVA, err)
			h.stats.errors.Add(1)
		}
	}
}

func (h *Handler) handleAbsentFault(uffdFD int, pageVA, pageOffset, pageIdx uint64, pageBuf []byte) {
	// Check the common 64 KiB state boundary before resolving source metadata.
	// This rejects a stale Absent event without allocating a Run. ChunkRun alone
	// performs a second, wider state scan after the single metadata lookup.
	commonHardEnd := h.stateHardEnd(pageOffset, pageIdx, StateAbsent, ordinaryDataFaultFillBytes)
	if commonHardEnd-pageOffset < PageSize {
		// The fault was classified as Absent before entering this method,
		// but a concurrent urgent or tail completion may have populated it
		// before the run scan acquired the state lock. Converge that stale
		// event exactly like an ioctl conflict: wake it without treating an
		// already-resolved page as a handler error. A Released page will fault
		// again and take the StateReleased zero path.
		if h.state.Get(pageIdx) != StateAbsent {
			h.wake(uffdFD, pageVA, PageSize)
			return
		}
		h.failUrgent(uffdFD, pageVA, "no full Absent page at offset 0x%x", pageOffset)
		return
	}

	// Cold-start memory is authoritative zero. Avoid constructing a sparse.Run
	// interface value on every fault and retain the same 1+15 page fill policy.
	if isZeroSource(h.cfg.Source) {
		h.handleResolvedZeroFault(uffdFD, pageVA, pageOffset, pageIdx, StateAbsent, commonHardEnd)
		return
	}

	// Resolve metadata once within the common 64 KiB bound. Even a one-page
	// forward anchor identifies ChunkRun without changing SnapshotReader; the
	// optional root-visibility resolver may expand it around the fault only after
	// the shared tail reservation succeeds.
	// Ordinary Data and no-read runs remain bounded by commonHardEnd below.
	var run sparse.Run
	err := readretry.Do(h.ctx, func() error {
		var err error
		run, err = h.cfg.Source.RunAt(pageOffset, commonHardEnd-pageOffset)
		if err == io.EOF {
			return readerr.Mark(err, false)
		}
		return err
	})
	if err != nil {
		h.failRead(fmt.Errorf("uffd: source.RunAt off=0x%x: %w", pageOffset, err))
		return
	}
	if err := validateFaultRun(run, pageOffset, commonHardEnd); err != nil {
		h.failRead(err)
		return
	}
	if run.End()-pageOffset < PageSize {
		h.failRead(fmt.Errorf("uffd: source Run [%d,%d) does not cover fault page", run.Offset(), run.End()))
		return
	}

	switch run.Kind() {
	case sparse.Data:
		if chunk, ok := run.(fetch.ChunkRun); ok {
			h.handleChunkFault(uffdFD, pageVA, pageOffset, pageIdx, chunk, pageBuf)
			return
		}
		h.handleOrdinaryDataFault(uffdFD, pageVA, pageOffset, pageIdx, run, min(run.End(), commonHardEnd), pageBuf)
	case sparse.Hole, sparse.Zero:
		h.handleResolvedZeroFault(uffdFD, pageVA, pageOffset, pageIdx, StateAbsent, min(run.End(), commonHardEnd))
	}
}

func (h *Handler) handleChunkFault(uffdFD int, pageVA, pageOffset, pageIdx uint64, anchor fetch.ChunkRun, pageBuf []byte) {
	if !h.tryReserveTail() {
		h.handleChunkUrgentOnly(uffdFD, pageVA, pageOffset, pageIdx, anchor, pageBuf)
		return
	}

	window := anchor
	if source, ok := h.cfg.Source.(chunkWindowSource); ok {
		resolved, err := source.resolveChunkWindow(anchor, chunkFaultFillBytes)
		if err != nil {
			h.logf("uffd: resolve ChunkRun window at 0x%x: %v; using anchor Run", pageOffset, err)
		} else if validChunkWindow(resolved, pageOffset, uint64(len(h.tailBuf))) {
			window = resolved
		} else {
			h.logf("uffd: invalid resolved ChunkRun window at 0x%x; using anchor Run", pageOffset)
		}
	}

	fillStart, fillEnd, ok := h.chunkPopulationBounds(window, pageOffset, pageIdx)
	if !ok || (fillStart == pageOffset && fillEnd == pageOffset+PageSize) {
		h.releaseTail()
		h.handleChunkUrgentOnly(uffdFD, pageVA, pageOffset, pageIdx, anchor, pageBuf)
		return
	}

	runLength := window.End() - window.Offset()
	if runLength > uint64(len(h.tailBuf)) {
		// Construction preallocates chunkFaultFillBytes for every non-zero
		// source. Preserve urgent progress if a custom source violates that
		// internal sizing invariant; never allocate or grow here.
		h.releaseTail()
		h.handleChunkUrgentOnly(uffdFD, pageVA, pageOffset, pageIdx, anchor, pageBuf)
		return
	}
	if err := h.readRequiredRun(window, h.tailBuf[:runLength], 0); err != nil {
		h.releaseTail()
		h.failRead(fmt.Errorf("uffd: chunk window read [%d,%d): %w", window.Offset(), window.End(), err))
		return
	}
	urgentStart := pageOffset - window.Offset()
	outcome, err := h.urgentCopy(
		uffdFD,
		pageVA,
		pageIdx,
		StateAbsent,
		h.tailBuf[urgentStart:urgentStart+PageSize],
	)
	if err != nil {
		h.releaseTail()
		h.logf("uffd: COPY(chunk urgent) va=0x%x: %v", pageVA, err)
		h.stats.errors.Add(1)
		return
	}
	if !outcome.tailEligible {
		h.releaseTail()
		return
	}
	task := tailTask{
		kind:        tailBufferedData,
		uffdFD:      uffdFD,
		dstVA:       pageVA + PageSize,
		pageIdx:     pageIdx + 1,
		start:       pageOffset + PageSize,
		end:         fillEnd,
		bufferStart: window.Offset(),
		prefixStart: fillStart,
		expected:    StateAbsent,
	}
	h.enqueueReservedTail(task)
}

func validChunkWindow(window fetch.ChunkRun, pageOffset, bufferBytes uint64) bool {
	if window == nil || window.Kind() != sparse.Data || window.End() <= window.Offset() {
		return false
	}
	if window.Offset() > pageOffset || pageOffset > ^uint64(0)-PageSize || window.End() < pageOffset+PageSize {
		return false
	}
	return window.End()-window.Offset() <= bufferBytes
}

func (h *Handler) chunkPopulationBounds(window fetch.ChunkRun, pageOffset, pageIdx uint64) (uint64, uint64, bool) {
	fillStart := alignedPageStart(window.Offset())
	fillEnd := alignedPageEnd(window.End())
	ramEnd := alignedPageEnd(uint64(h.cfg.Size))
	if fillEnd > ramEnd {
		fillEnd = ramEnd
	}
	regionStart, regionEnd, ok := h.addrMap.CHRegionBounds(pageOffset)
	if !ok {
		return pageOffset, pageOffset + PageSize, false
	}
	regionStart = alignedPageStart(regionStart)
	regionEnd = alignedPageEnd(regionEnd)
	if fillStart < regionStart {
		fillStart = regionStart
	}
	if fillEnd > regionEnd {
		fillEnd = regionEnd
	}
	if fillStart > pageOffset || pageOffset > ^uint64(0)-PageSize || fillEnd < pageOffset+PageSize {
		return pageOffset, pageOffset + PageSize, false
	}
	stateStart, stateEnd := h.state.RunBounds(
		pageIdx,
		fillStart/PageSize,
		fillEnd/PageSize,
		StateAbsent,
	)
	if stateStart == stateEnd {
		return pageOffset, pageOffset + PageSize, false
	}
	return stateStart * PageSize, stateEnd * PageSize, true
}

func alignedPageStart(offset uint64) uint64 {
	if remainder := offset % PageSize; remainder != 0 {
		return offset + PageSize - remainder
	}
	return offset
}

func alignedPageEnd(offset uint64) uint64 {
	return offset - offset%PageSize
}

func (h *Handler) handleChunkUrgentOnly(uffdFD int, pageVA, pageOffset, pageIdx uint64, run sparse.Run, pageBuf []byte) {
	if err := h.readRequiredRun(run, pageBuf, 0); err != nil {
		h.failRead(fmt.Errorf("uffd: chunk urgent read off=0x%x: %w", pageOffset, err))
		return
	}
	if _, err := h.urgentCopy(uffdFD, pageVA, pageIdx, StateAbsent, pageBuf); err != nil {
		h.logf("uffd: COPY(chunk urgent) va=0x%x: %v", pageVA, err)
		h.stats.errors.Add(1)
	}
}

func (h *Handler) handleOrdinaryDataFault(uffdFD int, pageVA, pageOffset, pageIdx uint64, run sparse.Run, fillEnd uint64, pageBuf []byte) {
	if err := h.readRequiredRun(run, pageBuf, 0); err != nil {
		h.failRead(fmt.Errorf("uffd: data urgent read off=0x%x: %w", pageOffset, err))
		return
	}
	outcome, err := h.urgentCopy(uffdFD, pageVA, pageIdx, StateAbsent, pageBuf)
	if err != nil {
		h.logf("uffd: COPY(data urgent) va=0x%x: %v", pageVA, err)
		h.stats.errors.Add(1)
		return
	}
	if !outcome.tailEligible {
		return
	}
	tailEnd := alignedRunEnd(pageOffset, fillEnd)
	if tailEnd <= pageOffset+PageSize {
		return
	}
	if !h.tryReserveTail() {
		return
	}
	task := tailTask{
		kind:     tailDeferredData,
		run:      run,
		uffdFD:   uffdFD,
		dstVA:    pageVA + PageSize,
		pageIdx:  pageIdx + 1,
		start:    pageOffset + PageSize,
		end:      tailEnd,
		expected: StateAbsent,
	}
	h.enqueueReservedTail(task)
}

func (h *Handler) handleZeroFault(uffdFD int, pageVA, pageOffset, pageIdx uint64, expected PageState) {
	hardEnd := h.stateHardEnd(pageOffset, pageIdx, expected, zeroFaultFillBytes)
	h.handleResolvedZeroFault(uffdFD, pageVA, pageOffset, pageIdx, expected, hardEnd)
}

func (h *Handler) handleResolvedZeroFault(uffdFD int, pageVA, pageOffset, pageIdx uint64, expected PageState, runEnd uint64) {
	outcome, err := h.urgentZero(uffdFD, pageVA, pageIdx, expected)
	if err != nil {
		h.logf("uffd: ZEROPAGE urgent va=0x%x expected=%d: %v", pageVA, expected, err)
		h.stats.errors.Add(1)
		return
	}
	if !outcome.tailEligible {
		return
	}
	tailEnd := alignedRunEnd(pageOffset, runEnd)
	if tailEnd <= pageOffset+PageSize {
		return
	}
	if !h.tryReserveTail() {
		return
	}
	task := tailTask{
		kind:     tailZero,
		uffdFD:   uffdFD,
		dstVA:    pageVA + PageSize,
		pageIdx:  pageIdx + 1,
		start:    pageOffset + PageSize,
		end:      tailEnd,
		expected: expected,
	}
	h.enqueueReservedTail(task)
}

func (h *Handler) stateHardEnd(pageOffset, pageIdx uint64, expected PageState, maxFillBytes uint64) uint64 {
	hardEnd := h.addressHardEnd(pageOffset, maxFillBytes)
	maxPages := (hardEnd - pageOffset) / PageSize
	pages := h.state.RunLength(pageIdx, maxPages, expected)
	return pageOffset + pages*PageSize
}

func (h *Handler) addressHardEnd(pageOffset, maxFillBytes uint64) uint64 {
	if pageOffset >= uint64(h.cfg.Size) {
		return pageOffset
	}
	length := min(maxFillBytes, uint64(h.cfg.Size)-pageOffset)
	if remaining, ok := h.addrMap.CHRegionRemaining(pageOffset, length); ok && remaining < length {
		length = remaining
	}
	length -= length % PageSize
	return pageOffset + length
}

func alignedRunEnd(start, end uint64) uint64 {
	if end <= start {
		return start
	}
	return start + ((end-start)/PageSize)*PageSize
}

func validateFaultRun(run sparse.Run, offset, hardEnd uint64) error {
	if run == nil {
		return fmt.Errorf("uffd: source RunAt at %d returned nil", offset)
	}
	if run.Offset() != offset || run.End() <= offset || run.End() > hardEnd {
		return fmt.Errorf("uffd: source RunAt at %d returned invalid range [%d,%d), hard end %d", offset, run.Offset(), run.End(), hardEnd)
	}
	switch run.Kind() {
	case sparse.Hole, sparse.Zero, sparse.Data:
		return nil
	default:
		return fmt.Errorf("uffd: source RunAt at %d returned invalid kind %d", offset, run.Kind())
	}
}

func (h *Handler) failUrgent(uffdFD int, pageVA uint64, format string, args ...any) {
	if !h.closing.Load() && !errors.Is(h.ctx.Err(), context.Canceled) {
		h.logf("uffd: "+format, args...)
		h.stats.errors.Add(1)
	}
	h.wake(uffdFD, pageVA, PageSize)
}

// failRead never wakes or fills a failed mandatory request. The owner can
// kill CH immediately, without waiting for this worker's cleanup.
func (h *Handler) failRead(err error) {
	if h.ctx.Err() != nil {
		return
	}
	h.fatalOnce.Do(func() {
		h.stats.errors.Add(1)
		h.logf("uffd: mandatory source read failed: %v", err)
		if h.cfg.ReadFatal != nil {
			h.cfg.ReadFatal(err)
		}
		h.cancel()
	})
}

func (h *Handler) readRequiredRun(run sparse.Run, buf []byte, innerOffset uint64) error {
	return readretry.Do(h.ctx, func() error { return h.readRun(run, buf, innerOffset) })
}

func (h *Handler) readRun(run sparse.Run, buf []byte, innerOffset uint64) error {
	started := time.Now()
	n, err := run.ReadAt(h.ctx, buf, innerOffset)
	elapsed := uint64(time.Since(started).Nanoseconds())
	h.stats.sourceReadCalls.Add(1)
	if n > 0 {
		h.stats.sourceReadBytes.Add(uint64(n))
	}
	h.stats.sourceReadNs.Add(elapsed)
	if !readerr.IsPermanent(err) && !readretry.IsTerminal(err) && n == len(buf) && (err == nil || err == io.EOF) {
		h.stats.pageIn.record(elapsed)
		return nil
	}
	if err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return readerr.Mark(err, false)
		}
		return err
	}
	return readerr.Mark(fmt.Errorf("short Run.ReadAt: %d of %d bytes", n, len(buf)), false)
}

func (h *Handler) urgentCopy(fd int, dst uint64, pageIdx uint64, expected PageState, page []byte) (urgentOutcome, error) {
	if err := h.ctx.Err(); err != nil {
		return urgentOutcome{}, err
	}
	// A synchronous source retry may outlive an observed REMOVE or another
	// installation. Its old bytes no longer describe a Released page. Apply
	// the same zero/EEXIST convergence as a fresh fault in that state, and do
	// not schedule a tail from the obsolete read plan.
	if state := h.state.Get(pageIdx); state != expected {
		_, err := h.urgentZero(fd, dst, pageIdx, state)
		return urgentOutcome{}, err
	}
	started := time.Now()
	completed, err := h.ops.copy(fd, dst, page)
	h.stats.urgentCopyNs.Add(uint64(time.Since(started).Nanoseconds()))
	h.stats.urgentCopyCalls.Add(1)
	h.stats.copies.Add(1)
	return h.finishUrgent(fd, dst, pageIdx, expected, completed, err, false)
}

func (h *Handler) urgentZero(fd int, dst uint64, pageIdx uint64, expected PageState) (urgentOutcome, error) {
	if err := h.ctx.Err(); err != nil {
		return urgentOutcome{}, err
	}
	started := time.Now()
	completed, err := h.ops.zeropage(fd, dst, PageSize)
	h.stats.urgentZeroNs.Add(uint64(time.Since(started).Nanoseconds()))
	h.stats.urgentZeroCalls.Add(1)
	h.stats.zeropages.Add(1)
	return h.finishUrgent(fd, dst, pageIdx, expected, completed, err, true)
}

func (h *Handler) finishUrgent(fd int, dst, pageIdx uint64, expected PageState, completed int64, ioctlErr error, zero bool) (urgentOutcome, error) {
	done, err := checkedCompletion(completed, PageSize)
	if err != nil {
		return urgentOutcome{}, err
	}
	completedPages := done / PageSize
	var committed uint64
	if completedPages > 0 {
		committed = h.state.SetRangeIf(pageIdx, pageIdx+completedPages, expected, StateLoaded)
		if zero {
			h.stats.pagesZeroed.Add(completedPages)
		} else {
			h.stats.pagesCopied.Add(completedPages)
		}
	}
	if ioctlErr == nil {
		if done != PageSize {
			return urgentOutcome{}, fmt.Errorf("short urgent completion: %d of %d bytes", done, PageSize)
		}
		return urgentOutcome{tailEligible: committed == 1}, nil
	}

	if errors.Is(ioctlErr, unix.EEXIST) {
		// EEXIST means another actor installed the folio. Preserve the
		// established WAKE convergence and conditionally settle state.
		if done == 0 {
			h.state.SetRangeIf(pageIdx, pageIdx+1, expected, StateLoaded)
		}
		h.wake(fd, dst, PageSize)
		return urgentOutcome{}, nil
	}
	if errors.Is(ioctlErr, unix.EAGAIN) || errors.Is(ioctlErr, unix.ENOENT) {
		h.wake(fd, dst, PageSize)
		return urgentOutcome{}, nil
	}
	return urgentOutcome{}, ioctlErr
}

func checkedCompletion(completed int64, requested uint64) (uint64, error) {
	if completed < 0 {
		return 0, fmt.Errorf("uffd ioctl returned negative completion %d", completed)
	}
	done := uint64(completed)
	if done > requested {
		return 0, fmt.Errorf("uffd ioctl completed %d bytes, requested %d", done, requested)
	}
	if done%PageSize != 0 {
		return 0, fmt.Errorf("uffd ioctl completion %d is not page-aligned", done)
	}
	return done, nil
}

func (h *Handler) wake(fd int, start, length uint64) {
	_ = h.ops.wake(fd, start, length)
	h.stats.wakes.Add(1)
}

func (h *Handler) tryReserveTail() bool {
	if h.closing.Load() {
		h.stats.tailCanceled.Add(1)
		return false
	}
	if !h.tailBusy.CompareAndSwap(false, true) {
		h.stats.tailDroppedBusy.Add(1)
		return false
	}
	select {
	case <-h.tailIdle:
	default:
	}
	if h.closing.Load() {
		h.stats.tailCanceled.Add(1)
		h.releaseTail()
		return false
	}
	return true
}

func (h *Handler) releaseTail() {
	if !h.tailBusy.CompareAndSwap(true, false) {
		return
	}
	select {
	case h.tailIdle <- struct{}{}:
	default:
	}
}

func (h *Handler) waitTailIdle() {
	for h.tailBusy.Load() {
		<-h.tailIdle
	}
}

func (h *Handler) enqueueReservedTail(task tailTask) bool {
	h.tailSubmit.Lock()
	defer h.tailSubmit.Unlock()
	if h.closing.Load() || h.ctx.Err() != nil {
		h.stats.tailCanceled.Add(1)
		h.releaseTail()
		return false
	}
	select {
	case h.tailQ <- task:
		h.stats.tailSubmitted.Add(1)
		switch task.kind {
		case tailBufferedData:
			h.stats.tailBuffered.Add(1)
		case tailDeferredData:
			h.stats.tailDeferred.Add(1)
		case tailZero:
			h.stats.tailZero.Add(1)
		}
		return true
	default:
		// tailBusy should make this unreachable, but preserve non-blocking
		// fault progress if an invariant is violated.
		h.stats.tailDroppedBusy.Add(1)
		h.releaseTail()
		return false
	}
}

func (h *Handler) runTailWorker() {
	defer h.tailWG.Done()
	for {
		select {
		case <-h.ctx.Done():
			select {
			case <-h.tailQ:
				h.stats.tailCanceled.Add(1)
				h.releaseTail()
			default:
			}
			return
		case task := <-h.tailQ:
			if h.ctx.Err() != nil {
				h.stats.tailCanceled.Add(1)
				h.releaseTail()
				continue
			}
			h.processTail(task)
			h.releaseTail()
		}
	}
}

func (h *Handler) processTail(task tailTask) {
	if task.kind == tailBufferedData {
		h.processBufferedTail(task)
		return
	}

	var maxTailBytes uint64
	switch task.kind {
	case tailDeferredData:
		maxTailBytes = ordinaryDataNeighborTailBytes
	case tailZero:
		maxTailBytes = zeroNeighborTailBytes
	default:
		return
	}
	maxPages := min((task.end-task.start)/PageSize, maxTailBytes/PageSize)
	statePages := h.state.RunLength(task.pageIdx, maxPages, task.expected)
	if statePages == 0 {
		h.stats.tailConflicts.Add(1)
		return
	}
	length := statePages * PageSize
	length -= length % PageSize
	if length == 0 {
		return
	}

	var data []byte
	switch task.kind {
	case tailDeferredData:
		data = h.tailBuf[:length]
		innerOffset := task.start - task.run.Offset()
		if err := h.readRun(task.run, data, innerOffset); err != nil {
			if errors.Is(err, context.Canceled) {
				h.stats.tailCanceled.Add(1)
			} else {
				h.logf("uffd: deferred tail read [%d,%d): %v", task.start, task.start+length, err)
			}
			return
		}
	case tailZero:
	}

	// A source read may be slow. Re-check expected state immediately before
	// the ioctl and shrink to the still-valid prefix.
	statePages = h.state.RunLength(task.pageIdx, length/PageSize, task.expected)
	if statePages == 0 {
		h.stats.tailConflicts.Add(1)
		return
	}
	if validLength := statePages * PageSize; validLength < length {
		length = validLength
		if data != nil {
			data = data[:length]
		}
		h.stats.tailConflicts.Add(1)
	}

	h.stats.tailPlanned.Add(length / PageSize)
	h.executeTailIO(task, data, length)
}

func (h *Handler) processBufferedTail(task tailTask) {
	if task.start < PageSize || task.end < task.start {
		return
	}
	faultOffset := task.start - PageSize
	if task.bufferStart > task.prefixStart || task.prefixStart > faultOffset {
		return
	}
	prefixLength := faultOffset - task.prefixStart
	suffixLength := task.end - task.start
	if prefixLength > chunkNeighborTailBytes || suffixLength > chunkNeighborTailBytes-prefixLength {
		return
	}
	if !h.processBufferedSuffix(task) {
		return
	}
	h.processBufferedPrefix(task, faultOffset)
}

// processBufferedSuffix preserves the natural nearest-to-farthest kernel
// prefix semantics. If state changed since the fault worker planned the task,
// copy the still-valid adjacent prefix once and stop the whole tail afterwards.
func (h *Handler) processBufferedSuffix(task tailTask) bool {
	length := task.end - task.start
	if length == 0 {
		return true
	}
	requestedPages := length / PageSize
	statePages := h.state.RunLength(task.pageIdx, requestedPages, task.expected)
	if statePages == 0 {
		h.stats.tailConflicts.Add(1)
		return false
	}
	stateChanged := statePages < requestedPages
	if stateChanged {
		length = statePages * PageSize
		h.stats.tailConflicts.Add(1)
	}
	data, ok := h.bufferedTailData(task.bufferStart, task.start, task.start+length)
	if !ok {
		return false
	}
	h.stats.tailPlanned.Add(length / PageSize)
	_, full := h.executeTailIO(task, data, length)
	return full && !stateChanged
}

// processBufferedPrefix scans backward from the page adjacent to the fault.
// If state changed since planning, it copies the still-valid adjacent suffix
// after the first mismatch and abandons everything farther away. UFFDIO_COPY
// itself still runs in ascending address order; any kernel partial completion
// is committed as reported and is never retried.
func (h *Handler) processBufferedPrefix(task tailTask, faultOffset uint64) bool {
	if faultOffset == task.prefixStart {
		return true
	}
	faultPageIdx := faultOffset / PageSize
	plannedStartIdx := task.prefixStart / PageSize
	startIdx, endIdx := h.state.RunBounds(
		faultPageIdx-1,
		plannedStartIdx,
		faultPageIdx,
		task.expected,
	)
	if startIdx == endIdx || endIdx != faultPageIdx {
		h.stats.tailConflicts.Add(1)
		return false
	}
	if startIdx > plannedStartIdx {
		h.stats.tailConflicts.Add(1)
	}
	start := startIdx * PageSize
	length := faultOffset - start
	pages := length / PageSize
	data, ok := h.bufferedTailData(task.bufferStart, start, faultOffset)
	if !ok || task.dstVA < PageSize {
		return false
	}
	faultVA := task.dstVA - PageSize
	if faultVA < length {
		return false
	}
	prefixTask := task
	prefixTask.dstVA = faultVA - length
	prefixTask.pageIdx = startIdx
	prefixTask.start = start
	prefixTask.end = faultOffset
	h.stats.tailPlanned.Add(pages)
	_, full := h.executeTailIO(prefixTask, data, length)
	return full
}

func (h *Handler) bufferedTailData(bufferStart, start, end uint64) ([]byte, bool) {
	if start < bufferStart || end < start || end-bufferStart > uint64(len(h.tailBuf)) {
		return nil, false
	}
	return h.tailBuf[start-bufferStart : end-bufferStart], true
}

func (h *Handler) executeTailIO(task tailTask, data []byte, length uint64) (uint64, bool) {
	started := time.Now()
	var completed int64
	var err error
	if task.kind == tailZero {
		completed, err = h.ops.zeropage(task.uffdFD, task.dstVA, length)
		h.stats.tailZeroNs.Add(uint64(time.Since(started).Nanoseconds()))
		h.stats.zeropages.Add(1)
	} else {
		completed, err = h.ops.copy(task.uffdFD, task.dstVA, data)
		h.stats.tailCopyNs.Add(uint64(time.Since(started).Nanoseconds()))
		h.stats.copies.Add(1)
	}
	done, validationErr := checkedCompletion(completed, length)
	if validationErr != nil {
		h.logf("uffd: tail ioctl invalid completion: %v", validationErr)
		h.stats.tailPartial.Add(1)
		return 0, false
	}
	pages := done / PageSize
	if done < length {
		h.stats.tailPartial.Add(1)
	}
	if pages > 0 {
		changed := h.state.SetRangeIf(task.pageIdx, task.pageIdx+pages, task.expected, StateLoaded)
		if task.kind == tailZero {
			h.stats.pagesZeroed.Add(pages)
		} else {
			h.stats.pagesCopied.Add(pages)
		}
		h.stats.tailCompleted.Add(pages)
		if changed != pages {
			h.stats.tailConflicts.Add(1)
			return done, false
		}
	}
	if err != nil {
		if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.ENOENT) {
			h.stats.tailConflicts.Add(1)
		} else if !errors.Is(err, context.Canceled) {
			h.logf("uffd: best-effort tail ioctl: %v", err)
		}
		return done, false
	}
	return done, done == length
}

func recordAtomicMax(dst *atomic.Uint64, value uint64) {
	for {
		current := dst.Load()
		if value <= current || dst.CompareAndSwap(current, value) {
			return
		}
	}
}
