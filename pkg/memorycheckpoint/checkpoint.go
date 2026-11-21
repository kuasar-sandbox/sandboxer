package memorycheckpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"golang.org/x/sys/unix"
)

var ErrDeleted = errors.New("memory checkpoint: version was deleted")

// Reference names one exact immutable version, never the newest attempt of a
// reused logical ID. A collection retry must retain these exact references.
type Reference struct {
	CheckpointID string `json:"checkpoint_id"`
	MemoryID     string `json:"memory_id"`
}

func (r Reference) Validate() error {
	if !validCheckpointID(r.CheckpointID) || !validID(r.MemoryID) {
		return errors.New("memory checkpoint: invalid version reference")
	}
	return nil
}

// ListCheckpoints is the business view, not the resource recovery inventory.
// An unavailable committed version remains visible for diagnosis. ListRecords
// continues to include revoked versions and all attempts for settlement.
func ListCheckpoints(directory string) ([]CaptureRecord, error) {
	records, err := ListRecords(directory)
	if err != nil {
		return nil, err
	}
	out := make([]CaptureRecord, 0, len(records))
	for _, r := range records {
		if r.Committed && !r.Aborted && !r.Deleted {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CheckpointID != out[j].CheckpointID {
			return out[i].CheckpointID < out[j].CheckpointID
		}
		return out[i].MemoryID < out[j].MemoryID
	})
	return out, nil
}

// DeleteCheckpoint revokes one backend version. Its caller must first prove
// that no durable source graph needs it and retain the source-mutation fence
// through this call. This low-level operation does not inspect external roots.
// Live views retain their directory/RAM pins and may still switch to a file.
func DeleteCheckpoint(ctx context.Context, directory string, ref Reference) (retErr error) {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	pin, err := PinDirectory(directory)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, pin.Close()) }()
	r, err := ReadRecord(directory, ref.CheckpointID, ref.MemoryID)
	if err != nil {
		return err
	}
	if !r.Committed || r.Aborted {
		return errors.New("memory checkpoint: cannot delete uncommitted version")
	}
	if r.Deleted {
		return nil
	}
	writer, err := openPreparationLock(directory, ref.CheckpointID, false)
	if errors.Is(err, unix.EWOULDBLOCK) {
		// The live holder is the only record writer. Old holders reject this
		// operation; never claim success by updating their record behind them.
		// A holder may exit after recording deletion but before delivering its
		// reply. The durable exact receipt decides logical deletion even when
		// transport fails; it says nothing about physical RAM release.
		defer func() {
			latest, err := ReadRecord(directory, ref.CheckpointID, ref.MemoryID)
			if err == nil && sameCaptureIdentity(*r, *latest) && latest.Deleted {
				retErr = nil
				return
			}
			retErr = errors.Join(retErr, err, errors.New("memory checkpoint: deletion not recorded"))
		}()
		c, err := dialHolder(ctx, r.Holder)
		if err != nil {
			return err
		}
		defer c.Close()
		stop := context.AfterFunc(ctx, func() { _ = c.Close() })
		defer stop()
		if err := writeHolderMessage(c, holderMessage{Type: "delete", CheckpointID: ref.CheckpointID, MemoryID: ref.MemoryID, Epoch: r.Epoch}); err != nil {
			return err
		}
		var reply holderMessage
		if err := readHolderMessage(c, &reply); err != nil {
			return err
		}
		if reply.Type != "deleted" {
			return fmt.Errorf("memory checkpoint: delete refused: %s", reply.Error)
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, writer.Close()) }()
	latest, err := ReadRecord(directory, ref.CheckpointID, ref.MemoryID)
	if err != nil {
		return err
	}
	if !sameCaptureIdentity(*r, *latest) {
		return errors.New("memory checkpoint: delete generation changed")
	}
	path, err := RecordPath(directory, ref.CheckpointID, ref.MemoryID)
	if err != nil {
		return err
	}
	w := &RecordWriter{path: path, lock: writer, record: *latest, published: true}
	return w.Update(func(r *CaptureRecord) { r.Deleted = true })
}

// markDeleted shares the RAM-open, FD-send and file-publication boundary.
func (h *Holder) markDeleted() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.record.Read()
	if !r.Committed || r.Aborted {
		return errors.New("memory checkpoint: cannot delete uncommitted version")
	}
	if h.closed {
		return os.ErrClosed
	}
	if err := h.record.Update(func(r *CaptureRecord) { r.Deleted = true }); err != nil {
		return err
	}
	select {
	case h.changed <- struct{}{}:
	default:
	}
	return nil
}
