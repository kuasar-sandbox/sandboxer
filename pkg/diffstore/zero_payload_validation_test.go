package diffstore

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// These tests assert the optional-payload contract, not a timing threshold.
// They intentionally fail on the frozen inline-payload version and can be used
// as a negative control there. The shared memory harness remains layout-neutral.

// Inventory helpers live with these regressions so this file does not depend
// on the experimental memory or benchmark harness. The caller holds cache.mu.
type zeroPayloadInventory struct {
	PageStructBytes              uint64 `json:"page_struct_bytes"`
	PayloadRepresentation        string `json:"payload_representation"`
	CapacityPages                int    `json:"capacity_pages"`
	DirtyLimitPages              int    `json:"dirty_limit_pages"`
	ActiveMetadataPages          int    `json:"active_metadata_pages"`
	FreeMetadataPages            int    `json:"free_metadata_pages"`
	ActiveZeroPages              int    `json:"active_zero_pages"`
	ActiveZeroWithPayloadPages   int    `json:"active_zero_with_payload_pages"`
	ActiveOtherWithPayloadPages  int    `json:"active_other_with_payload_pages"`
	FreeMetadataWithPayloadPages int    `json:"free_metadata_with_payload_pages"`
	FreeSeparatePayloadPages     int    `json:"free_separate_payload_pages"`
	HeldPayloadPages             int    `json:"held_payload_pages"`
	HeldPayloadBytes             uint64 `json:"held_payload_bytes"`
}

func zeroPayloadPageHasData(p *cachePage) bool {
	v := reflect.ValueOf(p).Elem().FieldByName("data")
	switch v.Kind() {
	case reflect.Array:
		return v.Len() != 0
	case reflect.Pointer:
		return !v.IsNil()
	default:
		panic("unexpected cachePage.data representation")
	}
}

// Caller holds cache.mu. Both layouts use the same cachePage/map/list protocol.
func zeroPayloadInventoryLocked(c *Cache) zeroPayloadInventory {
	typ := reflect.TypeOf(cachePage{})
	field, _ := typ.FieldByName("data")
	i := zeroPayloadInventory{
		PageStructBytes: uint64(typ.Size()), PayloadRepresentation: field.Type.String(),
		CapacityPages: c.capacity, DirtyLimitPages: c.maxDirty,
		ActiveMetadataPages: len(c.pages), FreeMetadataPages: len(c.free),
	}
	for _, p := range c.pages {
		if p.zero {
			i.ActiveZeroPages++
			if zeroPayloadPageHasData(p) {
				i.ActiveZeroWithPayloadPages++
			}
		} else if zeroPayloadPageHasData(p) {
			i.ActiveOtherWithPayloadPages++
		}
	}
	for _, p := range c.free {
		if zeroPayloadPageHasData(p) {
			i.FreeMetadataWithPayloadPages++
		}
	}
	// The baseline has no separate pool. The candidate's freeData holds the
	// same independently owned arrays that are attached to Data cache entries.
	pool := reflect.ValueOf(c).Elem().FieldByName("freeData")
	if pool.IsValid() {
		i.FreeSeparatePayloadPages = pool.Len()
	}
	i.HeldPayloadPages = i.ActiveZeroWithPayloadPages + i.ActiveOtherWithPayloadPages +
		i.FreeMetadataWithPayloadPages + i.FreeSeparatePayloadPages
	i.HeldPayloadBytes = uint64(i.HeldPayloadPages) * cowBlockSize
	return i
}

