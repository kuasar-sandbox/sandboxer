package diffstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"golang.org/x/sys/unix"
)

type memoryTemplate struct{ sparse.Source }

func (*memoryTemplate) Close() error { return nil }

func newMemoryTemplate(t *testing.T, size int) TemplateSource {
	t.Helper()
	source, err := sparse.NewSource(bytes.NewReader(bytes.Repeat([]byte{0x53}, size)), uint64(size), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &memoryTemplate{Source: source}
}

type observedTemplate struct {
	TemplateSource
	closes, verifies atomic.Int32
	verify           func(context.Context) error
	closeErr         error
}

func (s *observedTemplate) Verify(ctx context.Context) error {
	s.verifies.Add(1)
	if s.verify != nil {
		return s.verify(ctx)
	}
	return nil
}

func (s *observedTemplate) Close() error {
	s.closes.Add(1)
	return errors.Join(s.closeErr, s.TemplateSource.Close())
}

func templateOptions(checkpoint bool) []DiskOption {
	if checkpoint {
		return []DiskOption{WithCheckpoint("template-test", "")}
	}
	return nil
}

func assertNoFreshResources(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Checkpoint's independently owned active lock may persist after Close.
	for _, entry := range entries {
		if entry.Name() != "active.lock" {
			t.Fatalf("failed initialization retained %s", entry.Name())
		}
	}
}

func TestTemplateFactoryOnlyFreshOwnerOpens(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			options := templateOptions(checkpoint)
			var calls atomic.Int32
			factory := func() (TemplateSource, error) {
				calls.Add(1)
				return nil, errors.New("template must not open")
			}
			path := filepath.Join(t.TempDir(), "active")
			disk, err := OpenDisk(path, nil, DiffInit{CreateSize: cowBlockSize}, options...)
			if err != nil {
				t.Fatal(err)
			}
			if err := disk.Close(); err != nil {
				t.Fatal(err)
			}
			disk, err = OpenDisk(path, nil, DiffInit{Existing: true, Template: factory}, options...)
			if err != nil {
				t.Fatal(err)
			}
			if err := disk.Close(); err != nil {
				t.Fatal(err)
			}
			for _, existing := range [][]byte{nil, []byte("corrupt")} {
				path := filepath.Join(t.TempDir(), "active")
				if err := os.WriteFile(path, existing, 0o600); err != nil {
					t.Fatal(err)
				}
				for _, selectedExisting := range []bool{true, false} {
					disk, err := OpenDisk(path, nil, DiffInit{Existing: selectedExisting, Template: factory}, options...)
					if disk != nil || err == nil {
						t.Fatalf("invalid existing target accepted: %v", err)
					}
				}
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, existing) {
					t.Fatal("existing invalid target changed", err)
				}
			}
			dangling := filepath.Join(t.TempDir(), "active")
			if err := os.Symlink("missing-target", dangling); err != nil {
				t.Fatal(err)
			}
			for _, selectedExisting := range []bool{true, false} {
				disk, err := OpenDisk(dangling, nil, DiffInit{Existing: selectedExisting, Template: factory}, options...)
				if disk != nil || err == nil {
					t.Fatalf("dangling existing target accepted: %v", err)
				}
				if !selectedExisting && !errors.Is(err, unix.EEXIST) {
					t.Fatalf("fresh initializer did not reject existing symlink: %v", err)
				}
			}
			if target, err := os.Readlink(dangling); err != nil || target != "missing-target" {
				t.Fatal("existing symlink changed", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("ignored template opened %d times", calls.Load())
			}
			path = filepath.Join(t.TempDir(), "active")
			gate := newWorkerGate()
			source := &observedTemplate{TemplateSource: newMemoryTemplate(t, cowBlockSize), verify: func(context.Context) error { gate.wait(); return nil }}
			defer gate.open()
			disk, err = OpenDisk(path, nil, DiffInit{Template: func() (TemplateSource, error) { calls.Add(1); return source, nil }}, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { gate.open(); _ = disk.Close() }()
			awaitSignal(t, gate.entered)
			if _, err := OpenDisk(path, nil, DiffInit{Template: factory}, options...); !errors.Is(err, unix.EEXIST) {
				t.Fatalf("claim loser: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("fresh claim opened %d sources", calls.Load())
			}
		})
	}
}

