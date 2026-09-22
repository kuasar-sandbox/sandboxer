package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"golang.org/x/sys/unix"
)

// Checkpoint identifies a committed managed source. The caller must exclusively
// own Directory and fence every runner, restore and publisher using it until
// CleanupCheckpoint returns. Arbitrary FileSink outputs grant no such right.
type Checkpoint struct {
	Directory       string
	SandboxID       string // producer identity, not PathID or a migration stable identity
	SnapshotRef     string // empty for E-only
	SandboxRef      string
	RefLocations    config.RefLocations
	ResolveLocation func(string) (string, error) // optional trusted mapping policy; returns host directory
}

type checkpointKeep struct {
	resolveLocation func(string) (string, error)
	p               *Publisher
	directory       string
	files           map[string]bool
	aliases         map[string]string // semantic alias -> current role's physical carrier basename
}

// retain resolves the physical source BEFORE comparing basenames. Memory and
// disk lowers are flat payload layers: their historical execution/disk configs
// do not contribute edges. Bundle selection reads indexes, never leaf payloads.
func (k *checkpointKeep) retain(ctx context.Context, raw string, scope publishScope) (string, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return "", err
	}
	if err := k.addLocation(ref); err != nil {
		return "", err
	}
	if ref.Scheme == manifest.RefSchemeManifest {
		if local, ok := scope.fetcher.(*manifestbundle.ManifestFetcher); ok {
			key, err := manifest.ParseKeyRef(ref.Path)
			if err != nil {
				return "", err
			}
			err = readretry.Do(ctx, func() error {
				selected, err := local.SelectManifest(ctx, key)
				if err == nil {
					scope.rememberSelection(raw, selected)
				}
				return err
			})
			if err != nil {
				return "", err
			}
			ref, err = manifest.ParseRef(scopedRef(raw, scope))
			if err != nil {
				return "", err
			}
		}
		if ref.Scheme == manifest.RefSchemeManifest {
			return ref.String(), nil
		}
	}
	path, err := k.p.locations.ResolveFile(ref, scope.relativeDir)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Needed aliases retain their physical target as well as the alias. Never
	// turn an external basename into a local dependency.
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if filepath.Dir(physical) == k.directory {
		info, err := os.Lstat(physical)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("checkpoint dependency is not a regular file")
		}
		k.files[filepath.Base(physical)] = true
	}
	if filepath.Dir(path) == k.directory {
		k.files[filepath.Base(path)] = true
	}
	ref.Path, ref.Location = physical, ""
	return ref.String(), nil
}

func (k *checkpointKeep) retainRoot(ctx context.Context, raw string, scope publishScope, alias string) error {
	resolved, err := k.retain(ctx, raw, scope)
	if err != nil {
		return err
	}
	ref, err := manifest.ParseRef(resolved)
	if err != nil {
		return err
	}
	if ref.Scheme == manifest.RefSchemeFile && filepath.Dir(ref.Path) == k.directory {
		k.aliases[alias] = filepath.Base(ref.Path)
	}
	return nil
}

func (k *checkpointKeep) plan(ctx context.Context, c Checkpoint) (retErr error) {
	scope := publishScope{relativeDir: k.directory, fetcher: k.p.storage.Fetcher()}
	eScope := scope
	eRef := c.SandboxRef
	if c.SnapshotRef != "" {
		if err := k.retainRoot(ctx, c.SnapshotRef, scope, c.SandboxID+".snapshot"); err != nil {
			return err
		}
		stream, child, err := k.open(ctx, c.SnapshotRef, scope)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, stream.Close()) }()
		rawConfig, err := snapshotfile.ReadConfig(ctx, stream)
		if err != nil {
			return err
		}
		cfg, err := snapshot.ParseConfig(rawConfig)
		if err != nil {
			return err
		}
		encoded, err := snapshot.MarshalConfig(cfg)
		if err != nil || string(encoded) != string(rawConfig) {
			return errors.New("checkpoint snapshot config is not canonical")
		}
		// Verify the durable pair by full physical selector, including Bundle members.
		expected, err := k.retain(ctx, c.SandboxRef, scope)
		if err != nil {
			return err
		}
		actual, err := k.retain(ctx, cfg.SandboxRef, child)
		if err != nil {
			return err
		}
		if actual != expected {
			return errors.New("checkpoint S/E source binding mismatch")
		}
		for _, raw := range cfg.FromRefs {
			if _, err := k.retain(ctx, raw, child); err != nil {
				return err
			}
		}
		eRef, eScope = cfg.SandboxRef, child
	}
	if err := k.retainRoot(ctx, eRef, eScope, c.SandboxID+".sandbox"); err != nil {
		return err
	}
	stream, child, err := k.open(ctx, eRef, eScope)
	if err != nil {
		return err
	}
	root, err := sandboxfile.Open(ctx, stream)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	// Kernel/runtime basename identities bind host-supplied boot inputs at
	// restore. They are not artifact edges into this checkpoint directory.
	for _, dep := range portableDiskRefs(root.Portable) {
		if _, err := k.retain(ctx, dep.raw, child); err != nil {
			return err
		}
	}
	return nil
}

