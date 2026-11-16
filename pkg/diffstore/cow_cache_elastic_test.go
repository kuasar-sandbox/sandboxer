package diffstore

import (
	"bytes"
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Kernel mincore confirms the test process holds real resident physical pages.
// Logical quota alone would not pass this check.
func mappedResidentPages(t *testing.T, m []byte) int {
	t.Helper()
	if len(m) == 0 {
		return 0
	}
	vec := make([]byte, (len(m)+4095)/4096)
	_, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)), uintptr(unsafe.Pointer(&vec[0])))
	if errno != 0 {
		t.Fatalf("mincore: %v", errno)
	}
	n := 0
	for _, v := range vec {
		if v&1 != 0 {
			n++
		}
	}
	return n
}

func TestCOWCacheElasticPhysicalReturn(t *testing.T) {
	const mib = 1 << 20
	cache, err := NewElasticCache(16*mib, 8*mib, 8*mib)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	cache.mu.Lock()
	first := cache.backing.mappings[0]
	initial := mappedResidentPages(t, first)
	cache.mu.Unlock()
	if initial != len(first)/4096 {
		t.Fatalf("first segment not physically charged: %d", initial)
	}
	if ok, err := cache.TryResizeCapacity(16 * mib); err != nil || !ok {
		t.Fatalf("grow %v %v", ok, err)
	}
	cache.mu.Lock()
	var grown int
	for _, m := range cache.backing.mappings {
		grown += mappedResidentPages(t, m)
	}
	cache.mu.Unlock()
	if grown != 16*mib/4096 {
		t.Fatalf("actual charged pages after growth: %d", grown)
	}
	if ok, err := cache.TryResizeCapacity(8 * mib); err != nil || !ok {
		t.Fatalf("shrink %v %v", ok, err)
	}
	cache.mu.Lock()
	var shrunk int
	for _, m := range cache.backing.mappings {
		shrunk += mappedResidentPages(t, m)
	}
	cache.mu.Unlock()
	if shrunk != 8*mib/4096 {
		t.Fatalf("physical pages not returned: %d", shrunk)
	}
	if got := cache.Stats().ResidentPayload; got != 8*mib {
		t.Fatalf("resident budget %d", got)
	}
}

func TestCOWCacheAgeOnlyMarksAndHitRevives(t *testing.T) {
	cache, err := NewElasticCache(4*cowBlockSize, 2*cowBlockSize, 4*cowBlockSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	disk := cachedCOW(t, cache, 4, nil)
	for i := 0; i < 3; i++ {
		writePage(t, disk, i, byte(0x40+i))
	}
	if err := disk.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	if expired := cache.AgeClean(time.Millisecond); expired != 3*cowBlockSize {
		t.Fatalf("expired=%d", expired)
	}
	if got := cache.Stats(); got.Used != 3*cowBlockSize || got.ExpiredClean != 3*cowBlockSize {
		t.Fatalf("aging discarded pages: %+v", got)
	}
	checkRead(t, disk, 0, bytes.Repeat([]byte{0x40}, cowBlockSize))
	if got := cache.Stats(); got.ExpiredClean != 2*cowBlockSize {
		t.Fatalf("hot page remained expired: %+v", got)
	}
	if ok, err := cache.TryResizeCapacity(2 * cowBlockSize); err != nil || !ok {
		t.Fatalf("shrink %v %v", ok, err)
	}
	if got := cache.Stats(); got.Capacity != 2*cowBlockSize || got.Used != 2*cowBlockSize {
		t.Fatalf("bad shrink: %+v", got)
	}
	checkRead(t, disk, 0, bytes.Repeat([]byte{0x40}, cowBlockSize))
}

// Concurrent I/O must never observe an unmapped or unprepared page while
// physical capacity is growing or shrinking in the background.
func TestElasticCacheConcurrentResizeIO(t *testing.T) {
	const miB = 1 << 20
	cache, err := NewElasticCache(32*miB, 16*miB, 8*miB)
	if err != nil {
		t.Fatal(err)
	}
	disk := cachedCOW(t, cache, 16*1024, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for goroutine := 0; goroutine < 6; goroutine++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{byte(worker + 1)}, cowBlockSize)
			for i := 0; i < 400; i++ {
				offset := int64(((i*97 + worker*11) % 4096) * cowBlockSize)
				if _, err := disk.WriteAt(payload, offset); err != nil {
					errs <- err
					return
				}
				b := make([]byte, cowBlockSize)
				if _, err := disk.ReadAt(b, offset); err != nil {
					errs <- err
					return
				}
			}
		}(goroutine)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 120; i++ {
			target := uint64(8 * miB)
			if i%3 == 0 {
				target = 32 * miB
			}
			if i%3 == 1 {
				target = 16 * miB
			}
			if _, err := cache.TryResizeCapacity(target); err != nil {
				errs <- err
				return
			}
			runtime.Gosched()
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := disk.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
}

// Root and data writable layers share one elastic page budget, even when
// they use the same logical block numbers.
func TestElasticCacheMultipleDisksShareCapacity(t *testing.T) {
	const miB = uint64(1 << 20)
	c, err := NewElasticCache(16*miB, 8*miB, 8*miB)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	root := cachedCOW(t, c, 4096, nil)
	data := cachedCOW(t, c, 4096, nil)
	for i := 0; i < 256; i++ {
		writePage(t, root, i, 0x62)
		writePage(t, data, i, 0x91)
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := c.Stats(); s.Used != 2*256*cowBlockSize || s.Capacity != 8*miB {
		t.Fatalf("disks did not share one budget: %+v", s)
	}
	ok, err := c.TryResizeCapacity(16 * miB)
	if err != nil || !ok {
		t.Fatalf("growth: %v %v", ok, err)
	}
	checkRead(t, root, 0, bytes.Repeat([]byte{0x62}, cowBlockSize))
	checkRead(t, data, 0, bytes.Repeat([]byte{0x91}, cowBlockSize))
	if ok, err := c.TryResizeCapacity(8 * miB); err != nil || !ok {
		t.Fatalf("clean shrink: %v %v", ok, err)
	}
	checkRead(t, root, 12, bytes.Repeat([]byte{0x62}, cowBlockSize))
	checkRead(t, data, 12, bytes.Repeat([]byte{0x91}, cowBlockSize))
}
