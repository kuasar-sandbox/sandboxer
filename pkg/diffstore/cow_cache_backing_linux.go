package diffstore

import (
	"fmt"
	"os"
	"sort"
	"unsafe"

	"golang.org/x/sys/unix"
)

// elasticBacking owns the physical payload of an elastic COW cache. Virtual
// slots survive budget shrink; reclaim discards their anonymous physical pages
// and reuse requires explicit preparation under a new grant. A segment is
// virtual memory only until individual slots are prepared.
type elasticBacking struct {
	mappings  [][]byte
	available []*[cowBlockSize]byte
	retired   []*[cowBlockSize]byte
	resident  int
}

const elasticSegmentBytes = 2 << 20

// ElasticCacheSupported checks that a grant can be faulted and discarded at
// 4 KiB granularity before advertising cache budget support to the node.
func ElasticCacheSupported() bool {
	if safeElasticHostPageSize(os.Getpagesize()) != nil {
		return false
	}
	m, err := mapElasticSegment()
	if err != nil {
		return false
	}
	return unix.Munmap(m) == nil
}

// Each elastic slot is charged and returned independently. A transparent
// huge page can fault in an entire segment for one slot, or refill holes in
// a partially reclaimed segment. Keep these mappings on ordinary pages.
func mapElasticSegment() ([]byte, error) {
	m, err := unix.Mmap(-1, 0, elasticSegmentBytes, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		return nil, err
	}
	if err := unix.Madvise(m, unix.MADV_NOHUGEPAGE); err != nil {
		_ = unix.Munmap(m)
		return nil, fmt.Errorf("disable transparent huge pages: %w", err)
	}
	return m, nil
}

func safeElasticHostPageSize(size int) error {
	if size != cowBlockSize {
		return fmt.Errorf("diffstore: elastic cache currently requires 4KiB host pages (host page size %d)", size)
	}
	return nil
}

// The kernel accepts adjacent VMAs in one madvise call. Pass the address and
// extent directly so that no Go slice spans independently owned mmaps. All
// slots remain mapped for the duration of the serialized resize.
func populateCachePages(first *[cowBlockSize]byte, count int) error {
	_, _, errno := unix.Syscall(unix.SYS_MADVISE, uintptr(unsafe.Pointer(first)), uintptr(count)*cowBlockSize, unix.MADV_POPULATE_WRITE)
	if errno != 0 {
		return errno
	}
	return nil
}

// Fault only the funded slots. A failed MADV_POPULATE_WRITE can leave a prefix
// resident. Touching every slot in that run completes it on both partially
// successful and older kernels. Runs of one use a single ordinary fault.
func prefaultCacheSlots(slots []*[cowBlockSize]byte, populate func(*[cowBlockSize]byte, int) error) {
	for i := 0; i < len(slots); {
		j := i + 1
		for j < len(slots) && uintptr(unsafe.Pointer(slots[j])) == uintptr(unsafe.Pointer(slots[j-1]))+cowBlockSize {
			j++
		}
		if j-i == 1 || populate(slots[i], j-i) != nil {
			for _, slot := range slots[i:j] {
				slot[0] = 0
			}
		}
		i = j
	}
}

func (e *elasticBacking) prepare(target int) error {
	if err := safeElasticHostPageSize(os.Getpagesize()); err != nil {
		return err
	}
	for e.resident < target {
		if len(e.retired) == 0 {
			m, err := mapElasticSegment()
			if err != nil {
				return fmt.Errorf("diffstore: allocate cache virtual segment: %w", err)
			}
			e.mappings = append(e.mappings, m)
			for i := 0; i < len(m); i += cowBlockSize {
				e.retired = append(e.retired, (*[cowBlockSize]byte)(unsafe.Pointer(&m[i])))
			}
		}
		add := min(target-e.resident, len(e.retired))
		slots := e.retired[len(e.retired)-add:]
		prefaultCacheSlots(slots, populateCachePages)
		for i := len(slots) - 1; i >= 0; i-- {
			e.available = append(e.available, slots[i])
		}
		clear(slots)
		e.retired = e.retired[:len(e.retired)-add]
		e.resident += add
	}
	return nil
}

func (e *elasticBacking) take() *[cowBlockSize]byte {
	if len(e.available) == 0 {
		panic("diffstore: elastic cache consumed unprepared payload")
	}
	last := len(e.available) - 1
	p := e.available[last]
	e.available[last] = nil
	e.available = e.available[:last]
	return p
}

func (e *elasticBacking) put(p *[cowBlockSize]byte) {
	clear(p[:])
	e.available = append(e.available, p)
}

// reclaim releases only free payload pages. Pages are sorted and coalesced to
// bound MADV_DONTNEED calls rather than issuing a syscall for every 4KiB slot.
// A failure leaves unprocessed pages charged and reusable.
func (e *elasticBacking) reclaim(target int) error {
	if err := safeElasticHostPageSize(os.Getpagesize()); err != nil {
		return err
	}
	drop := e.resident - target
	if drop <= 0 {
		return nil
	}
	if drop > len(e.available) {
		return fmt.Errorf("diffstore: cannot reclaim %d cache pages with %d available", drop, len(e.available))
	}
	pages := append([]*[cowBlockSize]byte(nil), e.available[len(e.available)-drop:]...)
	e.available = e.available[:len(e.available)-drop]
	sort.Slice(pages, func(i, j int) bool { return uintptr(unsafe.Pointer(pages[i])) < uintptr(unsafe.Pointer(pages[j])) })
	for i := 0; i < len(pages); {
		j := i + 1
		for j < len(pages) && uintptr(unsafe.Pointer(pages[j])) == uintptr(unsafe.Pointer(pages[j-1]))+cowBlockSize {
			j++
		}
		data := unsafe.Slice((*byte)(unsafe.Pointer(pages[i])), (j-i)*cowBlockSize)
		if err := unix.Madvise(data, unix.MADV_DONTNEED); err != nil {
			e.available = append(e.available, pages[i:]...)
			return fmt.Errorf("diffstore: return cache pages: %w", err)
		}
		e.retired = append(e.retired, pages[i:j]...)
		e.resident -= j - i
		i = j
	}
	return nil
}