func zeroPayloadAssertPools(t *testing.T, c *Cache) zeroPayloadInventory {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	i := zeroPayloadInventoryLocked(c)
	if i.ActiveMetadataPages+i.FreeMetadataPages > c.capacity {
		t.Fatalf("unbounded metadata pool: %+v", i)
	}
	if i.HeldPayloadPages > c.capacity {
		t.Fatalf("unbounded payload pool: %+v", i)
	}
	if i.ActiveZeroWithPayloadPages != 0 || i.FreeMetadataWithPayloadPages != 0 {
		t.Fatalf("Zero/free metadata still owns a payload: %+v", i)
	}
	owners := make(map[uintptr]string)
	for key, p := range c.pages {
		v := reflect.ValueOf(p).Elem().FieldByName("data")
		if v.Kind() != reflect.Pointer {
			t.Fatal("optional-payload contract still has an inline array")
		}
		if v.IsNil() {
			continue
		}
		owner := fmt.Sprintf("active:%p:%d", key.cow, key.block)
		ptr := v.Pointer()
		if prev, exists := owners[ptr]; exists {
			t.Fatalf("payload shared by %s and %s", prev, owner)
		}
		owners[ptr] = owner
	}
	pool := reflect.ValueOf(c).Elem().FieldByName("freeData")
	if !pool.IsValid() {
		t.Fatal("separate payload pool is absent")
	}
	for n := 0; n < pool.Len(); n++ {
		v := pool.Index(n)
		if v.IsNil() {
			t.Fatal("nil payload in the live free pool")
		}
		ptr := v.Pointer()
		if prev, exists := owners[ptr]; exists {
			t.Fatalf("free payload %d still owned by %s", n, prev)
		}
		owners[ptr] = fmt.Sprintf("free:%d", n)
		array := v.Elem()
		for byteIndex := 0; byteIndex < array.Len(); byteIndex++ {
			if array.Index(byteIndex).Uint() != 0 {
				t.Fatalf("free payload %d retained plaintext at %d", n, byteIndex)
			}
		}
	}
	return i
}

type zeroPayloadProtocolBackend struct {
	diffBackend
	zeroPages atomic.Int64
}

func (d *zeroPayloadProtocolBackend) writePages(ctx context.Context, pages []pageWrite) error {
	for _, p := range pages {
		if p.Zero {
			if p.Data != nil {
				return fmt.Errorf("Zero page %d supplied a %d-byte payload", p.Block, len(p.Data))
			}
			d.zeroPages.Add(1)
		} else if len(p.Data) != cowBlockSize {
			return fmt.Errorf("Data page %d lacks a complete payload", p.Block)
		}
	}
	return d.diffBackend.writePages(ctx, pages)
}

func TestZeroPayloadColdDiscardReadExportAndClose(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			const pages = 17
			gate := newWorkerGate()
			defer gate.open()
			cache := testCache(t, 2*pages, pages, cacheHooks{beforeWrite: func([]*cachePage) error {
				gate.wait()
				return nil
			}})
			opts := []DiskOption{WithCheckpoint("zero-payload-cold", "base")}
			if encrypted {
				opts = append(opts, WithDiffEncryption(testDiffKey(61), true))
			}
			base := &fakeReader{data: bytes.Repeat([]byte{0x6d}, 2*pages*cowBlockSize)}
			cow := cachedCOW(t, cache, 2*pages, base, opts...)
			if err := cow.WaitMaterialized(context.Background()); err != nil {
				t.Fatal(err)
			}
			protocol := &zeroPayloadProtocolBackend{diffBackend: cow.diff}
			cow.diff = protocol
			if err := cow.Discard(0, pages*cowBlockSize); err != nil {
				t.Fatal(err)
			}
			awaitSignal(t, gate.entered)
			i := zeroPayloadAssertPools(t, cache)
			if i.ActiveZeroPages != pages || i.HeldPayloadPages != 0 {
				t.Fatalf("cold Zero materialized data arrays: %+v", i)
			}
			if protocol.zeroPages.Load() != 0 {
				t.Fatal("blocked writeback reached the physical backend")
			}
			checkRead(t, cow, 0, make([]byte, pages*cowBlockSize))
			checkRead(t, cow, pages, base.data[pages*cowBlockSize:])
			kinds := make([]sparse.RunKind, 2*pages)
			for n := range kinds {
				kinds[n] = sparse.Hole
				if n < pages {
					kinds[n] = sparse.Zero
				}
			}
			requireActiveKinds(t, cow, kinds)
			gate.open()
			if err := cow.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			if protocol.zeroPages.Load() != pages || cache.Stats().WrittenBytes != 0 {
				t.Fatalf("Zero protocol count=%d written=%d", protocol.zeroPages.Load(), cache.Stats().WrittenBytes)
			}
			i = zeroPayloadAssertPools(t, cache)
			if i.ActiveZeroPages != 0 || i.HeldPayloadPages != 0 || i.FreeMetadataPages != pages {
				t.Fatalf("completed cold Zero retained payload: %+v", i)
			}
			if err := cow.Close(); err != nil {
				t.Fatal(err)
			}
			if err := cache.Close(); err != nil {
				t.Fatal(err)
			}
			i = zeroPayloadAssertPools(t, cache)
			if i.FreeMetadataPages != 0 || i.FreeSeparatePayloadPages != 0 {
				t.Fatalf("Close retained buffers or metadata: %+v", i)
			}
		})
	}
}

