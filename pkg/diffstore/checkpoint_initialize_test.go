package diffstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// Delays the backend completion event at the frontend boundary. The versioned
// package separately gates the real initializer and all physical entry points.
// Using attachDisk exercises the production attachment, rollback, coordinator
// and Close paths without global hooks or scheduler timing assumptions.
type delayedInitBackend struct {
	diffBackend
	done       chan struct{}
	failure    error
	once       sync.Once
	waiting    chan struct{}
	waitOnce   sync.Once
	waitEvents chan struct{}
}

func (b *delayedInitBackend) finish()                      { b.once.Do(func() { close(b.done) }) }
func (b *delayedInitBackend) initialized() <-chan struct{} { return b.done }
func (b *delayedInitBackend) err() error {
	select {
	case <-b.done:
		if b.failure != nil {
			return b.failure
		}
	default:
	}
	return b.diffBackend.err()
}
func (b *delayedInitBackend) waitInitialized(ctx context.Context) error {
	if b.waitEvents != nil {
		b.waitEvents <- struct{}{}
	}
	if b.waiting != nil {
		b.waitOnce.Do(func() { close(b.waiting) })
	}
	select {
	case <-b.done:
		return b.failure
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *delayedInitBackend) writePages(ctx context.Context, pages []pageWrite) error {
	if err := b.waitInitialized(ctx); err != nil {
		return err
	}
	return b.diffBackend.writePages(ctx, pages)
}
func (b *delayedInitBackend) Close() error {
	<-b.done
	return errors.Join(b.failure, b.diffBackend.Close())
}

func pendingFresh(t *testing.T, size int64, template TemplateSource, failure error) (*freshDiffInit, *delayedInitBackend, diskOptions) {
	t.Helper()
	options, err := parseDiskOptions([]DiskOption{WithCheckpoint("pending", "base")})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := createFreshDiff(filepath.Join(t.TempDir(), "active.diff"), DiffInit{CreateSize: size}, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.diff.waitInitialized(context.Background()); err != nil {
		t.Fatal(err)
	}
	backend := &delayedInitBackend{diffBackend: fresh.diff, done: make(chan struct{}), failure: failure}
	fresh.diff, fresh.template = backend, template
	return fresh, backend, options
}

func attachPending(t *testing.T, fresh *freshDiffInit, backend *delayedInitBackend, base BlockReader, options diskOptions) *Disk {
	t.Helper()
	cow, err := attachDisk(backend, fresh, make([]uint64, (backend.Size()/cowBlockSize+63)/64), base, options)
	if err != nil {
		backend.finish()
		t.Fatal(err)
	}
	cow.versions = backend.diffBackend.(*checkpointDiff)
	t.Cleanup(func() { backend.finish(); _ = cow.Close() })
	return cow
}

func TestPendingCheckpointFrontendAndPublication(t *testing.T) {
	const size = 16 * cowBlockSize
	fresh, backend, options := pendingFresh(t, size, nil, nil)
	base := &fakeReader{data: bytes.Repeat([]byte{0x65}, 4*cowBlockSize)}
	cow := attachPending(t, fresh, backend, base, options)
	if err := cow.Flush(); err != nil {
		t.Fatal(err)
	}
	if id, baseID, ok := cow.CheckpointInfo(); !ok || id != "pending" || baseID != "base" {
		t.Fatal(id, baseID, ok)
	}
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal("empty cache should not wait for initialization", err)
	}
	if _, err := os.Stat(fresh.finalPath); !os.IsNotExist(err) {
		t.Fatal("pending file published", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cow.WaitMaterialReady(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := cow.WaitMaterialized(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	work := make(chan error, 1)
	go func() {
		buf := make([]byte, cowBlockSize)
		if _, err := cow.ReadAt(buf, 0); err != nil || !bytes.Equal(buf, base.data[:cowBlockSize]) {
			work <- fmt.Errorf("base read: %v", err)
			return
		}
		if _, err := cow.ReadAt(buf, 8*cowBlockSize); err != nil || !bytes.Equal(buf, make([]byte, cowBlockSize)) {
			work <- fmt.Errorf("zero read: %v", err)
			return
		}
		if _, err := cow.WriteAt(bytes.Repeat([]byte{0x71}, cowBlockSize), 0); err != nil {
			work <- err
			return
		}
		if _, err := cow.WriteAt([]byte{0x72}, 3); err != nil {
			work <- err
			return
		}
		if _, err := cow.WriteAt([]byte{0x73}, cowBlockSize+9); err != nil {
			work <- err
			return
		}
		if _, err := cow.ReadAt(buf, cowBlockSize); err != nil || buf[9] != 0x73 || buf[8] != 0x65 {
			work <- fmt.Errorf("partial lower read: %v", err)
			return
		}
		work <- nil
	}()
	if err := awaitError(t, work); err != nil {
		t.Fatal(err)
	}
	cow.cache.mu.Lock()
	for _, p := range cow.cache.pages {
		if p.chargedDirty && p.state != cacheDirty {
			t.Error("pending dirty page frozen", p.state)
		}
	}
	cow.cache.mu.Unlock()
	backend.finish()
	if err := cow.Checkpoint(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if err := cow.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDisk(fresh.finalPath, base, DiffInit{Existing: true}, WithCheckpoint("pending", "base"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	buf := make([]byte, cowBlockSize)
	if _, err := reopened.ReadAt(buf, 0); err != nil || buf[3] != 0x72 || buf[4] != 0x71 {
		t.Fatal("lost cached rewrite", err)
	}
	if _, err := reopened.ReadAt(buf, cowBlockSize); err != nil || buf[9] != 0x73 || buf[8] != 0x65 {
		t.Fatal("lost cached partial write", err)
	}
}

func TestSharedCacheSkipsLongPendingPrefix(t *testing.T) {
	const pendingPages = maxWritebackPages + 17
	written := make(chan error, 1)
	cache, err := newCache(512*cowBlockSize, 512*cowBlockSize, cacheHooks{beforeWrite: func(pages []*cachePage) error {
		if pages[0].key.cow.diff.name() == "" {
			return errors.New("missing owner")
		}
		select {
		case written <- nil:
		default:
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	fresh, backend, options := pendingFresh(t, 512*cowBlockSize, nil, nil)
	options.cache = cache
	pending := attachPending(t, fresh, backend, nil, options)
	if _, err := pending.WriteAt(bytes.Repeat([]byte{1}, pendingPages*cowBlockSize), 0); err != nil {
		t.Fatal(err)
	}
	ready := checkpointCOW(t, filepath.Join(t.TempDir(), "ready.diff"), nil, 8*cowBlockSize, false, WithCache(cache))
	t.Cleanup(func() { _ = ready.Close() })
	if err := ready.WaitMaterialized(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := ready.WriteAt(bytes.Repeat([]byte{2}, cowBlockSize), 0); err != nil {
		t.Fatal(err)
	}
	if err := awaitError(t, written); err != nil {
		t.Fatal("ready disk stalled behind pending prefix", err)
	}
	if err := ready.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	if cache.clients[pending] != pendingPages {
		t.Error("pending pages were persisted")
	}
	for key, p := range cache.pages {
		if key.cow == pending && p.state != cacheDirty {
			t.Error("pending page frozen", p.state)
		}
	}
	cache.mu.Unlock()
	if _, err := pending.WriteAt([]byte{3}, 0); err != nil {
		t.Fatal(err)
	}
	backend.finish()
	if err := pending.WaitMaterialized(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPendingAttachmentRollbackJoinsOwner(t *testing.T) {
	for _, kind := range []string{"base", "cache", "completed-failure"} {
		t.Run(kind, func(t *testing.T) {
			var failure error
			if kind == "completed-failure" {
				failure = io.ErrShortWrite
			}
			fresh, backend, options := pendingFresh(t, cowBlockSize, nil, failure)
			var base BlockReader
			if kind == "base" {
				base = &fakeReader{data: make([]byte, 2*cowBlockSize)}
			} else {
				cache, err := NewCache(cowBlockSize, cowBlockSize)
				if err != nil {
					t.Fatal(err)
				}
				_ = cache.Close()
				options.cache = cache
			}
			if kind == "completed-failure" {
				backend.finish()
			}
			done := make(chan error, 1)
			go func() { _, err := attachDisk(backend, fresh, make([]uint64, 1), base, options); done <- err }()
			backend.finish()
			if err := awaitError(t, done); err == nil || failure != nil && !errors.Is(err, failure) {
				t.Fatal("rollback result", err)
			}
			if _, err := os.Stat(fresh.tmpPath); !os.IsNotExist(err) {
				t.Fatal("rollback leaked private file", err)
			}
			if _, err := os.Stat(fresh.claimPath); !os.IsNotExist(err) {
				t.Fatal("rollback leaked claim", err)
			}
		})
	}
}

func TestPendingCloseRetainedAndDisposable(t *testing.T) {
	for _, disposable := range []bool{false, true} {
		for _, failure := range []error{nil, io.ErrShortWrite} {
			t.Run(fmt.Sprintf("disposable=%t/failure=%v", disposable, failure), func(t *testing.T) {
				fresh, backend, options := pendingFresh(t, 4*cowBlockSize, nil, failure)
				options.discardOnClose = disposable
				cow := attachPending(t, fresh, backend, nil, options)
				if _, err := cow.WriteAt(bytes.Repeat([]byte{0x41}, cowBlockSize), 0); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cow.Close() }()
				<-cow.lifeCtx.Done()
				// Frontend cancellation follows teardown admission, so releasing
				// initialization now cannot admit disposable publication.
				backend.finish()
				if err := awaitError(t, done); !errors.Is(err, failure) {
					t.Fatal("Close lost actual result", err)
				}
				if disposable && failure == nil {
					if err := cow.WaitMaterialized(context.Background()); !errors.Is(err, context.Canceled) {
						t.Fatal("disposal falsely reported successful publication", err)
					}
				}
				_, err := os.Stat(fresh.finalPath)
				if failure != nil || disposable {
					if !os.IsNotExist(err) {
						t.Fatal("unexpected publication", err)
					}
				} else if err != nil {
					t.Fatal("retained Close failed publication", err)
				}
				if _, err := os.Stat(fresh.tmpPath); !os.IsNotExist(err) {
					t.Fatal("Close retained partial", err)
				}
				if _, err := os.Stat(fresh.claimPath); !os.IsNotExist(err) {
					t.Fatal("Close retained claim", err)
				}
			})
		}
	}
}

type admittedTemplate struct {
	TemplateSource
	entered, release chan struct{}
	once             sync.Once
}

func (s *admittedTemplate) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.TemplateSource.ReadAt(ctx, p, off)
}

func TestInitializationFailureRetainsAdmittedTemplateReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "template.raw")
	want := bytes.Repeat([]byte{0x51}, 4*cowBlockSize)
	writeSparseTemplate(t, path, want, []sparse.Extent{{Size: uint64(len(want))}})
	source, err := openTestRawTemplate(path)
	if err != nil {
		t.Fatal(err)
	}
	template := &admittedTemplate{TemplateSource: source, entered: make(chan struct{}), release: make(chan struct{})}
	fresh, backend, options := pendingFresh(t, int64(len(want)), template, io.ErrShortWrite)
	cow := attachPending(t, fresh, backend, &fakeReader{data: bytes.Repeat([]byte{0x12}, len(want))}, options)
	read := make(chan error, 1)
	go func() {
		got := make([]byte, cowBlockSize)
		_, err := cow.ReadAt(got, 0)
		if err == nil && !bytes.Equal(got, want[:cowBlockSize]) {
			err = errors.New("admitted read fell through to base")
		}
		read <- err
	}()
	<-template.entered
	backend.finish()
	if err := cow.WaitMaterialized(context.Background()); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh.tmpPath); err != nil {
		t.Fatal("failed owner lost private file", err)
	}
	close(template.release)
	if err := awaitError(t, read); err != nil {
		t.Fatal(err)
	}
	late := make(chan error, 1)
	cow.SetFatalHandler(func(error) { late <- cow.WaitMaterialized(context.Background()) })
	if err := awaitError(t, late); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("late fatal replay", err)
	}
}

func TestPendingDiscardWaitsBeforeStripeAndCloseCancelsOnlyRequest(t *testing.T) {
	fresh, backend, options := pendingFresh(t, 4*cowBlockSize, nil, nil)
	backend.waitEvents = make(chan struct{}, 16)
	cow := attachPending(t, fresh, backend, nil, options)
	<-backend.waitEvents // coordinator joined initialization
	discarded := make(chan error, 1)
	go func() { discarded <- cow.Discard(0, cowBlockSize) }()
	<-backend.waitEvents // Discard reached the same gate, before taking stripes
	written := make(chan error, 1)
	go func() { _, err := cow.WriteAt([]byte{0x61}, 0); written <- err }()
	if err := awaitError(t, written); err != nil {
		t.Fatal("Discard blocked a bounded cached write", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- cow.Close() }()
	if err := awaitError(t, discarded); !errors.Is(err, context.Canceled) {
		t.Fatal("request cancellation", err)
	}
	select {
	case <-backend.done:
		t.Fatal("Close canceled initialization")
	default:
	}
	backend.finish()
	if err := awaitError(t, closed); err != nil {
		t.Fatal(err)
	}
}

func TestPendingCacheSleepsAndReadyPrecedesDrain(t *testing.T) {
	idle := make(chan error, 1)
	writeEntered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	cache, err := newCache(8*cowBlockSize, 8*cowBlockSize, cacheHooks{
		idle: func() {
			select {
			case idle <- nil:
			default:
			}
		},
		beforeWrite: func([]*cachePage) error { close(writeEntered); <-release; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	fresh, backend, options := pendingFresh(t, 8*cowBlockSize, nil, nil)
	options.cache = cache
	cow := attachPending(t, fresh, backend, nil, options)
	t.Cleanup(unblock)
	if _, err := cow.WriteAt([]byte{0x31}, 0); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	select {
	case <-idle:
	default:
	}
	cache.kickLocked(true)
	cache.mu.Unlock()
	if err := awaitError(t, idle); err != nil {
		t.Fatal(err)
	}
	// The pending owner was not selected or frozen; it stays writable.
	if _, err := cow.WriteAt([]byte{0x32}, 1); err != nil {
		t.Fatal(err)
	}
	backend.finish()
	<-writeEntered
	ready := make(chan error, 1)
	go func() { ready <- cow.WaitMaterialReady(context.Background()) }()
	if err := awaitError(t, ready); err != nil {
		t.Fatal("ready blocked on dirty drain", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cow.WaitMaterialized(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("published before writeback", err)
	}
	unblock()
	if err := cow.WaitMaterialized(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInitializationCompletedBeforeAttachment(t *testing.T) {
	for _, failure := range []error{nil, io.ErrShortWrite} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			fresh, backend, options := pendingFresh(t, cowBlockSize, nil, failure)
			backend.finish()
			cow := attachPending(t, fresh, backend, nil, options)
			if err := cow.WaitMaterialized(context.Background()); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if !errors.Is(cow.Err(), failure) {
				t.Fatal("late attachment lost failure", cow.Err())
			}
			if failure != nil {
				select {
				case <-cow.cache.done:
				default:
					<-cow.cache.done
				}
			}
		})
	}
}

func TestCheckpointInitializationPublicationProcess(t *testing.T) {
	if stage := os.Getenv("CHECKPOINT_INIT_CRASH"); stage != "" {
		path := os.Getenv("CHECKPOINT_INIT_PATH")
		options, err := parseDiskOptions([]DiskOption{WithCheckpoint("crash", "base")})
		if err != nil {
			t.Fatal(err)
		}
		fresh, err := createFreshDiff(path, DiffInit{CreateSize: 32 << 30}, options)
		if err != nil {
			t.Fatal(err)
		}
		fresh.beforePublish = func(actual string) error {
			if actual == stage {
				os.Exit(87)
			}
			return nil
		}
		cow, err := attachDisk(fresh.diff, fresh, make([]uint64, (fresh.diff.Size()/cowBlockSize+63)/64), nil, options)
		if err != nil {
			t.Fatal(err)
		}
		defer cow.Close()
		if err := cow.WaitMaterialized(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Fatal("crash boundary not reached")
	}
	for _, stage := range []string{"before-sync", "renamed", "directory-sync"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "disk.diff")
			cmd := exec.Command(os.Args[0], "-test.run=^TestCheckpointInitializationPublicationProcess$")
			cmd.Env = append(os.Environ(), "CHECKPOINT_INIT_CRASH="+stage, "CHECKPOINT_INIT_PATH="+path)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 87 {
				t.Fatalf("crash %s: %v\n%s", stage, err, out)
			}
			cow, err := OpenDisk(path, nil, DiffInit{Existing: true}, WithCheckpoint("crash", "base"))
			if stage == "before-sync" {
				if !os.IsNotExist(err) {
					if cow != nil {
						cow.Close()
					}
					t.Fatal("unpublished final path accepted", err)
				}
				return
			}
			if err != nil {
				t.Fatal("published format invalid after process exit", err)
			}
			defer cow.Close()
			page := bytes.Repeat([]byte{0x71}, cowBlockSize)
			if _, err := cow.ReadAt(page, cow.Size()-cowBlockSize); err != nil || !bytes.Equal(page, make([]byte, cowBlockSize)) {
				t.Fatal("published last-page state", err)
			}
		})
	}
}

func TestDisposableClosePreservesAdmittedPublicationError(t *testing.T) {
	for _, stage := range []string{"before-sync", "renamed", "directory-sync"} {
		t.Run(stage, func(t *testing.T) {
			fresh, backend, options := pendingFresh(t, cowBlockSize, nil, nil)
			options.discardOnClose = true
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			fresh.beforePublish = func(actual string) error {
				if actual == stage {
					close(entered)
					<-release
					return io.ErrShortWrite
				}
				return nil
			}
			cow := attachPending(t, fresh, backend, nil, options)
			t.Cleanup(unblock)
			backend.finish()
			<-entered
			done := make(chan error, 1)
			go func() { done <- cow.Close() }()
			<-cow.lifeCtx.Done()
			unblock()
			if err := awaitError(t, done); !errors.Is(err, io.ErrShortWrite) {
				t.Fatal("disposal masked admitted publication error", err)
			}
			_, err := os.Stat(fresh.finalPath)
			if stage == "before-sync" {
				if !os.IsNotExist(err) {
					t.Fatal("unexpected publication", err)
				}
			} else if err != nil {
				t.Fatal("Close rolled back renamed file", err)
			}
		})
	}
}

func TestPendingInitializationKeepsCachePressureCancellable(t *testing.T) {
	waiting := make(chan error, 1)
	cache, err := newCache(cowBlockSize, cowBlockSize, cacheHooks{waiting: func() {
		select {
		case waiting <- nil:
		default:
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	fresh, backend, options := pendingFresh(t, 4*cowBlockSize, nil, nil)
	options.cache = cache
	cow := attachPending(t, fresh, backend, nil, options)
	if _, err := cow.WriteAt([]byte{0x31}, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		n, err := cow.WriteAtContext(ctx, []byte{0x71}, cowBlockSize)
		if n != 0 {
			err = fmt.Errorf("unbudgeted write accepted %d bytes", n)
		}
		done <- err
	}()
	if err := awaitError(t, waiting); err != nil {
		t.Fatal(err)
	}
	if _, err := cow.WriteAt([]byte{0x32}, 1); err != nil {
		t.Fatal("pressure froze existing dirty page", err)
	}
	cancel()
	if err := awaitError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := cow.Err(); err != nil {
		t.Fatal("request cancellation poisoned owner", err)
	}
	backend.finish()
	if err := cow.WaitMaterialized(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointOversizedCapacityCleansFreshResources(t *testing.T) {
	const size = int64(32<<30) + cowBlockSize
	if err := ValidateWritableSize(size); err != nil {
		t.Fatalf("single-layer capacity rejected the checkpoint boundary fixture: %v", err)
	}
	for _, source := range []string{"size", "template"} {
		t.Run(source, func(t *testing.T) {
			init := DiffInit{CreateSize: size}
			if source == "template" {
				templatePath := filepath.Join(t.TempDir(), "template.raw")
				f, err := os.Create(templatePath)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(size); err != nil {
					_ = f.Close()
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				init = DiffInit{Template: rawTemplateFactory(templatePath)}
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "active.diff")
			cow, err := OpenDisk(path, nil, init, WithCheckpoint("oversized", ""))
			if cow != nil {
				_ = cow.Close()
				t.Fatal("oversized checkpoint escaped synchronous validation")
			}
			if err == nil || !strings.Contains(err.Error(), "32 GiB") {
				t.Fatalf("expected checkpoint capacity rejection, got %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("capacity rejection retained fresh resources: %v", entries)
			}
			retry, err := OpenDisk(path, nil, DiffInit{CreateSize: cowBlockSize}, WithCheckpoint("retry", ""))
			if err != nil {
				t.Fatalf("capacity rejection blocked a valid retry: %v", err)
			}
			t.Cleanup(func() { _ = retry.Close() })
			if err := retry.WaitMaterialized(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := retry.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
