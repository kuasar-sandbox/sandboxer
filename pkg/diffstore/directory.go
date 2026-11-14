package diffstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const directoryGCHandles = 2

// DirectoryMaintenance borrows an already-held owner. Active identities must be
// complete before construction. It never opens active files, retires files, or
// releases the directory owner. Close it before closing disks and their owner.
// One dispatcher serializes each finite GC batch with management requests.
// Two historical GC handles may persist across More; a management request for
// another file uses one additional, temporary handle, never waiting for a whole
// file's GC to finish. Request senders queue without an unbounded internal list.
type DirectoryMaintenance struct {
	owner            *DirectoryOwner
	activeMu         sync.RWMutex
	active           map[string]bool
	requests         chan directoryRequest
	stop, done       chan struct{}
	mu               sync.Mutex
	closing          bool
	ops              sync.WaitGroup
	closeOnce        sync.Once
	closeErr         error
	sweep            chan struct{}
	backgroundCancel context.CancelFunc
	backgroundDone   chan struct{}
}

type directoryRequest struct {
	ctx  context.Context
	run  func(map[string]*Store) error
	done chan error
}

type DirectoryGCResult struct {
	DiffID     string `json:"diff_id"`
	State      string `json:"state"`
	FreedSlots uint64 `json:"freed_slots"`
	Error      string `json:"error,omitempty"`
}

func NewDirectoryMaintenance(owner *DirectoryOwner, active []string) (*DirectoryMaintenance, error) {
	if err := owner.Validate(); err != nil {
		return nil, err
	}
	m := &DirectoryMaintenance{owner: owner, active: make(map[string]bool), requests: make(chan directoryRequest),
		stop: make(chan struct{}), done: make(chan struct{}), sweep: make(chan struct{}, 1)}
	for _, id := range active {
		if !validDiffID(id) {
			return nil, fmt.Errorf("diffstore: invalid active DiffID %q", id)
		}
		m.active[id] = true
	}
	go m.serve()
	return m, nil
}

func validDiffID(id string) bool { return id != "" && !strings.ContainsAny(id, "/\\.") }

func (m *DirectoryMaintenance) begin() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return os.ErrClosed
	}
	m.ops.Add(1)
	return nil
}

func (m *DirectoryMaintenance) call(ctx context.Context, fn func(map[string]*Store) error) error {
	if err := m.begin(); err != nil {
		return err
	}
	defer m.ops.Done()
	request := directoryRequest{ctx: ctx, run: fn, done: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.stop:
		return os.ErrClosed
	case m.requests <- request:
	}
	// Accepted operations finish before their caller releases ownership, even
	// when its context is cancelled while the store commits the current batch.
	return <-request.done
}

func (m *DirectoryMaintenance) serve() {
	handles := make(map[string]*Store)
	defer func() {
		for _, store := range handles {
			m.closeErr = errors.Join(m.closeErr, store.Close())
		}
		close(m.done)
	}()
	for {
		select {
		case <-m.stop:
			return
		default:
		}
		select {
		case <-m.stop:
			return
		case r := <-m.requests:
			err := r.ctx.Err()
			if err == nil {
				err = r.run(handles)
			}
			r.done <- err
		}
	}
}

func (m *DirectoryMaintenance) open(id string) (*Store, error) {
	if err := m.owner.Validate(); err != nil {
		return nil, err
	}
	if !validDiffID(id) {
		return nil, fmt.Errorf("diffstore: invalid DiffID %q", id)
	}
	if m.isActive(id) {
		return nil, fmt.Errorf("diffstore: active DIFF %s belongs to its Disk", id)
	}
	path := filepath.Join(m.owner.Directory, "diffs", id+".diff")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("diffstore: DIFF %s is not a regular file", id)
	}
	store, err := OpenMetadata(path)
	if err != nil {
		return nil, err
	}
	if store.Info().DiffID != id {
		return nil, errors.Join(fmt.Errorf("diffstore: DIFF %s filename/header identity mismatch", id), store.Close())
	}
	return store, nil
}

func (m *DirectoryMaintenance) withMetadata(ctx context.Context, id string, fn func(*Store) error) error {
	return m.call(ctx, func(handles map[string]*Store) error {
		if err := m.owner.Validate(); err != nil {
			return err
		}
		if store := handles[id]; store != nil {
			return fn(store)
		}
		store, err := m.open(id)
		if err != nil {
			return err
		}
		return errors.Join(fn(store), store.Close())
	})
}

func (m *DirectoryMaintenance) DeleteCheckpoints(ctx context.Context, id string, ids []string) error {
	err := m.withMetadata(ctx, id, func(store *Store) error { return store.DeleteCheckpoints(ctx, ids) })
	if errors.Is(err, ErrRetired) || os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("diffstore: DIFF %s: %w", id, err)
	}
	return nil
}

// ListCheckpoints visits historical files incrementally. Active files remain
// the caller's responsibility. A partial list is always returned as an error.
func (m *DirectoryMaintenance) ListCheckpoints(ctx context.Context, visit func(string, []string) error) error {
	if err := m.begin(); err != nil {
		return err
	}
	defer m.ops.Done()
	it, err := m.iterator()
	if err != nil {
		return err
	}
	defer it.Close()
	for {
		id, err := it.next(ctx)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if m.isActive(id) {
			continue
		}
		var ids []string
		err = m.withMetadata(ctx, id, func(store *Store) error {
			var e error
			ids, e = store.ListCheckpoints(ctx)
			if errors.Is(e, ErrRetired) {
				return nil
			}
			return e
		})
		if err != nil {
			return fmt.Errorf("diffstore: DIFF %s: %w", id, err)
		}
		if err = visit(id, ids); err != nil {
			return err
		}
	}
}

