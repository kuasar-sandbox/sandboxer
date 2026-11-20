package memorycheckpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var ErrMemoryLost = errors.New("checkpoint memory snapshot is lost")
var ErrMemoryUnavailable = errors.New("checkpoint memory producer exited before file publication")

type CaptureRecovery struct {
	Record         CaptureRecord
	ProducerExited bool
}

func producerExited(id HolderIdentity) (bool, error) {
	start, state, err := processIdentityState(id.PID)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("memory checkpoint: producer death unknown: %w", err)
	}
	return start != id.StartTime || state == 'Z' || state == 'X' || state == 'x', nil
}

// RecoverMemoryRelease is metadata-only and never waits for a live reader. The
// expected identity must first be checked against the caller's durable owner or
// resource receipt. No socket, PID disappearance or file existence proves RAM
// release without the original RAM-pin inode's exclusive lock. A dead,
// uncommitted preparation may only become Aborted; it does not prove that the
// ordinary VM retired. Committed but unretired captures remain unconfirmed.
func RecoverMemoryRelease(ctx context.Context, capturePath string, expected CaptureRecord) (out CaptureRecovery, retErr error) {
	out.Record = expected
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if err := expected.Validate(); err != nil {
		return out, err
	}
	directory := filepath.Dir(filepath.Dir(filepath.Dir(capturePath)))
	canonical, err := RecordPath(directory, expected.CheckpointID, expected.MemoryID)
	if err != nil || canonical != capturePath {
		directory = filepath.Dir(filepath.Dir(capturePath))
		canonical, err = legacyRecordPath(directory, expected.CheckpointID)
	}
	if err != nil || canonical != capturePath {
		return out, errors.Join(errors.New("memory checkpoint: recovery record location mismatch"), err)
	}
	pin, err := PinDirectory(directory)
	if err != nil {
		return out, err
	}
	defer func() { retErr = errors.Join(retErr, pin.Close()) }()
	read := func() (*CaptureRecord, error) {
		r, err := ReadCaptureRecord(capturePath)
		if err != nil {
			return nil, err
		}
		if !sameCaptureIdentity(*r, expected) {
			return nil, errors.New("memory checkpoint: recovery capture identity changed")
		}
		out.Record = *r
		return r, nil
	}
	r, err := read()
	if err != nil || r.RAMReleased || r.Aborted {
		return out, err
	}
	lock, err := openPreparationLock(directory, expected.CheckpointID, false)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer func() { retErr = errors.Join(retErr, lock.Close()) }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return out, nil
		}
		return out, err
	}
	r, err = read()
	if err != nil || r.RAMReleased || r.Aborted {
		return out, err
	}
	out.ProducerExited, err = producerExited(r.Holder)
	if err != nil || !out.ProducerExited || r.Committed && !r.RuntimeRetired {
		return out, err
	}
	ram, err := openRAMPin(capturePath, r.Holder.RAMLock, unix.LOCK_EX)
	if err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return out, nil
		}
		return out, err
	}
	defer func() { retErr = errors.Join(retErr, ram.Close()) }()
	r, err = read()
	if err != nil {
		return out, err
	}
	exited, err := producerExited(r.Holder)
	if err != nil || !exited {
		return out, errors.Join(errors.New("memory checkpoint: producer state changed during recovery"), err)
	}
	if r.Committed && !r.RuntimeRetired || r.Aborted || r.Discarded {
		return out, errors.New("memory checkpoint: capture is not retired")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	w := &RecordWriter{path: capturePath, lock: lock, record: *r, published: true}
	err = w.Update(func(r *CaptureRecord) {
		if r.Committed {
			r.RAMReleased = true
		} else {
			r.Aborted = true
		}
	})
	out.Record = w.Read()
	return out, err
}

// ObservePreviousSave checks physical completion before reporting source loss.
// A historical lost capture never poisons a new runtime based on an intact one.
func ObservePreviousSave(ctx context.Context, directory string, expected CaptureRecord) (CaptureRecovery, error) {
	path, err := RecordPath(directory, expected.CheckpointID, expected.MemoryID)
	if err != nil {
		return CaptureRecovery{}, err
	}
	result, err := RecoverMemoryRelease(ctx, path, expected)
	if err != nil || result.Record.RAMReleased || result.Record.Aborted {
		return result, err
	}
	if result.ProducerExited && !result.Record.FileReady {
		return result, fmt.Errorf("%w: checkpoint %q", ErrMemoryUnavailable, result.Record.CheckpointID)
	}
	return result, nil
}
