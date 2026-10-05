package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRestoreUsesSnapshotValidation(t *testing.T) {
	r, err := Restore(context.Background(), RestoreSpec{})
	if r != nil || err == nil || !strings.Contains(err.Error(), "SnapshotPath or SnapshotManifestKey required") {
		t.Fatalf("Restore=%v/%v", r, err)
	}
}
func TestCanceledLifecycleDoesNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, start := range []func() (*Runtime, error){func() (*Runtime, error) { return StartRuntime(ctx, RuntimeSpec{}) }, func() (*Runtime, error) { return Start(ctx, SandboxSpec{}) }, func() (*Runtime, error) { return Restore(ctx, RestoreSpec{}) }} {
		if r, err := start(); r != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled start=%v/%v", r, err)
		}
	}
}
