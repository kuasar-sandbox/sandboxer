// Package snapshotfile implements the strict memory Snapshot logical layout:
//
//	[sparse memory][ZIP(config.json, state.json, snapshot.cfg)]
//
// Physical tarstream, Manifest and Manifest Bundle carriers are resolved by
// callers. This package derives the memory boundary solely from ZIP geometry.
package snapshotfile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tailzip"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

const (
	ConfigJSONName  = "config.json"
	StateJSONName   = "state.json"
	SnapshotCfgName = "snapshot.cfg"

	MaxConfigJSONBytes  = 16 << 20
	MaxStateJSONBytes   = 16 << 20
	MaxSnapshotCfgBytes = 1 << 20

	localHeaderSignature   = 0x04034b50
	centralHeaderSignature = 0x02014b50
	eocdSignature          = 0x06054b50
	localHeaderSize        = 30
	centralHeaderSize      = 46
	eocdSize               = 22
	zipEpochDate           = 33
)

// Root owns one opened Snapshot. FullStream and Memory share a single close
// owner. Memory preserves the carrier's authoritative Hole/Zero/Data runs and
// ends exactly at ArchiveBase, so UFFD can never observe the ZIP tail.
type Root struct {
	FullStream     fetch.Stream
	Memory         fetch.Stream
	ConfigJSON     []byte
	StateJSON      []byte
	SnapshotConfig []byte
	ArchiveBase    uint64

	owner *streamOwner
}

func (r *Root) Close() error {
	if r == nil || r.owner == nil {
		return nil
	}
	return r.owner.Close()
}

// BuildZIP returns the sole canonical Snapshot ZIP encoding.
func BuildZIP(configJSON, stateJSON, snapshotConfig []byte) ([]byte, error) {
	entries := []tailzip.Entry{{Name: ConfigJSONName, Body: configJSON}, {Name: StateJSONName, Body: stateJSON}, {Name: SnapshotCfgName, Body: snapshotConfig}}
	for _, entry := range entries {
		if len(entry.Body) == 0 || len(entry.Body) > entryLimit(entry.Name) {
			return nil, fmt.Errorf("snapshot ZIP entry %s size is outside limits", entry.Name)
		}
	}
	return tailzip.EncodeCanonical(entries)
}

// BuildSource appends the canonical Snapshot ZIP while retaining memory's
// authoritative sparse map.
func BuildSource(memory sparse.Source, configJSON, stateJSON, snapshotConfig []byte) (sparse.Source, error) {
	if memory == nil || memory.Size() == 0 {
		return nil, errors.New("snapshot build: non-empty memory source is required")
	}
	tail, err := BuildZIP(configJSON, stateJSON, snapshotConfig)
	if err != nil {
		return nil, err
	}
	if memory.Size() > math.MaxUint64-uint64(len(tail)) {
		return nil, errors.New("snapshot build logical size overflow")
	}
	return tailzip.Append(memory, tail, tailzip.Options{})
}

// Open parses a V1 Snapshot fail-closed. It rejects old snapshot.cfg shapes
// later when the caller applies the strict schema, but every ZIP/JSON/size
// error is already returned here before run-directory or VM side effects.
func Open(ctx context.Context, stream fetch.Stream) (*Root, error) {
	if stream == nil {
		return nil, errors.New("snapshot: nil logical stream")
	}
	fail := func(err error) (*Root, error) {
		return nil, errors.Join(err, stream.Close())
	}
	base, bodies, err := parseArchive(ctx, stream)
	if err != nil {
		return fail(err)
	}
	if base == 0 {
		return fail(errors.New("snapshot memory payload is empty"))
	}
	if !json.Valid(bodies[ConfigJSONName]) {
		return fail(errors.New("snapshot config.json is not valid JSON"))
	}
	if !json.Valid(bodies[StateJSONName]) {
		return fail(errors.New("snapshot state.json is not valid JSON"))
	}
	owner := &streamOwner{stream: stream}
	return &Root{
		FullStream:     &sectionStream{owner: owner, size: stream.Size()},
		Memory:         &sectionStream{owner: owner, size: base},
		ConfigJSON:     append([]byte(nil), bodies[ConfigJSONName]...),
		StateJSON:      append([]byte(nil), bodies[StateJSONName]...),
		SnapshotConfig: append([]byte(nil), bodies[SnapshotCfgName]...),
		ArchiveBase:    base, owner: owner,
	}, nil
}

