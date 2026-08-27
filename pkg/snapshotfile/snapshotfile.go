// Package snapshotfile implements the strict memory Snapshot logical layout:
//
//	[sparse memory][ZIP(config.json, state.json, snapshot.cfg)]
//
// Physical tarstream, Manifest and Manifest Bundle carriers are resolved by
// callers. This package derives the memory boundary solely from ZIP geometry.
package snapshotfile

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
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
	entries := []struct {
		name string
		body []byte
	}{
		{ConfigJSONName, configJSON},
		{StateJSONName, stateJSON},
		{SnapshotCfgName, snapshotConfig},
	}
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, entry := range entries {
		limit := entryLimit(entry.name)
		if len(entry.body) == 0 || len(entry.body) > limit {
			_ = writer.Close()
			return nil, fmt.Errorf("snapshot ZIP entry %s size %d is outside (0,%d]", entry.name, len(entry.body), limit)
		}
		header := &zip.FileHeader{
			Name: entry.name, Method: zip.Store, Flags: 0,
			CreatorVersion: 20, ReaderVersion: 20,
			CRC32:            crc32.ChecksumIEEE(entry.body),
			CompressedSize64: uint64(len(entry.body)), UncompressedSize64: uint64(len(entry.body)),
			ModifiedTime: 0, ModifiedDate: zipEpochDate,
		}
		file, err := writer.CreateRaw(header)
		if err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("snapshot ZIP create %s: %w", entry.name, err)
		}
		if _, err := file.Write(entry.body); err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("snapshot ZIP write %s: %w", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("snapshot ZIP close: %w", err)
	}
	return output.Bytes(), nil
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
	return &appendedSource{payload: memory, tail: tail, size: memory.Size() + uint64(len(tail))}, nil
}

