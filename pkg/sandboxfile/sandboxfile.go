// Package sandboxfile implements the two strict Sandbox logical layouts:
//
//	[sparse root payload][ZIP(sandbox.runtime.cfg)]
//	[EROFS payload][ZIP(config.json, sandbox.runtime.cfg)]
//
// Physical tarstream, Manifest and Manifest Bundle carriers are opened before
// this package; all three yield the same fetch.Stream consumed here.
package sandboxfile

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
	"sort"
	"sync"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/image"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

const (
	ImageConfigName = "config.json"

	MaxImageConfigBytes = 1 << 20

	localHeaderSignature   = 0x04034b50
	centralHeaderSignature = 0x02014b50
	eocdSignature          = 0x06054b50

	localHeaderSize   = 30
	centralHeaderSize = 46
	eocdSize          = 22

	zipEpochTime = 0
	zipEpochDate = 33 // 1980-01-01 in MS-DOS date encoding.
)

// Root is an opened Sandbox logical stream. FullStream and Payload share one
// close owner; closing either view (or Root) releases the carrier exactly once.
// Payload is [0, ArchiveBase) and preserves Hole/Zero/Data RunAt semantics.
type Root struct {
	FullStream    fetch.Stream
	Payload       fetch.Stream
	ImageConfig   []byte
	RuntimeConfig []byte
	Portable      *config.PortableSandboxConfig
	ArchiveBase   uint64

	owner *streamOwner
}

func (r *Root) Close() error {
	if r == nil || r.owner == nil {
		return nil
	}
	return r.owner.Close()
}

// FlattenedImage is a validated EROFS + ZIP(config.json) input used to assemble
// a top-level Sandbox E. Its ImageConfig bytes are retained exactly.
type FlattenedImage struct {
	FullStream  fetch.Stream
	Payload     fetch.Stream
	ImageConfig []byte
	ArchiveBase uint64

	owner *streamOwner
}

func (i *FlattenedImage) Close() error {
	if i == nil || i.owner == nil {
		return nil
	}
	return i.owner.Close()
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
	centralStart     uint64
	localDataStart   uint64
	localRecordEnd   uint64
}

type parsedArchive struct {
	base    uint64
	entries []archiveEntry
	bodies  map[string][]byte
}

// Open parses a Sandbox fail-closed. Every format and config error is returned
// before the caller can hand Payload to a block backend.
func Open(ctx context.Context, stream fetch.Stream) (*Root, error) {
	if stream == nil {
		return nil, errors.New("sandbox: nil logical stream")
	}
	fail := func(err error) (*Root, error) {
		return nil, errors.Join(err, stream.Close())
	}
	archive, err := parseStrictArchive(ctx, stream)
	if err != nil {
		return fail(err)
	}
	runtimeBytes := archive.bodies[config.SandboxRuntimeConfigName]
	portable, err := config.ParsePortableSandboxConfig(runtimeBytes)
	if err != nil {
		return fail(fmt.Errorf("sandbox %s: %w", config.SandboxRuntimeConfigName, err))
	}
	canonical, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		return fail(err)
	}
	if !bytes.Equal(canonical, runtimeBytes) {
		return fail(fmt.Errorf("sandbox %s is not canonically encoded", config.SandboxRuntimeConfigName))
	}
	imageConfig, hasImage := archive.bodies[ImageConfigName]
	if hasImage {
		if !json.Valid(imageConfig) {
			return fail(errors.New("sandbox config.json is not valid JSON"))
		}
		if portable.Boot.Root.Base != "self" || portable.Boot.Root.Overlay == nil || portable.Boot.Root.Overlay.Base != "" {
			return fail(errors.New("sandbox EROFS layout requires boot.root.base=self and an empty overlay graph"))
		}
		erofsSize, err := image.ReadEROFSSize(streamReaderAt{ctx: ctx, stream: stream})
		if err != nil {
			return fail(fmt.Errorf("sandbox EROFS payload: %w", err))
		}
		if erofsSize != archive.base {
			return fail(fmt.Errorf("sandbox EROFS payload size %d does not equal ZIP archive base %d", erofsSize, archive.base))
		}
	} else {
		if portable.Boot.Root.Base == "self" && portable.Boot.Root.Overlay != nil && portable.Boot.Root.Overlay.Base == "" {
			return fail(errors.New("sandbox direct EROFS graph is missing config.json"))
		}
		if archive.base == 0 {
			return fail(errors.New("sandbox live root payload is empty"))
		}
	}

	owner := &streamOwner{stream: stream}
	full := &sectionStream{owner: owner, base: 0, size: stream.Size()}
	payload := &payloadStream{
		sectionStream: &sectionStream{owner: owner, base: 0, size: archive.base},
		imageConfig:   append([]byte(nil), imageConfig...),
	}
	return &Root{
		FullStream: full, Payload: payload, ImageConfig: append([]byte(nil), imageConfig...),
		RuntimeConfig: append([]byte(nil), runtimeBytes...), Portable: portable,
		ArchiveBase: archive.base, owner: owner,
	}, nil
}

