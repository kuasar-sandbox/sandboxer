package snapshotfile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tailzip"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
)

// MemoryCheckpointName identifies the node-local, zero-prefix Snapshot profile.
// Its immutable identity includes no process, socket or mutable save state.
const MemoryCheckpointName = "memory.checkpoint.json"

// Metadata is a Snapshot's immutable tail. Reading it never contacts a memory
// holder or reads a guest-memory payload. The caller retains stream ownership.
type Metadata struct {
	ConfigJSON, StateJSON, SnapshotConfig, MemoryDirty []byte
	ArchiveBase                                        uint64
	LocalCheckpoint                                    *memorycheckpoint.Descriptor
}

// LocalCheckpointView retains the capture's provenance through composition.
// Top and History are borrowed sources sharing Root's ownership. Dirty is the
// candidate union, which can preserve legacy non-page-aligned byte ranges.
type LocalCheckpointView struct {
	Descriptor  memorycheckpoint.Descriptor
	Top         sparse.Source
	TopDirty    []byte
	History     sparse.Source
	HistoryFull sparse.Source
	Dirty       sparse.Source
}

func decodeLocalDescriptor(body []byte) (*memorycheckpoint.Descriptor, error) {
	var descriptor memorycheckpoint.Descriptor
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return nil, fmt.Errorf("memory checkpoint descriptor: %w", err)
	}
	if err := descriptor.Validate(); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(descriptor)
	if err != nil || !bytes.Equal(canonical, body) {
		return nil, errors.New("memory checkpoint descriptor is not canonical")
	}
	return &descriptor, nil
}

func ReadMetadata(ctx context.Context, stream fetch.Stream) (*Metadata, error) {
	if stream == nil {
		return nil, errors.New("snapshot metadata: nil stream")
	}
	base, bodies, err := parseArchive(ctx, stream)
	if err != nil {
		return nil, err
	}
	if !json.Valid(bodies[ConfigJSONName]) || !json.Valid(bodies[StateJSONName]) {
		return nil, errors.New("snapshot metadata: invalid execution JSON")
	}
	metadata := &Metadata{ConfigJSON: bodies[ConfigJSONName], StateJSON: bodies[StateJSONName],
		SnapshotConfig: bodies[SnapshotCfgName], MemoryDirty: bodies[MemoryDirtyName], ArchiveBase: base}
	if body := bodies[MemoryCheckpointName]; body != nil {
		if base != 0 {
			return nil, errors.New("memory checkpoint descriptor has a memory prefix")
		}
		metadata.LocalCheckpoint, err = decodeLocalDescriptor(body)
		return metadata, err
	}
	if base == 0 {
		return nil, errors.New("snapshot memory payload is empty")
	}
	if err := ValidateDirty(tailzip.Section{Source: stream, Length: base}, metadata.MemoryDirty); err != nil {
		return nil, err
	}
	return metadata, nil
}

// ReadLocalDescriptor checks the lightweight canonical headers before reading
// local metadata. Ordinary snapshots return nil and never read CPU/state bodies.
func ReadLocalDescriptor(ctx context.Context, stream fetch.Stream) (*memorycheckpoint.Descriptor, error) {
	if stream == nil {
		return nil, errors.New("snapshot descriptor: nil stream")
	}
	footer, err := tailzip.ReadFooter(ctx, tailReaderAt{ctx: ctx, stream: stream}, stream.Size())
	if err != nil {
		return nil, err
	}
	if footer.Base != 0 {
		return nil, nil
	}
	var descriptor *memorycheckpoint.Descriptor
	_, err = readConfig(ctx, stream, &descriptor)
	return descriptor, err
}

// BuildLocalSource emits only the fixed descriptor and execution state. Dense
// page maps have their own existing image carrier and do not enlarge this ZIP.
func BuildLocalSource(configJSON, stateJSON, snapshotConfig []byte, descriptor memorycheckpoint.Descriptor) (sparse.Source, error) {
	if err := descriptor.Validate(); err != nil {
		return nil, err
	}
	if !json.Valid(configJSON) || !json.Valid(stateJSON) {
		return nil, errors.New("memory checkpoint: invalid execution JSON")
	}
	body, err := json.Marshal(descriptor)
	if err != nil {
		return nil, err
	}
	entries := []tailzip.Entry{{Name: ConfigJSONName, Body: configJSON}, {Name: StateJSONName, Body: stateJSON},
		{Name: SnapshotCfgName, Body: snapshotConfig}, {Name: MemoryCheckpointName, Body: body}}
	for _, entry := range entries {
		if len(entry.Body) == 0 || len(entry.Body) > entryLimit(entry.Name) {
			return nil, fmt.Errorf("snapshot ZIP entry %s size is outside limits", entry.Name)
		}
	}
	tail, err := tailzip.EncodeCanonical(entries)
	if err != nil {
		return nil, err
	}
	return sparse.NewSource(bytes.NewReader(tail), uint64(len(tail)), nil)
}

// ResolveLocal transfers the descriptor and all physical-reader owners to one
// Root. Closing either exposed stream closes the whole group exactly once.
func ResolveLocal(stream fetch.Stream, metadata *Metadata, memory sparse.Source, view *LocalCheckpointView, owners ...io.Closer) (*Root, error) {
	owner := &streamOwner{stream: stream, closes: owners}
	if stream == nil || metadata == nil || metadata.LocalCheckpoint == nil || memory == nil || view == nil ||
		memory.Size() != metadata.LocalCheckpoint.Size || view.Descriptor.MemoryID != metadata.LocalCheckpoint.MemoryID {
		return nil, errors.Join(errors.New("memory checkpoint: invalid resolved view"), owner.Close())
	}
	return &Root{FullStream: &sectionStream{owner: owner, size: stream.Size()},
		Memory: &ownedSourceStream{Source: memory, owner: owner}, ConfigJSON: metadata.ConfigJSON,
		StateJSON: metadata.StateJSON, SnapshotConfig: metadata.SnapshotConfig,
		ArchiveBase: metadata.ArchiveBase, LocalCheckpoint: view, owner: owner}, nil
}

type ownedSourceStream struct {
	sparse.Source
	owner *streamOwner
}

func (s *ownedSourceStream) Close() error { return s.owner.Close() }
