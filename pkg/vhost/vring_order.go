//go:build amd64 || arm64

package vhost

import (
	"fmt"
	"sync/atomic"
	"unsafe"
)

// Split-ring indices publish shared guest memory, outside Go's synchronization
// domain. Reading avail.idx needs acquire ordering before reading descriptors;
// writing used.idx needs release ordering after writing data, status and the
// used entry. Missing either ordering can expose a new index with stale content.
// This is a plausible source of invalid completion IDs; fault injection alone
// does not establish it as the cause of a particular guest failure.
//
// On supported little-endian amd64/arm64 hosts, load an aligned word containing
// idx. For avail at offset 0 mod 4 this is flags+idx; at offset 2 it is idx+ring[0].
// The latter requires six mapped bytes, but never accesses before the ring.
// Mixed-width guest/host accesses rely on coherent, normal cacheable RAM and
// architecture guarantees, not solely on the Go memory model. ARM hardware
// interoperability validation is still required.

func validateVringHeader(hdr []byte, addr uint64, alignment uintptr) error {
	if len(hdr) < 4 {
		return fmt.Errorf("vhost: short vring header: %d", len(hdr))
	}
	if addr%uint64(alignment) != 0 || uintptr(unsafe.Pointer(&hdr[0]))%alignment != 0 {
		return fmt.Errorf("vhost: vring address %#x or host mapping is not %d-byte aligned", addr, alignment)
	}
	return nil
}

// loadAvailIdxAcquire only reads the driver-owned available ring. It does not
// expose the aligned word, which may also contain flags or the first ring entry.
func loadAvailIdxAcquire(hdr []byte, addr uint64) (uint16, error) {
	if err := validateVringHeader(hdr, addr, 2); err != nil {
		return 0, err
	}
	if uintptr(unsafe.Pointer(&hdr[0]))%4 == 0 {
		return uint16(atomic.LoadUint32((*uint32)(unsafe.Pointer(&hdr[0]))) >> 16), nil
	}
	if len(hdr) < 6 {
		return 0, fmt.Errorf("vhost: available ring needs six mapped bytes for aligned index load")
	}
	return uint16(atomic.LoadUint32((*uint32)(unsafe.Pointer(&hdr[2])))), nil
}

// usedRingHeader holds one completion's snapshot of the device-owned header.
// The queue worker exclusively owns both used.flags and used.idx, from loading
// this snapshot through publication. Reload for each completion; any future
// flags writer must serialize with this entire interval.
type usedRingHeader struct {
	word  *uint32
	value uint32
}

func loadUsedHeaderAcquire(hdr []byte, addr uint64) (usedRingHeader, error) {
	if err := validateVringHeader(hdr, addr, 4); err != nil {
		return usedRingHeader{}, err
	}
	word := (*uint32)(unsafe.Pointer(&hdr[0]))
	return usedRingHeader{word: word, value: atomic.LoadUint32(word)}, nil
}

func (h usedRingHeader) index() uint16 {
	return uint16(h.value >> 16)
}

// publishNextRelease preserves flags from the same load that selected the used
// entry. Call once, after writing the completion's data, status and used entry.
func (h usedRingHeader) publishNextRelease() {
	nextIdx := h.index() + 1
	atomic.StoreUint32(h.word, h.value&0xffff|uint32(nextIdx)<<16)
}
