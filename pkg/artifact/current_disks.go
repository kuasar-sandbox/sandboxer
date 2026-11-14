package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"golang.org/x/sys/unix"
)

const currentDisksName = "current-disks.json"

// CurrentRun identifies a parent object instance and its exact disk-using run.
// Supplying it opts into parent-owned data retention; nil preserves temporary
// standalone runtime semantics. The caller must never reuse an instance/run pair.
type CurrentRun struct {
	SandboxID  string `json:"sandbox_id"`
	InstanceID string `json:"instance_id"`
	RunID      string `json:"run_id"`
}

func (r CurrentRun) Validate() error {
	for _, id := range []string{r.SandboxID, r.InstanceID, r.RunID} {
		if id == "" || len(id) > 128 || strings.ContainsAny(id, "/\\\x00\r\n") || id == "." || id == ".." {
			return errors.New("current disks require a valid sandbox, instance and run identity")
		}
	}
	return nil
}

type CurrentDisk struct {
	Name     string `json:"name"` // empty only for root
	DiffID   string `json:"diff_id"`
	BaseID   string `json:"base_id"`
	Capacity int64  `json:"capacity"`
}

type currentDependency struct {
	Ref      string `json:"ref"`
	Path     string `json:"path,omitempty"`
	Identity string `json:"identity,omitempty"`
}

// CurrentDiskSet is local parent-owned metadata, not a portable artifact or a
// checkpoint. Config is canonical portable C0 in checkpoint (zero-self) layout.
// Publication occurs before exposing any of the described writers to a guest.
type CurrentDiskSet struct {
	Version      int                 `json:"version"`
	Run          CurrentRun          `json:"run"`
	DirectoryID  string              `json:"directory_id"`
	Config       string              `json:"config"`
	Disks        []CurrentDisk       `json:"disks"`
	Dependencies []currentDependency `json:"dependencies"`
	Locations    config.RefLocations `json:"locations,omitempty"`
}

