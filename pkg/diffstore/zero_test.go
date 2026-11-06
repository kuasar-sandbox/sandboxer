package diffstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/presence"
)

func TestPresenceZeroDiscardReopen(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprintf("checkpoint=%t/encrypted=%t", checkpoint, encrypted), func(t *testing.T) {
				const size = 8 * cowBlockSize
				path := filepath.Join(t.TempDir(), "active.diff")
				base := &fakeReader{data: bytes.Repeat([]byte{0x6d}, size)}
				var opts []DiskOption
				if checkpoint {
					opts = append(opts, WithCheckpoint("zero-test", "base"))
				}
				if encrypted {
					opts = append(opts, WithDiffEncryption(testDiffKey(31), true))
				}
				d, err := OpenDisk(path, base, DiffInit{CreateSize: size}, opts...)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = d.Close() }()
				data := bytes.Repeat([]byte{0xa5}, 5*cowBlockSize)
				if _, err := d.WriteAt(data, cowBlockSize); err != nil {
					t.Fatal(err)
				}
				if err := d.Drain(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := d.Discard(cowBlockSize, cowBlockSize); err != nil {
					t.Fatal(err)
				}
				if err := d.Discard(2*cowBlockSize, cowBlockSize); err != nil {
					t.Fatal(err)
				}
				if err := d.Discard(6*cowBlockSize, cowBlockSize); err != nil {
					t.Fatal(err)
				}
				// Only page 4 is fully covered; neither boundary page is changed.
				if err := d.Discard(3*cowBlockSize+17, 3*cowBlockSize-34); err != nil {
					t.Fatal(err)
				}
				view, err := d.Export(context.Background(), ActiveDiff)
				if err != nil {
					t.Fatal(err)
				}
				kinds := []sparse.RunKind{sparse.Hole, sparse.Zero, sparse.Zero, sparse.Data, sparse.Zero, sparse.Data, sparse.Zero, sparse.Hole}
				for i, want := range kinds {
					if d.blockDirty(int64(i)) != (want != sparse.Hole) {
						t.Fatalf("page %d logical presence = %t, want %v", i, d.blockDirty(int64(i)), want)
					}
					run, err := view.RunAt(uint64(i*cowBlockSize), cowBlockSize)
					if err != nil || run.Kind() != want {
						t.Fatalf("page %d kind=%v err=%v", i, run, err)
					}
				}
				_ = view.Close()
				// v4 ACK publishes cached Zero; physical mapping removal is a
				// completion property of Drain, not the acceptance boundary.
				if err := d.Drain(context.Background()); err != nil {
					t.Fatal(err)
				}
				payload := make([]bool, len(kinds))
				if err := d.diff.payloadPresence(context.Background(), 0, payload); err != nil {
					t.Fatal(err)
				}
				for i, want := range kinds {
					if payload[i] != (want == sparse.Data) {
						t.Fatalf("page %d completed payload = %t, want %v", i, payload[i], want)
					}
				}
				if checkpoint {
					if err := d.Checkpoint(context.Background(), "zero"); err != nil {
						t.Fatal(err)
					}
				}
				// Partial writes to Zero initialize from zero, never from the base.
				if _, err := d.WriteAt([]byte{1, 2, 3}, 2*cowBlockSize+37); err != nil {
					t.Fatal(err)
				}
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
				d, err = OpenDisk(path, base, DiffInit{Existing: true}, opts...)
				if err != nil {
					t.Fatal(err)
				}
				want := append([]byte(nil), base.data...)
				for _, p := range []int{1, 2, 4, 6} {
					clear(want[p*cowBlockSize : (p+1)*cowBlockSize])
				}
				for _, p := range []int{3, 5} {
					copy(want[p*cowBlockSize:], data[:cowBlockSize])
				}
				copy(want[2*cowBlockSize+37:], []byte{1, 2, 3})
				got := make([]byte, size)
				if _, err := d.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
					t.Fatal("reopened logical contents", err)
				}
				if d.DirtyCount() != 6 {
					t.Fatal("presence lost Zero", d.DirtyCount())
				}
				if checkpoint {
					v, err := d.Export(context.Background(), "zero")
					if err != nil {
						t.Fatal(err)
					}
					defer v.Close()
					r, err := v.RunAt(2*cowBlockSize, cowBlockSize)
					if err != nil || r.Kind() != sparse.Zero {
						t.Fatal("checkpoint bitmap changed", r, err)
					}
				}
			})
		}
	}
}

