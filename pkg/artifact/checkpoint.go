package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"golang.org/x/sys/unix"
)

// ErrCheckpointMemoryReadersBusy reports that memory readers prevented cleanup
// from taking its exclusive directory pin. Backend versions may already be revoked; physical files remain pinned.
// A managed caller may defer this collection while completing runtime cleanup;
// this does not authorize deleting the directory or retiring its memory holder.
// Other errors, including errors closing cleanup's own files, do not match it.
var ErrCheckpointMemoryReadersBusy = errors.New("checkpoint has active memory readers")

// Checkpoint identifies a committed managed source. The caller must own
// Directory and hold its source mutation fence until CleanupCheckpoint returns.
// Successful collection also requires the runner, restore and publisher fences
// that permit deletion. Active memory readers instead return
// ErrCheckpointMemoryReadersBusy after logical revocation; that result does not permit
// removing Directory. Arbitrary FileSink outputs grant no cleanup rights.
type Checkpoint struct {
	Directory       string
	SandboxID       string // producer identity, not PathID or a migration stable identity
	SnapshotRef     string // empty for E-only
	SandboxRef      string
	RefLocations    config.RefLocations
	ResolveLocation func(string) (string, error) `json:"-"` // optional trusted mapping policy; returns host directory
	// Retain is the complete additional durable source set under the caller's
	// external registry fence. nil is unknown and rejected; use an explicit
	// empty slice when no additional roots exist. DirectoryOwner alone does
	// not fence an external registry. It protects other immutable captures.
	// Only the primary source determines the current semantic aliases.
	Retain []CheckpointSource
	// MemoryRefs are trusted runtime memory dependencies, independent of its
	// current disk E after materialization. They are not additional S/E pairs
	// and cannot be supplied through the maintenance wire protocol.
	MemoryRefs []string `json:"-"`
}

// CheckpointSource is an immutable E or S/E pair owned by the same sandbox.
type CheckpointSource = ctl.CheckpointSource

type checkpointKeep struct {
	resolveLocation    func(string) (string, error)
	p                  *Publisher
	directory          string
	files              map[string]bool
	aliases            map[string]string // semantic alias -> current role's physical carrier basename
	diffVersions       map[string]map[string]bool
	memoryVisited      map[string]bool
	memoryActive       map[string]bool
	memoryVersions     map[memorycheckpoint.Reference]bool
	sourceDirectories  map[string]bool
	sourceDiffVersions map[string]map[string]map[string]bool
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
	if c.SandboxRef == "" {
		return nil
	}
	// Portable roots cannot contain node-local checkpoint dependencies. Do not
	// fetch remote content merely to prove that it has no local keep edges.
	if strings.HasPrefix(c.SandboxRef, "manifest://") && (c.SnapshotRef == "" || strings.HasPrefix(c.SnapshotRef, "manifest://")) {
		return nil
	}
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
			if err := k.retainMemoryLower(ctx, raw, child); err != nil {
				return err
			}
		}
		if err := k.retainMemory(ctx, c.SnapshotRef, scope); err != nil {
			return err
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
	if root.Checkpoint != nil {
		sourceDir, err := filepath.Abs(child.relativeDir)
		if err != nil {
			return err
		}
		sourceDir, err = filepath.EvalSymlinks(sourceDir)
		if err != nil {
			return err
		}
		if k.sourceDirectories == nil {
			k.sourceDirectories = map[string]bool{}
		}
		k.sourceDirectories[sourceDir] = true
		if k.sourceDiffVersions == nil {
			k.sourceDiffVersions = map[string]map[string]map[string]bool{}
		}
		if k.sourceDiffVersions[sourceDir] == nil {
			k.sourceDiffVersions[sourceDir] = map[string]map[string]bool{}
		}
		addSource := func(id string) {
			if id == "" {
				return
			}
			if k.sourceDiffVersions[sourceDir][id] == nil {
				k.sourceDiffVersions[sourceDir][id] = map[string]bool{}
			}
			k.sourceDiffVersions[sourceDir][id][root.Checkpoint.CheckpointID] = true
		}
		addSource(root.Checkpoint.Root.DiffID)
		for _, disk := range root.Checkpoint.Disks {
			addSource(disk.DiffID)
		}
		if sourceDir == k.directory {
			if k.diffVersions == nil {
				k.diffVersions = make(map[string]map[string]bool)
			}
			add := func(id string) {
				if k.diffVersions[id] == nil {
					k.diffVersions[id] = make(map[string]bool)
				}
				k.diffVersions[id][root.Checkpoint.CheckpointID] = true
			}
			add(root.Checkpoint.Root.DiffID)
			for _, disk := range root.Checkpoint.Disks {
				if disk.DiffID != "" {
					add(disk.DiffID)
				}
			}
		}
	}
	// Kernel/runtime basename identities bind host-supplied boot inputs at
	// restore. They are not artifact edges into this checkpoint directory.
	for _, dep := range portableDiskRefs(root.Portable) {
		if _, err := k.retain(ctx, dep.raw, child); err != nil {
			return err
		}
	}
	return nil
}

