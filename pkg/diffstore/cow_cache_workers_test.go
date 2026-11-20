package diffstore

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

func TestCOWCacheIndependentDiskWriteback(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	var slow atomic.Pointer[Disk]
	c := testCache(t, 6, 6, cacheHooks{beforeWrite: func(p []*cachePage) error {
		if p[0].key.cow == slow.Load() {
			gate.wait()
		}
		return nil
	}})
	a := cachedCOW(t, c, 4, nil)
	slow.Store(a)
	b := cachedCOW(t, c, 4, nil)
	writePage(t, a, 0, 1)
	awaitSignal(t, gate.entered)
	writePage(t, b, 0, 2)
	finished := make(chan error, 1)
	go func() { finished <- b.Drain(context.Background()) }()
	if err := awaitError(t, finished); err != nil {
		t.Fatal(err)
	}
	// The other owner finishes while A's actual writeback gate is still held.
	c.mu.Lock()
	aPending, bPending := c.clients[a], c.clients[b]
	c.mu.Unlock()
	if aPending != 1 || bPending != 0 {
		t.Fatalf("dirty after B drain: A=%d B=%d", aPending, bPending)
	}
	gate.open()
	if err := c.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkDirtyCharges(t, c)
}

func TestCOWCacheConcurrentOwnersKeepQueuesAndAccounting(t *testing.T) {
	const owners, blocks, writes = 4, 12, 48
	c := testCacheWithLimits(t, 8, 6, 2, cacheHooks{})
	disks := make([]*Disk, owners)
	want := make([][]byte, owners)
	for i := range disks {
		disks[i] = cachedCOW(t, c, blocks, nil, WithCheckpoint(fmt.Sprintf("concurrent-%d", i), ""))
		if err := disks[i].WaitMaterialized(context.Background()); err != nil {
			t.Fatal(err)
		}
		want[i] = make([]byte, blocks*cowBlockSize)
	}
	finished := make(chan error, owners)
	for i, disk := range disks {
		go func() {
			for step := range writes {
				block := (step * 7) % blocks
				offset := block * cowBlockSize
				if step%5 == 0 {
					if err := disk.Discard(int64(offset), cowBlockSize); err != nil {
						finished <- err
						return
					}
					clear(want[i][offset : offset+cowBlockSize])
					continue
				}
				payload := bytes.Repeat([]byte{byte(i*writes + step + 1)}, 512)
				if n, err := disk.WriteAt(payload, int64(offset+17)); err != nil || n != len(payload) {
					finished <- fmt.Errorf("disk %d write %d: %d bytes, %v", i, step, n, err)
					return
				}
				copy(want[i][offset+17:], payload)
			}
			finished <- disk.Drain(context.Background())
		}()
	}
	for range owners {
		if err := awaitError(t, finished); err != nil {
			t.Fatal(err)
		}
	}
	for i, disk := range disks {
		checkRead(t, disk, 0, want[i])
	}
	checkDirtyCharges(t, c)
	if got := c.Stats().DirtyUsed; got != 0 {
		t.Fatalf("dirty after owner drains: %d", got)
	}
}

func TestCOWCacheCanAttachAfterLastWorkerDetaches(t *testing.T) {
	c := testCache(t, 2, 0, cacheHooks{})
	for range 2 {
		disk := cachedCOW(t, c, 2, nil)
		writePage(t, disk, 0, 0xa5)
		if err := disk.Close(); err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		workers, running, clients, pages := len(c.workers), c.workersRunning, len(c.clients), len(c.pages)
		c.mu.Unlock()
		if workers != 0 || running != 0 || clients != 0 || pages != 0 {
			t.Fatalf("detach retained resources: workers=%d running=%d clients=%d pages=%d", workers, running, clients, pages)
		}
		select {
		case <-c.done:
			t.Fatal("healthy empty cache ended before Close")
		default:
		}
	}
}
