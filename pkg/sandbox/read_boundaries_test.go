package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/image"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

type boundaryExt4Stream struct {
	fetch.Stream
	cause error
}

func (s boundaryExt4Stream) ReadAt(_ context.Context, p []byte, _ uint64) (int, error) {
	copy(p, []byte{0x53, 0xef})
	return len(p), s.cause
}
func (boundaryExt4Stream) Close() error { return nil }

func TestReadBoundaryExt4FullTerminal(t *testing.T) {
	cause := io.EOF
	base := vhost.NewStreamReader(context.Background(), boundaryExt4Stream{cause: readerr.Mark(cause, false)}, 4096)
	err := validateExt4BlockReader(context.Background(), base)
	if !readretry.IsTerminal(err) || !readerr.IsPermanent(err) || !errors.Is(err, cause) {
		t.Fatalf("sandbox ext4 accepted/lost terminal: %v", err)
	}
}

func TestExt4FullOrdinaryEOFRemainsValid(t *testing.T) {
	base := vhost.NewStreamReader(context.Background(), boundaryExt4Stream{cause: io.EOF}, 4096)
	if err := validateExt4BlockReader(context.Background(), base); err != nil {
		t.Fatal(err)
	}
}

type missingImageStream struct{ fetch.Stream }

func (missingImageStream) ReadAt(context.Context, []byte, uint64) (int, error) {
	return 0, readerr.Mark(fs.ErrNotExist, false)
}
func (missingImageStream) Close() error { return nil }
func TestReadBoundaryImageConfigSourceMissingIsNotSoftMiss(t *testing.T) {
	reader := vhost.NewStreamReader(context.Background(), missingImageStream{}, 4096)
	cfg, err := LoadImageConfigFrom(reader, reader.Size())
	if cfg != nil || !readretry.IsTerminal(err) || !readerr.IsPermanent(err) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("source missing accepted as absent config: cfg=%v err=%v", cfg, err)
	}
}

type boundaryConfigStream struct {
	sparse.Source
	calls, fail int
	cause       error
}

func (*boundaryConfigStream) Close() error { return nil }
func (s *boundaryConfigStream) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	s.calls++
	n, err := s.Source.ReadAt(ctx, p, off)
	if s.calls == s.fail {
		return n, s.cause
	}
	return n, err
}

func TestImageConfigFullReadErrorsSurviveZIP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root.erofs")
	if err := os.WriteFile(path, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	if err := image.AppendConfigZip(path, &image.RuntimeConfig{Cmd: []string{"/bin/true"}}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := sparse.NewSource(bytes.NewReader(body), uint64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	baseline := &boundaryConfigStream{Source: source}
	if _, err := LoadImageConfigFrom(vhost.NewStreamReader(context.Background(), baseline, int64(len(body))), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{readerr.Mark(io.EOF, false), readerr.Mark(syscall.EAGAIN, false), io.EOF} {
		for fail := 1; fail <= baseline.calls; fail++ {
			s := &boundaryConfigStream{Source: source, fail: fail, cause: cause}
			cfg, err := LoadImageConfigFrom(vhost.NewStreamReader(context.Background(), s, int64(len(body))), int64(len(body)))
			if cause == io.EOF {
				// Preserve the delegated ZIP reader's existing EOF contract:
				// some fields accept full EOF; entry.Open returns it to callers.
				control := &boundaryConfigStream{Source: source, fail: fail, cause: io.EOF}
				_, controlErr := image.ReadConfig(vhost.NewStreamReader(context.Background(), control, int64(len(body))), int64(len(body)))
				if controlErr != nil {
					if cfg != nil || !errors.Is(err, io.EOF) || readretry.IsTerminal(err) {
						t.Fatalf("changed ordinary EOF at read %d: cfg=%v err=%v control=%v", fail, cfg, err, controlErr)
					}
				} else if err != nil || cfg == nil || len(cfg.Cmd) != 1 || cfg.Cmd[0] != "/bin/true" {
					t.Fatalf("ordinary EOF at read %d: cfg=%v err=%v", fail, cfg, err)
				}
			} else if cfg != nil || !readretry.IsTerminal(err) || !readerr.IsPermanent(err) || !errors.Is(err, cause) {
				t.Fatalf("read %d lost source failure %v: cfg=%v err=%v", fail, cause, cfg, err)
			}
		}
	}
}