func normalizeCheckpoint(c Checkpoint) (Checkpoint, string, error) {
	if (c.SandboxID == "" || c.SandboxID == "." || c.SandboxID == "..") || filepath.Base(c.SandboxID) != c.SandboxID || strings.ContainsAny(c.SandboxID, `/\\`) {
		return c, "", errors.New("checkpoint cleanup requires producer identity")
	}
	if c.SnapshotRef != "" && c.SandboxRef == "" {
		return c, "", errors.New("checkpoint Snapshot requires its Sandbox binding")
	}
	dir, err := filepath.Abs(c.Directory)
	if err != nil || c.Directory == "" {
		return c, "", errors.New("checkpoint directory is required")
	}
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return c, "", err
	}
	if physical != dir {
		return c, "", errors.New("checkpoint directory must be canonical without symlinks")
	}
	for _, raw := range []*string{&c.SnapshotRef, &c.SandboxRef} {
		if *raw == "" {
			continue
		}
		if !strings.HasPrefix(*raw, "file://") && !strings.HasPrefix(*raw, "manifest://") {
			path := *raw
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			*raw = (manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path}).String()
		}
		ref, err := manifest.ParseRef(*raw)
		if err != nil {
			return c, "", err
		}
		if ref.Scheme == manifest.RefSchemeFile && ref.Location == "" {
			path, err := c.RefLocations.ResolveFile(ref, dir)
			if err != nil {
				return c, "", err
			}
			path, err = filepath.Abs(path)
			if err != nil {
				return c, "", err
			}
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				return c, "", err
			}
			if filepath.Dir(path) == dir && filepath.Dir(target) != dir {
				return c, "", errors.New("committed root alias escapes checkpoint directory")
			}
		}
	}
	c.Directory = dir
	return c, dir, nil
}

func (s *ProcessStorage) checkpointKeepPlan(ctx context.Context, c Checkpoint) (*checkpointKeep, error) {
	if c.Retain == nil {
		return nil, errors.New("checkpoint requires an explicit complete retention set")
	}
	if c.RefLocations == nil {
		c.RefLocations = config.RefLocations{}
	}
	k := &checkpointKeep{p: newPublisher(s, c.RefLocations, nil, nil), directory: c.Directory, files: map[string]bool{}, aliases: map[string]string{}, resolveLocation: c.ResolveLocation}
	for _, source := range c.Retain {
		keep := c
		keep.SnapshotRef, keep.SandboxRef = source.SnapshotRef, source.SandboxRef
		keep, _, err := normalizeCheckpoint(keep)
		if err != nil {
			return nil, fmt.Errorf("retained checkpoint: %w", err)
		}
		if err = k.plan(ctx, keep); err != nil {
			return nil, fmt.Errorf("retained checkpoint keep plan: %w", err)
		}
	}
	scope := publishScope{relativeDir: c.Directory, fetcher: s.Fetcher()}
	for _, raw := range c.MemoryRefs {
		if err := k.retainMemoryLower(ctx, raw, scope); err != nil {
			return nil, fmt.Errorf("runtime memory keep plan: %w", err)
		}
	}
	// A retained name cannot retarget the current semantic aliases.
	k.aliases = map[string]string{}
	if err := k.plan(ctx, c); err != nil {
		return nil, fmt.Errorf("checkpoint keep plan: %w", err)
	}
	return k, nil
}

// CheckpointVersions reports all local DIFF versions needed by the source and
// retained captures, using the same complete metadata plan as cleanup. The
// caller fences source mutation; unlike cleanup this read may run alongside a
// live writer. An online collector must exclude these versions before deleting.
func (s *ProcessStorage) CheckpointVersions(ctx context.Context, c Checkpoint) ([]DiffCheckpointRef, error) {
	if s == nil {
		return nil, errors.New("checkpoint inspection requires storage")
	}
	c, dir, err := normalizeCheckpoint(c)
	if err != nil {
		return nil, err
	}
	c.Directory = dir
	k, err := s.checkpointKeepPlan(ctx, c)
	if err != nil {
		return nil, err
	}
	refs := make([]DiffCheckpointRef, 0)
	for diff, versions := range k.diffVersions {
		for id := range versions {
			refs = append(refs, DiffCheckpointRef{DiffID: diff, ID: id})
		}
	}
	for ref := range k.memoryVersions {
		refs = append(refs, CheckpointRef{ID: ref.CheckpointID, MemoryID: ref.MemoryID})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].DiffID != refs[j].DiffID {
			return refs[i].DiffID < refs[j].DiffID
		}
		return refs[i].ID < refs[j].ID
	})
	return refs, nil
}