func (e *elasticBacking) close() error {
	var err error
	for _, m := range e.mappings {
		if e2 := unix.Munmap(m); e2 != nil && err == nil {
			err = e2
		}
	}
	e.mappings = nil
	e.available = nil
	e.retired = nil
	e.resident = 0
	return err
}

// cacheBackingRollbackFailure marks a page fault / mmap failure for which
// staged physical pages could not be returned. The cache must stop rather than
// silently regrant those potentially resident bytes.
type cacheBackingRollbackFailure struct{ cause error }

func (e *cacheBackingRollbackFailure) Error() string { return e.cause.Error() }
func (e *cacheBackingRollbackFailure) Unwrap() error { return e.cause }

// elasticPrepared is an off-hot-lock growth cohort. The original free retired
// slots are owned by the resize operation until install/abort, and the active
// pages/available pool remain completely untouched while page faults occur.
type elasticPrepared struct {
	reused   int
	ready    []*[cowBlockSize]byte
	extra    []*[cowBlockSize]byte
	mappings [][]byte
}

// stagePrepare faults the newly granted anonymous payload bytes while the
// cache's I/O mutex is not held. Concurrent resizes / Close are serialized by
// the cache resizeMu; ordinary I/O does not touch the retired slot slice.
func (e *elasticBacking) stagePrepare(count int) (_ *elasticPrepared, retErr error) {
	if err := safeElasticHostPageSize(os.Getpagesize()); err != nil {
		return nil, err
	}
	if count <= 0 {
		return &elasticPrepared{}, nil
	}
	p := &elasticPrepared{}
	defer func() {
		if retErr != nil {
			if err := p.abort(); err != nil {
				retErr = &cacheBackingRollbackFailure{cause: fmt.Errorf("cache prepare: %w; rollback: %v", retErr, err)}
			}
		}
	}()
	p.reused = min(count, len(e.retired))
	prefaultCacheSlots(e.retired[len(e.retired)-p.reused:], populateCachePages)
	for i := 0; i < p.reused; i++ {
		slot := e.retired[len(e.retired)-1-i]
		p.ready = append(p.ready, slot)
	}
	remaining := count - p.reused
	for remaining > 0 {
		m, err := mapElasticSegment()
		if err != nil {
			return nil, fmt.Errorf("diffstore: grow cache backing: %w", err)
		}
		p.mappings = append(p.mappings, m)
		first := len(p.ready)
		for i := 0; i < len(m); i += cowBlockSize {
			slot := (*[cowBlockSize]byte)(unsafe.Pointer(&m[i]))
			if remaining > 0 {
				p.ready = append(p.ready, slot)
				remaining--
			} else {
				p.extra = append(p.extra, slot)
			}
		}
		prefaultCacheSlots(p.ready[first:], populateCachePages)
	}
	// take pops from the end. Sequential I/O should consume adjacent freshly
	// faulted slots in increasing address order. Keep reused slots in their
	// own prefix so abort can discard them independently of new mappings.
	for _, slots := range [][]*[cowBlockSize]byte{p.ready[:p.reused], p.ready[p.reused:]} {
		sort.Slice(slots, func(i, j int) bool {
			return uintptr(unsafe.Pointer(slots[i])) > uintptr(unsafe.Pointer(slots[j]))
		})
	}
	return p, nil
}

func (e *elasticBacking) installPrepared(p *elasticPrepared) {
	e.retired = e.retired[:len(e.retired)-p.reused]
	e.retired = append(e.retired, p.extra...)
	e.available = append(e.available, p.ready...)
	e.mappings = append(e.mappings, p.mappings...)
	e.resident += len(p.ready)
}

// madvisePages coalesces adjacent complete 4KiB pages before MADV_DONTNEED.
// Only pointers known to be exclusively free may be passed.
func madvisePages(pages []*[cowBlockSize]byte) error {
	if err := safeElasticHostPageSize(os.Getpagesize()); err != nil {
		return err
	}
	sort.Slice(pages, func(i, j int) bool {
		return uintptr(unsafe.Pointer(pages[i])) < uintptr(unsafe.Pointer(pages[j]))
	})
	for i := 0; i < len(pages); {
		j := i + 1
		for j < len(pages) && uintptr(unsafe.Pointer(pages[j])) == uintptr(unsafe.Pointer(pages[j-1]))+cowBlockSize {
			j++
		}
		data := unsafe.Slice((*byte)(unsafe.Pointer(pages[i])), (j-i)*cowBlockSize)
		if err := unix.Madvise(data, unix.MADV_DONTNEED); err != nil {
			return fmt.Errorf("diffstore: madvise returned pages: %w", err)
		}
		i = j
	}
	return nil
}

func (p *elasticPrepared) abort() error {
	var result error
	// Only the first 'reused' pages are from existing mappings. New mappings
	// are released in their entirety.
	if err := madvisePages(p.ready[:p.reused]); err != nil {
		result = err
	}
	for _, m := range p.mappings {
		if err := unix.Munmap(m); err != nil {
			result = fmt.Errorf("%v; munmap staged cache segment: %w", result, err)
		}
	}
	p.ready, p.extra, p.mappings = nil, nil, nil
	return result
}