type directoryIterator struct {
	*os.File
	entries []os.DirEntry
	at      int
}

func (m *DirectoryMaintenance) iterator() (*directoryIterator, error) {
	if err := m.owner.Validate(); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(m.owner.Directory, "diffs"))
	if err != nil {
		return nil, err
	}
	return &directoryIterator{File: f}, nil
}

func (it *directoryIterator) next(ctx context.Context) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if it.at == len(it.entries) {
			entries, err := it.ReadDir(32)
			if err != nil {
				return "", err
			}
			it.entries, it.at = entries, 0
		}
		e := it.entries[it.at]
		it.at++
		// Initializer claims are sidecar locks, including after an interrupted
		// process leaves one behind. They are never DIFFs and are not ours to
		// unlink while enumerating a retained parent directory.
		if strings.HasSuffix(e.Name(), ".diff") && !strings.HasPrefix(e.Name(), ".claim-") {
			return strings.TrimSuffix(e.Name(), ".diff"), nil
		}
	}
}

// Sweep processes all presently enumerated historical files until Complete or
// BlockedByReaders, reporting each independently and continuing past file errors.
// It does not wait for readers. The callback must not reenter this maintainer.
func (m *DirectoryMaintenance) Sweep(ctx context.Context, report func(DirectoryGCResult)) (retErr error) {
	if err := m.begin(); err != nil {
		return err
	}
	defer m.ops.Done()
	select {
	case m.sweep <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-m.stop:
		return os.ErrClosed
	}
	defer func() { <-m.sweep }()
	it, err := m.iterator()
	if err != nil {
		return err
	}
	defer it.Close()
	defer func() {
		err := m.call(context.Background(), func(handles map[string]*Store) error {
			var err error
			for id, store := range handles {
				err = errors.Join(err, store.Close())
				delete(handles, id)
			}
			return err
		})
		if !errors.Is(err, os.ErrClosed) {
			retErr = errors.Join(retErr, err)
		}
	}()
	var pending []DirectoryGCResult
	var firstError error
	eof := false
	for !eof || len(pending) != 0 {
		for !eof && len(pending) < directoryGCHandles {
			id, e := it.next(ctx)
			if e == io.EOF {
				eof = true
				break
			}
			if e != nil {
				return errors.Join(firstError, e)
			}
			if !m.isActive(id) {
				pending = append(pending, DirectoryGCResult{DiffID: id})
			}
		}
		if len(pending) == 0 {
			continue
		}
		item := pending[0]
		pending = pending[1:]
		err = m.call(ctx, func(handles map[string]*Store) error {
			if err := m.owner.Validate(); err != nil {
				return err
			}
			store := handles[item.DiffID]
			if store == nil {
				if len(handles) >= directoryGCHandles {
					return fmt.Errorf("diffstore: GC handle budget exhausted")
				}
				var e error
				store, e = m.open(item.DiffID)
				if e != nil {
					return e
				}
				handles[item.DiffID] = store
			}
			result, e := store.GC(ctx, MaxBatchPages)
			item.FreedSlots += result.FreedSlots
			item.State = string(result.State)
			if errors.Is(e, ErrRetired) {
				item.State = "skipped_retired"
				e = nil
			}
			if e != nil || item.State != string(More) {
				e = errors.Join(e, store.Close())
				delete(handles, item.DiffID)
			}
			return e
		})
		if err != nil {
			item.State, item.Error = "error", err.Error()
			if firstError == nil {
				firstError = fmt.Errorf("diffstore: DIFF %s: %w", item.DiffID, err)
			}
		}
		if err == nil && item.State == string(More) {
			pending = append(pending, item)
		} else if report != nil {
			report(item)
		}
		if ctx.Err() != nil {
			return errors.Join(firstError, ctx.Err())
		}
		select {
		case <-m.stop:
			return errors.Join(firstError, os.ErrClosed)
		default:
		}
		runtime.Gosched()
	}
	return firstError
}

// Start enables independent startup and periodic maintenance. Merely creating
// a metadata manager, listing, or deleting never schedules GC.
func (m *DirectoryMaintenance) Start(ctx context.Context, report func(DirectoryGCResult)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return os.ErrClosed
	}
	if m.backgroundDone != nil {
		return fmt.Errorf("diffstore: directory maintenance already started")
	}
	ctx, m.backgroundCancel = context.WithCancel(ctx)
	m.backgroundDone = make(chan struct{})
	go func() {
		defer close(m.backgroundDone)
		for {
			if err := m.Sweep(ctx, report); err != nil && ctx.Err() == nil && report != nil {
				report(DirectoryGCResult{State: "error", Error: err.Error()})
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-m.stop:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return nil
}

func (m *DirectoryMaintenance) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closing = true
		close(m.stop)
		if m.backgroundCancel != nil {
			m.backgroundCancel()
		}
		background := m.backgroundDone
		m.mu.Unlock()
		<-m.done
		m.ops.Wait()
		if background != nil {
			<-background
		}
	})
	return m.closeErr
}

// ReserveActive prevents directory maintenance from opening a newly allocated live diff.
// The owner calls it before creating the writer and releases it if publication fails.
func (m *DirectoryMaintenance) ReserveActive(id string) error {
	if !validDiffID(id) {
		return fmt.Errorf("invalid active DiffID %q", id)
	}
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	if m.active[id] {
		return fmt.Errorf("active DiffID %q already reserved", id)
	}
	m.active[id] = true
	return nil
}
func (m *DirectoryMaintenance) ReleaseActive(id string) {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	delete(m.active, id)
}
func (m *DirectoryMaintenance) isActive(id string) bool {
	m.activeMu.RLock()
	defer m.activeMu.RUnlock()
	return m.active[id]
}
