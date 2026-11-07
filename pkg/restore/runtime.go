package restore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
)

// ErrRuntimeIncompatible means every permitted local candidate is either
// absent or has a different content identity. Invalid metadata, unreadable
// files and cancellation are not compatibility decisions.
var ErrRuntimeIncompatible = errors.New("no compatible runtime bundle")

// RuntimeIncompatibleError preserves the failed candidates for diagnostics
// while allowing admission callers to use errors.Is(err, ErrRuntimeIncompatible).
type RuntimeIncompatibleError struct {
	Required string
	Err      error
}

func (e *RuntimeIncompatibleError) Error() string {
	return fmt.Sprintf("%s for %s: %v", ErrRuntimeIncompatible, e.Required, e.Err)
}

func (e *RuntimeIncompatibleError) Unwrap() error { return e.Err }

func (e *RuntimeIncompatibleError) Is(target error) bool {
	return target == ErrRuntimeIncompatible
}

// ResolveRuntimeBinding selects a local runtime for an immutable portable
// identity. configured is an unqualified file:// host path; required is an
// unlocated file basename with a content digest. Content identity consists of
// the digest scheme and digest, so a matching primary may have any basename.
//
// The explicit path is checked first. Only a failed preflight tries required's
// basename in that path's directory, without following a default symlink to
// choose the directory. No other directory, download or VM retry is attempted.
// The bundle envelope and declared footer identity are checked without scanning
// its payload. The returned absolute path is not an open-file capability; the
// execution path must retain its own validation.
func ResolveRuntimeBinding(ctx context.Context, configured, required string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	want, err := manifest.ParseRef(required)
	if err != nil {
		return "", fmt.Errorf("boot.runtime portable identity: %w", err)
	}
	if want.Scheme != manifest.RefSchemeFile || want.Location != "" || want.DigestScheme == "" || want.Digest == "" ||
		want.Path == "." || want.Path == ".." || strings.ContainsAny(want.Path, `/\\`) {
		return "", errors.New("boot.runtime portable identity must be an unlocated file basename with digest")
	}
	host, err := manifest.ParseRef(configured)
	if err != nil {
		return "", fmt.Errorf("boot.runtime host binding: %w", err)
	}
	if host.Scheme != manifest.RefSchemeFile || host.Location != "" || host.Digest != "" || strings.ContainsRune(host.Path, 0) {
		return "", errors.New("boot.runtime host binding must be an unqualified local file:// path")
	}
	primary, err := filepath.Abs(host.Path)
	if err != nil {
		return "", fmt.Errorf("boot.runtime host path: %w", err)
	}
	// A candidate failure is classified only after all permitted candidates
	// fail. A mismatch must not hide an I/O error from the other candidate.
	check := func(path string) (incompatible bool, err error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		candidate := config.SandboxConfig{Boot: config.BootConfig{Runtime: "file://" + path}}
		actual, err := sandbox.ResolveRuntimeProjection(&candidate)
		if cancelled := ctx.Err(); cancelled != nil {
			return false, cancelled
		}
		if err != nil {
			return errors.Is(err, os.ErrNotExist), err
		}
		matches, err := config.RuntimeContentMatches(required, actual)
		if err != nil {
			return false, err
		}
		if !matches {
			return true, fmt.Errorf("identity mismatch: portable %s, host %s", required, actual)
		}
		return false, nil
	}
	failure := func(err error, incompatible bool) (string, error) {
		if incompatible {
			return "", &RuntimeIncompatibleError{Required: required, Err: err}
		}
		return "", err
	}
	primaryIncompatible, primaryErr := check(primary)
	if primaryErr == nil {
		return primary, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	primaryErr = fmt.Errorf("boot.runtime primary %s: %w", primary, primaryErr)
	fallback := filepath.Join(filepath.Dir(primary), want.Path)
	if fallback == primary {
		return failure(primaryErr, primaryIncompatible)
	}
	fallbackIncompatible, fallbackErr := check(fallback)
	if fallbackErr != nil {
		if cancelled := ctx.Err(); cancelled != nil {
			return "", cancelled
		}
		return failure(errors.Join(primaryErr, fmt.Errorf("boot.runtime fallback %s: %w", fallback, fallbackErr)),
			primaryIncompatible && fallbackIncompatible)
	}
	log.Printf("[sandbox runtime] selected %s after %v", fallback, primaryErr)
	return fallback, nil
}
