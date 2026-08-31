package artifact

import (
	"bytes"
	"context"
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
