package memorycheckpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type RunIdentity struct {
	SandboxID  string `json:"sandbox_id"`
	InstanceID string `json:"instance_id"`
	RunID      string `json:"run_id"`
}

type HolderIdentity struct {
	PID        int                     `json:"pid"`
	StartTime  uint64                  `json:"start_time"`
	SocketPath string                  `json:"socket_path"`
	Directory  HolderDirectoryIdentity `json:"directory"`
	RAMLock    FileIdentity            `json:"ram_lock"`
}

type FileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Nonce  string `json:"nonce,omitempty"`
}

type HolderDirectoryIdentity struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type CaptureRecord struct {
	Version         int            `json:"version"`
	Producer        RunIdentity    `json:"producer"`
	CheckpointID    string         `json:"checkpoint_id"`
	HostOperationID string         `json:"host_operation_id,omitempty"`
	MemoryID        string         `json:"memory_id"`
	Epoch           uint64         `json:"epoch"`
	SnapshotRef     string         `json:"snapshot_ref,omitempty"`
	SandboxRef      string         `json:"sandbox_ref,omitempty"`
	PayloadRef      string         `json:"payload_ref,omitempty"`
	Committed       bool           `json:"committed"`
	Aborted         bool           `json:"aborted,omitempty"`
	Discarded       bool           `json:"discarded,omitempty"`
	RuntimeRetired  bool           `json:"runtime_retired"`
	FileReady       bool           `json:"file_ready"`
	RAMReleased     bool           `json:"ram_released"`
	Holder          HolderIdentity `json:"holder"`
	SaveError       string         `json:"save_error,omitempty"`
}

func (r CaptureRecord) Validate() error {
	if r.Holder.RAMLock.Nonce != "" {
		b, err := hex.DecodeString(r.Holder.RAMLock.Nonce)
		if err != nil || len(b) != 32 {
			return errors.New("memory checkpoint: invalid RAM proof nonce")
		}
	}

	if r.Version != Version || !validCheckpointID(r.CheckpointID) || !validID(r.MemoryID) || r.Epoch == 0 || r.Producer.SandboxID == "" {
		return errors.New("memory checkpoint: invalid capture identity")
	}
	if r.Aborted && (r.Committed || r.FileReady || (r.RuntimeRetired || r.RAMReleased) && !r.Discarded) {
		return errors.New("memory checkpoint: aborted committed capture")
	}
	if r.Committed && (r.SnapshotRef == "" || r.SandboxRef == "") {
		return errors.New("memory checkpoint: committed capture lacks source refs")
	}
	if r.Discarded && !r.RAMReleased || r.RuntimeRetired && !r.Committed && !r.Discarded || r.FileReady && (!r.Committed || r.PayloadRef == "") || r.RAMReleased && !r.RuntimeRetired {
		return errors.New("memory checkpoint: inconsistent capture facts")
	}
	if r.Holder.PID <= 0 || r.Holder.StartTime == 0 || !filepath.IsAbs(r.Holder.Directory.Path) || filepath.Clean(r.Holder.Directory.Path) != r.Holder.Directory.Path || r.Holder.Directory.Inode == 0 || r.Holder.RAMLock.Inode == 0 || r.Holder.SocketPath != filepath.Join(r.Holder.Directory.Path, holderSocketName) {
		return errors.New("memory checkpoint: invalid holder identity")
	}
	return nil
}

// Lost separates source availability from the completed physical RAM obligation.
// A later runtime based on an intact source may proceed once RAMReleased is true.
func (r CaptureRecord) Lost() bool {
	return r.Committed && r.RuntimeRetired && r.RAMReleased && !r.FileReady && !r.Aborted && !r.Discarded
}

func sameCaptureIdentity(a, b CaptureRecord) bool {
	return a.Version == b.Version && a.Producer == b.Producer && a.CheckpointID == b.CheckpointID && a.HostOperationID == b.HostOperationID && a.MemoryID == b.MemoryID && a.Epoch == b.Epoch && a.Holder == b.Holder
}

// RecordPath with a memoryID names one immutable attempt. Without one it
// resolves the newest attempt, for callers that have no capture identity yet.
func RecordPath(directory, checkpointID string, memoryID ...string) (string, error) {
	legacy, err := legacyRecordPath(directory, checkpointID)
	if err != nil {
		return "", err
	}
	if len(memoryID) == 0 {
		r, err := ReadRecord(directory, checkpointID)
		if err != nil {
			return "", err
		}
		p, _ := RecordPath(directory, checkpointID, r.MemoryID)
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return legacy, nil
		}
		return p, nil
	}
	if len(memoryID) != 1 || !validID(memoryID[0]) {
		return "", errors.New("memory checkpoint: invalid attempt identity")
	}
	p := filepath.Join(strings.TrimSuffix(legacy, ".json"), memoryID[0]+".json")
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		// Existing pre-attempt-layout receipts and descriptors keep their paths.
		if old, err := ReadCaptureRecord(legacy); err == nil && old.CheckpointID == checkpointID && old.MemoryID == memoryID[0] {
			return legacy, nil
		}
	}
	return p, nil
}

