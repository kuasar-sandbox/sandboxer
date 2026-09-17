package vhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
)

func TestWritebackFailureVisibleBeforeCleanup(t *testing.T) {
	for _, writeErr := range []error{io.ErrShortWrite, errors.New("injected write failure")} {
		t.Run(writeErr.Error(), func(t *testing.T) {
			writeGate, cleanupGate := newWorkerGate(), newWorkerGate()
			defer writeGate.open()
			defer cleanupGate.open()
			cleanupErr := errors.New("injected cleanup failure")
			waiting := make(chan struct{}, 8)
			cache := testCache(t, 2, 1, cacheHooks{
				beforeWrite:   func([]*cachePage) error { writeGate.wait(); return writeErr },
				beforeCleanup: func() error { cleanupGate.wait(); return cleanupErr },
				waiting: func() {
					select {
					case waiting <- struct{}{}:
					default:
					}
				},
			})
			cow := cachedCOW(t, cache, 3, nil)
			reported := make(chan error, 2)
			var reports atomic.Int32
			cow.SetFatalHandler(func(err error) { reports.Add(1); reported <- err })
			writePage(t, cow, 0, 0x61)
			awaitSignal(t, writeGate.entered)
			pending := make(chan error, 1)
			go func() { _, err := cow.WriteAt(bytes.Repeat([]byte{0x62}, cowBlockSize), cowBlockSize); pending <- err }()
			awaitSignal(t, waiting) // the second write is really waiting for dirty quota
			writeGate.open()
			awaitSignal(t, cleanupGate.entered)
			if err := cow.Err(); !errors.Is(err, writeErr) {
				t.Fatalf("known write failure hidden by blocked cleanup: %v", err)
			}
			if err := awaitError(t, reported); !errors.Is(err, writeErr) {
				t.Fatalf("owner error: %v", err)
			}
			if err := awaitError(t, pending); !errors.Is(err, writeErr) {
				t.Fatalf("quota waiter error: %v", err)
			}
			if _, err := cow.WriteAt([]byte("new"), 2*cowBlockSize); !errors.Is(err, writeErr) {
				t.Fatalf("admitted new write: %v", err)
			}
			if err := cache.Drain(context.Background()); !errors.Is(err, writeErr) {
				t.Fatalf("Drain error: %v", err)
			}
			if err := cow.Flush(); !errors.Is(err, writeErr) {
				t.Fatalf("Flush masked fatal: %v", err)
			}
			checkBudget(t, cache, 1, 1, 1)
			cache.mu.Lock()
			active := cache.active
			cache.mu.Unlock()
			if active != cow {
				t.Fatal("I/O owner cleared before cleanup finished")
			}
			closed := make(chan error, 1)
			go func() { closed <- cow.Close() }()
			awaitSignal(t, waiting) // detach must wait for the still-active cleanup
			select {
			case err := <-closed:
				t.Fatalf("Close finished during cleanup: %v", err)
			default:
			}
			if _, err := cow.diff.f.Stat(); err != nil {
				t.Fatalf("fd closed during cleanup: %v", err)
			}
			checkBudget(t, cache, 1, 1, 1)
			cleanupGate.open()
			if err := awaitError(t, closed); !errors.Is(err, writeErr) {
				t.Fatalf("Close lost original failure: %v", err)
			}
			err := cache.Close()
			if !errors.Is(err, writeErr) || !errors.Is(err, cleanupErr) {
				t.Fatalf("late cleanup error not retained with original: %v", err)
			}
			if reports.Load() != 1 {
				t.Fatalf("owner notified %d times", reports.Load())
			}
			checkBudget(t, cache, 0, 0, 0)
		})
	}
}
