package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// memoryGraphOwner owns every opened backing exactly once, in a flat list.
// Closing a long history cannot recurse through a chain of child Root owners.
type memoryGraphOwner struct {
	closers     []io.Closer
	directories map[string]bool
	once        sync.Once
	err         error
}

func (g *memoryGraphOwner) Close() error {
	g.once.Do(func() {
		for i := len(g.closers) - 1; i >= 0; i-- {
			if g.closers[i] != nil {
				g.err = errors.Join(g.err, g.closers[i].Close())
			}
		}
		g.closers = nil
	})
	return g.err
}

func (g *memoryGraphOwner) pin(directory string) error {
	if g.directories[directory] {
		return nil
	}
	pin, err := memorycheckpoint.PinDirectory(directory)
	if err != nil {
		return err
	}
	if g.directories == nil {
		g.directories = make(map[string]bool)
	}
	g.directories[directory] = true
	g.closers = append(g.closers, pin)
	return nil
}

type memoryOpenNode struct {
	config        *snapshot.Config
	values, dirty *memoryIndex
	local         bool
}

type memoryOpenFrame struct {
	descriptor      *memorycheckpoint.Descriptor
	metadata        *snapshotfile.Metadata
	config          *snapshot.Config
	identity        string
	options         SnapshotOpenOptions
	openFile        func(context.Context, string) (fetch.Stream, error)
	top, candidates *memoryIndex
	topDirty        []byte
	parents         []*memoryOpenNode
}

// OpenSnapshot is the common execution/artifact reader. It takes ownership of
// stream on every path. Ordinary Snapshot parsing and memory semantics remain
// those of snapshotfile.Open. Local history is resolved in postorder without a
// checkpoint-count limit, payload copies or recursive read/close operations.
func (s *ProcessStorage) OpenSnapshot(ctx context.Context, stream fetch.Stream, opts SnapshotOpenOptions) (_ *snapshotfile.Root, retErr error) {
	if stream == nil {
		return nil, errors.New("snapshot: nil stream")
	}
	descriptor, err := snapshotfile.ReadLocalDescriptor(ctx, stream)
	if err != nil {
		return nil, errors.Join(err, stream.Close())
	}
	if descriptor == nil {
		return snapshotfile.Open(ctx, stream)
	}
	owners := &memoryGraphOwner{}
	transferred := false
	defer func() {
		if !transferred {
			retErr = errors.Join(retErr, owners.Close(), stream.Close())
		}
	}()
	if s == nil {
		return nil, errors.New("memory checkpoint requires process storage")
	}
	root, err := s.newMemoryOpenFrame(ctx, stream, descriptor, opts, owners, true)
	if err != nil {
		return nil, err
	}
	stack := []*memoryOpenFrame{root}
	active := map[string]bool{root.identity: true}
	for len(stack) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		frame := stack[len(stack)-1]
		if len(frame.parents) < len(frame.descriptor.Parents) {
			i := len(frame.parents)
			parentStream, err := frame.openFile(ctx, frame.descriptor.Parents[i])
			if err != nil {
				return nil, fmt.Errorf("memory checkpoint parent %d: %w", i, err)
			}
			parentDescriptor, err := snapshotfile.ReadLocalDescriptor(ctx, parentStream)
			if err != nil {
				return nil, errors.Join(err, parentStream.Close())
			}
			if parentDescriptor == nil {
				parent, err := openOrdinaryMemoryNode(ctx, parentStream, owners)
				if err != nil {
					return nil, err
				}
				frame.parents = append(frame.parents, parent)
				continue
			}
			parent, err := s.newMemoryOpenFrame(ctx, parentStream, parentDescriptor, frame.options, owners, false)
			// Only the selected root exposes its descriptor carrier. Historical
			// carriers are metadata-only and need no open fd after this read.
			if err = errors.Join(err, parentStream.Close()); err != nil {
				return nil, err
			}
			if active[parent.identity] {
				return nil, errors.New("memory checkpoint history cycle")
			}
			active[parent.identity] = true
			stack = append(stack, parent)
			continue
		}
		node, view, err := frame.compose(ctx)
		if err != nil {
			return nil, err
		}
		delete(active, frame.identity)
		stack = stack[:len(stack)-1]
		if len(stack) != 0 {
			parent := stack[len(stack)-1]
			parent.parents = append(parent.parents, node)
			continue
		}
		transferred = true
		return snapshotfile.ResolveLocal(stream, root.metadata, node.values, view, owners)
	}
	return nil, errors.New("memory checkpoint: empty resolution stack")
}

