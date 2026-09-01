package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestLocationTargetFreshPublicationReopensFinal(t *testing.T) {
	directory := t.TempDir()
	fs := newTrackingLocationFileSystem()
	target := locationTestTarget(directory, nil, false)
	closed := false
	fs.wrapCreate = func(_ string, file locationWriteFile) locationWriteFile {
		return &hookedLocationWriteFile{base: file, closeHook: func() error {
			closed = true
			return file.Close()
		}}
	}
	target.fs = fs
	body := bytes.Repeat([]byte("fresh-publish-reread"), 64*1024)

	if _, err := target.Put(context.Background(), RoleOverlay, locationTestSource(t, body)); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("fresh final write fd was not closed")
	}
	if got := fs.opens.Load(); got != 2 {
		t.Fatalf("fresh publication opened one guard and one verifier, got %d opens", got)
	}
}

type hookedLstatLocationFileSystem struct {
	*trackingLocationFileSystem
	lstatHook func(string) (os.FileInfo, error)
}

func (f *hookedLstatLocationFileSystem) lstat(path string) (os.FileInfo, error) {
	return f.lstatHook(path)
}

func TestLocationTargetFreshPublicationRejectsReplacedFinal(t *testing.T) {
	directory := t.TempDir()
	fs := newTrackingLocationFileSystem()
	target := locationTestTarget(directory, nil, false)
	lstatCalls := 0
	target.fs = &hookedLstatLocationFileSystem{
		trackingLocationFileSystem: fs,
		lstatHook: func(path string) (os.FileInfo, error) {
			lstatCalls++
			return fs.base.lstat(path)
		},
	}
	body := bytes.Repeat([]byte("fresh-publish-replaced-final"), 32*1024)
	replacement := []byte("replacement final from another publisher")
	var destination string

	fs.wrapCreate = func(path string, file locationWriteFile) locationWriteFile {
		destination = path
		return &hookedLocationWriteFile{base: file, syncHook: func() error {
			if err := file.Sync(); err != nil {
				return err
			}
			// Replace the canonical path while the original write fd is still
			// open. This models the namespace race the post-write SameFile check
			// must detect and prevents immediate inode-number reuse from making
			// the test itself ambiguous.
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.WriteFile(path, replacement, 0o644)
		}}
	}

	_, err := target.Put(context.Background(), RoleOverlay, locationTestSource(t, body))
	if !errors.Is(err, errLocationFinalVanished) {
		t.Fatalf("replaced-final error = %v, want %v", err, errLocationFinalVanished)
	}
	got, readErr := os.ReadFile(destination)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("replacement changed: got %q, want %q", got, replacement)
	}
	if got := fs.opens.Load(); got != 0 {
		t.Fatalf("fresh replacement check reopened final %d times, want 0", got)
	}
	if lstatCalls != 1 {
		t.Fatalf("replaced final lstat calls = %d, want only the ownership check", lstatCalls)
	}
}

func TestLocationTargetFreshPublicationPreservesReplacementAfterClose(t *testing.T) {
	directory := t.TempDir()
	fs := newTrackingLocationFileSystem()
	target := locationTestTarget(directory, nil, false)
	target.fs = fs
	body := bytes.Repeat([]byte("fresh-publish-close-replacement"), 32*1024)
	replacement := []byte("replacement installed while closing the writer")
	var destination string

	fs.wrapCreate = func(path string, file locationWriteFile) locationWriteFile {
		destination = path
		return &hookedLocationWriteFile{base: file, closeHook: func() error {
			if err := file.Close(); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.WriteFile(path, replacement, 0o644)
		}}
	}

	_, err := target.Put(context.Background(), RoleOverlay, locationTestSource(t, body))
	if !errors.Is(err, errLocationFinalVanished) {
		t.Fatalf("close replacement error = %v, want %v", err, errLocationFinalVanished)
	}
	got, readErr := os.ReadFile(destination)
	if readErr != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("close replacement changed: got %q, err=%v", got, readErr)
	}
	if got := fs.opens.Load(); got != 2 {
		t.Fatalf("close replacement opens = %d, want guard and verifier", got)
	}
}

func TestLocationTargetValidationFailurePreservesReplacement(t *testing.T) {
	directory := t.TempDir()
	fs := newTrackingLocationFileSystem()
	target := locationTestTarget(directory, nil, false)
	target.fs = fs
	body := bytes.Repeat([]byte("fresh-publish-validation-replacement"), 32*1024)
	replacement := []byte("replacement installed during validation")
	var destination string

	fs.wrapCreate = func(path string, file locationWriteFile) locationWriteFile {
		destination = path
		return file
	}
	target.validate = func(_ context.Context, _ locationReadFile, _ os.FileInfo, _ string, _ uint64, _, _ string, _ bool) error {
		if err := os.Remove(destination); err != nil {
			return err
		}
		if err := os.WriteFile(destination, replacement, 0o644); err != nil {
			return err
		}
		return errInjectedLocationFailure
	}

	_, err := target.Put(context.Background(), RoleOverlay, locationTestSource(t, body))
	if !errors.Is(err, errInjectedLocationFailure) {
		t.Fatalf("validation replacement error = %v", err)
	}
	got, readErr := os.ReadFile(destination)
	if readErr != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("validation replacement changed: got %q, err=%v", got, readErr)
	}
}
