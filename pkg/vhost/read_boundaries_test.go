package vhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"syscall"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
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

type boundaryHoleTemplate struct{ sparse.Source }

func (*boundaryHoleTemplate) Close() error { return nil }
func TestReadBoundaryDiffExt4FullTerminal(t *testing.T) {
	src, err := sparse.NewSource(bytes.NewReader(make([]byte, 4096)), 4096, []sparse.Extent{{Offset: 0, Size: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{io.EOF, syscall.EAGAIN} {
		t.Run(cause.Error(), func(t *testing.T) {
			base := NewStreamReader(context.Background(), boundaryExt4Stream{cause: readerr.Mark(cause, false)}, 4096)
			err := validateDiffSourceExt4(context.Background(), &boundaryHoleTemplate{Source: src}, base)
			if !readretry.IsTerminal(err) || !readerr.IsPermanent(err) || !errors.Is(err, cause) {
				t.Fatalf("diff ext4 accepted/lost terminal: %v", err)
			}
		})
	}
}

func TestDiffExt4FullOrdinaryEOFRemainsValid(t *testing.T) {
	src, err := sparse.NewSource(bytes.NewReader(make([]byte, 4096)), 4096, []sparse.Extent{{Size: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	base := NewStreamReader(context.Background(), boundaryExt4Stream{cause: io.EOF}, 4096)
	if err := validateDiffSourceExt4(context.Background(), &boundaryHoleTemplate{Source: src}, base); err != nil {
		t.Fatal(err)
	}
}