// PayloadIfSandbox narrows a logical Sandbox stream to its root payload. A
// non-Sandbox stream is returned unchanged when required is false. required is
// used for roots whose role is already known (run --from and *.sandbox file
// refs); those inputs never fall back to an ordinary disk on a format error.
//
// Optional detection inspects only a ZIP central-directory entry name before
// invoking the strict parser. This keeps existing EROFS+ZIP(config.json)
// images and pure overlay layers unchanged even when their payload bytes
// mention the reserved name, while malformed ZIP metadata at logical EOF and
// a malformed stream that claims the Sandbox entry both fail closed.
func PayloadIfSandbox(ctx context.Context, stream fetch.Stream, required bool) (fetch.Stream, bool, error) {
	if stream == nil {
		return nil, false, errors.New("sandbox payload: nil logical stream")
	}
	claimed := required
	if !claimed {
		var err error
		claimed, err = claimsSandboxRuntimeConfig(ctx, stream)
		if err != nil {
			return nil, false, errors.Join(err, stream.Close())
		}
		if !claimed {
			return stream, false, nil
		}
	}
	root, err := Open(ctx, stream)
	if err != nil {
		return nil, false, err
	}
	return root.Payload, true, nil
}

func claimsSandboxRuntimeConfig(ctx context.Context, stream fetch.Stream) (bool, error) {
	const (
		maxProbeEntries     = 64
		maxProbeCentralSize = 128 << 10
	)
	if stream.Size() < eocdSize {
		return false, nil
	}
	eocd, err := readStreamAt(ctx, stream, stream.Size()-eocdSize, eocdSize)
	if err != nil {
		return false, fmt.Errorf("sandbox payload probe: %w", err)
	}
	if binary.LittleEndian.Uint32(eocd[0:4]) != eocdSignature {
		return false, nil
	}
	_, centralStart, centralSize, count, err := parseEOCD(ctx, stream)
	if err != nil {
		return false, fmt.Errorf("sandbox payload ZIP metadata: %w", err)
	}
	if count > maxProbeEntries || centralSize > maxProbeCentralSize {
		return false, fmt.Errorf("sandbox payload ZIP metadata exceeds detection limits (%d entries, %d bytes)", count, centralSize)
	}
	position := centralStart
	centralEnd := centralStart + centralSize
	claimed := false
	for index := 0; index < count; index++ {
		if position > centralEnd || centralHeaderSize > centralEnd-position {
			return false, fmt.Errorf("sandbox payload ZIP metadata: central entry %d is truncated", index)
		}
		fixed, err := readStreamAt(ctx, stream, position, centralHeaderSize)
		if err != nil {
			return false, fmt.Errorf("sandbox payload ZIP metadata: central entry %d: %w", index, err)
		}
		if binary.LittleEndian.Uint32(fixed[0:4]) != centralHeaderSignature {
			return false, fmt.Errorf("sandbox payload ZIP metadata: central entry %d has invalid signature", index)
		}
		nameLength := uint64(binary.LittleEndian.Uint16(fixed[28:30]))
		extraLength := uint64(binary.LittleEndian.Uint16(fixed[30:32]))
		commentLength := uint64(binary.LittleEndian.Uint16(fixed[32:34]))
		variableLength := nameLength + extraLength + commentLength
		if variableLength > centralEnd-position-centralHeaderSize {
			return false, fmt.Errorf("sandbox payload ZIP metadata: central entry %d fields are truncated", index)
		}
		name, err := readStreamAt(ctx, stream, position+centralHeaderSize, nameLength)
		if err != nil {
			return false, fmt.Errorf("sandbox payload ZIP metadata: central entry %d name: %w", index, err)
		}
		if string(name) == config.SandboxRuntimeConfigName {
			claimed = true
		}
		position += centralHeaderSize + variableLength
	}
	if position != centralEnd {
		return false, errors.New("sandbox payload ZIP metadata: central directory size mismatch")
	}
	if claimed {
		return true, nil
	}
	return false, nil
}

