package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

type bundleAuditSource struct {
	sparse.Source
	directory string
	lastEnd   uint64
	pass      int
	maxRead   int
	mutate    bool
	failure   error
}

func (s *bundleAuditSource) check() error {
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 64+len(".bundle") || !strings.HasSuffix(name, ".bundle") {
			return fmt.Errorf("unexpected staging output: %s", name)
		}
	}
	return nil
}
func (s *bundleAuditSource) RunAt(off, limit uint64) (sparse.Run, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	r, err := s.Source.RunAt(off, limit)
	if err != nil {
		return nil, err
	}
	return bundleAuditRun{Run: r, owner: s}, nil
}

type bundleAuditRun struct {
	sparse.Run
	owner *bundleAuditSource
}

func (r bundleAuditRun) ReadAt(ctx context.Context, b []byte, inner uint64) (int, error) {
	s := r.owner
	if err := s.check(); err != nil {
		return 0, err
	}
	absolute := r.Offset() + inner
	if s.pass == 0 || absolute < s.lastEnd {
		s.pass++
	}
	s.lastEnd = absolute + uint64(len(b))
	s.maxRead = max(s.maxRead, len(b))
	if s.pass > 1 && s.failure != nil {
		return 0, s.failure
	}
	n, err := r.Run.ReadAt(ctx, b, inner)
	if s.pass > 1 && s.mutate && n > 0 {
		b[0] ^= 1
	}
	return n, err
}

func TestSingleRootBundleDirectOutputAndFixedKey(t *testing.T) {
	flattened, _ := sourceBundleFlattenedImage(t)
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	admission, err := cfg.WriteAdmission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	keyFn := func() ([32]byte, error) {
		calls++
		value := key
		if calls > 1 {
			value[0] ^= 1
		}
		return value, nil
	}
	directory := t.TempDir()
	publisher, err := NewSingleRootBundlePublisher(cfg, keyFn, admission, "release", directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Constructor validation is separate from one publication invocation.
	calls = 0
	target := publisher.target.(*singleRootBundleTarget)
	fs := newTrackingLocationFileSystem()
	target.fs = fs
	source := &bundleAuditSource{Source: publishSource(t, flattened), directory: directory}
	result, err := publisher.PublishSource(context.Background(), RoleImage, source)
	if err != nil {
		t.Fatal(err)
	}
	if result.Ref == "" || calls != 1 || source.pass != 2 {
		t.Fatalf("ref=%q keyCalls=%d passes=%d", result.Ref, calls, source.pass)
	}
	if fs.creates.Load() != 1 {
		t.Fatalf("created %d files, want final only", fs.creates.Load())
	}
	if source.maxRead > 4096 {
		t.Fatalf("read buffer %d exceeds configured Chunk", source.maxRead)
	}
}

func TestSingleRootBundleIdentityChangeAndSecondPassFailure(t *testing.T) {
	flattened, _ := sourceBundleFlattenedImage(t)
	for _, mode := range []string{"changed source", "read error"} {
		t.Run(mode, func(t *testing.T) {
			cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
			directory := t.TempDir()
			publisher := newSourceBundlePublisher(t, cfg, key, directory)
			source := &bundleAuditSource{Source: publishSource(t, flattened), directory: directory, mutate: mode == "changed source"}
			failure := errors.New("injected second pass read error")
			if mode == "read error" {
				source.failure = failure
			}
			result, err := publisher.PublishSource(context.Background(), RoleImage, source)
			if err == nil || result.Ref != "" {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if mode == "read error" && !errors.Is(err, failure) {
				t.Fatalf("source cause lost: %v", err)
			}
			if mode == "changed source" && !strings.Contains(err.Error(), "source changed") {
				t.Fatalf("missing identity mismatch: %v", err)
			}
			entries, readErr := os.ReadDir(directory)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("incomplete final remains: %v %v", entries, readErr)
			}
		})
	}
}

type failedBundleOutput struct {
	locationWriteFile
	writeErr, closeErr error
}

func (f *failedBundleOutput) Write(b []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.locationWriteFile.Write(b)
}
func (f *failedBundleOutput) Close() error {
	return errors.Join(f.locationWriteFile.Close(), f.closeErr)
}
func TestSingleRootBundleWriteAndCloseErrorsCleanOwnedFinal(t *testing.T) {
	flattened, _ := sourceBundleFlattenedImage(t)
	for _, mode := range []string{"write", "close"} {
		t.Run(mode, func(t *testing.T) {
			cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
			directory := t.TempDir()
			publisher := newSourceBundlePublisher(t, cfg, key, directory)
			fs := newTrackingLocationFileSystem()
			failure := errors.New("injected " + mode + " failure")
			fs.wrapCreate = func(_ string, f locationWriteFile) locationWriteFile {
				wrapped := &failedBundleOutput{locationWriteFile: f}
				if mode == "write" {
					wrapped.writeErr = failure
				} else {
					wrapped.closeErr = failure
				}
				return wrapped
			}
			publisher.target.(*singleRootBundleTarget).fs = fs
			result, err := publisher.PublishSource(context.Background(), RoleImage, publishSource(t, flattened))
			if !errors.Is(err, failure) || result.Ref != "" {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			entries, readErr := os.ReadDir(directory)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("incomplete final remains: %v %v", entries, readErr)
			}
		})
	}
}

type sparseLargeBundleReader struct{ limit uint64 }

func (r sparseLargeBundleReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || uint64(off) >= r.limit {
		return 0, io.EOF
	}
	for i := range p {
		p[i] = byte((off+int64(i))%251 + 1)
	}
	return len(p), nil
}
func TestSingleRootBundleLargeSparseSourceIsBounded(t *testing.T) {
	const size = uint64(8) << 30
	cfg, key := sourceBundleConfig(manifestcrypto.LocalOff)
	directory := t.TempDir()
	raw, err := sparse.NewSource(sparseLargeBundleReader{size}, size, []sparse.Extent{{Offset: 4096, Size: size - 8192}})
	if err != nil {
		t.Fatal(err)
	}
	source := &bundleAuditSource{Source: raw, directory: directory}
	publisher := newSourceBundlePublisher(t, cfg, key, directory)
	result, err := publisher.PublishSource(context.Background(), RoleImage, source)
	if err != nil {
		t.Fatal(err)
	}
	if source.pass != 2 || source.maxRead > 4096 {
		t.Fatalf("passes=%d maxRead=%d", source.pass, source.maxRead)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outputs %v %v", entries, err)
	}
	info, err := os.Stat(filepath.Join(directory, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 128<<10 || result.Ref == "" {
		t.Fatalf("unexpected physical size %d ref=%q", info.Size(), result.Ref)
	}
}