func legacyRecordPath(directory, checkpointID string) (string, error) {
	if !filepath.IsAbs(directory) || !validCheckpointID(checkpointID) {
		return "", errors.New("memory checkpoint: invalid record location")
	}
	return filepath.Join(directory, "memory", fmt.Sprintf("%x.json", sha256.Sum256([]byte(checkpointID)))), nil
}

func preparationLockPath(directory, checkpointID string) (string, error) {
	p, err := legacyRecordPath(directory, checkpointID)
	return p + ".lock", err
}

func openPreparationLock(directory, checkpointID string, create bool) (*os.File, error) {
	return openPreparationLockWithSync(directory, checkpointID, create, syncRecordDirectory)
}

func syncRecordDirectory(path string) error {
	d, err := openDirectory(path)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// The sync callback is local to one preparation so publication-failure tests
// cannot affect a concurrent producer.
func openPreparationLockWithSync(directory, checkpointID string, create bool, syncDirectory func(string) error) (*os.File, error) {
	p, err := preparationLockPath(directory, checkpointID)
	if err != nil {
		return nil, err
	}
	flags := os.O_RDWR | unix.O_NOFOLLOW
	if create {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return nil, err
		}
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(p, flags, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	if create {
		// A durable receipt must retain the same preparation lock after reboot.
		// Sync both the new lock entry and a possibly new memory directory.
		if err := f.Sync(); err != nil {
			return nil, errors.Join(err, f.Close())
		}
		for _, path := range []string{filepath.Dir(p), directory} {
			if err := syncDirectory(path); err != nil {
				return nil, errors.Join(err, f.Close())
			}
		}
	}
	return f, nil
}

type preparationGuard struct{ writer, pin *os.File }

func (g *preparationGuard) Close() error { return errors.Join(g.writer.Close(), g.pin.Close()) }

// LockPreparation fences both existing and late preparations of one logical
// checkpoint. It does not establish any RAM release fact.
func LockPreparation(directory, checkpointID string) (io.Closer, error) {
	if _, err := legacyRecordPath(directory, checkpointID); err != nil {
		return nil, err
	}
	pin, err := PinDirectory(directory)
	if err != nil {
		return nil, err
	}
	writer, err := openPreparationLock(directory, checkpointID, true)
	if err != nil {
		return nil, errors.Join(err, pin.Close())
	}
	return &preparationGuard{writer: writer, pin: pin}, nil
}

// ListRecords includes every attempt: old resource receipts continue to name
// their original JSON and proof inode even after a later attempt is prepared.
func ListRecords(directory string) ([]CaptureRecord, error) {
	entries, err := os.ReadDir(filepath.Join(directory, "memory"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []CaptureRecord
	for _, entry := range entries {
		p := filepath.Join(directory, "memory", entry.Name())
		paths := []string{p}
		if entry.IsDir() {
			children, err := os.ReadDir(p)
			if err != nil {
				return nil, err
			}
			paths = nil
			for _, child := range children {
				if strings.HasSuffix(child.Name(), ".json") {
					paths = append(paths, filepath.Join(p, child.Name()))
				}
			}
		}
		for _, path := range paths {
			if !strings.HasSuffix(path, ".json") {
				continue
			}
			r, err := ReadCaptureRecord(path)
			if err != nil {
				return nil, err
			}
			canonical, _ := RecordPath(directory, r.CheckpointID, r.MemoryID)
			legacy, _ := legacyRecordPath(directory, r.CheckpointID)
			if path != canonical && path != legacy {
				return nil, errors.New("memory checkpoint: record location identity mismatch")
			}
			records = append(records, *r)
		}
	}
	return records, nil
}

func ReadCaptureRecord(path string) (*CaptureRecord, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 64*1024 {
		return nil, errors.New("memory checkpoint: oversized capture record")
	}
	var r CaptureRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("memory checkpoint record: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// checkpointRecords inspects only one logical checkpoint. Preparing N distinct
// checkpoints must not repeatedly parse every older capture in the directory.
func checkpointRecords(directory, checkpointID string) ([]CaptureRecord, error) {
	legacy, err := legacyRecordPath(directory, checkpointID)
	if err != nil {
		return nil, err
	}
	var records []CaptureRecord
	appendRecord := func(path string) error {
		r, err := ReadCaptureRecord(path)
		if err != nil {
			return err
		}
		if r.CheckpointID != checkpointID {
			return errors.New("memory checkpoint: record checkpoint identity mismatch")
		}
		canonical, err := RecordPath(directory, checkpointID, r.MemoryID)
		if err != nil {
			return err
		}
		if path != canonical && path != legacy {
			return errors.New("memory checkpoint: record attempt location mismatch")
		}
		records = append(records, *r)
		return nil
	}
	if err := appendRecord(legacy); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dir := strings.TrimSuffix(legacy, ".json")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return records, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := appendRecord(filepath.Join(dir, entry.Name())); err != nil {
			return nil, err
		}
	}
	return records, nil
}

func ReadRecord(directory, checkpointID string, memoryID ...string) (*CaptureRecord, error) {
	if _, err := legacyRecordPath(directory, checkpointID); err != nil {
		return nil, err
	}
	if len(memoryID) != 0 {
		p, err := RecordPath(directory, checkpointID, memoryID...)
		if err != nil {
			return nil, err
		}
		r, err := ReadCaptureRecord(p)
		if errors.Is(err, os.ErrNotExist) {
			p, _ = legacyRecordPath(directory, checkpointID)
			r, err = ReadCaptureRecord(p)
		}
		if err == nil && (r.CheckpointID != checkpointID || r.MemoryID != memoryID[0]) {
			return nil, errors.New("memory checkpoint: record attempt identity mismatch")
		}
		return r, err
	}
	records, err := checkpointRecords(directory, checkpointID)
	if err != nil {
		return nil, err
	}
	var latest *CaptureRecord
	for _, r := range records {
		if r.CheckpointID != checkpointID {
			continue
		}
		if latest != nil && latest.Epoch == r.Epoch && latest.MemoryID != r.MemoryID {
			return nil, errors.New("memory checkpoint: duplicate attempt epoch")
		}
		if latest == nil || r.Epoch > latest.Epoch {
			copy := r
			latest = &copy
		}
	}
	if latest == nil {
		return nil, os.ErrNotExist
	}
	return latest, nil
}

type currentLocator struct {
	CheckpointID string `json:"checkpoint_id"`
	MemoryID     string `json:"memory_id"`
	Epoch        uint64 `json:"epoch"`
}

func ReadCurrent(directory string) (*CaptureRecord, error) {
	b, err := os.ReadFile(filepath.Join(directory, "current-memory.json"))
	if err != nil {
		return nil, err
	}
	var loc currentLocator
	if len(b) > 4096 {
		return nil, errors.New("memory checkpoint: oversized current locator")
	}
	if err := json.Unmarshal(b, &loc); err != nil {
		return nil, err
	}
	r, err := ReadRecord(directory, loc.CheckpointID, loc.MemoryID)
	if err != nil {
		// Only absence of the locator means no prior obligation. Losing a
		// record named by a present locator is an unknown state, not readiness.
		return nil, fmt.Errorf("memory checkpoint: current record unavailable: %v", err)
	}
	if r.MemoryID != loc.MemoryID || r.Epoch != loc.Epoch {
		return nil, errors.New("memory checkpoint: current identity mismatch")
	}
	return r, nil
}

// RecordWriter serializes monotonic updates. A stable flock excludes another
// producer across an atomic rename; the JSON file itself is never the lock.
type RecordWriter struct {
	mu        sync.Mutex
	path      string
	lock      *os.File
	record    CaptureRecord
	published bool
	// Per-writer fault injection keeps atomic-publication failure tests local;
	// production uses writeJSON. Updates invoke it while holding mu.
	writeJSON func(string, any) error
	abortOnce sync.Once
	abortErr  error
}

func CreateRecord(directory string, record CaptureRecord) (*RecordWriter, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	w, err := reserveRecord(directory, record)
	if err != nil {
		return nil, err
	}
	if err := publishRecord(directory, w); err != nil {
		if w.published && !record.Committed {
			_ = AbortRecord(w, nil, nil)
		} else {
			_ = w.Close()
		}
		return nil, err
	}
	return w, nil
}

// reserveRecord precedes RAM-pin creation. Holding this stable writer lock
// prevents another producer/recoverer from replacing a capture's proof inode.
func reserveRecord(directory string, record CaptureRecord) (*RecordWriter, error) {
	return reserveRecordWithSync(directory, record, syncRecordDirectory)
}

func reserveRecordWithSync(directory string, record CaptureRecord, syncDirectory func(string) error) (*RecordWriter, error) {
	p, err := RecordPath(directory, record.CheckpointID, record.MemoryID)
	if err != nil {
		return nil, err
	}
	lock, err := openPreparationLockWithSync(directory, record.CheckpointID, true, syncDirectory)
	if err != nil {
		return nil, err
	}
	w := &RecordWriter{path: p, lock: lock, record: record}
	fail := func(err error) (*RecordWriter, error) { return nil, errors.Join(err, w.Close()) }
	records, err := checkpointRecords(directory, record.CheckpointID)
	if err != nil {
		return fail(err)
	}
	var epoch uint64
	for _, old := range records {
		if old.CheckpointID != record.CheckpointID {
			continue
		}
		if old.Producer.SandboxID != record.Producer.SandboxID || !old.Aborted || old.MemoryID == record.MemoryID {
			return fail(errors.New("memory checkpoint: capture already exists"))
		}
		if old.Epoch > epoch {
			epoch = old.Epoch
		}
	}
	if epoch == ^uint64(0) {
		return fail(errors.New("memory checkpoint: attempt epoch exhausted"))
	}
	if w.record.Epoch <= epoch {
		w.record.Epoch = epoch + 1
	}
	if _, err = os.Lstat(p); err == nil {
		return fail(errors.New("memory checkpoint: attempt already exists"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if old, err := ReadCurrent(directory); err == nil && !old.RAMReleased && !old.Aborted {
		return fail(ErrSavePending)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return fail(err)
	}
	// Syncing files inside the attempt directory does not persist this new
	// directory's entry in memory. This must precede record/locator publication,
	// including retries after a previous failed parent-directory sync.
	if err = syncDirectory(filepath.Join(directory, "memory")); err != nil {
		return fail(err)
	}
	return w, nil
}

func publishRecord(directory string, w *RecordWriter) error {
	if err := w.record.Validate(); err != nil {
		return err
	}
	err := writeJSON(w.path, w.record)
	var published *recordPublishedError
	w.published = err == nil || errors.As(err, &published)
	if err != nil {
		return err
	}
	r := w.record
	return writeJSON(filepath.Join(directory, "current-memory.json"), currentLocator{r.CheckpointID, r.MemoryID, r.Epoch})
}

func (w *RecordWriter) Path() string        { return w.path }
func (w *RecordWriter) Read() CaptureRecord { w.mu.Lock(); defer w.mu.Unlock(); return w.record }
func (w *RecordWriter) Update(update func(*CaptureRecord)) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.record
	update(&r)
	if !sameCaptureIdentity(r, w.record) || w.record.RAMReleased && !w.record.FileReady && r.FileReady || w.record.Aborted && !r.Aborted || w.record.Discarded && !r.Discarded || w.record.Committed && !r.Committed || w.record.RuntimeRetired && !r.RuntimeRetired || w.record.FileReady && !r.FileReady || w.record.RAMReleased && !r.RAMReleased || w.record.SnapshotRef != "" && w.record.SnapshotRef != r.SnapshotRef || w.record.SandboxRef != "" && w.record.SandboxRef != r.SandboxRef || w.record.PayloadRef != "" && w.record.PayloadRef != r.PayloadRef {
		return errors.New("memory checkpoint: nonmonotonic record update")
	}
	if err := r.Validate(); err != nil {
		return err
	}
	write := w.writeJSON
	if write == nil {
		write = writeJSON
	}
	err := write(w.path, r)
	var published *recordPublishedError
	if err == nil || errors.As(err, &published) {
		w.record = r
	}
	return err
}
func (w *RecordWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lock == nil {
		return nil
	}
	err := w.lock.Close()
	w.lock = nil
	return err
}

func writeJSON(path string, value any) (retErr error) {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".memory-record-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(tmp) }()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return &recordPublishedError{err}
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return &recordPublishedError{err}
	}
	return nil
}

type recordPublishedError struct{ error }

func (e *recordPublishedError) Unwrap() error { return e.error }

var ErrSavePending = errors.New("previous checkpoint memory save is pending")

// WaitCurrent waits before any VM state transition. Cancellation never reaches
// the producer. Reading an unknown/broken record is an error, never readiness.
func WaitCurrent(ctx context.Context, directory, sandboxID string, noWait bool) (*CaptureRecord, error) {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		r, err := ReadCurrent(directory)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if r.Producer.SandboxID != sandboxID {
			return nil, errors.New("memory checkpoint: pending sandbox identity mismatch")
		}
		if r.RAMReleased || r.Aborted {
			return r, nil
		}
		result, err := ObservePreviousSave(ctx, directory, *r)
		r = &result.Record
		if err != nil || r.RAMReleased || r.Aborted {
			if noWait && errors.Is(err, ErrMemoryUnavailable) {
				return r, ErrSavePending
			}
			return r, err
		}
		if noWait {
			return r, ErrSavePending
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-t.C:
		}
	}
}
