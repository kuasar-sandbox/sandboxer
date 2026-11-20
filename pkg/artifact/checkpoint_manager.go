package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

var ErrCheckpointReferenced = errors.New("checkpoint is required by a durable source")

func validateCheckpointRef(ref CheckpointRef) error {
	if err := sandboxfile.ValidateCheckpointID(ref.ID); err != nil {
		return err
	}
	if (ref.DiffID == "") == (ref.MemoryID == "") {
		return errors.New("checkpoint requires exactly one DiffID or MemoryID")
	}
	if ref.MemoryID != "" {
		return (memorycheckpoint.Reference{CheckpointID: ref.ID, MemoryID: ref.MemoryID}).Validate()
	}
	// The path helper also validates the exact disk identity before any mutation.
	_, err := checkpointDiffPath("", ref.DiffID)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// CheckpointView keeps the native typed views, including memory Top, History,
// HistoryFull and Dirty. Close releases the pins; deletion does not close it.
type CheckpointView struct {
	Disk   *diffstore.View
	Memory *snapshotfile.Root
}

func (v *CheckpointView) Close() error {
	if v == nil {
		return nil
	}
	var err error
	if v.Disk != nil {
		err = v.Disk.Close()
	}
	if v.Memory != nil {
		err = errors.Join(err, v.Memory.Close())
	}
	return err
}

func (s *ProcessStorage) OpenCheckpoint(ctx context.Context, directory string, ref CheckpointRef) (*CheckpointView, error) {
	if err := validateCheckpointRef(ref); err != nil {
		return nil, err
	}
	if ref.DiffID != "" {
		view, err := s.openCheckpointVersion(ctx, directory, ref.ID, ref.DiffID)
		if err != nil {
			return nil, err
		}
		return &CheckpointView{Disk: view}, nil
	}
	// Pin before looking up metadata so physical collection cannot race this open.
	pin, err := memorycheckpoint.PinDirectory(directory)
	if err != nil {
		return nil, err
	}
	defer pin.Close()
	record, err := memorycheckpoint.ReadRecord(directory, ref.ID, ref.MemoryID)
	if err != nil {
		return nil, err
	}
	if record.Deleted {
		return nil, memorycheckpoint.ErrDeleted
	}
	if !record.Committed || record.Aborted {
		return nil, errors.New("checkpoint is not committed")
	}
	parsed, err := manifest.ParseRef(record.SnapshotRef)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != manifest.RefSchemeFile || parsed.Location != "" {
		return nil, errors.New("checkpoint has no local descriptor")
	}
	path := parsed.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(directory, path)
	}
	stream, err := s.OpenFile(ctx, path, parsed)
	if err != nil {
		return nil, err
	}
	view, err := s.OpenSnapshot(ctx, stream, SnapshotOpenOptions{})
	if err != nil {
		return nil, err
	}
	return &CheckpointView{Memory: view}, nil
}

func (s *ProcessStorage) ListCheckpoints(ctx context.Context, directory string) (_ []CheckpointRef, retErr error) {
	owner, err := diffstore.LockDirectory(directory)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, owner.Close()) }()
	refs, err := listDirectoryDiffCheckpoints(ctx, owner.Directory)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	memory, err := MemoryCheckpointRefs(owner.Directory)
	if err != nil {
		return nil, err
	}
	refs = append(refs, memory...)
	sortCheckpointRefs(refs)
	return refs, nil
}
func MemoryCheckpointRefs(directory string) ([]CheckpointRef, error) {
	records, err := memorycheckpoint.ListCheckpoints(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	refs := make([]CheckpointRef, 0, len(records))
	for _, record := range records {
		refs = append(refs, CheckpointRef{ID: record.CheckpointID, MemoryID: record.MemoryID})
	}
	return refs, nil
}
func sortCheckpointRefs(refs []CheckpointRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ID != refs[j].ID {
			return refs[i].ID < refs[j].ID
		}
		if refs[i].DiffID != refs[j].DiffID {
			return refs[i].DiffID < refs[j].DiffID
		}
		return refs[i].MemoryID < refs[j].MemoryID
	})
}

// DeleteCheckpoints checks the complete source graph before revoking the exact
// selected set. Retry this same set after partial failure. diskDelete is the
// live DirectoryOwner adapter; nil selects offline ownership. It must not
// publish source edges or acquire this source fence again.
func (s *ProcessStorage) DeleteCheckpoints(ctx context.Context, c Checkpoint, refs []CheckpointRef, diskDelete func(context.Context, []DiffCheckpointRef) error) (retErr error) {
	if s == nil {
		return errors.New("checkpoint requires storage")
	}
	if c.Retain == nil {
		return errors.New("checkpoint deletion requires an explicit complete retention set")
	}
	c, dir, err := normalizeCheckpoint(c)
	if err != nil {
		return err
	}
	refs = append([]CheckpointRef(nil), refs...)
	for _, ref := range refs {
		if err := validateCheckpointRef(ref); err != nil {
			return err
		}
	}
	if diskDelete == nil {
		owner, err := diffstore.LockDirectory(dir)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, owner.Close()) }()
		diskDelete = func(ctx context.Context, refs []DiffCheckpointRef) error {
			return deleteDirectoryDiffCheckpoints(ctx, dir, refs)
		}
	}
	guard, err := LockCheckpointSources(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, guard.Close()) }()
	k, err := s.checkpointKeepPlan(ctx, c)
	if err != nil {
		return err
	}
	var disks []DiffCheckpointRef
	for _, ref := range refs {
		if ref.DiffID != "" {
			if k.diffVersions[ref.DiffID][ref.ID] {
				return fmt.Errorf("%w: disk %s/%s", ErrCheckpointReferenced, ref.DiffID, ref.ID)
			}
			disks = append(disks, ref)
		} else {
			exact := memorycheckpoint.Reference{CheckpointID: ref.ID, MemoryID: ref.MemoryID}
			if k.memoryVersions[exact] {
				return fmt.Errorf("%w: memory %s/%s", ErrCheckpointReferenced, ref.MemoryID, ref.ID)
			}
			record, err := memorycheckpoint.ReadRecord(dir, ref.ID, ref.MemoryID)
			if err != nil {
				return err
			}
			if record.Producer.SandboxID != c.SandboxID {
				return errors.New("checkpoint producer identity mismatch")
			}
		}
	}
	if err := diskDelete(ctx, disks); err != nil {
		return err
	}
	for _, ref := range refs {
		if ref.MemoryID != "" {
			if err := memorycheckpoint.DeleteCheckpoint(ctx, dir, memorycheckpoint.Reference{CheckpointID: ref.ID, MemoryID: ref.MemoryID}); err != nil {
				return err
			}
		}
	}
	return nil
}