func TestTemplateVerificationGatesReadyAndExportAfterOverwrite(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("checkpoint=%v/invalid=%v", checkpoint, invalid), func(t *testing.T) {
				materialGate, verifyGate := newWorkerGate(), newWorkerGate()
				defer materialGate.open()
				defer verifyGate.open()
				cause := errors.New("wrong full payload identity")
				source := &observedTemplate{TemplateSource: &gatedMaterialRun{TemplateSource: newMemoryTemplate(t, cowBlockSize), gate: materialGate}, verify: func(context.Context) error {
					verifyGate.wait()
					if invalid {
						return cause
					}
					return nil
				}}
				dir := t.TempDir()
				path := filepath.Join(dir, "active")
				disk, err := OpenDisk(path, nil, DiffInit{Template: func() (TemplateSource, error) { return source, nil }}, templateOptions(checkpoint)...)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { materialGate.open(); verifyGate.open(); _ = disk.Close() }()
				awaitSignal(t, materialGate.entered)
				if _, err := disk.WriteAt(bytes.Repeat([]byte{0x91}, cowBlockSize), 0); err != nil {
					t.Fatal(err)
				}
				materialGate.open()
				awaitSignal(t, verifyGate.entered)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				if err := disk.WaitMaterialReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("Ready preceded full verification: %v", err)
				}
				if view, err := disk.OpenActiveView(ctx); view != nil || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("export preceded full verification: %v", err)
				}
				if checkpoint {
					if err := disk.Checkpoint(ctx, "unchecked"); !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("checkpoint preceded full verification: %v", err)
					}
				}
				if source.closes.Load() != 0 {
					t.Fatal("source closed during verification")
				}
				verifyGate.open()
				err = disk.WaitMaterialized(context.Background())
				if invalid {
					if !errors.Is(err, cause) || !errors.Is(disk.WaitMaterialReady(context.Background()), cause) || !errors.Is(disk.Err(), cause) {
						t.Fatalf("verification error not propagated: %v", err)
					}
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("invalid template published: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				_ = disk.Close()
				_ = disk.Close()
				if source.verifies.Load() != 1 || source.closes.Load() != 1 {
					t.Fatalf("verify=%d close=%d", source.verifies.Load(), source.closes.Load())
				}
				if invalid {
					assertNoFreshResources(t, dir)
				}
			})
		}
	}
}

