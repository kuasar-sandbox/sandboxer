package memorycheckpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// DiscardCapture is a destruction-only operation. The caller must fence every
// SID runtime under its source-operation lock. This function independently
// requires exact producer death, the stable producer lock and an exclusive
// reader pin before publishing physical release; it never invents a payload.
func DiscardCapture(ctx context.Context, directory string, producer RunIdentity, checkpointID string) (*CaptureRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := RecordPath(directory, checkpointID)
	if err != nil {
		return nil, err
	}
	r, err := ReadCaptureRecord(path)
	if err != nil {
		return nil, err
	}
	if r.Producer != producer {
		return nil, errors.New("memory checkpoint: discard producer mismatch")
	}
	if r.RAMReleased {
		return r, nil
	}
	exited, err := producerExited(r.Holder)
	if err != nil {
		return nil, err
	}
	if !exited {
		return nil, errors.New("memory checkpoint: discard requires producer fencing")
	}
	pin, err := os.OpenFile(filepath.Join(directory, ".memory-readers.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer pin.Close()
	if err := unix.Flock(int(pin.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("memory checkpoint: readers remain: %w", err)
	}
	lock, err := openPreparationLock(directory, checkpointID, false)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("memory checkpoint: producer remains: %w", err)
	}
	latest, err := ReadCaptureRecord(path)
	if err != nil {
		return nil, err
	}
	if !sameCaptureIdentity(*latest, *r) {
		return nil, errors.New("memory checkpoint: discard generation changed")
	}
	ram, err := openRAMPin(path, latest.Holder.RAMLock, unix.LOCK_EX)
	if err != nil {
		return nil, fmt.Errorf("memory checkpoint: discard RAM proof: %w", err)
	}
	defer ram.Close()
	if exited, err := producerExited(latest.Holder); err != nil || !exited {
		return nil, errors.Join(errors.New("memory checkpoint: discard producer state changed"), err)
	}
	w := &RecordWriter{path: path, lock: lock, record: *latest}
	if err := w.Update(func(r *CaptureRecord) { r.Discarded = true; r.RuntimeRetired = true; r.RAMReleased = true }); err != nil {
		return nil, err
	}
	out := w.Read()
	return &out, nil
}
