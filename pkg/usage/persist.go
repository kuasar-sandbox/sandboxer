package usage

import (
	"context"
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

// Writer is the narrow, injectable persistence boundary. No operation is
// assumed cancelable; a blocked call retains its one writer execution slot.
type Writer interface {
	WriteAt([]byte, int64) (int, error)
	Truncate(int64) error
	Sync() error
}

// View distinguishes accepted live input, confirmed saved input and a write
// whose rollback is not yet known. Queries do not schedule observation or I/O.
type View struct {
	Enabled     bool      `json:"enabled"`
	Live        *Snapshot `json:"live,omitempty"`
	Saved       *Record   `json:"saved,omitempty"`
	SavedEnd    int64     `json:"saved_end,string"`
	Saving      bool      `json:"saving"`
	UnknownTail bool      `json:"unknown_tail"`
	SaveError   string    `json:"save_error,omitempty"`
	ReadError   string    `json:"read_error,omitempty"`
}

// Manager owns S (saved/offset), F (immutable pending), and A (live.Window).
// Cumulative live totals already include all accepted F/A observations.
type Manager struct {
	mu        sync.Mutex
	live      Snapshot
	saved     *Record
	offset    int64
	pending   *Record
	writer    Writer
	dirSync   func() error
	reader    io.ReaderAt
	busy      bool
	unknown   bool
	stopping  bool
	saveError string
	readError string
	done      chan struct{}
	closeFn   func()
	closeOnce sync.Once
	runStart  time.Time
	// An existing empty/partial first record cannot establish prior CPU usage.
	// Once counters exist their persistent Complete bits retain this fact.
	historyUnknown bool
}

func newManager(s Snapshot, recovered Recovery, w Writer, dirSync func() error) *Manager {
	return &Manager{live: s, saved: recovered.Record, offset: recovered.End,
		writer: w, dirSync: dirSync, unknown: recovered.IncompleteTail}
}

// Open pins both the directory and the file before asynchronous writes. It
// never recreates BaseDir. Removing/replacing the directory cannot redirect a
// late writer into a new sandbox with the same pathname.
func Open(baseDir, sandboxID, epoch string, start time.Time, sample, flush time.Duration) (*Manager, error) {
	// The exported API must enforce the same domain as configuration and
	// Gauge.Observe before opening files or constructing sampler tickers.
	if sample <= 0 || sample > time.Duration(1<<62-1) || flush < sample {
		return nil, errors.New("usage: invalid sample/flush intervals")
	}
	if sandboxID == "" || sandboxID == "." || sandboxID == ".." || strings.ContainsAny(sandboxID, "/\\\x00") {
		return nil, errors.New("usage: invalid sandbox ID")
	}
	dirFD, err := unix.Open(baseDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("usage directory: %w", err)
	}
	dir := os.NewFile(uintptr(dirFD), baseDir)
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Openat(dirFD, sandboxID+".usage", flags|unix.O_CREAT|unix.O_EXCL, 0o600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(dirFD, sandboxID+".usage", flags, 0)
	}
	if err != nil {
		dir.Close()
		return nil, fmt.Errorf("usage file: %w", err)
	}
	f := os.NewFile(uintptr(fd), filepath.Join(baseDir, sandboxID+".usage"))
	closeFiles := func() { _ = f.Close(); _ = dir.Close() }
	stat, err := f.Stat()
	if err != nil {
		closeFiles()
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		closeFiles()
		return nil, errors.New("usage: not a regular file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeFiles()
		return nil, fmt.Errorf("usage writer ownership: %w", err)
	}
	recovered, err := Recover(f, stat.Size(), sandboxID)
	if err != nil {
		closeFiles()
		return nil, err
	}
	s := Snapshot{SandboxID: sandboxID}
	if recovered.Record != nil {
		s = recovered.Record.Snapshot.clone()
	}
	s.newRun(epoch, start, sample, flush)
	m := newManager(s, recovered, f, dir.Sync)
	m.reader, m.closeFn = f, closeFiles
	m.runStart = start
	// No startup checkpoint/WAL is part of this format. An existing file,
	// including a closed last record, cannot certify that no intervening
	// process consumed CPU and vanished before its first save.
	m.historyUnknown = !created
	return m, nil
}

func cloneRecord(r *Record) *Record {
	if r == nil {
		return nil
	}
	c := *r
	c.Snapshot = c.Snapshot.clone()
	return &c
}

func (m *Manager) View() View {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.live.clone()
	if m.pending != nil {
		// A frozen peak remains part of live until S advances to F.
		for i := range s.Gauges {
			for _, g := range m.pending.Snapshot.Gauges {
				if g.Name == s.Gauges[i].Name {
					if w, err := mergeWindow(g.Window, s.Gauges[i].Window); err == nil {
						s.Gauges[i].Window = w
					}
					break
				}
			}
		}
	}
	return View{Enabled: true, Live: &s, Saved: cloneRecord(m.saved), SavedEnd: m.offset,
		Saving: m.busy, UnknownTail: m.unknown, SaveError: m.saveError, ReadError: m.readError}
}

func (m *Manager) Gauge(name, source string, request uint64, at int64, value uint64, status string, width time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return context.Canceled
	}
	if request == 0 {
		return nil
	}
	for i := range m.live.Gauges {
		if m.live.Gauges[i].Name == name {
			g := &m.live.Gauges[i]
			// Lifecycle statuses also belong to the ordered request stream.
			// A late break must not discard a newer valid baseline.
			if request <= g.LastRequest {
				return nil
			}
			if status == Unsupported || status == Paused {
				g.Break(status)
				g.LastRequest = request
				return nil
			}
			err := g.Observe(source, request, at, value, status, width, time.Duration(m.live.SampleInterval))
			if err != nil {
				g.Status, g.Continuous = Invalid, false
			}
			return err
		}
	}
	if len(m.live.Gauges) >= MaxGauges {
		return errors.New("usage: too many gauges")
	}
	g := Gauge{Name: name}
	if status == Unsupported || status == Paused {
		g.Break(status)
		g.LastRequest = request
		m.live.Gauges = append(m.live.Gauges, g)
		return nil
	}
	if err := g.Observe(source, request, at, value, status, width, time.Duration(m.live.SampleInterval)); err != nil {
		return err
	}
	m.live.Gauges = append(m.live.Gauges, g)
	return nil
}

func (m *Manager) Counter(name, source string, raw, hertz uint64, created bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return context.Canceled
	}
	for i := range m.live.Counters {
		if m.live.Counters[i].Name == name {
			c := &m.live.Counters[i]
			err := c.Observe(source, raw, hertz, created)
			if m.historyUnknown {
				c.Complete = false
			}
			return err
		}
	}
	if len(m.live.Counters) >= MaxCounters {
		return errors.New("usage: too many counters")
	}
	c := Counter{Name: name}
	if err := c.Observe(source, raw, hertz, created); err != nil {
		return err
	}
	if m.historyUnknown {
		c.Complete = false
	}
	m.live.Counters = append(m.live.Counters, c)
	return nil
}

