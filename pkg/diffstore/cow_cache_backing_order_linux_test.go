package diffstore

import (
	"slices"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestElasticGrowthSlotOrderAndRollback(t *testing.T) {
	if !ElasticCacheSupported() {
		t.Skip("elastic backing requires 4 KiB host pages")
	}
	e := new(elasticBacking)
	t.Cleanup(func() { _ = e.close() })
	if err := e.prepare(1); err != nil {
		t.Fatal(err)
	}
	old := e.take()
	old[0] = 37
	retired := make(map[*[cowBlockSize]byte]bool, len(e.retired))
	for _, page := range e.retired {
		retired[page] = true
	}
	count := len(retired) + 2*elasticSegmentBytes/cowBlockSize
	stage := func() *elasticPrepared {
		t.Helper()
		p, err := e.stagePrepare(count)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	p := stage()
	for i, page := range p.ready {
		if retired[page] != (i < p.reused) {
			t.Fatalf("staged slot %d crossed the rollback ownership boundary", i)
		}
	}
	if err := p.abort(); err != nil {
		t.Fatal(err)
	}
	if got := mappedResidentPages(t, e.mappings[0]); got != 1 {
		t.Fatalf("after aborted mixed growth the kernel reports %d resident pages, want 1", got)
	}
	if old[0] != 37 || e.resident != 1 {
		t.Fatal("aborted growth disturbed the active page")
	}
	p = stage()
	newSlots := len(p.ready) - p.reused
	e.installPrepared(p)
	var last uintptr
	for i := 0; i < count; i++ {
		addr := uintptr(unsafe.Pointer(e.take()))
		if i != 0 && i != newSlots && addr <= last {
			t.Fatalf("slots out of allocation order at %d", i)
		}
		last = addr
	}
}

func TestPrefaultCacheSlotsKeepsUnfundedGaps(t *testing.T) {
	if !ElasticCacheSupported() {
		t.Skip("elastic backing requires independently chargeable 4 KiB pages")
	}
	for _, mode := range []string{"kernel", "unsupported", "partially populated"} {
		t.Run(mode, func(t *testing.T) {
			m, err := mapElasticSegment()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = unix.Munmap(m) })
			var slots []*[cowBlockSize]byte
			for _, page := range []int{1, 2, 5, 6, 8} {
				slots = append(slots, (*[cowBlockSize]byte)(m[page*cowBlockSize:]))
			}
			var starts []*[cowBlockSize]byte
			prefaultCacheSlots(slots, func(first *[cowBlockSize]byte, count int) error {
				starts = append(starts, first)
				if count != 2 {
					t.Fatalf("unexpected advised range of %d pages", count)
				}
				switch mode {
				case "unsupported":
					return unix.EINVAL
				case "partially populated":
					first[0] = 1
					return unix.ENOMEM
				default:
					return populateCachePages(first, count)
				}
			})
			if !slices.Equal(starts, []*[cowBlockSize]byte{slots[0], slots[2]}) {
				t.Fatal("advised ranges did not match the two funded pairs")
			}
			if got := mappedResidentPages(t, m); got != len(slots) {
				t.Fatalf("kernel reports %d resident pages, want %d", got, len(slots))
			}
			for _, slot := range slots {
				if got := mappedResidentPages(t, slot[:]); got != 1 {
					t.Fatal("funded slot is not resident")
				}
			}
		})
	}
}
