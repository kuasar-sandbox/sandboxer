package memorycheckpoint

import (
	"errors"
	"time"
)

// AbortRecord takes ownership of an uncommitted writer and completion cleanup.
// The caller first closes any provisional RAM fd/service. If metadata cannot be
// published now, an independent continuation retains only the record and its
// cleanup (typically a directory pin), never RAM or a borrowed runtime store.
// The record lock and cleanup remain owned until Aborted is durably published.
// Repeated calls join the same completion and never run cleanup twice.
func AbortRecord(record *RecordWriter, complete func() error, logf func(string, ...any)) error {
	if record == nil {
		return errors.New("memory checkpoint: nil abort record")
	}
	record.abortOnce.Do(func() {
		setError := func(err error) {
			record.mu.Lock()
			record.abortErr = err
			record.mu.Unlock()
		}
		if record.Read().Committed {
			setError(errors.New("memory checkpoint: cannot abort committed record"))
			return
		}
		finish := func() {
			err := record.Close()
			if complete != nil {
				err = errors.Join(err, complete())
			}
			setError(err)
			if err != nil && logf != nil {
				logf("checkpoint abort cleanup: %v", err)
			}
		}
		publish := func() error {
			return record.Update(func(r *CaptureRecord) { r.Aborted = true })
		}
		err := publish()
		if err == nil {
			finish()
			return
		}
		setError(err)
		if logf != nil {
			logf("checkpoint abort metadata retained for retry: %v", err)
		}
		go func() {
			for {
				time.Sleep(100 * time.Millisecond)
				if err := publish(); err == nil {
					finish()
					return
				} else if logf != nil {
					logf("checkpoint abort metadata retained for retry: %v", err)
				}
			}
		}()
	})
	record.mu.Lock()
	defer record.mu.Unlock()
	return record.abortErr
}
