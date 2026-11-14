package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

type ExportView interface {
	sparse.Source
	io.Closer
}

// checkpointCapture tracks the operation root boundary, not its final error.
// Once Commit is attempted its outcome may be uncertain; conservative retention
// is essential because Close, Resume and response delivery can fail afterward.
type checkpointCapture struct {
	id      string
	created []DiskDiff
	retain  bool
}

func (c *checkpointCapture) close() error {
	if c == nil || c.retain {
		return nil
	}
	var err error
	for _, d := range c.created {
		err = errors.Join(err, d.DeleteCheckpoint(context.Background(), c.id))
	}
	return err
}

func validateCheckpointCapture(id string, portable *config.PortableSandboxConfig, disks []DiskDiff) error {
	if id == "" {
		return nil
	}
	if err := sandboxfile.ValidateCheckpointID(id); err != nil {
		return err
	}
	for i, d := range disks {
		if portable != nil && i > 0 && i <= len(portable.Boot.Disks) && portable.Boot.Disks[i-1].Unbound() && d.DiffID == "" {
			continue
		}
		if d.DiffID == "" || d.Checkpoint == nil || d.DeleteCheckpoint == nil || d.WaitMaterialized == nil {
			return fmt.Errorf("%w: disk %d", ctl.ErrCheckpointUnsupported, i)
		}
	}
	if portable == nil || !portable.IsCheckpoint() {
		return errors.New("checkpoint capture requires resolved immutable base graph")
	}
	return portable.Validate()
}

func captureCheckpointAtFreeze(ctx context.Context, s ExportSources, sink ArtifactSink, commitRoot bool) (*SandboxOutput, error) {
	c := s.checkpointState
	if c == nil {
		return nil, errors.New("checkpoint capture state missing")
	}
	refs := &sandboxfile.Checkpoint{Version: 1, CheckpointID: s.CheckpointID, Disks: make([]sandboxfile.CheckpointNamedDisk, 0, len(s.Diffs)-1)}
	for i, d := range s.Diffs {
		if i > 0 && s.PortableConfig.Boot.Disks[i-1].Slot != nil {
			refs.Version = 2
		}
		if i > 0 && s.PortableConfig.Boot.Disks[i-1].Unbound() {
			refs.Disks = append(refs.Disks, sandboxfile.CheckpointNamedDisk{Name: s.PortableConfig.Boot.Disks[i-1].Name})
			continue
		}
		// Backend freeze has stopped guest writes. Checkpoint drains admitted
		// writes and joins the template's final Sync/rename before registering.
		if err := d.Checkpoint(ctx, s.CheckpointID); err != nil {
			return nil, fmt.Errorf("checkpoint disk %d: %w", i, err)
		}
		c.created = append(c.created, d)
		if err := d.WaitMaterialized(ctx); err != nil {
			return nil, err
		}
		if i == 0 {
			refs.Root.DiffID = d.DiffID
		} else {
			refs.Disks = append(refs.Disks, sandboxfile.CheckpointNamedDisk{Name: s.PortableConfig.Boot.Disks[i-1].Name, DiffID: d.DiffID})
		}
	}
	source, err := sandboxfile.BuildCheckpointSource(ctx, s.PortableConfig, refs)
	if err != nil {
		return nil, err
	}
	out := &SandboxOutput{PortableConfig: s.PortableConfig}
	out.SandboxRef, out.SandboxPath, err = sink.AbsorbSandbox(ctx, source)
	if err != nil {
		return nil, err
	}
	if err := checkDiskCapture(ctx, s.Diffs); err != nil {
		return nil, err
	}
	if commitRoot {
		c.retain = true
		if err := sink.CommitSandbox(ctx, out.SandboxRef, out.SandboxPath); err != nil {
			return nil, err
		}
		out.SandboxRef, out.SandboxPath, err = CommittedArtifactRef(sink, out.SandboxRef, out.SandboxPath)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func prepareDiskSource(ctx context.Context, d DiskDiff, codec tarstream.Codec, required bool, opener MergeBaseOpener) (sparse.Source, func() error, error) {
	if d.Export != nil {
		v, err := d.Export(ctx)
		if err != nil {
			return nil, nil, err
		}
		if d.MergeBase == "" {
			return v, v.Close, nil
		}
		base, err := openMergeBaseStream(ctx, d.MergeBase, codec, required, opener)
		if err != nil {
			return nil, nil, errors.Join(err, v.Close())
		}
		if base.Size() < v.Size() {
			return nil, nil, errors.Join(fmt.Errorf("merge base: entry size %d < expected disk size %d", base.Size(), v.Size()), base.Close(), v.Close())
		}
		// Bound the parent to disk bytes so its ZIP tail cannot enter this
		// payload. Layering preserves Zero as an opaque run above older data.
		merged := fetch.NewLayered(v, &payloadMergeBase{Stream: base, size: v.Size(), ctx: ctx})
		return merged, merged.Close, nil
	}
	r, holes, close, err := prepareDiskCapture(ctx, d, codec, required, opener)
	if err != nil {
		return nil, nil, err
	}
	src, err := newSeekerSource(r, holes)
	if err != nil {
		return nil, nil, errors.Join(err, close())
	}
	return src, close, nil
}

func validateCaptureGraph(id string, p *config.PortableSandboxConfig, parent string, merged []bool, disks []DiskDiff) error {
	if id != "" {
		return validateCheckpointCapture(id, p, disks)
	}
	return ValidateExportGraph(p, parent, merged)
}
