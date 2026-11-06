package diffstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/versioned"
	"golang.org/x/sys/unix"
)

// The backend's own tests inject real reservation failures and prove that only
// a fully joined/rolled-back pre-publication failure gets this type. These
// frontend fixtures use the same boundary while committing real Data/Zero
// mappings, so their ordering does not depend on host free space or GC timing.
type cachePressureBackend struct {
	diffBackend
	write func(context.Context, []pageWrite) error
}

func (d *cachePressureBackend) writePages(ctx context.Context, pages []pageWrite) error {
	return d.write(ctx, pages)
}

type cachePressureReadGate struct {
	diffBackend
	gate *workerGate
}

func (d *cachePressureReadGate) withDirectRead(length int, off int64, consume func([]byte) error) error {
	d.gate.wait()
	return d.diffBackend.withDirectRead(length, off, consume)
}

func cachePressureError() error { return &versioned.SpacePressureError{Err: unix.ENOSPC} }

func backendHasPayload(ctx context.Context, d diffBackend, block uint64) (bool, error) {
	var have [1]bool
	err := d.payloadPresence(ctx, block, have[:])
	return have[0], err
}

func TestCheckpointCachedZeroPressureDrainsEntireAcceptedCohort(t *testing.T) {
	const dataPages = maxWritebackPages
	const zeroPages = 2*maxWritebackMutations + 7
	const total = dataPages + zeroPages
	selectGate := newWorkerGate()
	defer selectGate.open()
	var pause atomic.Bool
	cache := testCache(t, total, total, cacheHooks{beforeSelect: func() {
		if pause.Load() {
			selectGate.wait()
		}
	}})
	cow := cachedCOW(t, cache, total, nil, WithCheckpoint("zero-pressure-tail", ""))
	writePage(t, cow, total-1, 0x79)
	if err := cow.WaitMaterialized(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := cow.diff
	var dataAttempts, zeroBatches, zeroCount atomic.Int32
	cow.diff = &cachePressureBackend{diffBackend: base, write: func(ctx context.Context, pages []pageWrite) error {
		if pages[0].Zero {
			zeroBatches.Add(1)
			zeroCount.Add(int32(len(pages)))
			return base.writePages(ctx, pages)
		}
		attempt := dataAttempts.Add(1)
		if len(pages) != dataPages {
			return fmt.Errorf("Data batch size %d, want %d", len(pages), dataPages)
		}
		if attempt == 1 {
			// A real first transaction may commit before the next reservation
			// fails. The retry must keep and replay the entire frozen buffer set.
			if err := base.writePages(ctx, pages[:64]); err != nil {
				return err
			}
			return cachePressureError()
		}
		have, err := backendHasPayload(ctx, base, total-1)
		if err != nil {
			return err
		}
		if have {
			return cachePressureError()
		}
		return base.writePages(ctx, pages)
	}}
	before := cache.Stats().WrittenBytes
	pause.Store(true)
	// Old Hole-Zero reservations become the oldest Data without moving FIFO
	// age. The sole reclaimable Data sits after more than two Zero windows.
	if err := cow.Discard(0, dataPages*cowBlockSize); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, selectGate.entered)
	if err := cow.Discard(dataPages*cowBlockSize, zeroPages*cowBlockSize); err != nil {
		t.Fatal(err)
	}
	if _, err := cow.WriteAt(bytes.Repeat([]byte{0x53}, dataPages*cowBlockSize), 0); err != nil {
		t.Fatal(err)
	}
	checkBudget(t, cache, total, total, 0)
	selectGate.open()
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal("accepted tail Zero could not repair Data reservation pressure", err)
	}
	if dataAttempts.Load() != 2 || zeroBatches.Load() != 3 || zeroCount.Load() != zeroPages {
		t.Fatalf("Data attempts=%d Zero batches/pages=%d/%d", dataAttempts.Load(), zeroBatches.Load(), zeroCount.Load())
	}
	if cache.Stats().WrittenBytes-before != dataPages*cowBlockSize {
		t.Fatal("replayed Data prefix or Zero was counted as extra logical payload")
	}
	want := make([]byte, total*cowBlockSize)
	copy(want, bytes.Repeat([]byte{0x53}, dataPages*cowBlockSize))
	checkRead(t, cow, 0, want)
	payload := make([]bool, total)
	for start := 0; start < len(payload); start += versioned.MaxPresencePages {
		end := min(start+versioned.MaxPresencePages, len(payload))
		if err := base.payloadPresence(context.Background(), uint64(start), payload[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	for block, have := range payload {
		if have != (block < dataPages) {
			t.Fatalf("page %d payload=%t after finite pressure repair", block, have)
		}
	}
}

func TestCheckpointCachedZeroFullDataQuotaUsesSynchronousAdmission(t *testing.T) {
	for _, test := range []struct {
		name                string
		capacity, dirty     int
		otherOwner, loading bool
	}{{"cold", 1, 1, false, false}, {"clean", 2, 1, false, false}, {"other-owner-zero", 3, 2, true, false}, {"total-quota-loading", 2, 2, false, true}} {
		t.Run(test.name, func(t *testing.T) {
			gate, readGate := newWorkerGate(), newWorkerGate()
			defer gate.open()
			defer readGate.open()
			var pause atomic.Bool
			cache := testCache(t, test.capacity, test.dirty, cacheHooks{beforeSelect: func() {
				if pause.Load() {
					gate.wait()
				}
			}})
			cow := cachedCOW(t, cache, 3, nil, WithCheckpoint("zero-admission", ""))
			writePage(t, cow, 1, 0x69)
			if test.loading {
				writePage(t, cow, 2, 0x73)
			}
			if err := cow.WaitMaterialized(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := cow.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			if test.loading {
				cache.mu.Lock()
				for _, p := range cache.pages {
					cache.releaseLocked(p)
				}
				cache.mu.Unlock()
			}
			var other *Disk
			if test.otherOwner {
				other = cachedCOW(t, cache, 1, nil, WithCheckpoint("zero-other-admission", ""))
				if err := other.WaitMaterialized(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			base := cow.diff
			var attempts atomic.Int32
			cow.diff = &cachePressureBackend{diffBackend: base, write: func(ctx context.Context, pages []pageWrite) error {
				attempts.Add(1)
				have, err := backendHasPayload(ctx, base, 1)
				if err != nil {
					return err
				}
				if have {
					return cachePressureError()
				}
				return base.writePages(ctx, pages)
			}}
			if test.loading {
				cow.diff = &cachePressureReadGate{diffBackend: cow.diff, gate: readGate}
			}
			pause.Store(true)
			writePage(t, cow, 0, 0xa1)
			awaitSignal(t, gate.entered)
			readDone := make(chan error, 1)
			if test.loading {
				go func() {
					buf := make([]byte, cowBlockSize)
					_, err := cow.ReadAt(buf, 2*cowBlockSize)
					if err == nil && !bytes.Equal(buf, bytes.Repeat([]byte{0x73}, cowBlockSize)) {
						err = errors.New("Loading page changed during unrelated synchronous Zero admission")
					}
					readDone <- err
				}()
				awaitSignal(t, readGate.entered)
			}
			wantDirty := 1
			if other != nil {
				if err := other.Discard(0, cowBlockSize); err != nil {
					t.Fatal(err)
				}
				wantDirty++
			}
			if s := cache.Stats(); s.DirtyUsed != uint64(wantDirty*cowBlockSize) || (test.loading && s.Loading != cowBlockSize) {
				t.Fatal("fixture did not exhaust the intended cache quota")
			}
			completed := make(chan error, 1)
			go func() { completed <- cow.Discard(cowBlockSize, cowBlockSize) }()
			if err := awaitError(t, completed); err != nil {
				t.Fatal("full Data quota prevented synchronous Zero admission", err)
			}
			have, err := backendHasPayload(context.Background(), base, 1)
			if err != nil || have || attempts.Load() != 0 {
				t.Fatalf("admission did not release payload before queued Data: have=%t attempts=%d err=%v", have, attempts.Load(), err)
			}
			if s := cache.Stats(); s.DirtyUsed != uint64(wantDirty*cowBlockSize) || s.PeakDirty > s.MaxDirty || s.PeakUsed > s.Capacity {
				t.Fatalf("synchronous admission borrowed extra quota: %+v", s)
			}
			readGate.open()
			if test.loading {
				if err := awaitError(t, readDone); err != nil {
					t.Fatal(err)
				}
			}
			gate.open()
			if err := cache.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			if attempts.Load() != 1 {
				t.Fatalf("Data attempts=%d after synchronous admission", attempts.Load())
			}
			checkRead(t, cow, 0, bytes.Repeat([]byte{0xa1}, cowBlockSize))
			checkRead(t, cow, 1, make([]byte, cowBlockSize))
		})
	}
}

func TestCheckpointCachedZeroPressureRepairIsFiniteAndOwnerScoped(t *testing.T) {
	selectGate, repairGate := newWorkerGate(), newWorkerGate()
	defer selectGate.open()
	defer repairGate.open()
	var pause atomic.Bool
	var owner *Disk
	cache := testCache(t, 8, 8, cacheHooks{
		beforeSelect: func() {
			if pause.Load() {
				selectGate.wait()
			}
		},
		beforeWrite: func(pages []*cachePage) error {
			if pause.Load() && pages[0].key.cow == owner && pages[0].zero {
				repairGate.wait()
			}
			return nil
		},
	})
	cow := cachedCOW(t, cache, 3, nil, WithCheckpoint("zero-finite-owner", ""))
	other := cachedCOW(t, cache, 1, nil, WithCheckpoint("zero-finite-other", ""))
	owner = cow
	writePage(t, cow, 1, 0x61)
	writePage(t, cow, 2, 0x62)
	writePage(t, other, 0, 0x63)
	for _, disk := range []*Disk{cow, other} {
		if err := disk.WaitMaterialized(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := cache.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := cow.diff
	var attempts atomic.Int32
	cow.diff = &cachePressureBackend{diffBackend: base, write: func(ctx context.Context, pages []pageWrite) error {
		if pages[0].Zero {
			return base.writePages(ctx, pages)
		}
		if attempts.Add(1) == 1 {
			return cachePressureError()
		}
		// Neither another owner's queued Zero nor a newly admitted Zero may
		// extend this repair before the original Data gets its one retry.
		for _, target := range []struct {
			d     diffBackend
			block uint64
		}{{base, 2}, {other.diff, 0}} {
			have, err := backendHasPayload(ctx, target.d, target.block)
			if err != nil {
				return err
			}
			if !have {
				return errors.New("pressure repair expanded beyond its captured owner/cohort")
			}
		}
		return base.writePages(ctx, pages)
	}}
	pause.Store(true)
	writePage(t, cow, 0, 0xa4)
	awaitSignal(t, selectGate.entered)
	if err := cow.Discard(cowBlockSize, cowBlockSize); err != nil {
		t.Fatal(err)
	}
	if err := other.Discard(0, cowBlockSize); err != nil {
		t.Fatal(err)
	}
	selectGate.open()
	awaitSignal(t, repairGate.entered)
	if err := cow.Discard(2*cowBlockSize, cowBlockSize); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	bounded := cache.active == cow && cache.pages[cacheKey{cow, 0}].state == cacheWriteback &&
		cache.pages[cacheKey{cow, 1}].state == cacheWriteback && cache.pages[cacheKey{cow, 2}].state == cacheDirty &&
		cache.pages[cacheKey{other, 0}].state == cacheDirty
	cache.mu.Unlock()
	if !bounded {
		t.Fatal("repair did not freeze exactly the accepted same-owner cohort")
	}
	checkRead(t, cow, 0, bytes.Repeat([]byte{0xa4}, cowBlockSize))
	repairGate.open()
	if err := cache.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("Data retries were not bounded: %d", attempts.Load())
	}
	checkRead(t, cow, 1, make([]byte, 2*cowBlockSize))
	checkRead(t, other, 0, make([]byte, cowBlockSize))
}

func TestCheckpointCachedZeroPressureRepairCloseAndFatal(t *testing.T) {
	for _, failure := range []string{"none", "zero", "retry", "shared-cache"} {
		t.Run(failure, func(t *testing.T) {
			selectGate, repairGate := newWorkerGate(), newWorkerGate()
			defer selectGate.open()
			defer repairGate.open()
			var pause atomic.Bool
			cache := testCache(t, 4, 4, cacheHooks{
				beforeSelect: func() {
					if pause.Load() {
						selectGate.wait()
					}
				},
				beforeWrite: func(pages []*cachePage) error {
					if pause.Load() && pages[0].zero {
						repairGate.wait()
					}
					return nil
				},
			})
			cow := cachedCOW(t, cache, 3, nil, WithCheckpoint("zero-repair-close", ""))
			if err := cow.WaitMaterialized(context.Background()); err != nil {
				t.Fatal(err)
			}
			base := cow.diff
			var attempts, zeroCalls atomic.Int32
			cow.diff = &cachePressureBackend{diffBackend: base, write: func(ctx context.Context, pages []pageWrite) error {
				if pages[0].Zero {
					zeroCalls.Add(1)
					if failure == "zero" {
						return io.ErrUnexpectedEOF
					}
					return base.writePages(ctx, pages)
				}
				if attempts.Add(1) == 1 || failure == "retry" {
					return cachePressureError()
				}
				return base.writePages(ctx, pages)
			}}
			reported := make(chan error, 4)
			cow.SetFatalHandler(func(err error) { reported <- err })
			pause.Store(true)
			writePage(t, cow, 0, 0x91)
			awaitSignal(t, selectGate.entered)
			if err := cow.Discard(cowBlockSize, 2*cowBlockSize); err != nil {
				t.Fatal(err)
			}
			selectGate.open()
			awaitSignal(t, repairGate.entered)
			closed := make(chan error, 1)
			go func() { closed <- cow.Close() }()
			awaitSignal(t, cow.lifeCtx.Done())
			if failure == "shared-cache" {
				cache.mu.Lock()
				report := cache.failLocked(io.ErrUnexpectedEOF)
				cache.mu.Unlock()
				if report != nil {
					report(io.ErrUnexpectedEOF)
				}
			}
			select {
			case err := <-closed:
				t.Fatalf("Close released active repair buffers before I/O joined: %v", err)
			default:
			}
			checkBudget(t, cache, 3, 3, 3)
			cache.mu.Lock()
			active := cache.active == cow
			cache.mu.Unlock()
			if !active {
				t.Fatal("repair dropped active owner before Data retry joined")
			}
			repairGate.open()
			err := awaitError(t, closed)
			if failure == "none" {
				if err != nil || attempts.Load() != 2 {
					t.Fatalf("Close did not finish healthy repair: %v attempts=%d", err, attempts.Load())
				}
			} else {
				cause := error(io.ErrUnexpectedEOF)
				if failure == "retry" {
					cause = unix.ENOSPC
				}
				if !errors.Is(err, cause) || !errors.Is(awaitError(t, reported), cause) {
					t.Fatalf("repair failure was not sticky/reported: %v", err)
				}
				if failure == "shared-cache" && (zeroCalls.Load() != 0 || attempts.Load() != 1) {
					t.Fatal("shared fatal allowed more Zero I/O or a Data retry")
				}
				if failure == "zero" && attempts.Load() != 1 {
					t.Fatal("failed Zero repair retried Data")
				}
				if failure == "retry" && attempts.Load() != 2 {
					t.Fatal("space repair retried more than once")
				}
				select {
				case extra := <-reported:
					t.Fatalf("duplicate fatal callback: %v", extra)
				default:
				}
			}
			if s := cache.Stats(); s.Used != 0 || s.DirtyUsed != 0 || s.Writeback != 0 {
				t.Fatalf("Close stranded repair quota: %+v", s)
			}
		})
	}
}
