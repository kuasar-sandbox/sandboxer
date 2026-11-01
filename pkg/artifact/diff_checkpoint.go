package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
)

// DiffCheckpointRef is a stable, exact disk/version identity. Deletion never
// re-resolves a capture name after the set has been selected.
type DiffCheckpointRef struct {
	DiffID string `json:"diff_id"`
	ID     string `json:"id"`
}

type checkpointPublication struct {
	directory string
	identity  os.FileInfo
	refs      []DiffCheckpointRef
}

type publishCompletion struct {
	mu     sync.Mutex
	closed bool
}

// PinnedDiffCheckpoints holds all source versions before a restore acquires a
// target writer. Views are sequential sparse sources, not active BlockCOWs.
type PinnedDiffCheckpoints struct {
	Directory string
	Root      *diffstore.View
	Disks     map[string]*diffstore.View
	Refs      []DiffCheckpointRef
	identity  os.FileInfo
	once      sync.Once
	err       error
}

func (p *PinnedDiffCheckpoints) Close() error {
	if p == nil {
		return nil
	}
	p.once.Do(func() {
		for _, view := range p.Disks {
			p.err = errors.Join(p.err, view.Close())
		}
		if p.Root != nil {
			p.err = errors.Join(p.err, p.Root.Close())
		}
	})
	return p.err
}

func (s *ProcessStorage) PinDiffCheckpoints(ctx context.Context, directory string, cp *sandboxfile.Checkpoint) (_ *PinnedDiffCheckpoints, retErr error) {
	if s == nil || cp == nil {
		return nil, errors.New("checkpoint: storage and metadata are required")
	}
	dir, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	identity, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !identity.IsDir() {
		return nil, errors.New("checkpoint source is not a directory")
	}
	p := &PinnedDiffCheckpoints{Directory: dir, identity: identity, Disks: make(map[string]*diffstore.View)}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, p.Close())
		}
	}()
	var key *[32]byte
	if s.CustomerKeyFunc() != nil {
		k, err := s.CustomerKeyFunc()()
		if err != nil {
			return nil, err
		}
		key = &k
		defer clear(k[:])
	}
	open := func(id string) (*diffstore.View, error) {
		path, err := checkpointDiffPath(dir, id)
		if err != nil {
			return nil, err
		}
		view, err := diffstore.OpenExport(ctx, path, cp.CheckpointID, key)
		if err != nil {
			return nil, err
		}
		if view.Info().DiffID != id {
			return nil, errors.Join(errors.New("checkpoint DiffID does not match its carrier"), view.Close())
		}
		if s.LocalRequired() && !view.Info().Encrypted {
			return nil, errors.Join(diffstore.ErrPlaintextForbidden, view.Close())
		}
		p.Refs = append(p.Refs, DiffCheckpointRef{DiffID: id, ID: cp.CheckpointID})
		return view, nil
	}
	p.Root, err = open(cp.Root.DiffID)
	if err != nil {
		return nil, err
	}
	for _, disk := range cp.Disks {
		if _, exists := p.Disks[disk.Name]; exists {
			return nil, errors.New("checkpoint duplicate disk name")
		}
		view, err := open(disk.DiffID)
		if err != nil {
			return nil, err
		}
		p.Disks[disk.Name] = view
	}
	return p, nil
}

func checkpointDiffPath(directory, id string) (string, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") || len(id) > 128 {
		return "", errors.New("checkpoint invalid DiffID")
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", errors.New("checkpoint invalid DiffID")
		}
	}
	path := filepath.Join(directory, "diffs", id+".diff")
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("checkpoint DIFF is not a regular file")
	}
	return path, nil
}

func (s *ProcessStorage) ListDiffCheckpoints(ctx context.Context, directory string) (_ []DiffCheckpointRef, retErr error) {
	owner, err := diffstore.LockDirectory(directory)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, owner.Close()) }()
	return listDirectoryDiffCheckpoints(ctx, owner.Directory)
}