func (s *ProcessStorage) newMemoryOpenFrame(ctx context.Context, stream fetch.Stream, descriptor *memorycheckpoint.Descriptor, opts SnapshotOpenOptions, owners *memoryGraphOwner, execution bool) (*memoryOpenFrame, error) {
	if opened, ok := stream.(interface{ ResolvedPath() string }); ok && opened.ResolvedPath() != "" {
		opts.LocalDirectory = filepath.Dir(opened.ResolvedPath())
	}
	if opts.LocalDirectory == "" {
		return nil, errors.New("memory checkpoint has no local carrier directory")
	}
	directory, err := filepath.Abs(opts.LocalDirectory)
	if err == nil {
		directory, err = filepath.EvalSymlinks(directory)
	}
	if err != nil {
		return nil, err
	}
	opts.LocalDirectory = directory
	if err := owners.pin(directory); err != nil {
		return nil, err
	}
	var metadata *snapshotfile.Metadata
	if execution {
		metadata, err = snapshotfile.ReadMetadata(ctx, stream)
	} else {
		// CPU/state bodies belong only to the selected execution root, never
		// to its internal historical memory graph.
		var cfg []byte
		cfg, err = snapshotfile.ReadConfig(ctx, stream)
		metadata = &snapshotfile.Metadata{SnapshotConfig: cfg, LocalCheckpoint: descriptor}
	}
	if err != nil {
		return nil, err
	}
	cfg, err := canonicalMemoryConfig(metadata.SnapshotConfig)
	if err != nil {
		return nil, err
	}
	openFile := func(ctx context.Context, raw string) (fetch.Stream, error) {
		ref, err := manifest.ParseRef(raw)
		if err != nil || ref.Scheme != manifest.RefSchemeFile || ref.Digest == "" {
			return nil, errors.New("memory checkpoint requires a content-identified local file ref")
		}
		path, err := opts.RefLocations.ResolveFile(ref, directory)
		if err != nil {
			return nil, err
		}
		return s.OpenFileWithLocations(ctx, path, ref, opts.RefLocations)
	}
	backing, err := memorycheckpoint.OpenTop(ctx, *descriptor, memorycheckpoint.OpenOptions{Directory: directory, OpenFile: openFile})
	if err != nil {
		return nil, err
	}
	backingOwner := len(owners.closers)
	owners.closers = append(owners.closers, backing)
	topSource, dirty, err := snapshot.CaptureMemory(backing.Reader, backing.Map, descriptor.WorkingSet)
	if err != nil {
		return nil, err
	}
	top, err := indexMemoryLeaf(ctx, topSource)
	if err != nil {
		return nil, err
	}
	candidates, err := snapshot.DirtyMemory(memoryIndexBranch{top}, dirty)
	if err != nil {
		return nil, err
	}
	indexedCandidates, err := indexMemoryComposite(ctx, candidates)
	if err != nil {
		return nil, err
	}
	// The index retains leaf ReaderAt/Run values, not CaptureView's page maps.
	// Historical dense maps can be reclaimed immediately after compilation.
	backing.Map = nil
	hasData := false
	for _, run := range top.runs {
		hasData = hasData || run.kind == sparse.Data
	}
	if !hasData {
		// Holes and explicit zeros need no RAM or payload fd. Directory pins
		// protect the still-being-opened graph independently of data readers.
		if err := backing.Close(); err != nil {
			return nil, err
		}
		owners.closers[backingOwner] = nil
	}
	if !execution {
		dirty = nil
	}
	return &memoryOpenFrame{descriptor: descriptor, metadata: metadata, config: cfg,
		identity: filepath.Join(directory, descriptor.MemoryID), options: opts, openFile: openFile,
		top: top, candidates: indexedCandidates, topDirty: dirty}, nil
}

