package snapshot

import (
	"context"
	"errors"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
)

// FrozenMemoryCapture is an internal lifecycle capability: Build writes only
// immutable metadata, and Commit transfers the frozen backing to its holder.
// It is never enabled for a VM that will resume in place.
type FrozenMemoryCapture struct {
	Build  func(context.Context, ArtifactSink, []byte, []byte, []byte, *memory.CaptureView) (sparse.Source, uint64, error)
	Commit func(*Outputs) error
}

var ErrFrozenMemoryCommitted = errors.New("frozen memory capture committed")
var ErrFrozenMemoryGeometry = errors.New("frozen memory top exceeds carrier budget")

type memoryPayload struct{ sparse.Source }

func (s memoryPayload) TarStreamDigest(string) ([32]byte, bool) { return [32]byte{}, false }

func (s memoryPayload) PayloadCommitment() (uint64, [32]byte, bool) {
	return s.Size(), [32]byte{}, false
}

// ValidateMemoryPayload walks metadata only, before retiring the VM. This
// ensures the async job cannot discover an unencodable sparse map after pause.
func ValidateMemoryPayload(ctx context.Context, source sparse.Source) error {
	if err := checkMemoryCarrier(ctx, memoryPayload{source}, memoryExtentBudget, memoryIndexBudget); err != nil {
		return errors.Join(ErrFrozenMemoryGeometry, err)
	}
	return nil
}
