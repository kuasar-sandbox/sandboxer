package artifact

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"golang.org/x/sys/unix"
)

func locationTestSource(t *testing.T, body []byte) sparse.Source {
	t.Helper()
	payload, err := sparse.NewSource(bytes.NewReader(body), uint64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	var carrier bytes.Buffer
	if _, _, err := tarstream.WriteTo(context.Background(), &carrier, "fixture", payload); err != nil {
		t.Fatal(err)
	}
	opened, _, err := tarstream.SourceAt(bytes.NewReader(carrier.Bytes()), int64(carrier.Len()), "")
	if err != nil {
		t.Fatal(err)
	}
	size, digest, ok := opened.(tarstream.IdentityProvider).PayloadCommitment()
	if !ok || size != uint64(len(body)) {
		t.Fatal("fixture carrier did not expose its payload commitment")
	}
	return &locationTestCarrier{Source: payload, payloadSize: size, payloadDigest: digest}
}

type locationTestCarrier struct {
	sparse.Source
	payloadSize   uint64
	payloadDigest [32]byte
}

func (s *locationTestCarrier) TarStreamDigest(name string) ([32]byte, bool) {
	digest, err := tarstream.ComposeDigest(name, s.Size(), s.payloadSize, s.payloadDigest, nil)
	return digest, err == nil
}

func (s *locationTestCarrier) PayloadCommitment() (uint64, [32]byte, bool) {
	return s.payloadSize, s.payloadDigest, true
}

func locationTestTarget(directory string, codec tarstream.Codec, required bool) *locationPublishTarget {
	target := newLocationPublishTarget("shared", directory, codec, required, nil)
	target.retry = locationRetryPolicy{window: 40 * time.Millisecond, initial: time.Millisecond, maximum: 5 * time.Millisecond}
	return target
}

type trackingLocationFileSystem struct {
	base locationFileSystem

	creates atomic.Int32
	opens   atomic.Int32
	written atomic.Int64

	wrapCreate func(string, locationWriteFile) locationWriteFile
	onOpen     func(int32)
	openErr    error
}

func newTrackingLocationFileSystem() *trackingLocationFileSystem {
	return &trackingLocationFileSystem{base: osLocationFileSystem{}}
}

func (f *trackingLocationFileSystem) createExclusive(path string) (locationWriteFile, error) {
	file, err := f.base.createExclusive(path)
	if err != nil {
		return nil, err
	}
	f.creates.Add(1)
	var result locationWriteFile = &trackingLocationWriteFile{base: file, owner: f}
	if f.wrapCreate != nil {
		result = f.wrapCreate(path, result)
	}
	return result, nil
}

func (f *trackingLocationFileSystem) openNoFollow(path string) (locationReadFile, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	file, err := f.base.openNoFollow(path)
	if err != nil {
		return nil, err
	}
	count := f.opens.Add(1)
	if f.onOpen != nil {
		f.onOpen(count)
	}
	return &trackingLocationReadFile{base: file, owner: f}, nil
}

func (f *trackingLocationFileSystem) lstat(path string) (os.FileInfo, error) {
	return f.base.lstat(path)
}

func (f *trackingLocationFileSystem) remove(path string) error { return f.base.remove(path) }

type trackingLocationWriteFile struct {
	base  locationWriteFile
	owner *trackingLocationFileSystem
}

func (f *trackingLocationWriteFile) Write(body []byte) (int, error) {
	n, err := f.base.Write(body)
	f.owner.written.Add(int64(n))
	return n, err
}

func (f *trackingLocationWriteFile) Stat() (os.FileInfo, error) { return f.base.Stat() }
func (f *trackingLocationWriteFile) Chmod(mode os.FileMode) error {
	return f.base.Chmod(mode)
}
func (f *trackingLocationWriteFile) Close() error { return f.base.Close() }

type trackingLocationReadFile struct {
	base  locationReadFile
	owner *trackingLocationFileSystem
}

func (f *trackingLocationReadFile) Read(body []byte) (int, error) { return f.base.Read(body) }
func (f *trackingLocationReadFile) ReadAt(body []byte, offset int64) (int, error) {
	return f.base.ReadAt(body, offset)
}
func (f *trackingLocationReadFile) Seek(offset int64, whence int) (int64, error) {
	return f.base.Seek(offset, whence)
}
func (f *trackingLocationReadFile) Stat() (os.FileInfo, error) { return f.base.Stat() }
func (f *trackingLocationReadFile) Close() error { return f.base.Close() }

func TestLocationTargetFreshPublicationWritesSharedFinalOnce(t *testing.T) {
	directory := t.TempDir()
	fs := newTrackingLocationFileSystem()
	target := locationTestTarget(directory, nil, false)
	target.fs = fs
	body := bytes.Repeat([]byte("rename-free-location"), 64*1024)
	source := &failingLocationSource{inner: locationTestSource(t, body), fail: ^uint64(0)}

	oldUmask := unix.Umask(0o077)
	t.Cleanup(func() { unix.Umask(oldUmask) })
	refRaw, err := target.Put(context.Background(), RoleOverlay, source)
	unix.Umask(oldUmask)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.ParseRef(refRaw)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Location != "shared" || ref.DigestScheme != tarstream.DigestScheme || filepath.Ext(ref.Path) != ".overlay" {
		t.Fatalf("published ref = %#v", ref)
	}
	path := filepath.Join(directory, ref.Path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("published mode = %o, want 644", info.Mode().Perm())
	}
	if fs.creates.Load() != 1 {
		t.Fatalf("successful exclusive creates = %d, want 1", fs.creates.Load())
	}
	if got := fs.written.Load(); got != info.Size() {
		t.Fatalf("shared-target write bytes = %d, final size = %d", got, info.Size())
	}
	if got := source.read.Load(); got != uint64(len(body)) {
		t.Fatalf("logical source read bytes = %d, want one %d-byte encoding pass", got, len(body))
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ref.Path {
		t.Fatalf("location entries = %v, want only final %q", entries, ref.Path)
	}
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	locations := config.RefLocations{"shared": directory}
	opened, err := storage.OpenFileWithLocations(context.Background(), path, ref, locations)
	if err != nil {
		t.Fatalf("official opener rejected fresh final: %v", err)
	}
	if err := consumeLocationSource(context.Background(), opened); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLocationTargetReusesValidFinalWithoutWriting(t *testing.T) {
	directory := t.TempDir()
	fs := newTrackingLocationFileSystem()
	target := locationTestTarget(directory, nil, false)
	target.fs = fs
	body := bytes.Repeat([]byte{0x51}, 256*1024)
	first, err := target.Put(context.Background(), RoleSandbox, locationTestSource(t, body))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.ParseRef(first)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, ref.Path)
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writesBefore := fs.written.Load()

	second, err := target.Put(context.Background(), RoleSandbox, locationTestSource(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("reused ref = %q, want %q", second, first)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeInfo, afterInfo) || !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatal("valid existing final was replaced or rewritten")
	}
	if fs.creates.Load() != 1 || fs.written.Load() != writesBefore {
		t.Fatalf("reuse created/wrote final: creates=%d writes=%d before=%d", fs.creates.Load(), fs.written.Load(), writesBefore)
	}
}

type blockingLocationWriteFile struct {
	base    locationWriteFile
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *blockingLocationWriteFile) Write(body []byte) (int, error) {
	n, err := f.base.Write(body)
	f.once.Do(func() {
		close(f.started)
		<-f.release
	})
	return n, err
}

func (f *blockingLocationWriteFile) Stat() (os.FileInfo, error) { return f.base.Stat() }
func (f *blockingLocationWriteFile) Chmod(mode os.FileMode) error {
	return f.base.Chmod(mode)
}
func (f *blockingLocationWriteFile) Close() error { return f.base.Close() }

func TestLocationTargetRetriesInProgressFinal(t *testing.T) {
	directory := t.TempDir()
	body := bytes.Repeat([]byte{0x37}, 8<<20)
	started := make(chan struct{})
	release := make(chan struct{})
	aFS := newTrackingLocationFileSystem()
	aFS.wrapCreate = func(_ string, file locationWriteFile) locationWriteFile {
		return &blockingLocationWriteFile{base: file, started: started, release: release}
	}
	a := locationTestTarget(directory, nil, false)
	a.fs = aFS
	a.retry = locationRetryPolicy{window: 2 * time.Second, initial: 2 * time.Millisecond, maximum: 20 * time.Millisecond}

	bFS := newTrackingLocationFileSystem()
	retried := make(chan struct{})
	var retriedOnce sync.Once
	bFS.onOpen = func(count int32) {
		if count >= 2 {
			retriedOnce.Do(func() { close(retried) })
		}
	}
	b := locationTestTarget(directory, nil, false)
	b.fs = bFS
	b.retry = a.retry

	type result struct {
		ref string
		err error
	}
	aResult := make(chan result, 1)
	aSource := locationTestSource(t, body)
	go func() {
		ref, err := a.Put(context.Background(), RoleSnapshot, aSource)
		aResult <- result{ref: ref, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first publisher did not expose its partial final")
	}
	bResult := make(chan result, 1)
	bSource := locationTestSource(t, body)
	go func() {
		ref, err := b.Put(context.Background(), RoleSnapshot, bSource)
		bResult <- result{ref: ref, err: err}
	}()
	select {
	case <-retried:
	case result := <-bResult:
		t.Fatalf("second publisher stopped without retrying: %+v", result)
	case <-time.After(5 * time.Second):
		t.Fatal("second publisher did not retry the in-progress final")
	}
	close(release)
	first := <-aResult
	second := <-bResult
	if first.err != nil || second.err != nil || first.ref != second.ref {
		t.Fatalf("concurrent publication: first=%+v second=%+v", first, second)
	}
	if opens := bFS.opens.Load(); opens < 2 || opens > 100 {
		t.Fatalf("validation attempts = %d, want bounded non-busy retry", opens)
	}
}

func locationTestDestination(t *testing.T, target *locationPublishTarget, role LogicalRole, body []byte) string {
	t.Helper()
	payload, err := locationPayloadName(role)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := tarstream.CarrierDigest(payload, locationTestSource(t, body), target.writeOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(target.directory, digest+"."+payload)
}

func TestLocationTargetPreservesAbandonedInvalidFinal(t *testing.T) {
	directory := t.TempDir()
	target := locationTestTarget(directory, nil, false)
	body := bytes.Repeat([]byte{0x61}, 128*1024)
	path := locationTestDestination(t, target, RoleOverlay, body)
	const partial = "abandoned-unknown-owner"
	if err := os.WriteFile(path, []byte(partial), 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = target.Put(context.Background(), RoleOverlay, locationTestSource(t, body))
	if err == nil || !strings.Contains(err.Error(), "cleanup/repair is required before retry") {
		t.Fatalf("invalid-final error = %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("invalid final was deleted: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != partial || !os.SameFile(before, after) {
		t.Fatalf("invalid final changed: bytes=%q info=%v err=%v", got, after, err)
	}
}

func TestLocationTargetPreservesExistingFinalOnOperationalErrors(t *testing.T) {
	body := bytes.Repeat([]byte{0x63}, 64*1024)
	for _, operationalErr := range []error{unix.EACCES, unix.EIO, unix.ESTALE} {
		t.Run(operationalErr.Error(), func(t *testing.T) {
			directory := t.TempDir()
			target := locationTestTarget(directory, nil, false)
			path := locationTestDestination(t, target, RoleOverlay, body)
			const existing = "unknown existing final"
			if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			fs := newTrackingLocationFileSystem()
			fs.openErr = &os.PathError{Op: "open", Path: path, Err: operationalErr}
			target.fs = fs
			started := time.Now()
			_, err = target.Put(context.Background(), RoleOverlay, locationTestSource(t, body))
			if !errors.Is(err, operationalErr) {
				t.Fatalf("operational error = %v, want %v", err, operationalErr)
			}
			if strings.Contains(err.Error(), "cleanup/repair") || time.Since(started) >= target.retry.window {
				t.Fatalf("operational error was treated as stable content corruption: %v", err)
			}
			after, statErr := os.Stat(path)
			got, readErr := os.ReadFile(path)
			if statErr != nil || readErr != nil || !os.SameFile(before, after) || string(got) != existing {
				t.Fatalf("existing final changed: stat=%v read=%v bytes=%q", statErr, readErr, got)
			}
		})
	}
}

func TestLocationTargetExistingRetryHonorsContext(t *testing.T) {
	directory := t.TempDir()
	target := locationTestTarget(directory, nil, false)
	target.retry = locationRetryPolicy{window: time.Second, initial: 5 * time.Millisecond, maximum: 20 * time.Millisecond}
	body := bytes.Repeat([]byte{0x64}, 64*1024)
	path := locationTestDestination(t, target, RoleOverlay, body)
	if err := os.WriteFile(path, []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := target.Put(ctx, RoleOverlay, locationTestSource(t, body))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retry cancellation error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed >= target.retry.window {
		t.Fatalf("context cancellation took %s, retry window %s", elapsed, target.retry.window)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "incomplete" {
		t.Fatalf("canceled retry changed existing final: bytes=%q err=%v", got, readErr)
	}
}

func TestLocationTargetPreservesSymlinkAndNonRegularFinals(t *testing.T) {
	body := bytes.Repeat([]byte{0x62}, 32*1024)
	for _, kind := range []string{"symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			target := locationTestTarget(directory, nil, false)
			path := locationTestDestination(t, target, RoleOverlay, body)
			switch kind {
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := target.Put(context.Background(), RoleOverlay, locationTestSource(t, body)); err == nil {
				t.Fatalf("%s final was accepted", kind)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("%s final changed: before=%v after=%v err=%v", kind, before, after, err)
			}
		})
	}
}

var errInjectedLocationFailure = errors.New("injected location publication failure")

type hookedLocationWriteFile struct {
	base      locationWriteFile
	writeHook func([]byte) (int, error)
	closeHook func() error
}

func (f *hookedLocationWriteFile) Write(body []byte) (int, error) {
	if f.writeHook != nil {
		return f.writeHook(body)
	}
	return f.base.Write(body)
}

func (f *hookedLocationWriteFile) Stat() (os.FileInfo, error) { return f.base.Stat() }
func (f *hookedLocationWriteFile) Chmod(mode os.FileMode) error {
	return f.base.Chmod(mode)
}
func (f *hookedLocationWriteFile) Close() error {
	if f.closeHook != nil {
		return f.closeHook()
	}
	return f.base.Close()
}

type failingLocationSource struct {
	inner sparse.Source
	read  atomic.Uint64
	fail  uint64
}

func (s *failingLocationSource) Size() uint64 { return s.inner.Size() }

func (s *failingLocationSource) RunAt(offset, limit uint64) (sparse.Run, error) {
	run, err := s.inner.RunAt(offset, limit)
	if err != nil {
		return nil, err
	}
	return &failingLocationRun{inner: run, source: s}, nil
}

func (s *failingLocationSource) ReadAt(ctx context.Context, body []byte, offset uint64) (int, error) {
	if s.read.Load() >= s.fail {
		return 0, errInjectedLocationFailure
	}
	n, err := s.inner.ReadAt(ctx, body, offset)
	s.read.Add(uint64(n))
	return n, err
}

func (s *failingLocationSource) TarStreamDigest(name string) ([32]byte, bool) {
	provider, ok := s.inner.(tarstream.IdentityProvider)
	if !ok {
		return [32]byte{}, false
	}
	return provider.TarStreamDigest(name)
}

func (s *failingLocationSource) PayloadCommitment() (uint64, [32]byte, bool) {
	provider, ok := s.inner.(tarstream.IdentityProvider)
	if !ok {
		return 0, [32]byte{}, false
	}
	return provider.PayloadCommitment()
}

type failingLocationRun struct {
	inner  sparse.Run
	source *failingLocationSource
}

func (r *failingLocationRun) Offset() uint64       { return r.inner.Offset() }
func (r *failingLocationRun) End() uint64          { return r.inner.End() }
func (r *failingLocationRun) Kind() sparse.RunKind { return r.inner.Kind() }
func (r *failingLocationRun) ReadAt(ctx context.Context, body []byte, offset uint64) (int, error) {
	if r.source.read.Load() >= r.source.fail {
		return 0, errInjectedLocationFailure
	}
	n, err := r.inner.ReadAt(ctx, body, offset)
	r.source.read.Add(uint64(n))
	return n, err
}

func TestLocationTargetCleansOnlyOwnedPartialFinal(t *testing.T) {
	body := bytes.Repeat([]byte{0x73}, 512*1024)
	for _, fault := range []string{"source-read", "target-write", "context", "short-write", "close", "validation"} {
		t.Run(fault, func(t *testing.T) {
			directory := t.TempDir()
			target := locationTestTarget(directory, nil, false)
			fs := newTrackingLocationFileSystem()
			ctx := context.Background()
			var cancel context.CancelFunc
			if fault == "context" {
				ctx, cancel = context.WithCancel(context.Background())
			}
			var source sparse.Source = locationTestSource(t, body)
			if fault == "source-read" {
				source = &failingLocationSource{inner: locationTestSource(t, body), fail: 64 * 1024}
			}
			fs.wrapCreate = func(_ string, file locationWriteFile) locationWriteFile {
				hooked := &hookedLocationWriteFile{base: file}
				switch fault {
				case "target-write":
					hooked.writeHook = func([]byte) (int, error) { return 0, errInjectedLocationFailure }
				case "context":
					var once sync.Once
					hooked.writeHook = func(p []byte) (int, error) {
						n, err := file.Write(p)
						once.Do(cancel)
						return n, err
					}
				case "short-write":
					hooked.writeHook = func([]byte) (int, error) { return 0, nil }
				case "close":
					hooked.closeHook = func() error { return errors.Join(file.Close(), errInjectedLocationFailure) }
				}
				return hooked
			}
			target.fs = fs
			if fault == "validation" {
				target.validate = func(context.Context, locationReadFile, os.FileInfo, string, uint64, string, string) error {
					return errInjectedLocationFailure
				}
			}
			_, err := target.Put(ctx, RoleOverlay, source)
			if cancel != nil {
				cancel()
			}
			if err == nil {
				t.Fatal("injected failure was ignored")
			}
			if fault == "short-write" && !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("short write error = %v, want io.ErrShortWrite", err)
			}
			if fault == "context" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error = %v, want context.Canceled", err)
			}
			entries, readErr := os.ReadDir(directory)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("owned partial final was not cleaned: %v (error %v)", entries, err)
			}
		})
	}
}

func TestLocationTargetOwnedCleanupPreservesReplacementInode(t *testing.T) {
	directory := t.TempDir()
	target := locationTestTarget(directory, nil, false)
	body := bytes.Repeat([]byte{0x74}, 128*1024)
	const replacement = "another publisher replacement"
	fs := newTrackingLocationFileSystem()
	fs.wrapCreate = func(path string, file locationWriteFile) locationWriteFile {
		var once sync.Once
		return &hookedLocationWriteFile{base: file, writeHook: func([]byte) (int, error) {
			once.Do(func() {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(replacement), 0o644); err != nil {
					t.Fatal(err)
				}
			})
			return 0, errInjectedLocationFailure
		}}
	}
	target.fs = fs
	if _, err := target.Put(context.Background(), RoleOverlay, locationTestSource(t, body)); err == nil {
		t.Fatal("injected write failure was ignored")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("replacement entries = %v err=%v", entries, err)
	}
	got, err := os.ReadFile(filepath.Join(directory, entries[0].Name()))
	if err != nil || string(got) != replacement {
		t.Fatalf("replacement changed: %q err=%v", got, err)
	}
}

func TestLocationTargetCryptoCollisionsFailClosed(t *testing.T) {
	key := [32]byte{0x82, 0x83, 0x84}
	codec, err := manifestcrypto.NewTarStreamCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte{0x75}, 256*1024)
	for _, collision := range []string{"plaintext", "wrong-digest", "authentication"} {
		t.Run(collision, func(t *testing.T) {
			directory := t.TempDir()
			target := locationTestTarget(directory, codec, false)
			refRaw, err := target.Put(context.Background(), RoleOverlay, locationTestSource(t, body))
			if err != nil {
				t.Fatal(err)
			}
			ref, err := manifest.ParseRef(refRaw)
			if err != nil {
				t.Fatal(err)
			}
			if ref.DigestScheme != tarstream.DigestSchemeHMAC {
				t.Fatalf("encrypted identity = %q", ref.DigestScheme)
			}
			path := filepath.Join(directory, ref.Path)
			var replacement []byte
			switch collision {
			case "plaintext":
				var output bytes.Buffer
				if _, _, err := tarstream.WriteTo(context.Background(), &output, "overlay", locationTestSource(t, body)); err != nil {
					t.Fatal(err)
				}
				replacement = output.Bytes()
			case "wrong-digest":
				var output bytes.Buffer
				other := bytes.Repeat([]byte{0x76}, len(body))
				if _, _, err := tarstream.WriteTo(context.Background(), &output, "overlay", locationTestSource(t, other), tarstream.WithCodec(codec, false)); err != nil {
					t.Fatal(err)
				}
				replacement = output.Bytes()
			case "authentication":
				replacement, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				replacement[len(replacement)/2] ^= 0x80
			}
			if err := os.WriteFile(path, replacement, 0o644); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = target.Put(context.Background(), RoleOverlay, locationTestSource(t, body))
			if err == nil || !strings.Contains(err.Error(), "cleanup/repair is required before retry") {
				t.Fatalf("%s collision error = %v", collision, err)
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("%s collision was removed: before=%v after=%v err=%v", collision, before, after, err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, replacement) {
				t.Fatalf("%s collision changed: err=%v", collision, err)
			}
		})
	}
}

func TestLocationTargetSourceContractExcludesRenameAndLinks(t *testing.T) {
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	forbidden := map[string]bool{
		"Rename": true, "Renameat2": true, "Link": true, "Linkat": true, "Symlink": true,
	}
	for _, name := range []string{"location_target.go", "location_bundle.go"} {
		path := filepath.Join(filepath.Dir(current), name)
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if ok && forbidden[selector.Sel.Name] {
				t.Errorf("named-location target %s calls forbidden filesystem operation %s", name, selector.Sel.Name)
			}
			return true
		})
	}
}

func TestLocalSinksRetainAtomicRenameCommit(t *testing.T) {
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(current), "..", "snapshot", "sink.go")
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"writeArtifact": false, "finalize": false}
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		if _, tracked := want[function.Name.Name]; !tracked {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "Renameat2" {
				want[function.Name.Name] = true
			}
			return true
		})
	}
	for function, found := range want {
		if !found {
			t.Errorf("local snapshot.%s no longer uses the atomic no-replace rename commit", function)
		}
	}
}

var _ locationFileSystem = (*trackingLocationFileSystem)(nil)
var _ sparse.Source = (*failingLocationSource)(nil)
var _ sparse.Run = (*failingLocationRun)(nil)
