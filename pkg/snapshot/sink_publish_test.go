package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"golang.org/x/sys/unix"
)

var errInjectedCopyFailure = errors.New("injected copy failure")

func bytesFinalValidator(want []byte) finalValidator {
	return func(ctx context.Context, path string, syncFile bool) (os.FileInfo, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return info, fmt.Errorf("%w: final is not regular", errFinalMismatch)
		}
		got, err := io.ReadAll(file)
		if err != nil {
			return info, err
		}
		if !bytes.Equal(got, want) {
			return info, tarstream.ErrDigestMismatch
		}
		if syncFile {
			return info, file.Sync()
		}
		return info, nil
	}
}

func TestPublishFileExclusively(t *testing.T) {
	source := []byte("artifact payload")
	cases := []struct {
		name      string
		existing  string
		wantReuse bool
	}{
		{name: "fresh publish copies source"},
		{name: "valid existing final is reused", existing: "valid", wantReuse: true},
		{name: "invalid regular final is repaired", existing: "invalid"},
		{name: "vanished existing final is retried", existing: "vanishing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "staging.partial")
			if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(dir, "final.artifact")
			if tc.existing != "" && tc.existing != "vanishing" {
				body := []byte("inconsistent")
				if tc.existing == "valid" {
					body = source
				}
				if err := os.WriteFile(destination, body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			validate := bytesFinalValidator(source)
			if tc.existing == "vanishing" {
				if err := os.WriteFile(destination, []byte("inconsistent"), 0o600); err != nil {
					t.Fatal(err)
				}
				base := validate
				var destinationCalls atomic.Int32
				validate = func(ctx context.Context, path string, syncFile bool) (os.FileInfo, error) {
					if path == destination && destinationCalls.Add(1) == 1 {
						if err := os.Remove(destination); err != nil {
							t.Fatal(err)
						}
						return nil, os.ErrNotExist
					}
					return base(ctx, path, syncFile)
				}
			}

			oldUmask := unix.Umask(0o077)
			t.Cleanup(func() { unix.Umask(oldUmask) })
			reused, err := publishFileExclusively(context.Background(), sourcePath, destination, validate, "artifact")
			unix.Umask(oldUmask)
			if err != nil {
				t.Fatal(err)
			}
			if reused != tc.wantReuse {
				t.Fatalf("reused = %v, want %v", reused, tc.wantReuse)
			}
			body, err := os.ReadFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, source) {
				t.Fatalf("published bytes = %q, want %q", body, source)
			}
			info, err := os.Stat(destination)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o644 {
				t.Fatalf("published mode = %o, want 644", info.Mode().Perm())
			}
			if _, err := os.Stat(sourcePath); err != nil {
				t.Fatalf("staging source changed: %v", err)
			}
		})
	}
}

func TestPublishFileExclusivelyValidatesSourceBeforeCreatingFinal(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "staging.partial")
	destination := filepath.Join(dir, "final.artifact")
	if err := os.WriteFile(sourcePath, []byte("invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := publishFileExclusively(context.Background(), sourcePath, destination,
		func(context.Context, string, bool) (os.FileInfo, error) { return nil, tarstream.ErrDigestMismatch }, "artifact")
	if !errors.Is(err, tarstream.ErrDigestMismatch) {
		t.Fatalf("error = %v, want digest mismatch", err)
	}
	if _, statErr := os.Lstat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("invalid source created final: %v", statErr)
	}
}

func TestPublishFileExclusivelyCleansUpOwnedPartialFinal(t *testing.T) {
	dir := t.TempDir()
	source := []byte("artifact payload")
	sourcePath := filepath.Join(dir, "staging.partial")
	destination := filepath.Join(dir, "final.artifact")
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatal(err)
	}
	originalCopy := exclusivePublishCopy
	exclusivePublishCopy = func(_ context.Context, destination io.Writer, source io.Reader) (int64, error) {
		buffer := make([]byte, 4)
		n, _ := source.Read(buffer)
		written, _ := destination.Write(buffer[:n])
		return int64(written), errInjectedCopyFailure
	}
	t.Cleanup(func() { exclusivePublishCopy = originalCopy })

	_, err := publishFileExclusively(context.Background(), sourcePath, destination, bytesFinalValidator(source), "artifact")
	if !errors.Is(err, errInjectedCopyFailure) {
		t.Fatalf("error = %v, want injected copy failure", err)
	}
	if _, statErr := os.Lstat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("owned partial final was not removed: %v", statErr)
	}
}

func TestPublishFileExclusivelyDoesNotRemoveReplacementAfterOwnedWriteFailure(t *testing.T) {
	dir := t.TempDir()
	source := []byte("artifact payload")
	replacement := []byte("replacement from another publisher")
	sourcePath := filepath.Join(dir, "staging.partial")
	destination := filepath.Join(dir, "final.artifact")
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatal(err)
	}
	originalCopy := exclusivePublishCopy
	exclusivePublishCopy = func(context.Context, io.Writer, io.Reader) (int64, error) {
		if err := os.Remove(destination); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, replacement, 0o644); err != nil {
			t.Fatal(err)
		}
		return 0, errInjectedCopyFailure
	}
	t.Cleanup(func() { exclusivePublishCopy = originalCopy })

	_, err := publishFileExclusively(context.Background(), sourcePath, destination, bytesFinalValidator(source), "artifact")
	if !errors.Is(err, errInjectedCopyFailure) {
		t.Fatalf("error = %v, want injected copy failure", err)
	}
	body, readErr := os.ReadFile(destination)
	if readErr != nil || !bytes.Equal(body, replacement) {
		t.Fatalf("replacement final changed: %q err=%v", body, readErr)
	}
}