func (m *Manager) CounterMissing(name string, final bool) {
	m.counterMissing(name, final, false)
}

// A vCPU without an accepted identity may have been replaced before its first
// successful read. Native process counters instead have a creation baseline
// from the sole process owner, even if their first proc read fails.
func (m *Manager) counterMissing(name string, final, requireSource bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.live.Counters {
		c := &m.live.Counters[i]
		if c.Name == name {
			c.Status = Missing
			if final || (requireSource && !c.SourceKnown) {
				c.Complete = false
			}
			return
		}
	}
	if len(m.live.Counters) < MaxCounters {
		m.live.Counters = append(m.live.Counters, Counter{Name: name, Status: Missing, Complete: !final && !requireSource && !m.historyUnknown})
	}
}

func (m *Manager) BreakGauges(status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.live.Gauges {
		m.live.Gauges[i].Break(status)
	}
}

func (m *Manager) discontinue() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.live.Gauges {
		m.live.Gauges[i].Continuous = false
	}
}

// Save has at most one physical operation in flight. Repeated calls while a
// writer is blocked are coalesced without creating goroutines or record queues.
func (m *Manager) Save(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveLocked(now)
}

func (m *Manager) saveLocked(now time.Time) {
	if m.busy || m.stopping || m.writer == nil || m.readError != "" {
		return
	}
	if m.pending == nil {
		seq := uint64(1)
		if m.saved != nil {
			if m.saved.Sequence == ^uint64(0) {
				m.saveError = ErrOverflow.Error()
				return
			}
			seq = m.saved.Sequence + 1
		}
		m.pending = &Record{Sequence: seq, SavedUTC: now.UnixNano(), Snapshot: m.live.clone()}
		for i := range m.live.Gauges {
			m.live.Gauges[i].Window = Window{}
		}
	}
	m.busy = true
	m.done = make(chan struct{})
	frozen, offset, unknown, done := m.pending, m.offset, m.unknown, m.done
	go m.write(frozen, offset, unknown, done)
}