func TestZeroPayloadReusePartialWritesAndSharedPoolBound(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			gate := newWorkerGate()
			defer gate.open()
			var pause atomic.Bool
			cache := testCache(t, 4, 4, cacheHooks{beforeSelect: func() {
				if pause.Load() {
					gate.wait()
				}
			}})
			options := []DiskOption{}
			if encrypted {
				options = append(options, WithDiffEncryption(testDiffKey(62), true))
			}
			optsA := append([]DiskOption{WithCheckpoint("zero-payload-reuse", "base")}, options...)
			a := cachedCOW(t, cache, 8, &fakeReader{data: bytes.Repeat([]byte{0x6d}, 8*cowBlockSize)}, optsA...)
			b := cachedCOW(t, cache, 8, nil, options...) // same cache, single-layer owner
			if err := a.WaitMaterialized(context.Background()); err != nil {
				t.Fatal(err)
			}
			pause.Store(true)
			writePage(t, a, 0, 0xa7)
			awaitSignal(t, gate.entered)
			cache.mu.Lock()
			oldPlaintext := cache.pages[cacheKey{a, 0}].data[:]
			cache.mu.Unlock()
			if err := a.Discard(0, cowBlockSize); err != nil {
				t.Fatal(err)
			}
			if !allZero(oldPlaintext) {
				t.Fatal("Data-to-Zero returned an uncleared plaintext buffer")
			}
			if err := a.Discard(cowBlockSize, cowBlockSize); err != nil {
				t.Fatal(err)
			}
			// The partial write must safely consume a buffer that previously held
			// 0xa7. No old Data or nonzero parent byte may fill its untouched tail.
			if _, err := a.WriteAt([]byte{0x31}, cowBlockSize+37); err != nil {
				t.Fatal(err)
			}
			wantPartial := make([]byte, cowBlockSize)
			wantPartial[37] = 0x31
			checkRead(t, a, 1, wantPartial)
			checkRead(t, a, 0, make([]byte, cowBlockSize))
			writePage(t, b, 0, 0x6e)
			writePage(t, b, 1, 0) // all-zero Data must still own a Data payload
			for round := 0; round < 32; round++ {
				if err := a.Discard(0, 2*cowBlockSize); err != nil {
					t.Fatal(err)
				}
				value := byte(round + 1)
				if _, err := a.WriteAt([]byte{value}, 13); err != nil {
					t.Fatal(err)
				}
				writePage(t, a, 1, byte(0x80+round)) // complete Zero-to-Data write
				if _, err := b.WriteAt([]byte{value}, 17); err != nil {
					t.Fatal(err)
				}
				want := make([]byte, cowBlockSize)
				want[13] = value
				checkRead(t, a, 0, want)
				checkRead(t, a, 1, bytes.Repeat([]byte{byte(0x80 + round)}, cowBlockSize))
				wantOther := bytes.Repeat([]byte{0x6e}, cowBlockSize)
				wantOther[17] = value
				checkRead(t, b, 0, wantOther)
				zeroPayloadAssertPools(t, cache)
			}
			requireActiveKinds(t, a, []sparse.RunKind{sparse.Data, sparse.Data})
			cache.mu.Lock()
			ordinaryZero := cache.pages[cacheKey{b, 1}]
			ordinaryIsData := ordinaryZero != nil && !ordinaryZero.zero && zeroPayloadPageHasData(ordinaryZero)
			cache.mu.Unlock()
			if !ordinaryIsData {
				t.Fatal("ordinary all-zero write was reclassified as compressed Zero")
			}
			pause.Store(false)
			gate.open()
			if err := cache.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Exercise new keys and eviction after the deterministic in-place
			// transitions. Both metadata and payload pools must stay bounded.
			for block := 2; block < 8; block++ {
				writePage(t, a, block, byte(0x40+block))
				writePage(t, b, block, byte(0x70+block))
				if err := cache.Drain(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := a.Discard(int64(block*cowBlockSize), cowBlockSize); err != nil {
					t.Fatal(err)
				}
				if err := cache.Drain(context.Background()); err != nil {
					t.Fatal(err)
				}
				zeroPayloadAssertPools(t, cache)
				checkRead(t, b, block, bytes.Repeat([]byte{byte(0x70 + block)}, cowBlockSize))
			}
			wantOther := bytes.Repeat([]byte{0x6e}, cowBlockSize)
			wantOther[17] = 32
			checkRead(t, b, 0, wantOther) // cold reload of an older, unrelated Data page
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			i := zeroPayloadAssertPools(t, cache)
			if i.ActiveMetadataPages != 0 || i.FreeSeparatePayloadPages == 0 {
				t.Fatalf("warm cache did not retain its bounded reusable payloads: %+v", i)
			}
			if err := cache.Close(); err != nil {
				t.Fatal(err)
			}
			i = zeroPayloadAssertPools(t, cache)
			if i.HeldPayloadPages != 0 || i.FreeMetadataPages != 0 {
				t.Fatalf("Close did not release both pools: %+v", i)
			}
		})
	}
}

