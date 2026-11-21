package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"golang.org/x/sys/unix"
)

// CheckpointSourceGuard serializes durable parent-edge publication and backend
// revocation. This local fence supplements, rather than replaces, the caller's
// external registry fence and complete retention set. Close only after the new
// source registration has committed (or definitively failed).
type CheckpointSourceGuard struct{ files []*os.File }

func (g *CheckpointSourceGuard) Close() error {
	if g == nil {
		return nil
	}
	var err error
	for i := len(g.files) - 1; i >= 0; i-- {
		err = errors.Join(err, g.files[i].Close())
	}
	g.files = nil
	return err
}

// LockCheckpointSources uses one stable inode per directory, independent of
// DirectoryOwner and reader pins. Multiple directories have a canonical order.
func LockCheckpointSources(ctx context.Context, directories ...string) (_ *CheckpointSourceGuard, retErr error) {
	dirs := make([]string, 0, len(directories))
	for _, directory := range directories {
		abs, err := filepath.Abs(directory)
		if err != nil {
			return nil, err
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, real)
	}
	sort.Strings(dirs)
	dirs = slices.Compact(dirs)
	g := &CheckpointSourceGuard{}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, g.Close())
		}
	}()
	for _, dir := range dirs {
		fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		lockFD, err := unix.Openat(fd, ".checkpoint-sources.lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(lockFD), filepath.Join(dir, ".checkpoint-sources.lock"))
		g.files = append(g.files, file)
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("checkpoint source fence is not a regular file")
		}
		for {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			err = unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB)
			if err == nil {
				break
			}
			if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
				return nil, err
			}
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		named, err := os.Lstat(file.Name())
		if err != nil || !os.SameFile(info, named) {
			return nil, errors.Join(errors.New("checkpoint source fence identity changed"), err)
		}
	}
	return g, nil
}

// ProtectCheckpointParents validates new durable edges under the same fence as
// deletion. Existing opened readers do not satisfy this check. Memory parents
// retain their strong source semantics; only metadata is inspected here.
func (s *ProcessStorage) ProtectCheckpointParents(ctx context.Context, c Checkpoint, memory []string) (_ io.Closer, retErr error) {
	if s == nil {
		return nil, errors.New("checkpoint requires storage")
	}
	c, dir, err := normalizeCheckpointSource(c, false)
	if err != nil {
		return nil, err
	}
	plan := func() (*checkpointKeep, error) {
		c.Retain = []CheckpointSource{}
		k, err := s.checkpointKeepPlan(ctx, c)
		if err != nil {
			return nil, err
		}
		scope := publishScope{relativeDir: dir, fetcher: s.Fetcher()}
		for _, raw := range memory {
			resolved, err := k.retain(ctx, raw, scope)
			if err != nil {
				return nil, err
			}
			ref, err := manifest.ParseRef(resolved)
			if err != nil {
				return nil, err
			}
			// A located parent can still be a node-local capture. The history
			// compaction boundary does not define the publication fence. Inspect
			// only positively identified captures; ordinary flat lowers are opaque.
			if ref.Scheme == manifest.RefSchemeFile && knownLocalMemoryCarrier(ref.Path, ref) {
				if err = k.retainMemoryRoot(ctx, raw, scope); err != nil {
					return nil, err
				}
			}
		}
		return k, nil
	}
	k, err := plan()
	if err != nil {
		return nil, err
	}
	var dirs []string
	if _, err := os.Stat(dir); err == nil {
		dirs = append(dirs, dir)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	// Actual local parent directories are always included, even when this
	// ordinary capture has no checkpoint destination of its own.
	for source := range k.sourceDirectories {
		dirs = append(dirs, source)
	}
	guard, err := LockCheckpointSources(ctx, dirs...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, guard.Close())
		}
	}()
	k, err = plan()
	if err != nil {
		return nil, err
	}
	for source := range k.sourceDirectories {
		if !slices.Contains(dirs, source) {
			return nil, errors.New("checkpoint parent directory changed during publication")
		}
	}
	// Reject a new edge to a disk version revoked before this fence, even if
	// the execution instance still owns a readable pinned view.
	for directory, disks := range k.sourceDiffVersions {
		for id, versions := range disks {
			for version := range versions {
				// Use a fresh named reader, which validates visibility and identity
				// alongside a live writer. An offline Store would conflict with
				// the runtime that owns this same DIFF after restore.
				view, err := s.openCheckpointVersion(ctx, directory, version, id)
				if err != nil {
					return nil, fmt.Errorf("checkpoint parent %s/%s: %w", id, version, err)
				}
				if err = view.Close(); err != nil {
					return nil, err
				}
			}
		}
	}
	return guard, nil
}