func parseArchive(ctx context.Context, stream fetch.Stream) (uint64, map[string][]byte, error) {
	names := []string{ConfigJSONName, StateJSONName, SnapshotCfgName}
	limits := map[string]int{ConfigJSONName: MaxConfigJSONBytes, StateJSONName: MaxStateJSONBytes, SnapshotCfgName: MaxSnapshotCfgBytes}
	base, bodies, err := tailzip.ReadCanonical(ctx, tailReadSource{stream}, names, limits)
	if err != nil {
		return 0, nil, fmt.Errorf("snapshot ZIP: %w", err)
	}
	for _, name := range names {
		if len(bodies[name]) == 0 {
			return 0, nil, fmt.Errorf("snapshot ZIP entry %s is empty", name)
		}
	}
	return base, bodies, nil
}

type tailReadSource struct{ fetch.Stream }

func (s tailReadSource) ReadAt(ctx context.Context, b []byte, off uint64) (int, error) {
	return readretry.ReadAt(ctx, len(b), func() (int, error) { return s.Stream.ReadAt(ctx, b, off) })
}

func entryLimit(name string) int {
	switch name {
	case ConfigJSONName:
		return MaxConfigJSONBytes
	case StateJSONName:
		return MaxStateJSONBytes
	default:
		return MaxSnapshotCfgBytes
	}
}

func parseEOCD(ctx context.Context, stream fetch.Stream) (base, centralStart, centralSize uint64, entries int, err error) {
	footer, err := tailzip.ReadFooter(ctx, tailReadSource{stream})
	return footer.Base, footer.CentralStart, footer.CentralSize, footer.Count, err
}

func readAt(ctx context.Context, stream fetch.Stream, offset, length uint64) ([]byte, error) {
	if length > uint64(int(^uint(0)>>1)) {
		return nil, errors.New("snapshot ZIP read exceeds addressable memory")
	}
	if offset > stream.Size() || length > stream.Size()-offset {
		return nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, int(length))
	if len(body) == 0 {
		return body, nil
	}
	n, err := readretry.ReadAt(ctx, len(body), func() (int, error) { return stream.ReadAt(ctx, body, offset) })
	if err != nil && (readretry.IsTerminal(err) || readerr.IsPermanent(err) || err != io.EOF) {
		return nil, err
	}
	if n != len(body) {
		return nil, io.ErrUnexpectedEOF
	}
	return body, nil
}

type streamOwner struct {
	stream fetch.Stream
	once   sync.Once
	err    error
}

func (o *streamOwner) Close() error {
	if o == nil {
		return nil
	}
	o.once.Do(func() {
		if o.stream != nil {
			o.err = o.stream.Close()
		}
	})
	return o.err
}

type sectionStream struct {
	owner *streamOwner
	base  uint64
	size  uint64
}

func (s *sectionStream) Size() uint64 { return s.size }
func (s *sectionStream) Close() error { return s.owner.Close() }

func (s *sectionStream) TarStreamDigest(name string) ([32]byte, bool) {
	if s.base != 0 || s.owner == nil || s.owner.stream == nil || s.size != s.owner.stream.Size() {
		return [32]byte{}, false
	}
	provider, ok := s.owner.stream.(tarstream.IdentityProvider)
	if !ok {
		return [32]byte{}, false
	}
	return provider.TarStreamDigest(name)
}

func (s *sectionStream) PayloadCommitment() (uint64, [32]byte, bool) {
	if s.base != 0 {
		return s.size, [32]byte{}, false
	}
	provider, ok := s.owner.stream.(tarstream.IdentityProvider)
	if !ok {
		return s.size, [32]byte{}, false
	}
	size, digest, ok := provider.PayloadCommitment()
	return size, digest, ok && size == s.size
}

func (s *sectionStream) RunAt(offset, limit uint64) (sparse.Run, error) {
	return (tailzip.Section{Source: s.owner.stream, Base: s.base, Length: s.size}).RunAt(offset, limit)
}
func (s *sectionStream) ReadAt(ctx context.Context, b []byte, offset uint64) (int, error) {
	return (tailzip.Section{Source: s.owner.stream, Base: s.base, Length: s.size}).ReadAt(ctx, b, offset)
}