// OpenFlattenedEROFS validates the project's existing flattened image layout.
// Its legacy one-entry ZIP may contain the timestamp/data-descriptor metadata
// emitted by accelerator/pkg/image; the output rebuilt by BuildSource is the
// stricter Sandbox ZIP.
func OpenFlattenedEROFS(ctx context.Context, stream fetch.Stream) (*FlattenedImage, error) {
	if stream == nil {
		return nil, errors.New("flattened EROFS: nil logical stream")
	}
	fail := func(err error) (*FlattenedImage, error) {
		return nil, errors.Join(err, stream.Close())
	}
	base, imageConfig, err := parseFlattenedImageArchive(ctx, stream)
	if err != nil {
		return fail(err)
	}
	erofsSize, err := image.ReadEROFSSize(streamReaderAt{ctx: ctx, stream: stream})
	if err != nil {
		return fail(fmt.Errorf("flattened EROFS payload: %w", err))
	}
	if erofsSize != base {
		return fail(fmt.Errorf("flattened EROFS payload size %d does not equal ZIP archive base %d", erofsSize, base))
	}
	owner := &streamOwner{stream: stream}
	return &FlattenedImage{
		FullStream: &sectionStream{owner: owner, size: stream.Size()},
		Payload: &payloadStream{
			sectionStream: &sectionStream{owner: owner, size: base},
			imageConfig:   append([]byte(nil), imageConfig...),
		},
		ImageConfig: append([]byte(nil), imageConfig...), ArchiveBase: base, owner: owner,
	}, nil
}

// OpenEROFSArtifact accepts either a bare EROFS payload or the project's
// flattened EROFS + ZIP(config.json) container-image form. It is used only for
// an explicitly typed root-image dependency. Arbitrary disk layers never take
// this fallback. A malformed non-empty tail fails closed.
func OpenEROFSArtifact(ctx context.Context, stream fetch.Stream) (*FlattenedImage, error) {
	if stream == nil {
		return nil, errors.New("EROFS artifact: nil logical stream")
	}
	erofsSize, err := image.ReadEROFSSize(streamReaderAt{ctx: ctx, stream: stream})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("EROFS artifact payload: %w", err), stream.Close())
	}
	if erofsSize != stream.Size() {
		return OpenFlattenedEROFS(ctx, stream)
	}
	owner := &streamOwner{stream: stream}
	var imageConfig []byte
	if provider, ok := stream.(interface{ ImageConfigBytes() []byte }); ok {
		imageConfig = provider.ImageConfigBytes()
	}
	payload := &payloadStream{
		sectionStream: &sectionStream{owner: owner, size: erofsSize},
		imageConfig:   append([]byte(nil), imageConfig...),
	}
	full := fetch.Stream(payload)
	if imageConfig != nil {
		tail, err := buildFlattenedImageConfigZIP(imageConfig)
		if err != nil {
			return nil, errors.Join(err, owner.Close())
		}
		if erofsSize > math.MaxUint64-uint64(len(tail)) {
			return nil, errors.Join(errors.New("flattened image logical size overflow"), owner.Close())
		}
		source := &appendedSource{payload: payload, tail: tail, size: erofsSize + uint64(len(tail))}
		full = &appendedStream{appendedSource: source, owner: owner}
	}
	return &FlattenedImage{
		FullStream: full, Payload: payload, ImageConfig: append([]byte(nil), imageConfig...),
		ArchiveBase: erofsSize, owner: owner,
	}, nil
}

// buildFlattenedImageConfigZIP reproduces the accelerator flattened-image
// trailer while preserving the exact config.json bytes carried beside a
// Sandbox E payload.
func buildFlattenedImageConfigZIP(imageConfig []byte) ([]byte, error) {
	if len(imageConfig) == 0 || len(imageConfig) > MaxImageConfigBytes {
		return nil, fmt.Errorf("flattened image config.json size %d is outside (0,%d]", len(imageConfig), MaxImageConfigBytes)
	}
	if !json.Valid(imageConfig) {
		return nil, errors.New("flattened image config.json is not valid JSON")
	}
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	header := &zip.FileHeader{
		Name: ImageConfigName, Method: zip.Store,
		Modified: time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	entry, err := writer.CreateHeader(header)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("flattened image ZIP create config.json: %w", err)
	}
	if _, err := entry.Write(imageConfig); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("flattened image ZIP write config.json: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("flattened image ZIP close: %w", err)
	}
	return output.Bytes(), nil
}

// BuildSource appends the canonical strict ZIP using a background context.
// Request paths should use BuildSourceContext so cancellation also covers the
// EROFS superblock read performed before the output stream is constructed.
func BuildSource(payload sparse.Source, imageConfig, runtimeConfig []byte) (sparse.Source, error) {
	return BuildSourceContext(context.Background(), payload, imageConfig, runtimeConfig)
}

