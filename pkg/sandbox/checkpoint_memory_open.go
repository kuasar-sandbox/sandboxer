package sandbox

import (
	"context"
	"errors"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// openMemoryCheckpointSnapshot gives every memory-history consumer the same
// local descriptor resolution and owns a complete storage view for its reader.
func openMemoryCheckpointSnapshot(ctx context.Context, opts RunOptions, stream fetch.Stream, directory string) (*snapshotfile.Root, error) {
	storage, err := borrowRunStorage(opts)
	if err != nil {
		return nil, errors.Join(err, stream.Close())
	}
	return artifact.OpenOwnedSnapshot(ctx, storage, stream, artifact.SnapshotOpenOptions{
		LocalDirectory: directory, RefLocations: opts.RefLocations,
	})
}
