package artifact

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// A flat memory lower is ordinarily an opaque payload. Only an exact local
// capture record's digest and inode prove that it instead owns an internal
// memory graph. Resolving first also covers named locations without treating
// an unrelated external basename as a local dependency.
func (k *checkpointKeep) retainMemoryLower(ctx context.Context, raw string, scope publishScope) error {
	resolved, err := k.retain(ctx, raw, scope)
	if err != nil {
		return err
	}
	ref, err := manifest.ParseRef(resolved)
	if err != nil {
		return err
	}
	if ref.Scheme != manifest.RefSchemeFile || k.memoryVisited[resolved] || !knownLocalMemoryCarrier(ref.Path, ref) {
		return nil
	}
	return k.retainMemory(ctx, raw, scope)
}

// An explicitly supplied memory root includes its external suffix. The latest
// current-memory locator is not a durable root. For a selected root,
// historical lowers' external configurations are not recursive memory edges.
func (k *checkpointKeep) retainMemoryRoot(ctx context.Context, raw string, scope publishScope) (retErr error) {
	stream, child, err := k.open(ctx, raw, scope)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, stream.Close()) }()
	rawConfig, err := snapshotfile.ReadConfig(ctx, stream)
	if err != nil {
		return err
	}
	cfg, err := canonicalMemoryConfig(rawConfig)
	if err != nil {
		return err
	}
	for _, lower := range cfg.FromRefs {
		if err := k.retainMemoryLower(ctx, lower, child); err != nil {
			return err
		}
	}
	return k.retainMemory(ctx, raw, scope)
}

// retainMemory follows only the local descriptor's internal edges. Historical
// S execution and disk configs remain irrelevant to a memory lower. It reads
// neither RAM nor CPU/state bodies and does not contact a holder.
func (k *checkpointKeep) retainMemory(ctx context.Context, raw string, scope publishScope) (retErr error) {
	if k.memoryVisited == nil {
		k.memoryVisited = make(map[string]bool)
	}
	if k.memoryActive == nil {
		k.memoryActive = make(map[string]bool)
	}
	defer func() {
		if retErr != nil {
			clear(k.memoryActive)
		}
	}()
	type visit struct {
		raw    string
		scope  publishScope
		finish string
	}
	pending := []visit{{raw: raw, scope: scope}}
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if item.finish != "" {
			delete(k.memoryActive, item.finish)
			k.memoryVisited[item.finish] = true
			continue
		}
		resolved, err := k.retain(ctx, item.raw, item.scope)
		if err != nil {
			return err
		}
		if k.memoryActive[resolved] {
			return errors.New("checkpoint memory keep graph contains a cycle")
		}
		if k.memoryVisited[resolved] {
			continue
		}
		stream, child, err := k.open(ctx, item.raw, item.scope)
		if err != nil {
			return err
		}
		descriptor, readErr := snapshotfile.ReadLocalDescriptor(ctx, stream)
		if err := errors.Join(readErr, stream.Close()); err != nil {
			return err
		}
		if descriptor == nil {
			k.memoryVisited[resolved] = true
			continue
		}
		if child.relativeDir == "" {
			return errors.New("checkpoint memory keep graph has no local directory")
		}
		if k.sourceDirectories == nil {
			k.sourceDirectories = map[string]bool{}
		}
		physical, err := filepath.EvalSymlinks(child.relativeDir)
		if err != nil {
			return err
		}
		k.sourceDirectories[physical] = true
		record, err := memorycheckpoint.ReadRecord(child.relativeDir, descriptor.CheckpointID, descriptor.MemoryID)
		if err != nil {
			return err
		}
		if record.Deleted {
			return memorycheckpoint.ErrDeleted
		}
		if record.MemoryID != descriptor.MemoryID || !record.Committed || record.Discarded || record.Aborted {
			return errors.New("checkpoint memory capture identity mismatch")
		}
		if sameCheckpointDirectory(child.relativeDir, k.directory) {
			if k.memoryVersions == nil {
				k.memoryVersions = make(map[memorycheckpoint.Reference]bool)
			}
			k.memoryVersions[memorycheckpoint.Reference{CheckpointID: record.CheckpointID, MemoryID: record.MemoryID}] = true
		}
		if _, err := k.retain(ctx, descriptor.MapRef, child); err != nil {
			return fmt.Errorf("checkpoint memory map: %w", err)
		}
		if record.PayloadRef != "" {
			if _, err := k.retain(ctx, record.PayloadRef, child); err != nil {
				return fmt.Errorf("checkpoint memory payload: %w", err)
			}
		}
		k.memoryActive[resolved] = true
		pending = append(pending, visit{finish: resolved})
		for i := len(descriptor.Parents) - 1; i >= 0; i-- {
			pending = append(pending, visit{raw: descriptor.Parents[i], scope: child})
		}
	}
	return nil
}