type archiveEntry struct {
	name             string
	flags            uint16
	method           uint16
	modifiedTime     uint16
	modifiedDate     uint16
	crc              uint32
	compressedSize   uint32
	uncompressedSize uint32
	localOffset      uint32
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
	base, centralStart, centralSize, count, err := parseEOCD(ctx, stream)
	if err != nil {
		return 0, nil, fmt.Errorf("snapshot ZIP EOCD: %w", err)
	}
	if count != 3 {
		return 0, nil, fmt.Errorf("snapshot ZIP has %d entries (want 3)", count)
	}
	wantNames := []string{ConfigJSONName, StateJSONName, SnapshotCfgName}
	entries := make([]archiveEntry, 0, count)
	position := centralStart
	for index := 0; index < count; index++ {
		fixed, err := readAt(ctx, stream, position, centralHeaderSize)
		if err != nil {
			return 0, nil, fmt.Errorf("snapshot ZIP central entry %d: %w", index, err)
		}
		if binary.LittleEndian.Uint32(fixed[0:4]) != centralHeaderSignature {
			return 0, nil, fmt.Errorf("snapshot ZIP central entry %d has invalid signature", index)
		}
		versionMade := binary.LittleEndian.Uint16(fixed[4:6])
		versionNeeded := binary.LittleEndian.Uint16(fixed[6:8])
		flags := binary.LittleEndian.Uint16(fixed[8:10])
		method := binary.LittleEndian.Uint16(fixed[10:12])
		modifiedTime := binary.LittleEndian.Uint16(fixed[12:14])
		modifiedDate := binary.LittleEndian.Uint16(fixed[14:16])
		crc := binary.LittleEndian.Uint32(fixed[16:20])
		compressedSize := binary.LittleEndian.Uint32(fixed[20:24])
		uncompressedSize := binary.LittleEndian.Uint32(fixed[24:28])
		nameLength := binary.LittleEndian.Uint16(fixed[28:30])
		extraLength := binary.LittleEndian.Uint16(fixed[30:32])
		commentLength := binary.LittleEndian.Uint16(fixed[32:34])
		diskStart := binary.LittleEndian.Uint16(fixed[34:36])
		internalAttrs := binary.LittleEndian.Uint16(fixed[36:38])
		externalAttrs := binary.LittleEndian.Uint32(fixed[38:42])
		localOffset := binary.LittleEndian.Uint32(fixed[42:46])
		if versionNeeded >= 45 || compressedSize == math.MaxUint32 || uncompressedSize == math.MaxUint32 || localOffset == math.MaxUint32 {
			return 0, nil, fmt.Errorf("snapshot ZIP entry %d uses ZIP64", index)
		}
		if versionMade != 20 || versionNeeded != 20 || flags != 0 || method != zip.Store {
			return 0, nil, fmt.Errorf("snapshot ZIP entry %d has non-canonical version, flags, or method", index)
		}
		if modifiedTime != 0 || modifiedDate != zipEpochDate || internalAttrs != 0 || externalAttrs != 0 {
			return 0, nil, fmt.Errorf("snapshot ZIP entry %d has non-canonical metadata", index)
		}
		if diskStart != 0 || extraLength != 0 || commentLength != 0 {
			return 0, nil, fmt.Errorf("snapshot ZIP entry %d has multi-disk, extra, or comment metadata", index)
		}
		if compressedSize != uncompressedSize {
			return 0, nil, fmt.Errorf("snapshot ZIP entry %d Store sizes differ", index)
		}
		nameBytes, err := readAt(ctx, stream, position+centralHeaderSize, uint64(nameLength))
		if err != nil {
			return 0, nil, fmt.Errorf("snapshot ZIP central entry %d name: %w", index, err)
		}
		name := string(nameBytes)
		if name != wantNames[index] {
			return 0, nil, fmt.Errorf("snapshot ZIP entry %d is %q (want %q)", index, name, wantNames[index])
		}
		limit := entryLimit(name)
		if uncompressedSize == 0 || uint64(uncompressedSize) > uint64(limit) {
			return 0, nil, fmt.Errorf("snapshot ZIP entry %s size %d is outside (0,%d]", name, uncompressedSize, limit)
		}
		entries = append(entries, archiveEntry{
			name: name, flags: flags, method: method, modifiedTime: modifiedTime,
			modifiedDate: modifiedDate, crc: crc, compressedSize: compressedSize,
			uncompressedSize: uncompressedSize, localOffset: localOffset,
		})
		position += centralHeaderSize + uint64(nameLength)
	}
	if position != centralStart+centralSize {
		return 0, nil, errors.New("snapshot ZIP central directory size mismatch")
	}

	bodies := make(map[string][]byte, count)
	wantLocalOffset := uint64(0)
	for i := range entries {
		entry := &entries[i]
		if uint64(entry.localOffset) != wantLocalOffset {
			return 0, nil, fmt.Errorf("snapshot ZIP local entry %s is not contiguous", entry.name)
		}
		localStart := base + uint64(entry.localOffset)
		fixed, err := readAt(ctx, stream, localStart, localHeaderSize)
		if err != nil {
			return 0, nil, fmt.Errorf("snapshot ZIP local entry %s: %w", entry.name, err)
		}
		if binary.LittleEndian.Uint32(fixed[0:4]) != localHeaderSignature {
			return 0, nil, fmt.Errorf("snapshot ZIP local entry %s has invalid signature", entry.name)
		}
		if binary.LittleEndian.Uint16(fixed[4:6]) != 20 ||
			binary.LittleEndian.Uint16(fixed[6:8]) != entry.flags ||
			binary.LittleEndian.Uint16(fixed[8:10]) != entry.method ||
			binary.LittleEndian.Uint16(fixed[10:12]) != entry.modifiedTime ||
			binary.LittleEndian.Uint16(fixed[12:14]) != entry.modifiedDate ||
			binary.LittleEndian.Uint32(fixed[14:18]) != entry.crc ||
			binary.LittleEndian.Uint32(fixed[18:22]) != entry.compressedSize ||
			binary.LittleEndian.Uint32(fixed[22:26]) != entry.uncompressedSize {
			return 0, nil, fmt.Errorf("snapshot ZIP local/central header mismatch for %s", entry.name)
		}
		nameLength := binary.LittleEndian.Uint16(fixed[26:28])
		extraLength := binary.LittleEndian.Uint16(fixed[28:30])
		if extraLength != 0 {
			return 0, nil, fmt.Errorf("snapshot ZIP local entry %s has extra data", entry.name)
		}
		name, err := readAt(ctx, stream, localStart+localHeaderSize, uint64(nameLength))
		if err != nil {
			return 0, nil, err
		}
		if string(name) != entry.name {
			return 0, nil, fmt.Errorf("snapshot ZIP local/central name mismatch for %s", entry.name)
		}
		dataStart := localStart + localHeaderSize + uint64(nameLength)
		recordEnd := dataStart + uint64(entry.compressedSize)
		if recordEnd > centralStart {
			return 0, nil, fmt.Errorf("snapshot ZIP entry %s overlaps central directory", entry.name)
		}
		body, err := readAt(ctx, stream, dataStart, uint64(entry.uncompressedSize))
		if err != nil {
			return 0, nil, fmt.Errorf("snapshot ZIP read %s: %w", entry.name, err)
		}
		if crc32.ChecksumIEEE(body) != entry.crc {
			return 0, nil, fmt.Errorf("snapshot ZIP CRC mismatch for %s", entry.name)
		}
		bodies[entry.name] = body
		wantLocalOffset = recordEnd - base
	}
	if base+wantLocalOffset != centralStart {
		return 0, nil, errors.New("snapshot ZIP has bytes between local entries and central directory")
	}
	return base, bodies, nil
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
	if stream.Size() < eocdSize {
		return 0, 0, 0, 0, io.ErrUnexpectedEOF
	}
	eocdOffset := stream.Size() - eocdSize
	eocd, err := readAt(ctx, stream, eocdOffset, eocdSize)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if binary.LittleEndian.Uint32(eocd[0:4]) != eocdSignature {
		return 0, 0, 0, 0, errors.New("EOCD is not at logical EOF")
	}
	disk := binary.LittleEndian.Uint16(eocd[4:6])
	centralDisk := binary.LittleEndian.Uint16(eocd[6:8])
	entriesDisk := binary.LittleEndian.Uint16(eocd[8:10])
	entriesTotal := binary.LittleEndian.Uint16(eocd[10:12])
	centralSize32 := binary.LittleEndian.Uint32(eocd[12:16])
	centralOffset32 := binary.LittleEndian.Uint32(eocd[16:20])
	commentLength := binary.LittleEndian.Uint16(eocd[20:22])
	if commentLength != 0 {
		return 0, 0, 0, 0, errors.New("EOCD comment is not empty")
	}
	if entriesTotal == math.MaxUint16 || centralSize32 == math.MaxUint32 || centralOffset32 == math.MaxUint32 {
		return 0, 0, 0, 0, errors.New("ZIP64 is unsupported")
	}
	if disk != 0 || centralDisk != 0 || entriesDisk != entriesTotal {
		return 0, 0, 0, 0, errors.New("multi-disk ZIP is unsupported")
	}
	centralSize = uint64(centralSize32)
	centralOffset := uint64(centralOffset32)
	if centralSize > eocdOffset || centralOffset > eocdOffset-centralSize {
		return 0, 0, 0, 0, errors.New("central directory geometry is invalid")
	}
	base = eocdOffset - centralSize - centralOffset
	centralStart = base + centralOffset
	if centralStart+centralSize != eocdOffset {
		return 0, 0, 0, 0, errors.New("central directory does not end at EOCD")
	}
	return base, centralStart, centralSize, int(entriesTotal), nil
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
	n, err := stream.ReadAt(ctx, body, offset)
	if err != nil && !errors.Is(err, io.EOF) {
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

func (s *sectionStream) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.size {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, errors.New("snapshot section RunAt limit is zero")
	}
	end := offset + limit
	if end < offset || end > s.size {
		end = s.size
	}
	run, err := s.owner.stream.RunAt(s.base+offset, end-offset)
	if err != nil {
		return nil, err
	}
	runEnd := run.End() - s.base
	if runEnd > end {
		runEnd = end
	}
	if run.Offset() != s.base+offset || runEnd <= offset {
		return nil, errors.New("snapshot section source returned invalid run")
	}
	return sectionRun{inner: run, offset: offset, end: runEnd}, nil
}

func (s *sectionStream) ReadAt(ctx context.Context, buffer []byte, offset uint64) (int, error) {
	if offset >= s.size {
		return 0, io.EOF
	}
	length := len(buffer)
	var eof error
	if uint64(length) > s.size-offset {
		length = int(s.size - offset)
		eof = io.EOF
	}
	n, err := s.owner.stream.ReadAt(ctx, buffer[:length], s.base+offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}
	if n != length {
		return n, io.ErrUnexpectedEOF
	}
	return n, eof
}

type sectionRun struct {
	inner  sparse.Run
	offset uint64
	end    uint64
}

func (r sectionRun) Offset() uint64       { return r.offset }
func (r sectionRun) End() uint64          { return r.end }
func (r sectionRun) Kind() sparse.RunKind { return r.inner.Kind() }

func (r sectionRun) ReadAt(ctx context.Context, buffer []byte, innerOffset uint64) (int, error) {
	if innerOffset > r.end-r.offset || uint64(len(buffer)) > r.end-r.offset-innerOffset {
		return 0, errors.New("snapshot section run read is out of bounds")
	}
	return r.inner.ReadAt(ctx, buffer, innerOffset)
}

type appendedSource struct {
	payload sparse.Source
	tail    []byte
	size    uint64
}

func (s *appendedSource) Size() uint64 { return s.size }

func (s *appendedSource) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.size {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, errors.New("snapshot appended source RunAt limit is zero")
	}
	end := offset + limit
	if end < offset || end > s.size {
		end = s.size
	}
	if offset < s.payload.Size() {
		payloadEnd := end
		if payloadEnd > s.payload.Size() {
			payloadEnd = s.payload.Size()
		}
		run, err := s.payload.RunAt(offset, payloadEnd-offset)
		if err != nil {
			return nil, err
		}
		return appendedRun{source: s, offset: offset, end: run.End(), kind: run.Kind(), payloadRun: run}, nil
	}
	return appendedRun{source: s, offset: offset, end: end, kind: sparse.Data}, nil
}