// BuildSourceContext appends the canonical strict ZIP to payload without
// flattening its sparse map. imageConfig nil selects the live layout; non-nil
// selects the EROFS layout and is preserved byte-for-byte.
func BuildSourceContext(ctx context.Context, payload sparse.Source, imageConfig, runtimeConfig []byte) (sparse.Source, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, errors.New("sandbox build: nil root payload")
	}
	if payload.Size() == 0 {
		return nil, errors.New("sandbox build: empty root payload")
	}
	portable, err := config.ParsePortableSandboxConfig(runtimeConfig)
	if err != nil {
		return nil, fmt.Errorf("sandbox build runtime config: %w", err)
	}
	canonical, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, runtimeConfig) {
		return nil, errors.New("sandbox build runtime config is not canonical")
	}
	entries := map[string][]byte{config.SandboxRuntimeConfigName: runtimeConfig}
	if imageConfig != nil {
		if len(imageConfig) > MaxImageConfigBytes {
			return nil, fmt.Errorf("sandbox build config.json exceeds %d bytes", MaxImageConfigBytes)
		}
		if !json.Valid(imageConfig) {
			return nil, errors.New("sandbox build config.json is not valid JSON")
		}
		if portable.Boot.Root.Base != "self" || portable.Boot.Root.Overlay == nil || portable.Boot.Root.Overlay.Base != "" {
			return nil, errors.New("sandbox build EROFS layout requires boot.root.base=self and an empty overlay graph")
		}
		preparedPayload, erofsSize, err := prepareEROFSBuildPayload(ctx, payload)
		if err != nil {
			return nil, fmt.Errorf("sandbox build EROFS payload: %w", err)
		}
		payload = preparedPayload
		if erofsSize != payload.Size() {
			return nil, fmt.Errorf("sandbox build EROFS logical size %d does not equal payload size %d", erofsSize, payload.Size())
		}
		entries[ImageConfigName] = imageConfig
	} else if portable.Boot.Root.Base == "self" && portable.Boot.Root.Overlay != nil && portable.Boot.Root.Overlay.Base == "" {
		return nil, errors.New("sandbox build direct EROFS graph requires config.json")
	}
	tail, err := BuildZIP(entries)
	if err != nil {
		return nil, err
	}
	if payload.Size() > math.MaxUint64-uint64(len(tail)) {
		return nil, errors.New("sandbox build logical size overflow")
	}
	return &appendedSource{payload: payload, tail: tail, size: payload.Size() + uint64(len(tail))}, nil
}

// BuildZIP returns the sole canonical Sandbox ZIP encoding.
func BuildZIP(entries map[string][]byte) ([]byte, error) {
	names, err := exactEntryOrder(entries)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, name := range names {
		body := entries[name]
		limit := MaxImageConfigBytes
		if name == config.SandboxRuntimeConfigName {
			limit = config.MaxPortableConfigBytes
		}
		if len(body) == 0 || len(body) > limit {
			return nil, fmt.Errorf("sandbox ZIP entry %s size %d is outside (0,%d]", name, len(body), limit)
		}
		header := &zip.FileHeader{
			Name: name, Method: zip.Store, Flags: 0,
			CreatorVersion: 20, ReaderVersion: 20,
			CRC32:            crc32.ChecksumIEEE(body),
			CompressedSize64: uint64(len(body)), UncompressedSize64: uint64(len(body)),
			ModifiedTime: zipEpochTime, ModifiedDate: zipEpochDate,
		}
		entry, err := writer.CreateRaw(header)
		if err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("sandbox ZIP create %s: %w", name, err)
		}
		if _, err := entry.Write(body); err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("sandbox ZIP write %s: %w", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("sandbox ZIP close: %w", err)
	}
	return output.Bytes(), nil
}

