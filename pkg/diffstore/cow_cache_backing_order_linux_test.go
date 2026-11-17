package diffstore

import (
	"testing"
	"unsafe"
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