// CleanupCheckpoint deletes only recognized obsolete direct entries after a
// complete metadata-only keep plan succeeds (including reader Close). It never
// opens a candidate payload. A pinned directory fd confines enumeration and
// unlink even if an ancestor or the pathname is concurrently replaced.
func (s *ProcessStorage) CleanupCheckpoint(ctx context.Context, c Checkpoint) (retErr error) {
	if s == nil || (c.SandboxID == "" || c.SandboxID == "." || c.SandboxID == "..") || filepath.Base(c.SandboxID) != c.SandboxID || strings.ContainsAny(c.SandboxID, `/\\`) || c.SandboxRef == "" {
		return errors.New("checkpoint cleanup requires producer identity and committed source")
	}
	dir, err := filepath.Abs(c.Directory)
	if err != nil || c.Directory == "" {
		return errors.New("checkpoint directory is required")
	}
	for _, raw := range []*string{&c.SnapshotRef, &c.SandboxRef} {
		if *raw != "" && !strings.HasPrefix(*raw, "file://") && !strings.HasPrefix(*raw, "manifest://") {
			path := *raw
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			*raw = (manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path}).String()
		}
	}
	primary := c.SandboxRef
	if c.SnapshotRef != "" {
		primary = c.SnapshotRef
	}
	rootRef, err := manifest.ParseRef(primary)
	if err != nil || rootRef.Scheme != manifest.RefSchemeFile || rootRef.Location != "" {
		return errors.New("checkpoint cleanup requires a committed local root")
	}
	rootPath, err := c.RefLocations.ResolveFile(rootRef, dir)
	if err != nil {
		return err
	}
	rootPath, err = filepath.Abs(rootPath)
	if err != nil || filepath.Dir(rootPath) != dir {
		return errors.New("committed root is outside checkpoint directory")
	}
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if physical != dir {
		return errors.New("checkpoint directory must be canonical without symlinks")
	}
	rootPhysical, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return err
	}
	if filepath.Dir(rootPhysical) != dir {
		return errors.New("committed root target is outside checkpoint directory")
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), dir)
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	pinned, err := f.Stat()
	if err != nil {
		return err
	}
	if c.RefLocations == nil {
		c.RefLocations = config.RefLocations{}
	}
	k := &checkpointKeep{p: newPublisher(s, c.RefLocations, nil, nil), directory: dir, files: map[string]bool{}, aliases: map[string]string{}, resolveLocation: c.ResolveLocation}
	if err := k.plan(ctx, c); err != nil {
		return fmt.Errorf("checkpoint keep plan: %w", err)
	}
	// Path-based reads above must still describe the same pinned directory.
	current, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	physical, err = filepath.EvalSymlinks(dir)
	if err != nil || physical != dir || !os.SameFile(pinned, current) {
		return errors.New("checkpoint directory identity changed")
	}
	for {
		entries, err := f.ReadDir(128)
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, io.EOF) {
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			var st unix.Stat_t
			if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				if errors.Is(err, unix.ENOENT) {
					continue
				}
				return err
			}
			mode := os.FileMode(0)
			switch st.Mode & unix.S_IFMT {
			case unix.S_IFREG:
				mode = 0
			case unix.S_IFLNK:
				mode = os.ModeSymlink
			default:
				continue
			}
			target := ""
			if mode&os.ModeSymlink != 0 {
				buf := make([]byte, 256)
				n, err := unix.Readlinkat(fd, name, buf)
				if errors.Is(err, unix.ENOENT) {
					continue
				}
				if err != nil {
					return err
				}
				target = string(buf[:n])
			}
			if !snapshot.CheckpointCandidate(name, c.SandboxID, mode, target) {
				continue
			}
			if name == c.SandboxID+".snapshot" || name == c.SandboxID+".sandbox" {
				// A lower/member retaining a carrier does not make its old
				// semantic root alias current. E-only has no Snapshot alias.
				if k.aliases[name] == target {
					continue
				}
			} else if k.files[name] {
				continue
			}
			// No AT_REMOVEDIR: a raced directory replacement is never removed.
			if err := unix.Unlinkat(fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
		}
		if len(entries) == 0 {
			return nil
		}
	}
}

func (k *checkpointKeep) addLocation(ref manifest.Ref) error {
	if ref.Location == "" {
		return nil
	}
	if _, ok := k.p.locations[ref.Location]; ok {
		return nil
	}
	if k.resolveLocation == nil {
		return fmt.Errorf("checkpoint location %q is not configured", ref.Location)
	}
	path, err := k.resolveLocation(ref.Location)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		return errors.New("checkpoint location must resolve to an absolute directory")
	}
	k.p.locations[ref.Location] = path
	return nil
}

// open preserves the existing current -> refs -> remote selection. Resolve a
// candidate's location only when the selector visits that candidate, so an
// unused or unavailable source cannot prevent a complete local keep plan.
func (k *checkpointKeep) open(ctx context.Context, raw string, scope publishScope) (fetch.Stream, publishScope, error) {
	stream, child, err := k.p.open(ctx, raw, scope)
	if err != nil {
		return nil, child, err
	}
	opened, ok := stream.(*OpenedFile)
	if !ok || opened.resolver == nil {
		return stream, child, nil
	}
	current, err := opened.ManifestFetcher().SelectRoot(opened.rootKey)
	if err != nil {
		return nil, child, errors.Join(err, stream.Close())
	}
	resolver := manifestbundle.SourceResolverFunc(func(ctx context.Context, raw string) (manifestbundle.ManifestSource, error) {
		if err := ctx.Err(); err != nil {
			return manifestbundle.ManifestSource{}, err
		}
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			return manifestbundle.ManifestSource{}, err
		}
		if err := k.addLocation(ref); err != nil {
			if ctx.Err() != nil {
				return manifestbundle.ManifestSource{}, ctx.Err()
			}
			return manifestbundle.ManifestSource{}, fmt.Errorf("%w: %s: %w", manifestbundle.ErrSourceUnavailable, raw, err)
		}
		return opened.resolver.ResolveBundle(ctx, raw)
	})
	// Reuse the opened carrier's authenticated fetcher and reader ownership.
	// Its resolver retains the lazy cache and closes every consulted reader.
	child.fetcher = manifestbundle.NewManifestFetcherWithResolver(opened.BundleReader(), current.Fetcher, resolver, k.p.storage.Fetcher())
	return stream, child, nil
}
