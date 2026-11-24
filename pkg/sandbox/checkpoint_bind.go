package sandbox

import (
	"context"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func bindCheckpointDisks(refs []SnapDiskRef, disks []DiskBackend) []SnapDiskRef {
	for i := range refs {
		if i >= len(disks) || disks[i].Cow == nil {
			continue
		}
		cow := disks[i].Cow
		refs[i].Export = func(ctx context.Context) (snapshot.ExportView, error) { return cow.OpenActiveView(ctx) }
		refs[i].WaitMaterialized = cow.WaitMaterialized
		manager, enabled := cow.Checkpoints()
		if !enabled {
			continue
		}
		id, _, _ := manager.CheckpointInfo()
		refs[i].DiffID = id
		refs[i].TemporaryCheckpoint = func(ctx context.Context, id string) error {
			return cow.TemporaryCheckpoint(ctx, diffstore.CheckpointID(id))
		}
		refs[i].Checkpoint = func(ctx context.Context, id string) error { return manager.Checkpoint(ctx, diffstore.CheckpointID(id)) }
		refs[i].OpenCheckpoint = func(ctx context.Context, id string) (snapshot.ExportView, error) {
			return manager.OpenCheckpoint(ctx, diffstore.CheckpointID(id))
		}
		refs[i].DeleteCheckpoint = func(ctx context.Context, id string) error {
			return manager.DeleteCheckpoint(ctx, diffstore.CheckpointID(id))
		}
		refs[i].DeleteCheckpoints = func(ctx context.Context, ids []string) error {
			versions := make([]diffstore.CheckpointID, len(ids))
			for j, id := range ids {
				versions[j] = diffstore.CheckpointID(id)
			}
			return manager.DeleteCheckpoints(ctx, versions)
		}
		refs[i].ListCheckpoints = func(ctx context.Context) ([]string, error) {
			ids, err := manager.ListCheckpoints(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]string, len(ids))
			for j, id := range ids {
				out[j] = string(id)
			}
			return out, nil
		}
	}
	return refs
}