func TestPublishFileExclusivelyConcurrentRepair(t *testing.T) {
	dir := t.TempDir()
	source := bytes.Repeat([]byte{0x37}, 16<<20)
	sourcePath := filepath.Join(dir, "staging.partial")
	destination := filepath.Join(dir, "final.artifact")
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var copyCalls atomic.Int32
	originalCopy := exclusivePublishCopy
	exclusivePublishCopy = func(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
		if copyCalls.Add(1) == 1 {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		return copySequentially(ctx, destination, source)
	}
	t.Cleanup(func() { exclusivePublishCopy = originalCopy })

	type result struct {
		reused bool
		err    error
	}
	results := make(chan result, 2)
	publish := func() {
		go func() {
			reused, err := publishFileExclusively(ctx, sourcePath, destination, bytesFinalValidator(source), "artifact")
			results <- result{reused: reused, err: err}
		}()
	}
	publish()
	select {
	case <-firstStarted:
	case result := <-results:
		t.Fatalf("first publisher stopped before copying: %#v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	publish()
	var second result
	select {
	case second = <-results:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(releaseFirst)
	first := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent publish errors: first=%v second=%v", first.err, second.err)
	}
	if _, err := bytesFinalValidator(source)(context.Background(), destination, false); err != nil {
		t.Fatalf("final validation: %v", err)
	}
}

func TestPublishFileExclusivelyConcurrentRepairBindsRemovalToValidatedInode(t *testing.T) {
	// A second publisher validates the first publisher's still-partial inode as
	// invalid, then pauses. Before it removes the path, the first publisher
	// finishes copying the same inode. The removal must decline: the path now
	// names a complete artifact that its owner validated successfully.
	partial := []byte("partial")
	complete := bytes.Repeat([]byte{0x37}, 4096)
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "staging.partial")
	destination := filepath.Join(dir, "final.artifact")
	if err := os.WriteFile(sourcePath, complete, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, partial, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	validatedInvalid := make(chan os.FileInfo, 1)
	validate := func(ctx context.Context, path string, syncFile bool) (os.FileInfo, error) {
		if path != destination {
			return nil, nil // staging source is always acceptable
		}
		info, err := bytesFinalValidator(complete)(ctx, path, syncFile)
		if err == nil || !errors.Is(err, tarstream.ErrDigestMismatch) {
			return info, err
		}
		select {
		case validatedInvalid <- info:
		default:
		}
		// Hold the repair at the decision point until the owner completes.
		select {
		case <-ctx.Done():
			return info, ctx.Err()
		case <-time.After(2 * time.Second):
			return info, err
		}
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := publishFileExclusively(context.Background(), sourcePath, destination, validate, "artifact")
		secondDone <- err
	}()
	select {
	case info := <-validatedInvalid:
		if !os.SameFile(before, info) {
			t.Fatalf("validated identity changed: before=%v validated=%v", before, info)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second publisher never validated the partial final")
	}
	// The owner completes the very inode the second publisher judged invalid.
	// WriteFile keeps the path, so the binding must rely on the size and
	// modification time changing, not just the inode identity.
	if err := os.WriteFile(destination, complete, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-secondDone:
		// The repair may either decline silently and republish the identical
		// content, or observe the now-valid final and reuse it; both are
		// correct. It must not report success while deleting the owner's file.
		if err != nil && !errors.Is(err, errFinalReplaced) {
			t.Fatalf("second publisher error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second publisher never finished")
	}
	// Whatever the outcome, the final must remain present and complete.
	body, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, complete) {
		t.Fatalf("final deleted or corrupted after concurrent repair: %d bytes", len(body))
	}
}

func TestPublishFileExclusivelyPreservesFinalOnOperationalValidationError(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "cancellation", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "permission", err: os.ErrPermission},
		{name: "io failure", err: &os.PathError{Op: "read", Path: "final", Err: unix.EIO}},
		{name: "stale handle", err: &os.PathError{Op: "read", Path: "final", Err: unix.ESTALE}},
		{name: "unknown", err: errors.New("backend unavailable")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "staging.partial")
			destination := filepath.Join(dir, "final.artifact")
			if err := os.WriteFile(sourcePath, []byte("source"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(destination, []byte("preserve"), 0o644); err != nil {
				t.Fatal(err)
			}
			validate := func(_ context.Context, path string, _ bool) (os.FileInfo, error) {
				if path == sourcePath {
					return nil, nil
				}
				return nil, test.err
			}
			_, err := publishFileExclusively(context.Background(), sourcePath, destination, validate, "artifact")
			if !errors.Is(err, test.err) {
				t.Fatalf("error = %v, want %v", err, test.err)
			}
			body, readErr := os.ReadFile(destination)
			if readErr != nil || string(body) != "preserve" {
				t.Fatalf("existing final changed: %q err=%v", body, readErr)
			}
		})
	}
}

func TestPublishFileExclusivelyDoesNotReplaceNonRegularFinal(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "staging.partial")
	destination := filepath.Join(dir, "final.artifact")
	if err := os.WriteFile(sourcePath, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := publishFileExclusively(context.Background(), sourcePath, destination, bytesFinalValidator([]byte("source")), "artifact")
	if err == nil {
		t.Fatal("existing directory was accepted or replaced")
	}
	if info, statErr := os.Stat(destination); statErr != nil || !info.IsDir() {
		t.Fatalf("existing directory changed: info=%v err=%v", info, statErr)
	}
}
