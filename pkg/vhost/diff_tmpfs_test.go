package vhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

func tmpfsTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/dev/shm", "diff-cow-238-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != unix.TMPFS_MAGIC {
		t.Fatalf("fixture fd is not tmpfs: 0x%x", fs.Type)
	}
	return dir
}

func checkTmpfsActiveIO(t *testing.T, d *diffFile) {
	t.Helper()
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(d.f.Fd()), &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != unix.TMPFS_MAGIC {
		t.Fatalf("active diff fd is not tmpfs: 0x%x", fs.Type)
	}
	flags, err := unix.FcntlInt(d.f.Fd(), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_DIRECT == 0 {
		t.Fatalf("tmpfs must use the common O_DIRECT API: flags=%x err=%v", flags, err)
	}
	if d.direct == nil || len(d.direct.read.bytes) != maxDiffScratchSize || len(d.direct.write.bytes) != maxDiffScratchSize {
		t.Fatal("missing bounded owned I/O workspace")
	}
}

func TestDiffTmpfsActiveCreateReopen(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			path := filepath.Join(tmpfsTestDir(t), "active.diff")
			cache := testCache(t, 2, 1, cacheHooks{})
			options := []BlockCOWOption{WithCOWCache(cache)}
			if encrypted {
				options = append(options, WithDiffEncryption(testDiffKey(8), true))
			}
			base := patternedBytes(8 * cowBlockSize)
			cow, err := OpenBlockCOW(path, &fakeReader{data: base}, DiffInit{CreateSize: int64(len(base))}, options...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cow.Close() })
			checkTmpfsActiveIO(t, cow.diff)
			want := bytes.Clone(base)
			for _, write := range []struct {
				offset int
				data   []byte
			}{
				{0, bytes.Repeat([]byte{0xb7}, 2*cowBlockSize)},
				{cowBlockSize + 513, []byte("subsector update")},
				{4*cowBlockSize + 511, bytes.Repeat([]byte{0xa2}, 513)},
			} {
				if n, err := cow.WriteAt(write.data, int64(write.offset)); err != nil || n != len(write.data) {
					t.Fatalf("write n=%d err=%v", n, err)
				}
				copy(want[write.offset:], write.data)
				clear(write.data) // the cache must own the accepted guest bytes
			}
			checkRead(t, cow, 0, want)
			if err := cow.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := cow.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenBlockCOW(path, &fakeReader{data: base}, DiffInit{Existing: true}, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			checkTmpfsActiveIO(t, reopened.diff)
			checkRead(t, reopened, 0, want)
			s := cache.Stats()
			if s.PeakUsed > s.Capacity || s.PeakDirty > s.MaxDirty || s.DirtyUsed != 0 {
				t.Fatalf("cache budget %+v", s)
			}
		})
	}
}

func TestDiffTmpfsOwnedWorkspaceAndCopyout(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			var options []BlockCOWOption
			if encrypted {
				options = append(options, WithDiffEncryption(testDiffKey(19), true))
			}
			cache := testCache(t, 2, 1, cacheHooks{afterReadCopy: func() {}})
			cow, err := OpenBlockCOW(filepath.Join(tmpfsTestDir(t), "diff"), nil, DiffInit{CreateSize: 3 * maxDiffScratchSize}, append(options, WithCOWCache(cache))...)
			if err != nil {
				t.Fatal(err)
			}
			defer cow.Close()
			d := cow.diff
			checkTmpfsActiveIO(t, d)
			backing := patternedBytes(2*maxDiffScratchSize + 2)
			want := bytes.Clone(backing[1 : len(backing)-1])
			if _, err := d.WriteAt(backing[1:len(backing)-1], 0); err != nil {
				t.Fatal(err)
			}
			update := []byte("subsector physical update")
			if _, err := d.WriteAt(update, 513); err != nil {
				t.Fatal(err)
			}
			copy(want[513:], update)
			got := make([]byte, len(want)+2)
			if n, err := d.ReadAt(got[1:len(got)-1], 0); err != nil || n != len(want) {
				t.Fatalf("unaligned read %d: %v", n, err)
			}
			if !bytes.Equal(got[1:len(got)-1], want) {
				t.Fatal("owned tmpfs/XTS staging lost bytes")
			}
			if !allZero(d.direct.read.bytes) || !allZero(d.direct.write.bytes) {
				t.Fatal("scratch not cleared")
			}
			for i := 0; i < len(want)/cowBlockSize; i++ {
				cow.markDirty(int64(i))
			}
			// Change the output immediately after copyout, before ReadAt returns.
			// Clean cache publication must have used the owned plaintext instead.
			out := make([]byte, cowBlockSize)
			cache.hooks.afterReadCopy = func() { clear(out) }
			if _, err := cow.ReadAt(out, 0); err != nil {
				t.Fatal(err)
			}
			checkRead(t, cow, 0, want[:cowBlockSize])
			if err := cow.Discard(cowBlockSize, cowBlockSize); err != nil {
				t.Fatal(err)
			}
			checkRead(t, cow, 1, make([]byte, cowBlockSize))
			if n, err := d.ReadAt(got[:17], d.logicalSize-8); n != 8 || !errors.Is(err, io.EOF) {
				t.Fatalf("EOF n=%d: %v", n, err)
			}
		})
	}
}

