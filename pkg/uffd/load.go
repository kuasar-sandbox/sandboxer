package uffd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"golang.org/x/sys/unix"
)

type loadRequest struct {
	ctx    context.Context
	offset uint64
	// A canceled caller can leave without stranding its worker on delivery.
	done chan error
}

// EnsureLoaded prepares every intersecting page before a backend mmap is
// exposed. Loaded ranges require no source read or ioctl and preserve live
// bytes. Only the owning CH registration can atomically install a new folio.
// Normal guest descriptor ownership is required; this does not pin DMA pages
// against arbitrary concurrent balloon/discard after the barrier returns.
func (h *Handler) EnsureLoaded(caller context.Context, offset, length uint64) error {
	h.uffdsMu.Lock()
	if h.closing.Load() || h.ctx.Err() != nil || !h.started {
		h.uffdsMu.Unlock()
		return fmt.Errorf("uffd: handler is not running")
	}
	h.users.Add(1) // serialized with closing and Wait, even for Loaded fast paths
	h.uffdsMu.Unlock()
	defer h.users.Done()
	if offset > uint64(h.cfg.Size) || length > uint64(h.cfg.Size)-offset {
		return fmt.Errorf("uffd: load range [0x%x,+0x%x) outside RAM", offset, length)
	}
	ctx, cancel := context.WithCancel(caller)
	stop := context.AfterFunc(h.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if length == 0 {
		return nil
	}
	end := (offset + length + PageSize - 1) / PageSize * PageSize
	for off := offset &^ uint64(PageSize-1); off < end; {
		// Hold admission while taking a notification snapshot to avoid lost
		// registrations. Waits and queue sends always happen outside both locks.
		h.uffdsMu.Lock()
		changed := h.registrationChanged
		h.installMu.Lock()
		_, fd, covered, err := h.addrMap.registration(off, end-off)
		loaded := uint64(0)
		if err == nil && fd > 0 {
			loaded = h.state.RunLength(off/PageSize, covered/PageSize, StateLoaded)
		}
		h.installMu.Unlock()
		h.uffdsMu.Unlock()
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if fd == 0 {
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if loaded > 0 {
			off += loaded * PageSize
			continue
		}
		req := &loadRequest{ctx: ctx, offset: off, done: make(chan error, 1)}
		q := h.queue[pageIdxHash(off/PageSize)%uint64(len(h.queue))]
		select {
		case q <- faultEvent{mandatory: req, queued: time.Now()}:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case err := <-req.done:
			if errors.Is(err, unix.EAGAIN) {
				// Release the worker before waiting. Otherwise all mandatory workers
				// could await mmap_changing while the reader awaits space in a full
				// CPU fault queue ahead of the REMOVE event that clears it.
				timer := time.NewTimer(time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
				continue
			}
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		off += PageSize
	}
	return ctx.Err()
}

var errPageChanged = errors.New("uffd: page state changed during source read")

// loadPage runs on an existing fault worker with its private 4KiB buffer.
func (h *Handler) loadPage(ctx context.Context, off uint64, page []byte) error {
	idx := off / PageSize
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h.installMu.Lock()
		va, fd, covered, err := h.addrMap.registration(off, PageSize)
		state := h.state.Get(idx)
		h.installMu.Unlock()
		if err != nil {
			return err
		}
		if fd <= 0 || covered != PageSize {
			return fmt.Errorf("uffd: no owning registration at 0x%x", off)
		}
		if state == StateLoaded {
			return nil
		}
		clear(page)
		if state == StateAbsent && !isZeroSource(h.cfg.Source) {
			err = readretry.Do(ctx, func() error {
				// Absent never returns after REMOVE, even if Released is subsequently
				// reused. No per-page generation or lock allocation is needed.
				if h.state.Get(idx) != StateAbsent {
					return readretry.Terminal(errPageChanged)
				}
				run, err := h.cfg.Source.RunAt(off, PageSize)
				if err == io.EOF {
					return readerr.Mark(err, false)
				}
				if err != nil {
					return err
				}
				if err := validateFaultRun(run, off, off+PageSize); err != nil {
					return readerr.Mark(err, false)
				}
				if run.End() != off+PageSize {
					return readerr.Mark(fmt.Errorf("uffd: source does not cover full page"), false)
				}
				if run.Kind() != sparse.Data {
					clear(page)
					return nil
				}
				return h.readRunContext(ctx, run, page, 0)
			})
			if errors.Is(err, errPageChanged) {
				continue
			}
			if err != nil {
				return err
			}
		}
		h.installMu.Lock()
		if err = ctx.Err(); err != nil {
			h.installMu.Unlock()
			return err
		}
		if !h.addrMap.owns(fd, va, off, PageSize) {
			h.installMu.Unlock()
			return fmt.Errorf("uffd: registration lost during load")
		}
		if h.state.Get(idx) != state {
			h.installMu.Unlock()
			continue
		}
		// COPY of a zero scratch page works on MISSING_SHMEM on the 5.10 floor;
		// ZEROPAGE is not assumed to be available for shmem.
		started := time.Now()
		completed, copyErr := h.ops.copy(fd, va, page)
		h.stats.copies.Add(1)
		h.stats.urgentCopyCalls.Add(1)
		h.stats.urgentCopyNs.Add(uint64(time.Since(started).Nanoseconds()))
		done, err := checkedCompletion(completed, PageSize)
		if err == nil {
			switch {
			case copyErr == nil && done == PageSize, errors.Is(copyErr, unix.EEXIST):
				h.state.Set(idx, StateLoaded)
				h.stats.pagesCopied.Add(done / PageSize)
				if errors.Is(copyErr, unix.EEXIST) {
					h.wake(fd, va, PageSize)
				}
			case copyErr == nil:
				err = fmt.Errorf("uffd: short mandatory COPY: %d of %d", done, PageSize)
			default:
				err = copyErr
			}
		}
		h.installMu.Unlock()
		// The caller retries EAGAIN cancelably after releasing this worker.
		// ENOENT is registration loss and must fail without exposing a slice.
		return err
	}
}