func TestZeroPayloadFrozenDataNotRecycledAcrossOwners(t *testing.T) {
	gate := newWorkerGate()
	defer gate.open()
	waiting := make(chan struct{}, 16)
	cache := testCache(t, 2, 2, cacheHooks{waiting: func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}})
	a := cachedCOW(t, cache, 2, nil, WithCheckpoint("zero-payload-frozen", ""), WithDiffEncryption(testDiffKey(63), true))
	b := cachedCOW(t, cache, 2, nil, WithDiffEncryption(testDiffKey(64), true))
	if err := a.WaitMaterialized(context.Background()); err != nil {
		t.Fatal(err)
	}
	backend := a.diff
	borrowed := make(chan []byte, 1)
	var first atomic.Bool
	a.diff = &cachePressureBackend{diffBackend: backend, write: func(ctx context.Context, pages []pageWrite) error {
		if !pages[0].Zero && first.CompareAndSwap(false, true) {
			borrowed <- pages[0].Data
			gate.wait()
			if !bytes.Equal(pages[0].Data, bytes.Repeat([]byte{0xa9}, cowBlockSize)) {
				return fmt.Errorf("frozen Data buffer changed before physical write completed")
			}
		}
		for _, p := range pages {
			if p.Zero && p.Data != nil {
				return fmt.Errorf("frozen Zero supplied a payload")
			}
		}
		return backend.writePages(ctx, pages)
	}}
	writePage(t, a, 0, 0xa9)
	awaitSignal(t, gate.entered)
	buf := <-borrowed
	discarded := make(chan error, 1)
	go func() { discarded <- a.Discard(0, cowBlockSize) }()
	awaitSignal(t, waiting)
	writePage(t, b, 0, 0x52)
	zeroPayloadAssertPools(t, cache)
	if !bytes.Equal(buf, bytes.Repeat([]byte{0xa9}, cowBlockSize)) {
		t.Fatal("another owner reused the still-frozen Data buffer")
	}
	gate.open()
	if err := awaitError(t, discarded); err != nil {
		t.Fatal(err)
	}
	if err := cache.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !allZero(buf) {
		t.Fatal("completed Data-to-Zero retained former plaintext")
	}
	zeroPayloadAssertPools(t, cache)
	checkRead(t, a, 0, make([]byte, cowBlockSize))
	checkRead(t, b, 0, bytes.Repeat([]byte{0x52}, cowBlockSize))
}