func (s *appendedSource) ReadAt(ctx context.Context, buffer []byte, offset uint64) (int, error) {
	if offset >= s.size {
		return 0, io.EOF
	}
	length := len(buffer)
	var eof error
	if uint64(length) > s.size-offset {
		length = int(s.size - offset)
		eof = io.EOF
	}
	done := 0
	if offset < s.payload.Size() {
		part := length
		if available := s.payload.Size() - offset; uint64(part) > available {
			part = int(available)
		}
		n, err := s.payload.ReadAt(ctx, buffer[:part], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			return n, err
		}
		if n != part {
			return n, io.ErrUnexpectedEOF
		}
		done += part
		offset += uint64(part)
	}
	if done < length {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		copy(buffer[done:length], s.tail[offset-s.payload.Size():])
	}
	return length, eof
}

type appendedRun struct {
	source     *appendedSource
	offset     uint64
	end        uint64
	kind       sparse.RunKind
	payloadRun sparse.Run
}

func (r appendedRun) Offset() uint64       { return r.offset }
func (r appendedRun) End() uint64          { return r.end }
func (r appendedRun) Kind() sparse.RunKind { return r.kind }

func (r appendedRun) ReadAt(ctx context.Context, buffer []byte, innerOffset uint64) (int, error) {
	if innerOffset > r.end-r.offset || uint64(len(buffer)) > r.end-r.offset-innerOffset {
		return 0, errors.New("snapshot appended run read is out of bounds")
	}
	if r.payloadRun != nil {
		return r.payloadRun.ReadAt(ctx, buffer, innerOffset)
	}
	return r.source.ReadAt(ctx, buffer, r.offset+innerOffset)
}