// CleanupCheckpoint deletes only recognized obsolete direct entries after a
// complete metadata-only keep plan succeeds (including reader Close). It never
// opens a candidate payload. A pinned directory fd confines enumeration and
// unlink even if an ancestor or the pathname is concurrently replaced.
// ErrCheckpointMemoryReadersBusy permits deferring only this collection.
func (s *ProcessStorage) CleanupCheckpoint(ctx context.Context, c Checkpoint) (retErr error) {
	if s == nil {
		return errors.New("checkpoint cleanup requires storage")
	}
	c, dir, err := normalizeCheckpoint(c)
	if err != nil {
		return err
	}
	if c.Retain == nil {
		return errors.New("checkpoint requires an explicit complete retention set")
	}
	guard, err := LockCheckpointSources(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, guard.Close()) }()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), dir)
	defer func() { retErr = checkpointCleanupCloseResult(retErr, f.Close()) }()
	memoryLockFD, err := unix.Openat(fd, ".memory-readers.lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	memoryLock := os.NewFile(uintptr(memoryLockFD), "checkpoint memory readers")
	defer func() { retErr = checkpointCleanupCloseResult(retErr, memoryLock.Close()) }()
	pinned, err := f.Stat()
	if err != nil {
		return err
	}
	k, err := s.checkpointKeepPlan(ctx, c)
	if err != nil {
		return err
	}
	var currentOwner *diffstore.DirectoryOwner
	if _, err := os.Stat(filepath.Join(dir, "diffs")); err == nil {
		owner, err := diffstore.LockDirectory(dir)
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, owner.Close()) }()
		currentOwner = owner
	} else if !os.IsNotExist(err) {
		return err
	}
	records, err := memorycheckpoint.ListRecords(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, record := range records {
		if record.Producer.SandboxID != c.SandboxID {
			return errors.New("memory capture differs from cleanup owner")
		}
	}
	// Current disks are a separate parent-owned root, even when the last
	// ResumeSource describes an older checkpoint. Retain their local lowers
	// and active stores until a later complete run binding replaces this set.
	active := map[string]bool{}
	var currentSet CurrentDiskSet
	if _, err := readCurrentJSON(filepath.Join(dir, currentDisksName), &currentSet); err == nil {
		if _, err := currentSet.validate(); err != nil {
			return err
		}
		if currentSet.Run.SandboxID != c.SandboxID || currentOwner == nil || currentSet.DirectoryID != currentOwner.Identity {
			return errors.New("current disk parent differs from cleanup owner")
		}
		for _, dep := range currentSet.Dependencies {
			if filepath.Dir(dep.Path) == dir {
				k.files[filepath.Base(dep.Path)] = true
			}
		}
		for _, disk := range currentSet.Disks {
			if disk.DiffID != "" {
				active[disk.DiffID] = true
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// Path-based reads above must still describe the same pinned directory.
	current, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil || physical != dir || !os.SameFile(pinned, current) {
		return errors.New("checkpoint directory identity changed")
	}
	// First revoke names using the full durable graph. Holder and reader pins
	// are not durable roots, and cannot prevent logical cancellation.
	for _, record := range records {
		ref := memorycheckpoint.Reference{CheckpointID: record.CheckpointID, MemoryID: record.MemoryID}
		if record.Committed && !record.Aborted && !record.Deleted && !k.memoryVersions[ref] {
			if err := memorycheckpoint.DeleteCheckpoint(ctx, dir, ref); err != nil {
				return err
			}
		}
	}
	if err := cleanupDiffVersionSet(ctx, dir, k.diffVersions, active); err != nil {
		return err
	}
	if err := unix.Flock(memoryLockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("%w: %w", ErrCheckpointMemoryReadersBusy, err)
		}
		return fmt.Errorf("lock checkpoint memory readers: %w", err)
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
				if selected, ok := k.aliases[name]; ok && selected == target {
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
			// Capture records and their stable proof locks outlive graph GC.
			// A released or aborted record may still settle an unacknowledged
			// resource receipt after a newer capture replaced the SID locator.
			// Only terminal BaseDir destruction collects that recovery evidence.
			return nil
		}
	}
}

func checkpointCleanupCloseResult(result, closeErr error) error {
	if closeErr != nil && errors.Is(result, ErrCheckpointMemoryReadersBusy) {
		// A caller may defer only the lock conflict, not a failed close. Keep
		// the lock context in the message without preserving its busy marker.
		return fmt.Errorf("%v; checkpoint cleanup close: %w", result, closeErr)
	}
	return errors.Join(result, closeErr)
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