// RetainCurrentDisks records the entire selected disk set while the runtime owns
// the directory. Config refs must already name their retained host bindings;
// callers materializing a source must rebind it before publishing this plan.
// Local unlocated lowers are hard-linked into the parent directory, so releasing
// a materialization or RunDir cannot destroy a dependency. No disk is created,
// opened or awaited here: an interrupted initializer must fail offline Reset.
func (s *ProcessStorage) RetainCurrentDisks(ctx context.Context, owner *diffstore.DirectoryOwner, run CurrentRun, cfg *config.PortableSandboxConfig, disks []CurrentDisk, locations config.RefLocations) error {
	if s == nil || owner == nil || owner.Identity == "" {
		return errors.New("current disks require storage and directory ownership")
	}
	if err := owner.Validate(); err != nil {
		return err
	}
	raw, err := config.MarshalPortableSandboxConfig(cfg)
	if err != nil {
		return err
	}
	version := 1
	for _, disk := range cfg.Boot.Disks {
		if disk.Slot != nil {
			version = 2
			break
		}
	}
	set := CurrentDiskSet{Version: version, Run: run, DirectoryID: owner.Identity, Config: string(raw), Disks: disks, Locations: locations}
	if _, err := set.validate(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, dep := range portableDiskRefs(cfg) {
		if seen[dep.raw] {
			continue
		}
		seen[dep.raw] = true
		ref, err := manifest.ParseRef(dep.raw)
		if err != nil {
			return err
		}
		actual := ref
		entry := currentDependency{Ref: dep.raw}
		if actual.Scheme == manifest.RefSchemeFile {
			path, err := locations.ResolveFile(actual, owner.Directory)
			if err != nil {
				return err
			}
			opened, err := s.OpenFileWithLocations(ctx, path, actual, locations)
			if err != nil {
				return fmt.Errorf("current dependency %s: %w", dep.raw, err)
			}
			physical := opened.ResolvedPath()
			if err := opened.Close(); err != nil {
				return err
			}
			if physical == "" {
				physical = path
			}
			physical, err = filepath.EvalSymlinks(physical)
			if err != nil {
				return err
			}
			if ref.Scheme == manifest.RefSchemeFile && ref.Location == "" {
				target := filepath.Join(owner.Directory, ref.Path)
				if filepath.Dir(target) != owner.Directory {
					return errors.New("current dependency escapes directory")
				}
				if err := os.Link(physical, target); err != nil {
					if !os.IsExist(err) {
						return fmt.Errorf("retain current dependency: %w", err)
					}
					// Reusing a pathname is not evidence that it is the selected file.
					a, e1 := os.Stat(physical)
					b, e2 := os.Stat(target)
					if e1 != nil || e2 != nil || !os.SameFile(a, b) {
						return errors.New("current dependency identity collision")
					}
				}
				physical = target
			}
			entry.Path = physical
			entry.Identity, err = currentFileIdentity(physical)
			if err != nil {
				return err
			}
		}
		set.Dependencies = append(set.Dependencies, entry)
	}
	return writeCurrentJSON(filepath.Join(owner.Directory, currentDisksName), &set)
}

func (c *CurrentDiskSet) validate() (*config.PortableSandboxConfig, error) {
	if (c.Version != 1 && c.Version != 2) || c.DirectoryID == "" {
		return nil, errors.New("unsupported current disk description")
	}
	if err := c.Run.Validate(); err != nil {
		return nil, err
	}
	p, err := config.ParseCheckpointSandboxConfig([]byte(c.Config))
	if err != nil {
		return nil, err
	}
	if len(c.Disks) != len(p.Boot.Disks)+1 {
		return nil, errors.New("current disk topology mismatch")
	}
	ids := map[string]bool{}
	for i, d := range c.Disks {
		root, name := p.Boot.Root, ""
		if i > 0 {
			disk := p.Boot.Disks[i-1]
			root, name = disk.PortableRootConfig, disk.Name
			if disk.Slot != nil && c.Version < 2 {
				return nil, errors.New("current slot requires version 2")
			}
			if disk.Unbound() {
				size, err := disk.Slot.Capacity()
				if err != nil || d.DiffID != "" || d.BaseID != "" || name != d.Name || d.Capacity != size {
					return nil, errors.New("invalid unbound current disk")
				}
				continue
			}
		}
		if d.DiffID == "" || strings.ContainsAny(d.DiffID, "/\\.") || ids[d.DiffID] || d.Name != name || d.BaseID != DiffCheckpointBaseID(root) || d.Capacity <= 0 {
			return nil, errors.New("current disk identity, capacity or base graph mismatch")
		}
		ids[d.DiffID] = true
	}
	return p, nil
}

func currentFileIdentity(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", errors.New("current dependency is not a regular file")
	}
	return fmt.Sprintf("%x:%x:%x", st.Dev, st.Ino, st.Size), nil
}

func readCurrentJSON(path string, v any) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 2*config.MaxPortableConfigBytes+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, err
	}
	if len(b) > 2*config.MaxPortableConfigBytes {
		return nil, errors.New("current disk metadata exceeds size limit")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("current disk metadata trailing JSON")
	}
	canonical, err := json.Marshal(v)
	if err != nil || !bytes.Equal(canonical, b) {
		return nil, errors.New("current disk metadata is not canonical")
	}
	return b, nil
}

// ErrCurrentMetadataPublished means the replacement has become visible, but
// persisting the directory entry failed. Its referenced writers must be kept.
var ErrCurrentMetadataPublished = errors.New("current metadata published with pending directory sync")

func writeCurrentJSON(path string, v any) (retErr error) {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".current-disks-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		return errors.Join(ErrCurrentMetadataPublished, err)
	}
	return nil
}

type resetDiskOperation struct {
	Version         int        `json:"version"`
	Run             CurrentRun `json:"run"`
	ID              string     `json:"id"`
	DescriptionHash string     `json:"description_hash"`
	Before          []string   `json:"before"`
	SandboxRef      string     `json:"sandbox_ref,omitempty"`
}