func TestDiffTmpfsMixedCacheSnapshotBackpressure(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, writeback := range []bool{false, true} {
			t.Run(fmt.Sprintf("encrypted=%t/writeback=%t", encrypted, writeback), func(t *testing.T) {
				gate := newWorkerGate()
				defer gate.open()
				waiting := make(chan struct{}, 16)
				hooks := cacheHooks{waiting: func() {
					select {
					case waiting <- struct{}{}:
					default:
					}
				}}
				if writeback {
					hooks.beforeWrite = func([]*cachePage) error { gate.wait(); return nil }
				} else {
					hooks.beforeSelect = gate.wait
				}
				cache := testCache(t, 3, 2, hooks)
				var options []BlockCOWOption
				if encrypted {
					options = append(options, WithDiffEncryption(testDiffKey(20), true))
				}
				root, err := OpenBlockCOW(filepath.Join(tmpfsTestDir(t), "root"), nil, DiffInit{CreateSize: 4 * cowBlockSize}, append(options, WithCOWCache(cache))...)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { gate.open(); _ = root.Close() }()
				data := cachedCOW(t, cache, 4, nil, options...)
				checkTmpfsActiveIO(t, root.diff)
				checkDiskActiveIO(t, data.diff)
				var syncs atomic.Int32
				root.diff.syncFile = func() error { syncs.Add(1); return nil }
				data.diff.syncFile = root.diff.syncFile
				writePage(t, root, 0, 0x61)
				awaitSignal(t, gate.entered)
				writePage(t, data, 0, 0x62)
				wb := 0
				if writeback {
					wb = 1
				}
				checkBudget(t, cache, 2, 2, wb)
				for _, c := range []*BlockCOW{root, data} {
					value := byte(0x61)
					if c == data {
						value = 0x62
					}
					want := bytes.Repeat([]byte{value}, cowBlockSize)
					checkRead(t, c, 0, want)
					view, holes, err := c.SnapshotView()
					if err != nil || len(holes) != 1 {
						t.Fatalf("snapshot %v %v", holes, err)
					}
					got := make([]byte, 4*cowBlockSize)
					if _, err := io.ReadFull(view, got); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got[:cowBlockSize], want) || !allZero(got[cowBlockSize:]) {
						t.Fatal("snapshot missed latest upper or holes")
					}
					if err := c.Flush(); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				pending := make(chan error, 1)
				go func() { _, err := root.writeAt(ctx, []byte{1}, 2*cowBlockSize); pending <- err }()
				awaitSignal(t, waiting)
				checkBudget(t, cache, 2, 2, wb) // total space remains; shared dirty quota is exhausted
				cancel()
				if err := awaitError(t, pending); !errors.Is(err, context.Canceled) {
					t.Fatalf("backpressure cancellation: %v", err)
				}
				if syncs.Load() != 0 {
					t.Fatal("guest FLUSH called Sync")
				}
				gate.open()
				if err := cache.Drain(context.Background()); err != nil {
					t.Fatal(err)
				}
				checkBudget(t, cache, 2, 0, 0)
				if syncs.Load() != 0 {
					t.Fatal("Drain called Sync")
				}
				// Normal Close must drain a newly accepted tmpfs write as well.
				writePage(t, root, 1, 0x63)
				path := root.diff.f.Name()
				if err := root.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := OpenBlockCOW(path, nil, DiffInit{Existing: true}, append(options, WithCOWCache(cache))...)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				checkTmpfsActiveIO(t, reopened.diff)
				checkRead(t, reopened, 1, bytes.Repeat([]byte{0x63}, cowBlockSize))
			})
		}
	}
}
