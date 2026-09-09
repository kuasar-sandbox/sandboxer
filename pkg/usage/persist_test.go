package usage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

type faultWriter struct {
	mu                             sync.Mutex
	data                           []byte
	writeErr, syncErr, truncateErr error
	partial                        int
	entered, release               chan struct{}
}

func (f *faultWriter) WriteAt(b []byte, offset int64) (int, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(b)
	if f.partial > 0 && f.partial < n {
		n = f.partial
	}
	end := int(offset) + n
	if end > len(f.data) {
		f.data = append(f.data, make([]byte, end-len(f.data))...)
	}
	copy(f.data[int(offset):], b[:n])
	return n, f.writeErr
}
func (f *faultWriter) Sync() error { f.mu.Lock(); defer f.mu.Unlock(); return f.syncErr }
func (f *faultWriter) Truncate(n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.truncateErr != nil {
		return f.truncateErr
	}
	f.data = f.data[:int(n)]
	return nil
}

func testManager(w Writer) *Manager {
	s := sampleRecord().Snapshot
	s.Counters = nil
	s.Gauges = nil
	return newManager(s, Recovery{}, w, nil)
}
func awaitSave(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not finish")
	}
}

func TestSaveConcurrentActiveAndFrozen(t *testing.T) {
	w := &faultWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	m := testManager(w)
	_ = m.Gauge("ram", "source", 1, 0, 10, OK, 0)
	_ = m.Gauge("ram", "source", 2, 1e9, 20, OK, 0)
	m.Save(time.Now())
	<-w.entered
	_ = m.Gauge("ram", "source", 3, 2e9, 30, OK, 0)
	v := m.View()
	if v.Saved != nil || !v.Saving || v.Live.Gauges[0].IntegralTotal != (Uint128{Lo: 30e9}) || v.Live.Gauges[0].Window.Peak != 30 {
		t.Fatalf("%+v", v)
	}
	close(w.release)
	awaitSave(t, m)
	v = m.View()
	if v.Saved.Snapshot.Gauges[0].IntegralTotal != (Uint128{Lo: 10e9}) || v.Live.Gauges[0].IntegralTotal != (Uint128{Lo: 30e9}) {
		t.Fatal("S advanced beyond F")
	}
}

func TestSaveRollbackAndUncertainTail(t *testing.T) {
	for _, mode := range []string{"partial", "enospc", "sync", "truncate"} {
		t.Run(mode, func(t *testing.T) {
			w := &faultWriter{}
			switch mode {
			case "partial":
				w.partial = 25
			case "enospc":
				w.partial, w.writeErr = 25, syscall.ENOSPC
			case "sync":
				w.syncErr = syscall.EIO
			case "truncate":
				w.writeErr, w.truncateErr = syscall.ENOSPC, syscall.EIO
			}
			m := testManager(w)
			_ = m.Gauge("ram", "source", 1, 0, 90, OK, 0)
			_ = m.Gauge("ram", "source", 2, 1e9, 10, OK, 0)
			m.Save(time.Now())
			awaitSave(t, m)
			v := m.View()
			if v.Saved != nil || v.SaveError == "" || v.Live.Gauges[0].Window.Peak != 90 {
				t.Fatalf("%+v", v)
			}
			uncertain := mode == "sync" || mode == "truncate"
			if v.UnknownTail != uncertain || (m.pending != nil) != uncertain {
				t.Fatalf("unknown=%v pending=%v", v.UnknownTail, m.pending)
			}
			frozen := cloneRecord(m.pending)
			_ = m.Gauge("ram", "source", 3, 2e9, 100, OK, 0)
			if uncertain && !reflect.DeepEqual(frozen, m.pending) {
				t.Fatal("changed F content")
			}
			w.mu.Lock()
			w.partial, w.writeErr, w.syncErr, w.truncateErr = 0, nil, nil, nil
			w.mu.Unlock()
			m.Save(time.Now())
			awaitSave(t, m)
			v = m.View()
			if v.UnknownTail || v.SaveError != "" || v.Saved == nil {
				t.Fatalf("%+v", v)
			}
			if uncertain && !reflect.DeepEqual(frozen, v.Saved) {
				t.Fatal("retry changed frozen identity/content")
			}
			w.mu.Lock()
			data := append([]byte(nil), w.data...)
			w.mu.Unlock()
			records, _, err := ReadHistory(bytes.NewReader(data), int64(len(data)), 0, 10, "test")
			if err != nil || len(records) != 1 {
				t.Fatalf("duplicate write: %d %v", len(records), err)
			}
		})
	}
}

func TestBlockedWriterRemainsBoundedAndCloseHasBudget(t *testing.T) {
	w := &faultWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	m := testManager(w)
	m.Save(time.Now())
	<-w.entered
	baseline := runtime.NumGoroutine()
	for i := 0; i < 10000; i++ {
		m.Save(time.Now())
		_ = m.Gauge("ram", "source", uint64(i+1), int64(i)*1e9, 1, OK, 0)
	}
	if runtime.NumGoroutine() > baseline+2 {
		t.Fatal("writer retries created goroutines")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { m.Close(ctx, time.Now()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close waited for blocked filesystem")
	}
	close(w.release)
	awaitSave(t, m)
}

func TestPinnedFileDeletionAndOfflineRecovery(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "instance")
	if err := os.Mkdir(base, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := Open(base, "test", "new", time.Now(), time.Second, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(base, "test.usage")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(base, 0700); err != nil {
		t.Fatal(err)
	}
	m.Save(time.Now())
	awaitSave(t, m)
	if _, err := os.Stat(filepath.Join(base, "test.usage")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("late writer created new path: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Close(ctx, time.Now())
}
