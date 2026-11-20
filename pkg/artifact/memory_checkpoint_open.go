package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	mu          sync.Mutex // file-switch callbacks can register directories during traversal
	closers     []io.Closer
	pins        []io.Closer
	pool        *checkpointFilePool
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
		if g.pool != nil {
			g.err = errors.Join(g.err, g.pool.Close())
		}
		for i := len(g.pins) - 1; i >= 0; i-- {
			g.err = errors.Join(g.err, g.pins[i].Close())
		}
		g.pins = nil
	})
	return g.err
}

// pin serializes registration with asynchronous RAM-to-file switch callbacks.
// An external source without a pre-existing GC lock retains its original open
// FD. Remember that decision even if another opener later creates a GC lock.
func (g *memoryGraphOwner) pin(directory string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if pinned, seen := g.directories[directory]; seen {
		return pinned, nil
	}
	pin, err := memorycheckpoint.PinExistingDirectory(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if g.directories == nil {
		g.directories = make(map[string]bool)
	}
	pinned := err == nil
	g.directories[directory] = pinned
	if pinned {
		g.pins = append(g.pins, pin)
	}
	return pinned, nil
}

type memoryOpenNode struct {
	config                    *snapshot.Config
	values, candidates, dirty sparse.Source
	local                     bool
}

type memoryOpenFrame struct {
	descriptor      *memorycheckpoint.Descriptor
	metadata        *snapshotfile.Metadata
	config          *snapshot.Config
	identity        string
	options         SnapshotOpenOptions
	openFile        func(context.Context, string) (fetch.Stream, error)
	top, candidates sparse.Source
	topDirty        []byte
	topEmpty        bool
	parents         []*memoryOpenNode
}

// OpenSnapshot is the common execution/artifact reader. It takes ownership of
// stream on every path. Ordinary Snapshot parsing and memory semantics remain
// those of snapshotfile.Open. Local history is resolved in postorder without a
// checkpoint-count limit or payload copies. Data selection uses the existing
// fetch/snapshot operators; ownership closes iteratively.
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
	pool := newCheckpointFilePool()
	owners := &memoryGraphOwner{pool: pool}
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
	if _, err := owners.pin(directory); err != nil {
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
		return s.openFile(ctx, path, ref, opts.RefLocations, false, func(physical string) (*checkpointFilePool, error) {
			pinned, err := owners.pin(filepath.Dir(physical))
			if err != nil || !pinned {
				return nil, err
			}
			return owners.pool, nil
		})
	}
	backing, err := memorycheckpoint.OpenTop(ctx, *descriptor, memorycheckpoint.OpenOptions{Directory: directory, OpenFile: openFile, SkipDirectoryPin: true})
	if err != nil {
		return nil, err
	}
	backingOwner := len(owners.closers)
	owners.closers = append(owners.closers, backing)
	topSource, dirty, err := snapshot.CaptureMemory(backing.Reader, backing.Map, descriptor.WorkingSet)
	if err != nil {
		return nil, err
	}
	candidates, err := snapshot.DirtyMemory(topSource, dirty)
	if err != nil {
		return nil, err
	}
	// CaptureMemory owns the fixed maps; only actual data needs the backing.
	hasData, allHole := false, true
	for off := uint64(0); off < topSource.Size(); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		run, err := topSource.RunAt(off, topSource.Size()-off)
		if err != nil {
			return nil, err
		}
		allHole = allHole && run.Kind() == sparse.Hole
		if run.Kind() == sparse.Data {
			hasData = true
			break
		}
		off = run.End()
	}
	if !hasData {
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
		top: topSource, candidates: candidates, topDirty: dirty, topEmpty: allHole}, nil
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
	candidates, err := snapshot.DirtyMemory(root.Memory, root.MemoryDirty)
	if err != nil {
		return nil, err
	}
	return &memoryOpenNode{config: cfg, values: root.Memory, candidates: candidates, dirty: candidates}, nil
}

func (f *memoryOpenFrame) compose(ctx context.Context) (*memoryOpenNode, *snapshotfile.LocalCheckpointView, error) {
	var parentValues, parentCandidates []sparse.Source
	var singleHistory sparse.Source
	for i, parent := range f.parents {
		if parent.values.Size() != f.descriptor.Size {
			return nil, nil, errors.New("memory checkpoint parent size mismatch")
		}
		expected := f.config.FromRefs
		if !parent.local {
			expected = append(append([]string(nil), f.descriptor.Parents[i+1:]...), expected...)
		}
		candidate := parent.candidates
		singleHistory = parent.dirty
		if !SnapshotParentViewEqual(parent.config.FromRefs, expected) {
			// Dirty is a hint relative to the complete ordered suffix. Preserve
			// every explicit value if that suffix cannot be proved.
			candidate = parent.values
			singleHistory = parent.values
		}
		parentValues = append(parentValues, parent.values)
		parentCandidates = append(parentCandidates, candidate)
	}
	var history, historyFull, historyCandidates sparse.Source
	var err error
	if len(parentValues) != 0 {
		historyFull = layerMemorySources(parentValues...)
		historyCandidates = layerMemorySources(parentCandidates...)
		if len(parentValues) == 1 && singleHistory != nil {
			history = singleHistory
		} else {
			history, err = snapshot.SelectMemory(historyFull, historyCandidates)
		}
		if err != nil {
			return nil, nil, err
		}
	}
	values, candidates := f.top, f.candidates
	if history != nil {
		values = layerMemorySources(f.top, history)
		// Candidates carry addresses only. Reusing the selected value expression
		// here doubles traversal at each generation. Every candidate already
		// has a value in its parent's view, so this preserves exact coverage.
		candidates = layerMemorySources(f.candidates, historyCandidates)
	}
	var dirty sparse.Source
	if f.topEmpty && history != nil {
		// An all-Hole capture leaves the already-selected history unchanged.
		// Reuse that expression instead of wrapping it in another identity selection.
		values, dirty, candidates = history, history, historyCandidates
	} else {
		dirty, err = snapshot.SelectMemory(values, candidates)
	}
	if err != nil {
		return nil, nil, err
	}
	view := &snapshotfile.LocalCheckpointView{Descriptor: *f.descriptor,
		Top: f.top, TopDirty: f.topDirty, Dirty: dirty,
		History: history, HistoryFull: historyFull}
	return &memoryOpenNode{config: f.config, values: values, candidates: candidates, dirty: dirty, local: true}, view, nil
}