func canonicalMemoryConfig(raw []byte) (*snapshot.Config, error) {
	cfg, err := snapshot.ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	canonical, err := snapshot.MarshalConfig(cfg)
	if err != nil || string(canonical) != string(raw) {
		return nil, errors.New("memory checkpoint snapshot.cfg is not canonical")
	}
	return cfg, nil
}

func openOrdinaryMemoryNode(ctx context.Context, stream fetch.Stream, owners *memoryGraphOwner) (*memoryOpenNode, error) {
	root, err := snapshotfile.Open(ctx, stream)
	if err != nil {
		return nil, err
	}
	// FullStream owns the carrier without retaining the historical CPU/state
	// or dirty metadata slices stored in Root.
	owners.closers = append(owners.closers, root.FullStream)
	cfg, err := canonicalMemoryConfig(root.SnapshotConfig)
	if err != nil {
		return nil, err
	}
	values, err := indexMemoryLeaf(ctx, root.Memory)
	if err != nil {
		return nil, err
	}
	dirty, err := snapshot.DirtyMemory(memoryIndexBranch{values}, root.MemoryDirty)
	if err != nil {
		return nil, err
	}
	candidates, err := indexMemoryComposite(ctx, dirty)
	if err != nil {
		return nil, err
	}
	return &memoryOpenNode{config: cfg, values: values, dirty: candidates}, nil
}

func (f *memoryOpenFrame) compose(ctx context.Context) (*memoryOpenNode, *snapshotfile.LocalCheckpointView, error) {
	var parentValues, parentCandidates []*memoryIndex
	for i, parent := range f.parents {
		if parent.values.Size() != f.descriptor.Size {
			return nil, nil, errors.New("memory checkpoint parent size mismatch")
		}
		expected := f.config.FromRefs
		if !parent.local {
			expected = append(append([]string(nil), f.descriptor.Parents[i+1:]...), expected...)
		}
		candidate := parent.dirty
		if !SnapshotParentViewEqual(parent.config.FromRefs, expected) {
			// Dirty is a hint relative to the complete ordered suffix. Preserve
			// every explicit value if that suffix cannot be proved.
			candidate = parent.values
		}
		parentValues = append(parentValues, parent.values)
		parentCandidates = append(parentCandidates, candidate)
	}
	var history, historyFull *memoryIndex
	var err error
	if len(parentValues) != 0 {
		historyFull, err = indexMemoryComposite(ctx, layerMemoryIndexes(parentValues))
		if err != nil {
			return nil, nil, err
		}
		selected, err := snapshot.SelectMemory(memoryIndexBranch{historyFull}, layerMemoryIndexes(parentCandidates))
		if err != nil {
			return nil, nil, err
		}
		history, err = indexMemoryComposite(ctx, selected)
		if err != nil {
			return nil, nil, err
		}
	}
	values := f.top
	candidates := sparse.Source(memoryIndexBranch{f.candidates})
	if history != nil {
		values, err = indexMemoryComposite(ctx, layerMemoryIndexes([]*memoryIndex{f.top, history}))
		if err != nil {
			return nil, nil, err
		}
		candidates = layerMemoryIndexes([]*memoryIndex{f.candidates, history})
	}
	selected, err := snapshot.SelectMemory(memoryIndexBranch{values}, candidates)
	if err != nil {
		return nil, nil, err
	}
	dirty, err := indexMemoryComposite(ctx, selected)
	if err != nil {
		return nil, nil, err
	}
	view := &snapshotfile.LocalCheckpointView{Descriptor: *f.descriptor,
		Top: f.top, TopDirty: f.topDirty, Dirty: dirty}
	if history != nil {
		view.History, view.HistoryFull = history, historyFull
	}
	return &memoryOpenNode{config: f.config, values: values, dirty: dirty, local: true}, view, nil
}