func TestActiveZeroExportIncludesAcceptedWrite(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			gate := make(chan struct{})
			var once sync.Once
			// Hold selection so v4 can convert accepted dirty Zero to Data
			// before either state is frozen. A beforeWrite gate could otherwise
			// freeze Zero first and make the partial writer wait on this test.
			cache, err := newCache(4*cowBlockSize, 2*cowBlockSize, cacheHooks{beforeSelect: func() { <-gate }})
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			opts := []DiskOption{WithCache(cache)}
			if checkpoint {
				opts = append(opts, WithCheckpoint("cached-zero", ""))
			}
			d, err := OpenDisk(filepath.Join(t.TempDir(), "d"), nil, DiffInit{CreateSize: 2 * cowBlockSize}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			defer once.Do(func() { close(gate) })
			// Finish fresh checkpoint publication before adding writes whose
			// writeback is deliberately blocked by this test. Otherwise its
			// initialization drain can wait on gate while Export waits on it.
			if err := d.WaitMaterialized(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := d.Discard(0, cowBlockSize); err != nil {
				t.Fatal(err)
			}
			if _, err := d.WriteAt([]byte{0x51}, 17); err != nil {
				t.Fatal(err)
			}
			v, err := d.Export(context.Background(), ActiveDiff)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			r, err := v.RunAt(0, cowBlockSize)
			if err != nil || r.Kind() != sparse.Data {
				t.Fatal("cached payload classified as Zero", r, err)
			}
			p := make([]byte, cowBlockSize)
			if _, err := v.ReadAt(context.Background(), p, 0); err != nil || p[17] != 0x51 {
				t.Fatal(err)
			}
			p[17] = 0
			if !allZero(p) {
				t.Fatal("partial write leaked base")
			}
		})
	}
}

type partialNewPayload struct {
	diffBodyIO
	body   int64
	failed bool
}

func (f *partialNewPayload) WriteAt(p []byte, off int64) (int, error) {
	if off >= f.body && !f.failed {
		f.failed = true
		if _, err := f.diffBodyIO.WriteAt(p, off); err != nil {
			return 0, err
		}
		return 512, io.ErrShortWrite
	}
	return f.diffBodyIO.WriteAt(p, off)
}

func TestSingleZeroFirstPayloadFailureCleanup(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "d")
			var enc *diffEncryption
			if encrypted {
				enc = mustDiffEncryption(t, testDiffKey(22))
			}
			d, err := initializeFreshDiffFile(path, 2*cowBlockSize, enc, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = d.Close() }()
			if err := d.discard(context.Background(), 0, cowBlockSize); err != nil {
				t.Fatal(err)
			}
			d.bodyIO = &partialNewPayload{diffBodyIO: d.bodyIO, body: d.bodyOffset}
			if _, err := d.WriteAt(bytes.Repeat([]byte{0x81}, cowBlockSize), 0); !errors.Is(err, io.ErrShortWrite) {
				t.Fatal(err)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			d, err = openExistingDiffFile(path, enc, encrypted, false)
			if err != nil {
				t.Fatal(err)
			}
			kind, _ := d.classify(0, cowBlockSize)
			if kind != sparse.Zero {
				t.Fatal("failed first payload changed Zero", kind)
			}
			p := make([]byte, cowBlockSize)
			if _, err := d.ReadAt(p, 0); err != nil || !allZero(p) {
				t.Fatal("failed bytes exposed", err)
			}
		})
	}
}

func TestSinglePresenceFailureRestoresCommittedHoleAndZero(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			const farBlock = presence.PayloadSize * 8
			path := filepath.Join(t.TempDir(), "diff")
			var enc *diffEncryption
			if encrypted {
				enc = mustDiffEncryption(t, testDiffKey(23))
			}
			d, err := initializeFreshDiffFile(path, (farBlock+1)*cowBlockSize, enc, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = d.Close() }()
			if err := d.discard(context.Background(), cowBlockSize, cowBlockSize); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("second presence page failed")
			var published []int
			d.beforePresenceWrite = func(page int) error {
				published = append(published, page)
				if len(published) == 2 {
					return injected
				}
				return nil
			}
			pages := []pageWrite{
				{Block: 0, Data: bytes.Repeat([]byte{0x51}, cowBlockSize)},
				{Block: 1, Data: bytes.Repeat([]byte{0x52}, cowBlockSize)},
				{Block: farBlock, Data: bytes.Repeat([]byte{0x53}, cowBlockSize)},
			}
			if err := d.writePages(context.Background(), pages); !errors.Is(err, injected) {
				t.Fatal(err)
			}
			for _, page := range pages {
				if !page.Fresh {
					t.Fatal("Hole and Zero both require fresh-payload cleanup", page.Block)
				}
			}
			if err := d.cleanupWriteFailure(pages); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(published) != "[0 1 0 1]" {
				t.Fatal("partial publication was not restored", published)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			d, err = openExistingDiffFile(path, enc, encrypted, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, page := range pages {
				want := sparse.Hole
				if page.Block == 1 {
					want = sparse.Zero
				}
				kind, _ := d.classify(page.Block*cowBlockSize, cowBlockSize)
				if kind != want {
					t.Fatalf("page %d: %v, want %v", page.Block, kind, want)
				}
				payload := []bool{true}
				if err := d.payloadPresence(context.Background(), page.Block, payload); err != nil || payload[0] {
					t.Fatal("failed payload survived", page.Block, payload, err)
				}
				buf := make([]byte, cowBlockSize)
				if _, err := d.ReadAt(buf, int64(page.Block)*cowBlockSize); err != nil || !allZero(buf) {
					t.Fatal("failed payload visible", page.Block, err)
				}
			}
		})
	}
}