func (m *Manager) rollback(offset int64) error {
	if err := m.writer.Truncate(offset); err != nil {
		return err
	}
	return m.writer.Sync()
}

func (m *Manager) write(frozen *Record, offset int64, unknown bool, done chan struct{}) {
	encoded, err := EncodeRecord(*frozen)
	if err == nil && unknown {
		err = m.rollback(offset)
	}
	if err == nil {
		var n int
		n, err = m.writer.WriteAt(encoded, offset)
		if err == nil && n != len(encoded) {
			err = io.ErrShortWrite
		}
		if err == nil {
			err = m.writer.Sync()
		}
		if err == nil && m.dirSync != nil {
			err = m.dirSync()
		}
	}
	rolledBack := false
	if err != nil {
		rolledBack = m.rollback(offset) == nil
	}
	m.mu.Lock()
	if err == nil {
		m.saved, m.offset = frozen, offset+int64(len(encoded))
		m.pending, m.unknown, m.saveError = nil, false, ""
	} else {
		m.saveError, m.unknown = err.Error(), !rolledBack
		if rolledBack {
			// Only a synchronized rollback permits F to be merged into A.
			for i := range m.live.Gauges {
				for _, g := range frozen.Snapshot.Gauges {
					if g.Name == m.live.Gauges[i].Name {
						w, mergeErr := mergeWindow(g.Window, m.live.Gauges[i].Window)
						if mergeErr != nil {
							m.readError = mergeErr.Error()
						} else {
							m.live.Gauges[i].Window = w
						}
						break
					}
				}
			}
			m.pending = nil
		}
	}
	m.busy = false
	stopping := m.stopping
	close(done)
	m.mu.Unlock()
	if stopping {
		m.closeFiles()
	}
}

func (m *Manager) closeFiles() {
	m.closeOnce.Do(func() {
		if m.closeFn != nil {
			m.closeFn()
		}
	})
}

// Close makes at most two bounded attempts: finish F, then seal the current A.
// Expiring the caller budget cannot cancel a filesystem syscall. Its sole
// worker retains and eventually closes the original file descriptors.
func (m *Manager) Close(ctx context.Context, now time.Time) {
	m.mu.Lock()
	m.live.Closed = true
	m.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		m.mu.Lock()
		m.saveLocked(now)
		done := m.done
		busy := m.busy
		m.mu.Unlock()
		if !busy {
			break
		}
		select {
		case <-done:
		case <-ctx.Done():
			attempt = 2
		}
		m.mu.Lock()
		complete := m.saved != nil && m.saved.Snapshot.Closed
		m.mu.Unlock()
		if complete {
			break
		}
	}
	m.mu.Lock()
	m.stopping = true
	busy := m.busy
	m.mu.Unlock()
	if !busy {
		m.closeFiles()
	}
}

func (m *Manager) History(cursor int64, limit int) ([]Record, int64, error) {
	m.mu.Lock()
	end, reader, id := m.offset, m.reader, m.live.SandboxID
	m.mu.Unlock()
	if reader == nil {
		return nil, cursor, errors.New("usage: history reader unavailable")
	}
	return ReadHistory(reader, end, cursor, limit, id)
}
