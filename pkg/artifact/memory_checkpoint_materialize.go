package artifact

import (
	"context"
	"errors"
	"os"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// materializeMemoryCheckpointRoot converts the local single-layer view into
// portable Snapshot semantics. Only the original current top can be W; all
// internal history is a separate dirty H. This function is shared by normal
// publication and the rewrite planner, before either emits the public root.
func (p *Publisher) materializeMemoryCheckpointRoot(ctx context.Context, root *snapshotfile.Root) (_ *snapshotfile.Root, _ func() error, retErr error) {
	local := root.LocalCheckpoint
	if local == nil {
		return root, func() error { return nil }, nil
	}
	cfg, err := snapshot.ParseConfig(root.SnapshotConfig)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() error { return nil }
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, cleanup())
		}
	}()
	// The capture budget planner uses FileSink's established bounded carrier
	// fallback. Constructing this sink alone creates no output or directory.
	sink := snapshot.NewFileSink("", "memory-checkpoint-materialize", p.storage.LocalCodec(), p.storage.LocalRequired(), p.logf)
	defer func() { retErr = errors.Join(retErr, sink.Close()) }()
	mem, dirty := root.Memory, []byte(nil)
	coverage, values := sparse.Source(local.Top), sparse.Source(local.Top)
	if local.HistoryFull != nil {
		coverage = layerMemorySources(snapshot.DataCoverage(local.Top), local.HistoryFull)
		values = layerMemorySources(local.Top, local.HistoryFull)
	} else {
		coverage = snapshot.DataCoverage(local.Top)
	}
	if local.Descriptor.WorkingSet {
		mem, dirty = borrowedMemorySource{local.Top}, local.TopDirty
		coverage, values = snapshot.DataCoverage(local.Top), local.Top
		if local.History != nil {
			directory, err := os.MkdirTemp("", "sandbox-memory-checkpoint-")
			if err != nil {
				return nil, nil, err
			}
			cleanup = func() error { return os.RemoveAll(directory) }
			sink = snapshot.NewFileSink(directory, "memory-checkpoint-materialize", p.storage.LocalCodec(), p.storage.LocalRequired(), p.logf)
			historySource, _, err := snapshot.BuildCaptureSource(ctx, sink, local.History, nil,
				root.ConfigJSON, root.StateJSON, root.SnapshotConfig, local.HistoryFull, local.HistoryFull, nil, p.logf)
			if err != nil {
				return nil, nil, err
			}
			raw, _, err := sink.AbsorbSnapshot(ctx, historySource)
			if err != nil {
				return nil, nil, err
			}
			ref, err := manifest.ParseRef(raw)
			if err != nil {
				return nil, nil, err
			}
			location := "memory-checkpoint-temp-" + diffstore.NewDiffID()
			locations := make(config.RefLocations, len(p.locations)+1)
			for key, value := range p.locations {
				locations[key] = value
			}
			locations[location] = directory
			p.locations = locations
			if p.transientLocations == nil {
				p.transientLocations = make(map[string]bool)
			}
			p.transientLocations[location] = true
			ref.Location = location
			cfg.FromRefs = append([]string{ref.String()}, cfg.FromRefs...)
		}
	}
	encoded, err := snapshot.MarshalConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	source, _, err := snapshot.BuildCaptureSource(ctx, sink, mem, dirty, root.ConfigJSON, root.StateJSON,
		encoded, coverage, values, nil, p.logf)
	if err != nil {
		return nil, nil, err
	}
	materialized, err := snapshotfile.Open(ctx, borrowedMemorySource{source})
	if err != nil {
		return nil, nil, err
	}
	return materialized, cleanup, nil
}