func TestLegacyDiffRejectedWithoutMutation(t *testing.T) {
	for _, version := range []byte{0, 1, 2, 7} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old")
			p := bytes.Repeat([]byte{0x7a}, 2*cowBlockSize)
			if version != 0 {
				copy(p, diffMagic[:])
				p[8] = 0
				p[9] = version
			}
			if err := os.WriteFile(path, p, 0600); err != nil {
				t.Fatal(err)
			}
			if d, err := OpenDisk(path, nil, DiffInit{Existing: true}); !errors.Is(err, ErrFormat) {
				if d != nil {
					_ = d.Close()
				}
				t.Fatal("old format accepted", err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(p, got) {
				t.Fatal("old file mutated", err)
			}
		})
	}
}

func TestZeroTemplateSeedKeepsPayloadSparse(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			var enc *diffEncryption
			if encrypted {
				enc = mustDiffEncryption(t, testDiffKey(19))
			}
			dir := t.TempDir()
			source, err := initializeFreshDiffFile(filepath.Join(dir, "source"), 130*cowBlockSize, enc, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := source.discard(context.Background(), 0, 130*cowBlockSize); err != nil {
				t.Fatal(err)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			template, err := openDiffTemplate(filepath.Join(dir, "source"), enc)
			if err != nil {
				t.Fatal(err)
			}
			defer template.Close()
			target, err := initializeFreshDiffFile(filepath.Join(dir, "target"), 130*cowBlockSize, enc, template)
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			for off := uint64(0); off < uint64(target.Size()); off += cowBlockSize {
				kind, _ := target.classify(off, off+cowBlockSize)
				if kind != sparse.Zero {
					t.Fatal("template Zero expanded", off, kind)
				}
			}
		})
	}
}

type gatedTemplateSource struct {
	diffTemplateSource
	beforeRun func()
	afterRead func()
}

func (s *gatedTemplateSource) RunAt(off, limit uint64) (sparse.Run, error) {
	if off == cowBlockSize && s.beforeRun != nil {
		s.beforeRun()
	}
	return s.diffTemplateSource.RunAt(off, limit)
}

func (s *gatedTemplateSource) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	n, err := s.diffTemplateSource.ReadAt(ctx, p, off)
	if off == cowBlockSize && s.afterRead != nil {
		s.afterRead()
	}
	return n, err
}

type gatedTemplateBackend struct {
	diffBackend
	beforeCommit func()
}

func (d *gatedTemplateBackend) writePages(ctx context.Context, pages []pageWrite) error {
	d.beforeCommit()
	return d.diffBackend.writePages(ctx, pages)
}

func (d *gatedTemplateBackend) discard(ctx context.Context, off, length int64) error {
	d.beforeCommit()
	return d.diffBackend.discard(ctx, off, length)
}

