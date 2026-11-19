package uffd

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// seedRegion runs before the CH va_report ACK. Pages that already exist can
// have been installed outside our COPY protocol; include them in D_seed.
func (h *Handler) seedRegion(fd int) error {
	if h.cfg.Dirty == nil {
		return nil
	}
	h.addrMap.mu.RLock()
	defer h.addrMap.mu.RUnlock()
	for _, v := range h.addrMap.vmas {
		if v.process != ProcessCH || v.uffdFD != fd || v.invalid {
			continue
		}
		end := v.memfdOffset + (v.end - v.start)
		for off := v.memfdOffset; off < end; {
			data, err := unix.Seek(h.cfg.MemfdFD, int64(off), unix.SEEK_DATA)
			if errors.Is(err, unix.ENXIO) {
				break
			}
			if err != nil {
				return err
			}
			if uint64(data) >= end {
				break
			}
			hole, err := unix.Seek(h.cfg.MemfdFD, data, unix.SEEK_HOLE)
			if err != nil {
				return err
			}
			if hole <= data {
				return fmt.Errorf("uffd: invalid seed residency")
			}
			if err := h.cfg.Dirty.Mark(uint64(data), min(uint64(hole), end)-uint64(data)); err != nil {
				return err
			}
			if h.pageTrace != nil {
				h.pageTrace.record("seed", uint64(data), min(uint64(hole), end)-uint64(data), h.state.Get(h.pageTrace.page/PageSize), nil)
			}
			off = uint64(hole)
		}
	}
	return nil
}

// All callers hold installMu. An unknown existing folio is conservative dirty;
// COPY reporting EEXIST does not prove that its contents still equal B.
func (h *Handler) markInstall(fd int, dst, length uint64) error {
	if h.cfg.Dirty == nil {
		return nil
	}
	off, ok := h.addrMap.Locate(dst)
	if !ok || !h.addrMap.owns(fd, dst, off, length) {
		return fmt.Errorf("uffd: dirty installation registration missing")
	}
	return h.cfg.Dirty.Mark(off, length)
}
func (h *Handler) copyCleanLocked(fd int, dst uint64, data []byte) (int64, error) {
	// Ordinary installation restores the fixed baseline, not a new write.
	// CPU, CH and Go writers supply D; released pages remain covered by R.
	n, err := h.ops.copy(fd, dst, data)
	if h.pageTrace != nil {
		h.traceIO("copy", dst, uint64(len(data)), data, n, err)
	}
	if errors.Is(err, unix.EEXIST) {
		if markErr := h.markInstall(fd, dst, uint64(len(data))); markErr != nil {
			return n, markErr
		}
	}
	return n, err
}
func (h *Handler) zeroCleanLocked(fd int, dst, length uint64) (int64, error, bool) {
	n, err := h.ops.zeropage(fd, dst, length)
	if h.pageTrace != nil {
		h.traceIO("zero", dst, length, nil, n, err)
	}
	if n == 0 && (errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP)) {
		n, err = h.copyCleanLocked(fd, dst, zeroScratch[:length])
		h.stats.copies.Add(1)
		return n, err, false
	}
	if errors.Is(err, unix.EEXIST) {
		if markErr := h.markInstall(fd, dst, length); markErr != nil {
			return n, markErr, true
		}
	}
	return n, err, true
}

func (h *Handler) invalidateCapture(err error) {
	h.installMu.Lock()
	if h.captureError == nil {
		h.captureError = err
	}
	h.installMu.Unlock()
}