func exactEntryOrder(entries map[string][]byte) ([]string, error) {
	if len(entries) == 1 {
		if _, ok := entries[config.SandboxRuntimeConfigName]; !ok {
			return nil, errors.New("sandbox ZIP live layout requires only sandbox.runtime.cfg")
		}
		return []string{config.SandboxRuntimeConfigName}, nil
	}
	if len(entries) == 2 {
		if _, ok := entries[ImageConfigName]; !ok {
			return nil, errors.New("sandbox ZIP EROFS layout is missing config.json")
		}
		if _, ok := entries[config.SandboxRuntimeConfigName]; !ok {
			return nil, errors.New("sandbox ZIP EROFS layout is missing sandbox.runtime.cfg")
		}
		return []string{ImageConfigName, config.SandboxRuntimeConfigName}, nil
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return nil, fmt.Errorf("sandbox ZIP has invalid entry set %v", names)
}

func parseStrictArchive(ctx context.Context, stream fetch.Stream) (*parsedArchive, error) {
	base, centralStart, centralSize, count, err := parseEOCD(ctx, stream)
	if err != nil {
		return nil, fmt.Errorf("sandbox ZIP EOCD: %w", err)
	}
	if count != 1 && count != 2 {
		return nil, fmt.Errorf("sandbox ZIP has %d entries (want 1 or 2)", count)
	}
	entries := make([]archiveEntry, 0, count)
	position := centralStart
	for index := 0; index < count; index++ {
		fixed, err := readStreamAt(ctx, stream, position, centralHeaderSize)
		if err != nil {
			return nil, fmt.Errorf("sandbox ZIP central entry %d: %w", index, err)
		}
		if binary.LittleEndian.Uint32(fixed[0:4]) != centralHeaderSignature {
			return nil, fmt.Errorf("sandbox ZIP central entry %d has invalid signature", index)
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
			return nil, fmt.Errorf("sandbox ZIP entry %d uses ZIP64", index)
		}
		if versionMade != 20 || versionNeeded != 20 {
			return nil, fmt.Errorf("sandbox ZIP entry %d has non-canonical version metadata", index)
		}
		if flags != 0 {
			return nil, fmt.Errorf("sandbox ZIP entry %d has unsupported flags 0x%x", index, flags)
		}
		if method != zip.Store {
			return nil, fmt.Errorf("sandbox ZIP entry %d method is %d (want Store)", index, method)
		}
		if modifiedTime != zipEpochTime || modifiedDate != zipEpochDate || internalAttrs != 0 || externalAttrs != 0 {
			return nil, fmt.Errorf("sandbox ZIP entry %d has non-canonical metadata", index)
		}
		if diskStart != 0 {
			return nil, fmt.Errorf("sandbox ZIP entry %d starts on disk %d", index, diskStart)
		}
		if extraLength != 0 || commentLength != 0 {
			return nil, fmt.Errorf("sandbox ZIP entry %d has extra data or comment", index)
		}
		if compressedSize != uncompressedSize {
			return nil, fmt.Errorf("sandbox ZIP entry %d Store sizes differ", index)
		}
		nameBytes, err := readStreamAt(ctx, stream, position+centralHeaderSize, uint64(nameLength))
		if err != nil {
			return nil, fmt.Errorf("sandbox ZIP central entry %d name: %w", index, err)
		}
		name := string(nameBytes)
		if name != ImageConfigName && name != config.SandboxRuntimeConfigName {
			return nil, fmt.Errorf("sandbox ZIP unknown entry %q", name)
		}
		limit := MaxImageConfigBytes
		if name == config.SandboxRuntimeConfigName {
			limit = config.MaxPortableConfigBytes
		}
		if uint64(uncompressedSize) > uint64(limit) {
			return nil, fmt.Errorf("sandbox ZIP entry %s exceeds %d bytes", name, limit)
		}
		entries = append(entries, archiveEntry{
			name: name, flags: flags, method: method, modifiedTime: modifiedTime,
			modifiedDate: modifiedDate, crc: crc, compressedSize: compressedSize,
			uncompressedSize: uncompressedSize, localOffset: localOffset, centralStart: position,
		})
		position += centralHeaderSize + uint64(nameLength)
	}
	if position != centralStart+centralSize {
		return nil, errors.New("sandbox ZIP central directory size mismatch")
	}
	wantNames := []string{config.SandboxRuntimeConfigName}
	if count == 2 {
		wantNames = []string{ImageConfigName, config.SandboxRuntimeConfigName}
	}
	seen := make(map[string]struct{}, count)
	for i := range entries {
		if _, duplicate := seen[entries[i].name]; duplicate {
			return nil, fmt.Errorf("sandbox ZIP duplicate entry %q", entries[i].name)
		}
		seen[entries[i].name] = struct{}{}
	}
	for i := range entries {
		if entries[i].name != wantNames[i] {
			return nil, fmt.Errorf("sandbox ZIP entry order is %q at index %d (want %q)", entries[i].name, i, wantNames[i])
		}
	}

	bodies := make(map[string][]byte, count)
	wantLocalOffset := uint64(0)
	for i := range entries {
		entry := &entries[i]
		if uint64(entry.localOffset) != wantLocalOffset {
			return nil, fmt.Errorf("sandbox ZIP local entry %s is not contiguous", entry.name)
		}
		localStart := base + uint64(entry.localOffset)
		fixed, err := readStreamAt(ctx, stream, localStart, localHeaderSize)
		if err != nil {
			return nil, fmt.Errorf("sandbox ZIP local entry %s: %w", entry.name, err)
		}
		if binary.LittleEndian.Uint32(fixed[0:4]) != localHeaderSignature {
			return nil, fmt.Errorf("sandbox ZIP local entry %s has invalid signature", entry.name)
		}
		if binary.LittleEndian.Uint16(fixed[4:6]) != 20 ||
			binary.LittleEndian.Uint16(fixed[6:8]) != entry.flags ||
			binary.LittleEndian.Uint16(fixed[8:10]) != entry.method ||
			binary.LittleEndian.Uint16(fixed[10:12]) != entry.modifiedTime ||
			binary.LittleEndian.Uint16(fixed[12:14]) != entry.modifiedDate ||
			binary.LittleEndian.Uint32(fixed[14:18]) != entry.crc ||
			binary.LittleEndian.Uint32(fixed[18:22]) != entry.compressedSize ||
			binary.LittleEndian.Uint32(fixed[22:26]) != entry.uncompressedSize {
			return nil, fmt.Errorf("sandbox ZIP local/central header mismatch for %s", entry.name)
		}
		nameLength := binary.LittleEndian.Uint16(fixed[26:28])
		extraLength := binary.LittleEndian.Uint16(fixed[28:30])
		if extraLength != 0 {
			return nil, fmt.Errorf("sandbox ZIP local entry %s has extra data", entry.name)
		}
		name, err := readStreamAt(ctx, stream, localStart+localHeaderSize, uint64(nameLength))
		if err != nil {
			return nil, err
		}
		if string(name) != entry.name {
			return nil, fmt.Errorf("sandbox ZIP local/central name mismatch for %s", entry.name)
		}
		entry.localDataStart = localStart + localHeaderSize + uint64(nameLength)
		entry.localRecordEnd = entry.localDataStart + uint64(entry.compressedSize)
		if entry.localRecordEnd > centralStart {
			return nil, fmt.Errorf("sandbox ZIP entry %s overlaps central directory", entry.name)
		}
		body, err := readStreamAt(ctx, stream, entry.localDataStart, uint64(entry.uncompressedSize))
		if err != nil {
			return nil, fmt.Errorf("sandbox ZIP read %s: %w", entry.name, err)
		}
		if crc32.ChecksumIEEE(body) != entry.crc {
			return nil, fmt.Errorf("sandbox ZIP CRC mismatch for %s", entry.name)
		}
		bodies[entry.name] = body
		wantLocalOffset = entry.localRecordEnd - base
	}
	if base+wantLocalOffset != centralStart {
		return nil, errors.New("sandbox ZIP has bytes between local entries and central directory")
	}
	return &parsedArchive{base: base, entries: entries, bodies: bodies}, nil
}

func parseEOCD(ctx context.Context, stream fetch.Stream) (base, centralStart, centralSize uint64, entries int, err error) {
	if stream.Size() < eocdSize {
		return 0, 0, 0, 0, io.ErrUnexpectedEOF
	}
	eocdOffset := stream.Size() - eocdSize
	eocd, err := readStreamAt(ctx, stream, eocdOffset, eocdSize)
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

func parseFlattenedImageArchive(ctx context.Context, stream fetch.Stream) (uint64, []byte, error) {
	base, _, _, count, err := parseEOCD(ctx, stream)
	if err != nil {
		return 0, nil, fmt.Errorf("flattened image ZIP EOCD: %w", err)
	}
	if count != 1 {
		return 0, nil, fmt.Errorf("flattened image ZIP has %d entries (want config.json only)", count)
	}
	if stream.Size() > math.MaxInt64 {
		return 0, nil, errors.New("flattened image is too large for ZIP reader")
	}
	reader, err := zip.NewReader(streamReaderAt{ctx: ctx, stream: stream}, int64(stream.Size()))
	if err != nil {
		return 0, nil, fmt.Errorf("flattened image ZIP: %w", err)
	}
	if len(reader.File) != 1 || reader.File[0].Name != ImageConfigName {
		return 0, nil, errors.New("flattened image ZIP must contain only config.json")
	}
	file := reader.File[0]
	if file.Method != zip.Store {
		return 0, nil, errors.New("flattened image config.json must use Store")
	}
	if file.UncompressedSize64 > MaxImageConfigBytes {
		return 0, nil, fmt.Errorf("flattened image config.json exceeds %d bytes", MaxImageConfigBytes)
	}
	body, err := file.Open()
	if err != nil {
		return 0, nil, err
	}
	data, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		return 0, nil, errors.Join(readErr, closeErr)
	}
	if !json.Valid(data) {
		return 0, nil, errors.New("flattened image config.json is not valid JSON")
	}
	return base, data, nil
}

func readStreamAt(ctx context.Context, stream fetch.Stream, offset, length uint64) ([]byte, error) {
	if length > uint64(int(^uint(0)>>1)) {
		return nil, errors.New("read length exceeds addressable memory")
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

type streamReaderAt struct {
	ctx    context.Context
	stream fetch.Stream
}

func (r streamReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, io.EOF
	}
	return r.stream.ReadAt(r.ctx, buffer, uint64(offset))
}

const erofsBuildProbeBytes = 1024 + 128

type prefetchedSourceRun struct {
	offset uint64
	end    uint64
	kind   sparse.RunKind
	body   []byte
}

func (r prefetchedSourceRun) Offset() uint64       { return r.offset }
func (r prefetchedSourceRun) End() uint64          { return r.end }
func (r prefetchedSourceRun) Kind() sparse.RunKind { return r.kind }

func (r prefetchedSourceRun) ReadAt(ctx context.Context, buffer []byte, innerOffset uint64) (int, error) {
	if innerOffset > r.end-r.offset || uint64(len(buffer)) > r.end-r.offset-innerOffset {
		return 0, errors.New("sandbox prefetched run read is out of bounds")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.kind == sparse.Hole || r.kind == sparse.Zero {
		clear(buffer)
		return len(buffer), nil
	}
	start := r.offset + innerOffset
	copy(buffer, r.body[start:start+uint64(len(buffer))])
	return len(buffer), nil
}

type prefetchedSource struct {
	inner  sparse.Source
	prefix []byte
	runs   []prefetchedSourceRun
}

func (s *prefetchedSource) Size() uint64 { return s.inner.Size() }

func (s *prefetchedSource) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.Size() {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, errors.New("sandbox prefetched source RunAt limit is zero")
	}
	if offset >= uint64(len(s.prefix)) {
		return s.inner.RunAt(offset, limit)
	}
	bound := offset + limit
	if bound < offset || bound > uint64(len(s.prefix)) {
		bound = uint64(len(s.prefix))
	}
	for _, run := range s.runs {
		if offset < run.offset || offset >= run.end {
			continue
		}
		end := run.end
		if end > bound {
			end = bound
		}
		return prefetchedSourceRun{offset: offset, end: end, kind: run.kind, body: s.prefix}, nil
	}
	return nil, fmt.Errorf("sandbox prefetched source has no run at offset %d", offset)
}

func (s *prefetchedSource) ReadAt(ctx context.Context, buffer []byte, offset uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if offset >= s.Size() {
		return 0, io.EOF
	}
	want := len(buffer)
	if uint64(want) > s.Size()-offset {
		want = int(s.Size() - offset)
	}
	written := 0
	if offset < uint64(len(s.prefix)) {
		prefixEnd := uint64(len(s.prefix))
		if prefixEnd > offset+uint64(want) {
			prefixEnd = offset + uint64(want)
		}
		written = copy(buffer[:want], s.prefix[offset:prefixEnd])
		offset += uint64(written)
	}
	if written < want {
		n, err := s.inner.ReadAt(ctx, buffer[written:want], offset)
		written += n
		if err != nil {
			return written, err
		}
		if written != want {
			return written, io.ErrUnexpectedEOF
		}
	}
	if want != len(buffer) {
		return written, io.EOF
	}
	return written, nil
}

func (s *prefetchedSource) TarStreamDigest(name string) ([32]byte, bool) {
	provider, ok := s.inner.(tarstream.IdentityProvider)
	if !ok {
		return [32]byte{}, false
	}
	return provider.TarStreamDigest(name)
}

func (s *prefetchedSource) PayloadCommitment() (uint64, [32]byte, bool) {
	provider, ok := s.inner.(tarstream.IdentityProvider)
	if !ok {
		return s.Size(), [32]byte{}, false
	}
	return provider.PayloadCommitment()
}

// prepareEROFSBuildPayload validates the EROFS superblock without requiring a
// random-access Source. It consumes only the minimum prefix once, records its
// authoritative Hole/Zero/Data runs, and returns a wrapper that replays that
// prefix before continuing monotonically through the original source.
func prepareEROFSBuildPayload(ctx context.Context, source sparse.Source) (sparse.Source, uint64, error) {
	prefixSize := min(source.Size(), uint64(erofsBuildProbeBytes))
	prefix := make([]byte, int(prefixSize))
	runs := make([]prefetchedSourceRun, 0, 4)
	for offset := uint64(0); offset < prefixSize; {
		run, err := source.RunAt(offset, prefixSize-offset)
		if err != nil {
			return nil, 0, err
		}
		if run == nil || run.Offset() != offset || run.End() <= offset || run.End() > prefixSize {
			return nil, 0, fmt.Errorf("invalid sparse run at offset %d", offset)
		}
		runs = append(runs, prefetchedSourceRun{offset: offset, end: run.End(), kind: run.Kind()})
		offset = run.End()
	}
	n, err := source.ReadAt(ctx, prefix, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	if n != len(prefix) {
		return nil, 0, io.ErrUnexpectedEOF
	}
	erofsSize, err := image.ReadEROFSSize(bytes.NewReader(prefix))
	if err != nil {
		return nil, 0, err
	}
	return &prefetchedSource{inner: source, prefix: prefix, runs: runs}, erofsSize, nil
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

// payloadStream retains metadata that belongs beside, but never inside, the
// block payload. vhost consumes only sectionStream bytes; the cold-start image
// config reader obtains the separately validated JSON through ImageConfigBytes.
// Declared artifact identity is delegated to the full carrier so callers that
// canonicalize a parent .sandbox keep the identity of E, not of an invented
// payload-only object.
type payloadStream struct {
	*sectionStream
	imageConfig []byte
}

func (s *payloadStream) ImageConfigBytes() []byte {
	return append([]byte(nil), s.imageConfig...)
}

func (s *payloadStream) Digest() (string, string) {
	if declared, ok := s.owner.stream.(interface{ Digest() (string, string) }); ok {
		return declared.Digest()
	}
	return "", ""
}

func (s *payloadStream) PayloadCommitment() (uint64, [32]byte, bool) {
	provider, ok := s.owner.stream.(tarstream.IdentityProvider)
	if !ok {
		return s.size, [32]byte{}, false
	}
	size, digest, ok := provider.PayloadCommitment()
	return size, digest, ok && size == s.size
}

func (s *payloadStream) TarStreamDigest(name string) ([32]byte, bool) {
	size, payload, ok := s.PayloadCommitment()
	if !ok {
		return [32]byte{}, false
	}
	digest, err := tarstream.ComposeDigest(name, s.size, size, payload, nil)
	return digest, err == nil
}

func (s *payloadStream) RootManifestKey() (store.ContentKey, bool) {
	if selected, ok := s.owner.stream.(interface {
		RootManifestKey() (store.ContentKey, bool)
	}); ok {
		return selected.RootManifestKey()
	}
	return store.ContentKey{}, false
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
	if offset >= s.size {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, errors.New("sandbox section RunAt limit is zero")
	}
	end := offset + limit
	if end < offset || end > s.size {
		end = s.size
	}
	run, err := s.owner.stream.RunAt(s.base+offset, end-offset)
	if err != nil {
		return nil, err
	}
	wantOffset := s.base + offset
	wantEnd := s.base + end
	if run == nil || run.Offset() != wantOffset || run.End() <= wantOffset || run.End() > wantEnd {
		return nil, errors.New("sandbox section source returned invalid run")
	}
	// Sandbox payload is a prefix section (base == 0). Return the carrier Run
	// unchanged so manifest-only capabilities such as fetch.ChunkRun survive
	// through the logical Sandbox boundary. The strict wantEnd check above
	// prevents the returned Run from exposing any byte in the ZIP tail.
	if s.base == 0 {
		return run, nil
	}
	runEnd := run.End() - s.base
	if runEnd <= offset {
		return nil, errors.New("sandbox section source returned invalid run")
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
		return 0, errors.New("sandbox section run read is out of bounds")
	}
	return r.inner.ReadAt(ctx, buffer, innerOffset)
}

type appendedSource struct {
	payload sparse.Source
	tail    []byte
	size    uint64
}

type appendedStream struct {
	*appendedSource
	owner *streamOwner
}

func (s *appendedStream) Close() error { return s.owner.Close() }

func (s *appendedSource) Size() uint64 { return s.size }

func (s *appendedSource) PayloadCommitment() (uint64, [32]byte, bool) {
	provider, ok := s.payload.(tarstream.IdentityProvider)
	if !ok {
		// A newly captured payload has no prior carrier commitment, but its
		// authoritative boundary is still needed by WriteTo. The writer computes
		// and records the commitment during this first encoding.
		return s.payload.Size(), [32]byte{}, false
	}
	size, digest, ok := provider.PayloadCommitment()
	if !ok || size != s.payload.Size() {
		return s.payload.Size(), [32]byte{}, false
	}
	return size, digest, true
}

func (s *appendedSource) TarStreamDigest(name string) ([32]byte, bool) {
	provider, ok := s.payload.(tarstream.IdentityProvider)
	if !ok {
		return [32]byte{}, false
	}
	size, digest, ok := provider.PayloadCommitment()
	if !ok || size != s.payload.Size() {
		return [32]byte{}, false
	}
	result, err := tarstream.ComposeDigest(name, s.size, size, digest, s.tail)
	return result, err == nil
}

func (s *appendedSource) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.size {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, errors.New("sandbox appended source RunAt limit is zero")
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
		if run == nil || run.Offset() != offset || run.End() <= offset || run.End() > payloadEnd {
			return nil, errors.New("sandbox appended payload returned invalid run")
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
		return 0, errors.New("sandbox appended run read is out of bounds")
	}
	if r.payloadRun != nil {
		return r.payloadRun.ReadAt(ctx, buffer, innerOffset)
	}
	return r.source.ReadAt(ctx, buffer, r.offset+innerOffset)
}