func TestTemplatePresenceAloneCoordinatesDiscardAndWrites(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, encrypted := range []bool{false, true} {
			for _, kind := range []sparse.RunKind{sparse.Data, sparse.Zero} {
				for _, phase := range []string{"before", "read-before-publish", "committing", "after"} {
					if kind == sparse.Zero && phase == "read-before-publish" {
						continue // Zero has no payload read.
					}
					for _, operation := range []string{"discard", "write"} {
						t.Run(fmt.Sprintf("checkpoint=%t/encrypted=%t/kind=%d/%s/%s", checkpoint, encrypted, kind, phase, operation), func(t *testing.T) {
							const size = 3 * cowBlockSize
							dir := t.TempDir()
							var options []DiskOption
							if checkpoint {
								options = append(options, WithCheckpoint("template-race", "base"))
							}
							if encrypted {
								options = append(options, WithDiffEncryption(testDiffKey(61), true))
							}
							opts, err := parseDiskOptions(options)
							if err != nil {
								t.Fatal(err)
							}
							// The immutable source contains Hole, the selected state, and
							// nonzero Data. Both the template and base have observable bytes.
							sourcePath := filepath.Join(dir, "source")
							source, err := initializeFreshDiffFile(sourcePath, size, opts.encryption, nil)
							if err != nil {
								t.Fatal(err)
							}
							if _, err := source.WriteAt(bytes.Repeat([]byte{0x59}, 2*cowBlockSize), cowBlockSize); err != nil {
								t.Fatal(err)
							}
							if kind == sparse.Zero {
								if err := source.discard(context.Background(), cowBlockSize, cowBlockSize); err != nil {
									t.Fatal(err)
								}
							}
							if err := source.Close(); err != nil {
								t.Fatal(err)
							}
							template, err := openDiffTemplate(sourcePath, opts.encryption)
							if err != nil {
								t.Fatal(err)
							}
							gate := newWorkerGate()
							defer gate.open()
							var once sync.Once
							pause := func() { once.Do(gate.wait) }
							wrapped := &gatedTemplateSource{diffTemplateSource: template}
							if phase == "before" {
								wrapped.beforeRun = pause
							} else if phase == "read-before-publish" {
								wrapped.afterRead = pause
							}
							path := filepath.Join(dir, "active")
							fresh, err := createFreshDiff(path, DiffInit{CreateSize: size}, opts)
							if err != nil {
								t.Fatal(err)
							}
							fresh.template = wrapped
							base := &presenceCountedBase{fakeReader: fakeReader{data: bytes.Repeat([]byte{0x27}, size)}}
							cow := &Disk{
								diff: fresh.diff, base: base, size: size, blockSize: cowBlockSize,
								bitmap: make([]uint64, 1), blockMu: make([]sync.RWMutex, cowLockStripes),
								materialReady: make(chan struct{}), materialDone: make(chan struct{}),
								materialTemplate: wrapped, materialFresh: fresh,
								cache: testCache(t, 2, 1, cacheHooks{}),
							}
							cow.versions, _ = fresh.diff.(*checkpointDiff)
							cow.lifeCtx, cow.cancel = context.WithCancel(context.Background())
							materialCtx, cancel := context.WithCancel(context.Background())
							cow.materialCancel = cancel
							if err := cow.cache.attach(cow); err != nil {
								t.Fatal(err)
							}
							if phase == "committing" {
								cow.diff = &gatedTemplateBackend{diffBackend: fresh.diff, beforeCommit: pause}
							}
							defer func() { gate.open(); _ = cow.Close() }()
							go cow.runMaterializer(materialCtx)
							if phase == "after" {
								if err := cow.WaitMaterialized(context.Background()); err != nil {
									t.Fatal(err)
								}
							} else {
								awaitSignal(t, gate.entered)
							}
							mutation := func() error {
								if operation == "discard" {
									return cow.Discard(cowBlockSize, cowBlockSize)
								}
								_, err := cow.WriteAt(bytes.Repeat([]byte{0xa7}, cowBlockSize), cowBlockSize)
								return err
							}
							if phase == "committing" {
								if cow.blockLock(1).TryLock() {
									cow.blockLock(1).Unlock()
									t.Fatal("template commit did not hold the foreground stripe")
								}
								done := make(chan error, 1)
								go func() { done <- mutation() }()
								gate.open()
								if err := awaitError(t, done); err != nil {
									t.Fatal(err)
								}
							} else if err := mutation(); err != nil {
								t.Fatal(err)
							}
							want := bytes.Repeat([]byte{0xa7}, cowBlockSize)
							if operation == "discard" {
								clear(want)
								payload := []bool{true}
								if !cow.blockDirty(1) {
									t.Fatal("discard failed to take logical ownership")
								}
								if err := cow.Drain(context.Background()); err != nil {
									t.Fatal(err)
								}
								if err := cow.diff.payloadPresence(context.Background(), 1, payload); err != nil || payload[0] {
									t.Fatal("discard retained active payload", payload, err)
								}
								checkRead(t, cow, 1, want)
								if _, err := cow.WriteAt([]byte{0x7c}, cowBlockSize+19); err != nil {
									t.Fatal(err)
								}
								want[19] = 0x7c
							}
							if base.reads.Load() != 0 {
								t.Fatal("complete operation or Zero partial write read the old base")
							}
							gate.open()
							if err := cow.WaitMaterialized(context.Background()); err != nil {
								t.Fatal(err)
							}
							checkRead(t, cow, 1, want)
							checkRead(t, cow, 0, base.data[:cowBlockSize])
							checkRead(t, cow, 2, bytes.Repeat([]byte{0x59}, cowBlockSize))
							view, err := cow.Export(context.Background(), ActiveDiff)
							if err != nil {
								t.Fatal(err)
							}
							run, err := view.RunAt(0, cowBlockSize)
							if err != nil || run.Kind() != sparse.Hole {
								t.Fatal("template Hole no longer falls through", run, err)
							}
							_ = view.Close()
						})
					}
				}
			}
		}
	}
}