func TestTemplateFailuresCloseSourceOnceAndReleaseClaim(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, failure := range []string{"factory", "nil", "size", "base", "close"} {
			t.Run(fmt.Sprintf("checkpoint=%v/%s", checkpoint, failure), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "active")
				size := cowBlockSize
				if failure == "size" {
					size--
				}
				source := &observedTemplate{TemplateSource: newMemoryTemplate(t, size)}
				cause := errors.New("injected source failure")
				if failure == "close" {
					source.closeErr = cause
				}
				var base BlockReader
				if failure == "base" {
					base = &fakeReader{data: make([]byte, 2*cowBlockSize)}
				}
				calls := 0
				factory := func() (TemplateSource, error) {
					calls++
					if failure == "factory" {
						return nil, cause
					}
					if failure == "nil" {
						return nil, nil
					}
					return source, nil
				}
				disk, err := OpenDisk(path, base, DiffInit{Template: factory}, templateOptions(checkpoint)...)
				if failure == "close" {
					if err != nil {
						t.Fatal(err)
					}
					err = disk.WaitMaterialReady(context.Background())
					if !errors.Is(err, cause) {
						t.Fatalf("source Close error lost: %v", err)
					}
					if !errors.Is(disk.Close(), cause) {
						t.Fatal("Disk.Close lost source failure")
					}
					_ = disk.Close()
				} else if err == nil || disk != nil {
					t.Fatalf("injected %s failure accepted", failure)
				}
				wantCloses := int32(1)
				if failure == "factory" || failure == "nil" {
					wantCloses = 0
				}
				if calls != 1 || source.closes.Load() != wantCloses {
					t.Fatalf("factory calls=%d source closes=%d", calls, source.closes.Load())
				}
				assertNoFreshResources(t, dir)
				retry, err := OpenDisk(path, nil, DiffInit{CreateSize: cowBlockSize}, templateOptions(checkpoint)...)
				if err != nil {
					t.Fatalf("failure blocked retry: %v", err)
				}
				if err := retry.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestTemplateVerifierTeardownLifetime(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, disposable := range []bool{false, true} {
			t.Run(fmt.Sprintf("checkpoint=%v/disposable=%v", checkpoint, disposable), func(t *testing.T) {
				gate := newWorkerGate()
				defer gate.open()
				source := &observedTemplate{TemplateSource: newMemoryTemplate(t, cowBlockSize), verify: func(ctx context.Context) error {
					close(gate.entered)
					select {
					case <-gate.release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}}
				options := templateOptions(checkpoint)
				if disposable {
					options = append(options, WithDiscardOnClose())
				}
				dir := t.TempDir()
				disk, err := OpenDisk(filepath.Join(dir, "active"), nil, DiffInit{Template: func() (TemplateSource, error) { return source, nil }}, options...)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { gate.open(); _ = disk.Close() }()
				awaitSignal(t, gate.entered)
				closed := make(chan error, 1)
				go func() { closed <- disk.Close() }()
				if !disposable {
					select {
					case err := <-closed:
						t.Fatalf("retained Close canceled verification: %v", err)
					case <-time.After(20 * time.Millisecond):
					}
					gate.open()
				}
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("Close did not join verifier")
				}
				if source.closes.Load() != 1 {
					t.Fatalf("source closes=%d", source.closes.Load())
				}
				if disposable {
					assertNoFreshResources(t, dir)
				}
			})
		}
	}
}

func TestFreshPublishNeverReplacesRacingEmptyFile(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "active")
			options, err := parseDiskOptions(templateOptions(checkpoint))
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := createFreshDiff(path, DiffInit{CreateSize: cowBlockSize}, options)
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.abort()
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := fresh.publish(); !errors.Is(err, unix.EEXIST) {
				t.Fatalf("racing empty target replaced: %v", err)
			}
			after, err := os.Stat(path)
			if err != nil || after.Size() != 0 || !os.SameFile(before, after) {
				t.Fatalf("racing empty target mutated: %v", err)
			}
		})
	}
}

type admittedByteTemplate struct {
	TemplateSource
	gate *workerGate
}

func (s *admittedByteTemplate) RunAt(off, limit uint64) (sparse.Run, error) {
	if limit == 1 {
		s.gate.wait()
	}
	return s.TemplateSource.RunAt(off, limit)
}

func TestTemplateSuccessJoinsAdmittedReaderBeforeCloseAndReady(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			gate := newWorkerGate()
			defer gate.open()
			holes, err := sparse.NewSource(bytes.NewReader(make([]byte, 2*cowBlockSize)), 2*cowBlockSize, []sparse.Extent{{Size: 2 * cowBlockSize}})
			if err != nil {
				t.Fatal(err)
			}
			reader := &admittedByteTemplate{TemplateSource: &memoryTemplate{Source: holes}, gate: newWorkerGate()}
			verified := make(chan struct{})
			source := &observedTemplate{TemplateSource: &fullRunGatedSource{TemplateSource: reader, gate: gate}, verify: func(context.Context) error { close(verified); return nil }}
			disk, err := OpenDisk(filepath.Join(t.TempDir(), "active"), nil, DiffInit{Template: func() (TemplateSource, error) { return source, nil }}, templateOptions(checkpoint)...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { gate.open(); reader.gate.open(); _ = disk.Close() }()
			awaitSignal(t, gate.entered)
			readDone := make(chan error, 1)
			go func() {
				var p [1]byte
				_, err := disk.ReadAt(p[:], 0)
				if err == nil && p[0] != 0 {
					err = fmt.Errorf("admitted read lost source")
				}
				readDone <- err
			}()
			awaitSignal(t, reader.gate.entered)
			gate.open()
			awaitSignal(t, verified)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := disk.WaitMaterialReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Ready passed admitted source reader: %v", err)
			}
			if source.closes.Load() != 0 {
				t.Fatal("source closed with an admitted read")
			}
			reader.gate.open()
			if err := <-readDone; err != nil {
				t.Fatal(err)
			}
			if err := disk.WaitMaterialized(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := disk.Close(); err != nil {
				t.Fatal(err)
			}
			if source.closes.Load() != 1 {
				t.Fatalf("source closed %d times", source.closes.Load())
			}
		})
	}
}
