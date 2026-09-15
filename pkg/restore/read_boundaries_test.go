package restore

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
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
	err := validateRestoreExt4Reader(context.Background(), base)
	if !readretry.IsTerminal(err) || !readerr.IsPermanent(err) || !errors.Is(err, cause) {
		t.Fatalf("restore ext4 accepted/lost terminal: %v", err)
	}
}

func TestExt4FullOrdinaryEOFRemainsValid(t *testing.T) {
	base := vhost.NewStreamReader(context.Background(), boundaryExt4Stream{cause: io.EOF}, 4096)
	if err := validateRestoreExt4Reader(context.Background(), base); err != nil {
		t.Fatal(err)
	}
}
