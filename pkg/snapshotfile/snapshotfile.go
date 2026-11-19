// Package snapshotfile implements the strict memory Snapshot logical layout:
//
//	[sparse memory][ZIP(config.json, state.json, snapshot.cfg[, memory.dirty])]
//
// Physical tarstream, Manifest and Manifest Bundle carriers are resolved by
// callers. This package derives the memory boundary solely from ZIP geometry.
package snapshotfile

import (
	"context"
	"encoding/binary"
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

var ErrTailBudget = errors.New("snapshot ZIP tail budget exceeded")

// Root owns one opened Snapshot. FullStream and Memory share a single close
// owner. Memory preserves the carrier's authoritative Hole/Zero/Data runs and
// ends exactly at ArchiveBase, so UFFD can never observe the ZIP tail.
type Root struct {
	FullStream     fetch.Stream
	Memory         fetch.Stream
	ConfigJSON     []byte
	StateJSON      []byte
	SnapshotConfig []byte
	// MemoryDirty is the original encoded entry; nil means absent (all explicit dirty).
	MemoryDirty []byte
	ArchiveBase uint64
	// LocalCheckpoint is non-nil for a resolved node-local memory checkpoint.
	// FullStream is then the descriptor carrier, never a portable Snapshot.
	LocalCheckpoint *LocalCheckpointView

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
	return BuildZIPWithDirty(configJSON, stateJSON, snapshotConfig, nil)
}

// BuildZIPWithDirty preserves entry presence and bytes. A present entry must
// carry a valid encoding; BuildSourceWithDirty additionally checks coverage.
func BuildZIPWithDirty(configJSON, stateJSON, snapshotConfig, dirty []byte) ([]byte, error) {
	entries := []tailzip.Entry{{Name: ConfigJSONName, Body: configJSON}, {Name: StateJSONName, Body: stateJSON}, {Name: SnapshotCfgName, Body: snapshotConfig}}
	if dirty != nil {
		if len(dirty) < DirtyHeaderSize {
			return nil, errors.New("memory.dirty: missing header")
		}
		if err := validateDirtyBits(dirty, binary.LittleEndian.Uint64(dirty[8:])); err != nil {
			return nil, err
		}
		entries = append(entries, tailzip.Entry{Name: MemoryDirtyName, Body: dirty})
	}
	for _, entry := range entries {
		if len(entry.Body) == 0 || len(entry.Body) > entryLimit(entry.Name) {
			return nil, fmt.Errorf("snapshot ZIP entry %s size is outside limits", entry.Name)
		}
	}
	total := uint64(eocdSize)
	for _, entry := range entries {
		total += uint64(localHeaderSize + centralHeaderSize + 2*len(entry.Name) + len(entry.Body))
	}
	if total > MaxTailBytes {
		return nil, ErrTailBudget
	}
	return tailzip.EncodeCanonical(entries)
}

// BuildSource appends the canonical Snapshot ZIP while retaining memory's
// authoritative sparse map.
func BuildSource(memory sparse.Source, configJSON, stateJSON, snapshotConfig []byte) (sparse.Source, error) {
	return BuildSourceWithDirty(memory, configJSON, stateJSON, snapshotConfig, nil)
}

// BuildSourceWithDirty retains memory states and validates the optional bitmap.
func BuildSourceWithDirty(memory sparse.Source, configJSON, stateJSON, snapshotConfig, dirty []byte) (sparse.Source, error) {
	if memory == nil || memory.Size() == 0 {
		return nil, errors.New("snapshot build: non-empty memory source is required")
	}
	if err := ValidateDirty(memory, dirty); err != nil {
		return nil, err
	}
	tail, err := BuildZIPWithDirty(configJSON, stateJSON, snapshotConfig, dirty)
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
	if err := ValidateDirty(tailzip.Section{Source: stream, Length: base}, bodies[MemoryDirtyName]); err != nil {
		return fail(err)
	}
	owner := &streamOwner{stream: stream}
	return &Root{
		FullStream:     &sectionStream{owner: owner, size: stream.Size()},
		Memory:         &sectionStream{owner: owner, size: base},
		ConfigJSON:     append([]byte(nil), bodies[ConfigJSONName]...),
		StateJSON:      append([]byte(nil), bodies[StateJSONName]...),
		SnapshotConfig: append([]byte(nil), bodies[SnapshotCfgName]...),
		MemoryDirty:    append([]byte(nil), bodies[MemoryDirtyName]...),
		ArchiveBase:    base, owner: owner,
	}, nil
}

func parseArchive(ctx context.Context, stream fetch.Stream) (uint64, map[string][]byte, error) {
	footer, err := tailzip.ReadFooter(ctx, tailReaderAt{ctx: ctx, stream: stream}, stream.Size())
	if err != nil {
		return 0, nil, err
	}
	names, err := archiveNamesAt(footer.Count, footer.Base)
	if err != nil {
		return 0, nil, err
	}
	limits := map[string]int{ConfigJSONName: MaxConfigJSONBytes, StateJSONName: MaxStateJSONBytes, SnapshotCfgName: MaxSnapshotCfgBytes, MemoryDirtyName: MaxDirtyBytes, MemoryCheckpointName: MaxSnapshotCfgBytes}
	base, bodies, err := tailzip.ReadCanonical(ctx, tailReaderAt{ctx: ctx, stream: stream}, stream.Size(), names, limits)
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

// The owning artifact stream guarantees random access; retain retry context
// when adapting it to the common suffix reader's explicit io.ReaderAt contract.
type tailReaderAt struct {
	ctx    context.Context
	stream fetch.Stream
}

func (r tailReaderAt) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 {
		return 0, io.EOF
	}
	return readretry.ReadAt(r.ctx, len(b), func() (int, error) { return r.stream.ReadAt(r.ctx, b, uint64(off)) })
}

func entryLimit(name string) int {
	switch name {
	case ConfigJSONName:
		return MaxConfigJSONBytes
	case StateJSONName:
		return MaxStateJSONBytes
	case MemoryDirtyName:
		return MaxDirtyBytes
	default:
		return MaxSnapshotCfgBytes
	}
}

func parseEOCD(ctx context.Context, stream fetch.Stream) (base, centralStart, centralSize uint64, entries int, err error) {
	footer, err := tailzip.ReadFooter(ctx, tailReaderAt{ctx: ctx, stream: stream}, stream.Size())
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
	closes []io.Closer
	once   sync.Once
	err    error
}

func (o *streamOwner) Close() error {
	if o == nil {
		return nil
	}
	o.once.Do(func() {
		for i := len(o.closes) - 1; i >= 0; i-- {
			o.err = errors.Join(o.err, o.closes[i].Close())
		}
		if o.stream != nil {
			o.err = errors.Join(o.err, o.stream.Close())
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

func archiveNames(count int) ([]string, error) {
	names := []string{ConfigJSONName, StateJSONName, SnapshotCfgName}
	switch count {
	case 3:
		return names, nil
	case 4:
		return append(names, MemoryDirtyName), nil
	default:
		return nil, errors.New("snapshot ZIP: expected three or four entries")
	}
}

func archiveNamesAt(count int, base uint64) ([]string, error) {
	if base == 0 && count == 4 {
		return []string{ConfigJSONName, StateJSONName, SnapshotCfgName, MemoryCheckpointName}, nil
	}
	return archiveNames(count)
}