func listDirectoryDiffCheckpoints(ctx context.Context, directory string) ([]DiffCheckpointRef, error) {
	entries, err := os.ReadDir(filepath.Join(directory, "diffs"))
	if err != nil {
		return nil, err
	}
	refs := make([]DiffCheckpointRef, 0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(entry.Name(), ".diff") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".diff")
		path, err := checkpointDiffPath(directory, id)
		if err != nil {
			return nil, err
		}
		store, err := diffstore.OpenMetadata(path)
		if err != nil {
			return nil, err
		}
		ids, listErr := store.ListCheckpoints(ctx)
		if errors.Is(listErr, diffstore.ErrRetired) {
			listErr = nil
		}
		if store.Info().DiffID != id {
			listErr = errors.Join(listErr, errors.New("checkpoint DiffID mismatch"))
		}
		if err := errors.Join(listErr, store.Close()); err != nil {
			return nil, err
		}
		for _, checkpoint := range ids {
			refs = append(refs, DiffCheckpointRef{DiffID: id, ID: checkpoint})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].DiffID == refs[j].DiffID {
			return refs[i].ID < refs[j].ID
		}
		return refs[i].DiffID < refs[j].DiffID
	})
	return refs, nil
}

func (s *ProcessStorage) DeleteDiffCheckpoint(ctx context.Context, directory string, ref DiffCheckpointRef) (retErr error) {
	owner, err := diffstore.LockDirectory(directory)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, owner.Close()) }()
	return deleteDirectoryDiffCheckpoints(ctx, owner.Directory, []DiffCheckpointRef{ref})
}

func deleteDirectoryDiffCheckpoints(ctx context.Context, directory string, refs []DiffCheckpointRef) error {
	grouped := make(map[string][]string)
	for _, ref := range refs {
		if err := sandboxfile.ValidateCheckpointID(ref.ID); err != nil {
			return err
		}
		grouped[ref.DiffID] = append(grouped[ref.DiffID], ref.ID)
	}
	var result error
	for id, ids := range grouped {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		path, err := checkpointDiffPath(directory, id)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		store, err := diffstore.OpenMetadata(path)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if store.Info().DiffID != id {
			err = errors.New("checkpoint DiffID mismatch")
		} else {
			err = store.DeleteCheckpoints(ctx, ids)
		}
		if errors.Is(err, diffstore.ErrRetired) {
			err = nil
		}
		result = errors.Join(result, err, store.Close())
	}
	return result
}

// DeletePublishedDiffCheckpoints is opt-in cleanup of the exact pinned sources.
// It requires both successful publication and a successful Publisher.Close.
// A busy directory is routed to its live owner and is never mutated directly.
func (s *ProcessStorage) DeletePublishedDiffCheckpoints(ctx context.Context, result PublishResult) error {
	if len(result.checkpointSources) == 0 {
		return nil
	}
	if result.Ref == "" || result.completion == nil {
		return errors.New("checkpoint cleanup requires completed publication")
	}
	result.completion.mu.Lock()
	closed := result.completion.closed
	result.completion.mu.Unlock()
	if !closed {
		return errors.New("checkpoint cleanup requires successful Publisher.Close")
	}
	var errs error
	for _, source := range result.checkpointSources {
		err := deletePublishedDiffSource(ctx, source)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("checkpoint cleanup directory %s references %+v: %w", source.directory, source.refs, err))
		}
	}
	return errs
}

// Retain the actual directory across ownership acquisition and routing retries.
// Resolving the original name again could select a replacement after rename.
func deletePublishedDiffSource(ctx context.Context, source checkpointPublication) (retErr error) {
	directory, err := os.Open(source.directory)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, directory.Close()) }()
	current, err := directory.Stat()
	if err != nil || !os.SameFile(source.identity, current) {
		return errors.Join(errors.New("checkpoint source directory identity changed"), err)
	}
	pinnedPath := fmt.Sprintf("/proc/self/fd/%d", directory.Fd())
	owner, err := diffstore.LockDirectory(pinnedPath)
	if errors.Is(err, diffstore.ErrBusy) {
		refs := make([]ctl.DiffCheckpointRef, len(source.refs))
		for i, ref := range source.refs {
			refs[i] = ctl.DiffCheckpointRef{DiffID: ref.DiffID, ID: ref.ID}
		}
		return ctl.DeleteOwnedDiffCheckpoints(ctx, pinnedPath, refs)
	}
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, owner.Close()) }()
	current, err = os.Stat(owner.Directory)
	if err != nil || !os.SameFile(source.identity, current) {
		return errors.Join(errors.New("checkpoint source directory identity changed"), err)
	}
	return deleteDirectoryDiffCheckpoints(ctx, pinnedPath, source.refs)
}

