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
	mu                    sync.Mutex
	data                  []byte
	writeErr, truncateErr error
	partial               int
	entered, release      chan struct{}
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
	return newManager(s, Recovery{}, w)
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
	for _, mode := range []string{"partial", "enospc", "write-error", "truncate"} {
		t.Run(mode, func(t *testing.T) {
			w := &faultWriter{}
			switch mode {
			case "partial":
				w.partial = 25
			case "enospc":
				w.partial, w.writeErr = 25, syscall.ENOSPC
			case "write-error":
				// Even a complete readable frame must not advance S when
				// WriteAt reports an error alongside its byte count.
				w.writeErr = syscall.EIO
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
			uncertain := mode == "truncate"
			if v.UnknownTail != uncertain || (m.pending != nil) != uncertain {
				t.Fatalf("unknown=%v pending=%v", v.UnknownTail, m.pending)
			}
			frozen := cloneRecord(m.pending)
			_ = m.Gauge("ram", "source", 3, 2e9, 100, OK, 0)
			if uncertain && !reflect.DeepEqual(frozen, m.pending) {
				t.Fatal("changed F content")
			}
			w.mu.Lock()
			w.partial, w.writeErr, w.truncateErr = 0, nil, nil
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

func TestWriterHandoffPreservesFinalRecord(t *testing.T) {
	for _, priorSave := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "saved"}[priorSave], func(t *testing.T) {
			base := t.TempDir()
			open := func(epoch string) *Manager {
				m, err := Open(base, "test", epoch, time.Now(), time.Second, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(m.closeFiles)
				return m
			}
			old := open("old")
			if err := old.Counter("cpu", "boot/pid/start", 10, 100, true); err != nil {
				t.Fatal(err)
			}
			if priorSave {
				old.Save(time.Now())
				awaitSave(t, old)
			}
			if other, err := Open(base, "test", "blocked", time.Now(), time.Second, time.Minute); err == nil || other != nil || !errors.Is(err, syscall.EWOULDBLOCK) {
				if other != nil {
					other.closeFiles()
				}
				t.Fatalf("second writer acquired the owned file: %v", err)
			}
			if err := old.Counter("cpu", "boot/pid/start", 110, 100, true); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			old.Close(ctx, time.Now())
			closed := old.View()
			prefix, err := os.ReadFile(filepath.Join(base, "test.usage"))
			if err != nil || closed.Saved == nil || closed.SaveError != "" || int64(len(prefix)) != closed.SavedEnd {
				t.Fatalf("old owner failed final append: %+v, %v", closed, err)
			}
			next := open("next")
			v := next.View()
			if !reflect.DeepEqual(v.Saved, closed.Saved) || v.SavedEnd != closed.SavedEnd || v.Live.Counters[0].KnownTotal != (Uint128{Lo: 1100000000}) {
				t.Fatalf("successor lost the final record: %+v", v)
			}
			next.Close(ctx, time.Now())
			data, err := os.ReadFile(filepath.Join(base, "test.usage"))
			if err != nil || !bytes.HasPrefix(data, prefix) || len(data) <= len(prefix) {
				t.Fatalf("successor overwrote confirmed history: %v", err)
			}
			r, err := Recover(bytes.NewReader(data), int64(len(data)), "test")
			if err != nil || r.Record == nil || r.Record.Sequence != closed.Saved.Sequence+1 || r.Record.Snapshot.Counters[0].KnownTotal != (Uint128{Lo: 1100000000}) {
				t.Fatalf("successor final recovery: %+v, %v", r, err)
			}
		})
	}
}

func TestBufferedFileSaveReopenAndTailRepair(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "test.usage")
	start := time.Now()
	m, err := Open(base, "test", "first", start, time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.closeFiles)
	if err := m.Gauge("ram", "memory", 1, 0, 10, OK, 0); err != nil {
		t.Fatal(err)
	}
	m.Save(start)
	awaitSave(t, m)
	first := m.View()
	if first.Saved == nil || first.SaveError != "" || first.Saved.Sequence != 1 {
		t.Fatalf("complete buffered append was not adopted: %+v", first)
	}
	// Release the owner without a final save. This is a same-host restart,
	// not a claim that buffered bytes survive a host crash or power loss.
	m.closeFiles()
	partial := *first.Saved
	partial.Sequence++
	frame, err := EncodeRecord(partial)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteAt(frame[:len(frame)/2], first.SavedEnd)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("partial tail setup: %v, %v", writeErr, closeErr)
	}
	m, err = Open(base, "test", "second", start.Add(time.Second), time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.closeFiles)
	if v := m.View(); !v.UnknownTail || !reflect.DeepEqual(v.Saved, first.Saved) || v.SavedEnd != first.SavedEnd {
		t.Fatalf("recovery changed the surviving baseline: %+v", v)
	}
	if err := m.Gauge("ram", "memory", 1, 0, 20, OK, 0); err != nil {
		t.Fatal(err)
	}
	m.Save(start.Add(2 * time.Second))
	awaitSave(t, m)
	if v := m.View(); v.UnknownTail || v.SaveError != "" || v.Saved.Sequence != 2 || v.Saved.Snapshot.RunEpoch != "second" {
		t.Fatalf("truncate/reappend failed: %+v", v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Close(ctx, start.Add(3*time.Second))
	v := m.View()
	if v.Saved.Sequence != 3 || !v.Saved.Snapshot.Closed || v.SaveError != "" {
		t.Fatalf("buffered close did not seal the current run: %+v", v)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	records, end, err := ReadHistory(bytes.NewReader(data), int64(len(data)), 0, 10, "test")
	if err != nil || len(records) != 3 || end != v.SavedEnd {
		t.Fatalf("repaired history: %d records, end=%d, err=%v", len(records), end, err)
	}
	if records[1].Snapshot.Gauges[0].IntegralTotal != (Uint128{}) || records[1].Snapshot.Gauges[0].Window.Peak != 20 {
		t.Fatal("restart integrated across epochs or reused the previous peak")
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