func TestSingleTiBPresenceZeroDiscardReopen(t *testing.T) {
	const size int64 = 1 << 40
	const high int64 = (32 << 30) + cowBlockSize
	ctx := context.Background()
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "active.diff")
			base := presenceProbeBase{size: size}
			var opts []DiskOption
			if encrypted {
				opts = append(opts, WithDiffEncryption(testDiffKey(32), true))
			}
			d, err := OpenDisk(path, base, DiffInit{CreateSize: size}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if d != nil {
					_ = d.Close()
				}
			}()
			if d.Size() != size {
				t.Fatalf("capacity=%d want=%d", d.Size(), size)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() != singleBodyOffset(size)+size {
				t.Fatalf("file size=%d want=%d", info.Size(), singleBodyOffset(size)+size)
			}

			// Only these few pages have payload; the 1 TiB logical body stays sparse.
			write := func(p []byte, off int64) {
				t.Helper()
				if n, err := d.WriteAt(p, off); err != nil || n != len(p) {
					t.Fatalf("WriteAt(%d)=%d, %v", off, n, err)
				}
			}
			patch := []byte{1, 2, 3}
			write(bytes.Repeat([]byte{0x5c}, cowBlockSize), high)
			write(patch, size-int64(len(patch)))
			if err := d.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			// Discard both Data and Hole above the former 32 GiB boundary.
			if err := d.Discard(high, 2*cowBlockSize); err != nil {
				t.Fatal(err)
			}
			write(patch, high+cowBlockSize+37)
			if err := d.Drain(ctx); err != nil {
				t.Fatal(err)
			}

			zeroPartial := make([]byte, cowBlockSize)
			copy(zeroPartial[37:], patch)
			tail := bytes.Repeat([]byte{0xa7}, cowBlockSize)
			copy(tail[len(tail)-len(patch):], patch)
			check := func(stage string) {
				t.Helper()
				for _, page := range []struct {
					name             string
					offset           int64
					want             []byte
					logical, payload bool
				}{
					{"discarded", high, make([]byte, cowBlockSize), true, false},
					{"partial-zero", high + cowBlockSize, zeroPartial, true, true},
					{"hole", high + 2*cowBlockSize, bytes.Repeat([]byte{0xa7}, cowBlockSize), false, false},
					{"last", size - cowBlockSize, tail, true, true},
				} {
					got := make([]byte, cowBlockSize)
					if n, err := d.ReadAt(got, page.offset); err != nil || n != len(got) || !bytes.Equal(got, page.want) {
						t.Fatalf("%s %s ReadAt(%d)=%d, %v: contents differ", stage, page.name, page.offset, n, err)
					}
					payload := make([]bool, 1)
					if err := d.diff.payloadPresence(ctx, uint64(page.offset/cowBlockSize), payload); err != nil {
						t.Fatal(err)
					}
					if logical := d.blockDirty(page.offset / cowBlockSize); logical != page.logical || payload[0] != page.payload {
						t.Fatalf("%s %s logical/payload=%t/%t want=%t/%t",
							stage, page.name, logical, payload[0], page.logical, page.payload)
					}
				}
			}
			check("written")
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			d = nil
			d, err = OpenDisk(path, base, DiffInit{Existing: true}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			check("reopened")
		})
	}
}

func TestSingleRejectsCapacityAboveTiB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.diff")
	d, err := OpenDisk(path, nil, DiffInit{CreateSize: (1 << 40) + cowBlockSize})
	if err == nil {
		_ = d.Close()
		t.Fatal("accepted capacity above 1 TiB")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid capacity created target: %v", err)
	}
}
