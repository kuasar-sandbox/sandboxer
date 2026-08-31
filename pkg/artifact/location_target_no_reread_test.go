package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestLocationTargetFreshPublicationDoesNotReopenFinal(t *testing.T) {
	directory := t.TempDir()
	fs := newTrackingLocationFileSystem()
	target := locationTestTarget(directory, nil, false)
	target.fs = fs
	body := bytes.Repeat([]byte("fresh-publish-no-reread"), 64*1024)

	if _, err := target.Put(context.Background(), RoleOverlay, locationTestSource(t, body)); err != nil {
		t.Fatal(err)
	}
	if got := fs.opens.Load(); got != 0 {
		t.Fatalf("fresh publication reopened final %d times, want 0", got)
	}
}

func TestLocationTargetFreshPublicationRejectsReplacedFinal(t *testing.T) {
	directory := t.TempDir()
	fs := newTrackingLocationFileSystem()
	target := locationTestTarget(directory, nil, false)
	target.fs = fs
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
}