// ResetCurrentDisks prepares a standard E from the exact retained run. The caller
// supplies a durable unique operation ID and fences lifecycle/source users until
// committing the returned source. Repeating the same ID recovers partial work;
// it never substitutes a template, historical checkpoint or previous run.
func (s *ProcessStorage) ResetCurrentDisks(ctx context.Context, directory string, run CurrentRun, operationID string) (string, error) {
	return s.resetCurrentDisks(ctx, directory, run, operationID, nil)
}

func (s *ProcessStorage) resetCurrentDisks(ctx context.Context, directory string, run CurrentRun, operationID string, boundary func(string) error) (_ string, retErr error) {
	if s == nil {
		return "", errors.New("reset requires process storage")
	}
	if err := run.Validate(); err != nil {
		return "", err
	}
	if err := sandboxfile.ValidateCheckpointID(operationID); err != nil {
		return "", err
	}
	// Read before LockDirectory, which can create directories. A missing source
	// must not leave behind a replacement disk directory.
	var set CurrentDiskSet
	raw, err := readCurrentJSON(filepath.Join(directory, currentDisksName), &set)
	if err != nil {
		return "", fmt.Errorf("current disks unavailable: %w", err)
	}
	owner, err := diffstore.LockDirectory(directory)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, owner.Close()) }()
	var locked CurrentDiskSet
	lockedRaw, err := readCurrentJSON(filepath.Join(owner.Directory, currentDisksName), &locked)
	if err != nil || !bytes.Equal(raw, lockedRaw) {
		return "", errors.New("current disk description changed during admission")
	}
	if set.Run != run || set.DirectoryID != owner.Identity {
		return "", errors.New("current disks belong to a different object or run")
	}
	p, err := set.validate()
	if err != nil {
		return "", err
	}
	deps := portableDiskRefs(p)
	seen := map[string]bool{}
	for _, dep := range set.Dependencies {
		if seen[dep.Ref] {
			return "", errors.New("duplicate current dependency")
		}
		seen[dep.Ref] = true
		if dep.Path != "" {
			identity, err := currentFileIdentity(dep.Path)
			if err != nil || identity != dep.Identity {
				return "", fmt.Errorf("current dependency %s missing or changed", dep.Ref)
			}
		}
		ref, err := manifest.ParseRef(dep.Ref)
		if err != nil {
			return "", err
		}
		path := dep.Path
		if path == "" && ref.Scheme == manifest.RefSchemeFile {
			path, err = set.Locations.ResolveFile(ref, owner.Directory)
			if err != nil {
				return "", err
			}
		}
		if ref.Scheme == manifest.RefSchemeFile {
			opened, err := s.OpenFileWithLocations(ctx, path, ref, set.Locations)
			if err != nil {
				return "", err
			}
			if err := opened.Close(); err != nil {
				return "", err
			}
		} else {
			stream, _, err := newPublisher(s, set.Locations, nil, nil).open(ctx, dep.Ref, publishScope{relativeDir: owner.Directory, fetcher: s.Fetcher()})
			if err != nil {
				return "", err
			}
			if err := stream.Close(); err != nil {
				return "", err
			}
		}
	}
	for _, dep := range deps {
		if !seen[dep.raw] {
			return "", errors.New("current disk dependency description incomplete")
		}
	}
	var key *[32]byte
	if s.localCodec != nil {
		k, err := s.keyFn()
		if err != nil {
			return "", err
		}
		defer clear(k[:])
		key = &k
	}
	stores := make([]*diffstore.Store, len(set.Disks))
	defer func() {
		for i := len(stores) - 1; i >= 0; i-- {
			if stores[i] != nil {
				retErr = errors.Join(retErr, stores[i].Close())
			}
		}
	}()
	before := make([]string, len(set.Disks))
	for i, d := range set.Disks {
		if i > 0 && d.DiffID == "" {
			continue
		}
		path := filepath.Join(owner.Directory, "diffs", d.DiffID+".diff")
		st, err := diffstore.Open(path, diffstore.Options{DiffID: d.DiffID, BaseID: d.BaseID, LogicalSize: d.Capacity, Key: key, RequireEncryption: s.localRequired})
		if err != nil {
			return "", fmt.Errorf("current disk %q unavailable: %w", d.Name, err)
		}
		stores[i] = st
		before[i] = st.Info().CurrentCheckpoint
	}
	digest := sha256.Sum256(raw)
	op := resetDiskOperation{Version: 1, Run: run, ID: operationID, DescriptionHash: hex.EncodeToString(digest[:]), Before: before}
	idHash := sha256.Sum256([]byte(operationID))
	opPath := filepath.Join(owner.Directory, "reset-"+hex.EncodeToString(idHash[:])+".json")
	var existing resetDiskOperation
	if _, err := readCurrentJSON(opPath, &existing); err == nil {
		if existing.Version != 1 || existing.Run != run || existing.ID != operationID || existing.DescriptionHash != op.DescriptionHash || len(existing.Before) != len(stores) {
			return "", errors.New("reset operation ownership mismatch")
		}
		op = existing
	} else if os.IsNotExist(err) {
		for _, st := range stores {
			if st == nil {
				continue
			}
			ids, err := st.ListCheckpoints(ctx)
			if err != nil {
				return "", err
			}
			for _, id := range ids {
				if id == operationID {
					return "", errors.New("reset version exists without operation ownership")
				}
			}
		}
		if err := writeCurrentJSON(opPath, &op); err != nil {
			return "", err
		}
	} else {
		return "", err
	}
	if boundary != nil {
		if err := boundary("accepted"); err != nil {
			return "", err
		}
	}
	cp := &sandboxfile.Checkpoint{Version: set.Version, CheckpointID: operationID, Root: sandboxfile.CheckpointDisk{DiffID: set.Disks[0].DiffID}}
	for i, st := range stores {
		if st == nil {
			cp.Disks = append(cp.Disks, sandboxfile.CheckpointNamedDisk{Name: set.Disks[i].Name})
			continue
		}
		ids, err := st.ListCheckpoints(ctx)
		if err != nil {
			return "", err
		}
		exists := false
		for _, id := range ids {
			exists = exists || id == operationID
		}
		current := st.Info().CurrentCheckpoint
		if exists {
			if current != operationID {
				return "", errors.New("reset version no longer describes the active disk")
			}
		} else {
			if current != op.Before[i] {
				return "", errors.New("current disk changed since reset admission")
			}
			if err := st.Checkpoint(ctx, operationID); err != nil {
				return "", err
			}
		}
		if i > 0 {
			cp.Disks = append(cp.Disks, sandboxfile.CheckpointNamedDisk{Name: set.Disks[i].Name, DiffID: set.Disks[i].DiffID})
		}
		if boundary != nil {
			if err := boundary(fmt.Sprintf("disk:%d", i)); err != nil {
				return "", err
			}
		}
	}
	source, err := sandboxfile.BuildCheckpointSource(ctx, p, cp)
	if err != nil {
		return "", err
	}
	sink := snapshot.NewFileSink(owner.Directory, run.SandboxID, s.localCodec, s.localRequired, nil)
	ref, path, err := sink.AbsorbSandbox(ctx, source)
	if err != nil {
		return "", err
	}
	if op.SandboxRef != "" && op.SandboxRef != ref {
		return "", errors.New("reset output identity mismatch")
	}
	// Verify the existing/new content using the standard reader, not existence.
	parsed, err := manifest.ParseRef(ref)
	if err != nil {
		return "", err
	}
	opened, err := s.OpenFile(ctx, path, parsed)
	if err != nil {
		return "", err
	}
	e, err := sandboxfile.Open(ctx, opened)
	if err != nil {
		return "", err
	}
	valid := reflect.DeepEqual(e.Checkpoint, cp)
	err = e.Close()
	if err != nil {
		return "", err
	}
	if !valid {
		return "", errors.New("reset output disk set mismatch")
	}
	if err := sink.CommitSandbox(ctx, ref, path); err != nil {
		return "", err
	}
	if err := sink.Close(); err != nil {
		return "", err
	}
	if boundary != nil {
		if err := boundary("sandbox"); err != nil {
			return "", err
		}
	}
	op.SandboxRef = ref
	if err := writeCurrentJSON(opPath, &op); err != nil {
		return "", err
	}
	return ref, nil
}