// ReopenIDs checks which pinned source writers still match the live epoch.
// Without the target directory owner this is only a read-only candidate query:
// it provides no stability guarantee. A caller acquiring writers must repeat
// the check with the owner held after ownership transfer; an earlier result or
// pinned Info alone is insufficient if a former owner wrote before exiting.
func (p *PinnedDiffCheckpoints) ReopenIDs(directory string, cfg *config.PortableSandboxConfig) ([]string, error) {
	if p == nil || cfg == nil || !cfg.IsCheckpoint() {
		return nil, errors.New("checkpoint reopen requires pinned source and checkpoint graph")
	}
	physical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	if physical != p.Directory {
		return nil, nil
	}
	current, err := os.Stat(physical)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(current, p.identity) {
		return nil, errors.New("checkpoint source directory identity changed")
	}
	if len(p.Refs) == 0 {
		return nil, errors.New("checkpoint source has no pinned identities")
	}
	ids := []string{p.Root.Info().DiffID}
	for _, disk := range cfg.Boot.Disks {
		view := p.Disks[disk.Name]
		if view == nil {
			return nil, errors.New("checkpoint pinned disk missing")
		}
		ids = append(ids, view.Info().DiffID)
	}
	unchanged := true
	for i, id := range ids {
		path, err := checkpointDiffPath(directory, id)
		if err != nil {
			return nil, err
		}
		info, err := diffstore.Probe(path)
		if err != nil {
			return nil, err
		}
		view := p.Root
		base := cfg.Boot.Root
		if i > 0 {
			disk := cfg.Boot.Disks[i-1]
			view = p.Disks[disk.Name]
			base = disk.PortableRootConfig
		}
		if info.DiffID != id || info.BaseID != DiffCheckpointBaseID(base) || info.LogicalSize != view.Info().LogicalSize {
			return nil, errors.New("checkpoint disk identity/capacity/base mismatch")
		}
		if info.CurrentCheckpoint != p.Refs[0].ID {
			unchanged = false
		}
	}
	if !unchanged {
		return nil, nil
	}
	return ids, nil
}

func cleanupDiffVersionSet(ctx context.Context, directory string, keep map[string]map[string]bool) error {
	entries, err := os.ReadDir(filepath.Join(directory, "diffs"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	type candidate struct {
		id, path string
		remove   []string
	}
	var plans []candidate
	found := map[string]bool{}
	// Validate the complete selected keep set before the first mutation.
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".diff") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".diff")
		path, err := checkpointDiffPath(directory, id)
		if err != nil {
			return err
		}
		meta, err := diffstore.OpenMetadata(path)
		if err != nil {
			return err
		}
		ids, listErr := meta.ListCheckpoints(ctx)
		if errors.Is(listErr, diffstore.ErrRetired) && len(keep[id]) == 0 {
			listErr = nil
		}
		if meta.Info().DiffID != id {
			listErr = errors.Join(listErr, errors.New("checkpoint DIFF identity differs from its filename"))
		}
		err = errors.Join(listErr, meta.Close())
		if err != nil {
			return err
		}
		existing := map[string]bool{}
		plan := candidate{id: id, path: path}
		for _, cp := range ids {
			existing[cp] = true
			if !keep[id][cp] {
				plan.remove = append(plan.remove, cp)
			}
		}
		for cp := range keep[id] {
			if !existing[cp] {
				return fmt.Errorf("checkpoint retained version %s/%s is missing", id, cp)
			}
		}
		found[id] = true
		plans = append(plans, plan)
	}
	for id := range keep {
		if !found[id] {
			return fmt.Errorf("checkpoint retained DIFF %s is missing", id)
		}
	}
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta, err := diffstore.OpenMetadata(plan.path)
		if err != nil {
			return err
		}
		if len(keep[plan.id]) == 0 {
			err = meta.Retire(ctx)
		} else if len(plan.remove) > 0 {
			err = meta.DeleteCheckpoints(ctx, plan.remove)
		}
		if err = errors.Join(err, meta.Close()); err != nil {
			return err
		}
	}
	return nil
}
