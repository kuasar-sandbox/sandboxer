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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/image"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tailzip"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
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

type archiveEntry struct{ name string }

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
		erofsSize, err := image.ReadEROFSSize(&streamReaderAt{ctx: ctx, stream: stream})
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
	names, _, err := tailzip.Names(ctx, tailReaderAt{ctx: ctx, stream: stream}, stream.Size(), 64, 128<<10)
	if err != nil {
		return false, fmt.Errorf("sandbox payload ZIP metadata: %w", err)
	}
	for _, name := range names {
		if name == config.SandboxRuntimeConfigName {
			return true, nil
		}
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
	erofsSize, err := image.ReadEROFSSize(&streamReaderAt{ctx: ctx, stream: stream})
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
	erofsSize, err := image.ReadEROFSSize(&streamReaderAt{ctx: ctx, stream: stream})
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
		source, err := tailzip.Append(payload, tail, tailzip.Options{})
		if err != nil {
			return nil, errors.Join(err, owner.Close())
		}
		full = &appendedStream{Source: source, owner: owner}
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
	return tailzip.Encode([]tailzip.Entry{{Name: ImageConfigName, Body: imageConfig}})
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
	return tailzip.Append(payload, tail, tailzip.Options{})
}

// BuildZIP returns the sole canonical Sandbox ZIP encoding.
func BuildZIP(entries map[string][]byte) ([]byte, error) {
	names, err := exactEntryOrder(entries)
	if err != nil {
		return nil, err
	}
	ordered := make([]tailzip.Entry, 0, len(names))
	for _, name := range names {
		limit := MaxImageConfigBytes
		if name == config.SandboxRuntimeConfigName {
			limit = config.MaxPortableConfigBytes
		}
		body := entries[name]
		if len(body) == 0 || len(body) > limit {
			return nil, fmt.Errorf("sandbox ZIP entry %s size is outside limits", name)
		}
		ordered = append(ordered, tailzip.Entry{Name: name, Body: body})
	}
	return tailzip.EncodeCanonical(ordered)
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
	footer, err := tailzip.ReadFooter(ctx, tailReaderAt{ctx: ctx, stream: stream}, stream.Size())
	if err != nil {
		return nil, fmt.Errorf("sandbox ZIP EOCD: %w", err)
	}
	names := []string{config.SandboxRuntimeConfigName}
	switch footer.Count {
	case 1:
	case 2:
		names = []string{ImageConfigName, config.SandboxRuntimeConfigName}
	default:
		return nil, fmt.Errorf("sandbox ZIP has %d entries (want 1 or 2)", footer.Count)
	}
	base, bodies, err := tailzip.ReadCanonical(ctx, tailReaderAt{ctx: ctx, stream: stream}, stream.Size(), names, map[string]int{ImageConfigName: MaxImageConfigBytes, config.SandboxRuntimeConfigName: config.MaxPortableConfigBytes})
	if err != nil {
		return nil, fmt.Errorf("sandbox ZIP: %w", err)
	}
	entries := make([]archiveEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, archiveEntry{name: name})
	}
	return &parsedArchive{base: base, entries: entries, bodies: bodies}, nil
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
func parseEOCD(ctx context.Context, stream fetch.Stream) (base, centralStart, centralSize uint64, entries int, err error) {
	footer, err := tailzip.ReadFooter(ctx, tailReaderAt{ctx: ctx, stream: stream}, stream.Size())
	return footer.Base, footer.CentralStart, footer.CentralSize, footer.Count, err
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
	// ZIP's internal ReadFull/ReadAll calls can discard a full read's error.
	// Keep the source cause for this parse, including successful ZIP results.
	source := &streamReaderAt{ctx: ctx, stream: stream}
	reader, _, err := tailzip.Open(source, int64(stream.Size()), tailzip.Options{MaxEntries: 1, KnownEntries: []string{ImageConfigName}, RequireStored: true, MaxSize: MaxImageConfigBytes + 1024})
	if source.err != nil {
		return 0, nil, source.err
	}
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
	if source.err != nil {
		if body != nil {
			_ = body.Close()
		}
		return 0, nil, source.err
	}
	if err != nil {
		return 0, nil, err
	}
	data, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if source.err != nil || readErr != nil || closeErr != nil {
		return 0, nil, errors.Join(source.err, readErr, closeErr)
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
	n, err := readretry.ReadAt(ctx, len(body), func() (int, error) { return stream.ReadAt(ctx, body, offset) })
	if err != nil && (readretry.IsTerminal(err) || readerr.IsPermanent(err) || err != io.EOF) {
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
	err    error // first terminal result in this synchronous metadata parse
}

func (r *streamReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if offset < 0 {
		return 0, io.EOF
	}
	if uint64(offset) >= r.stream.Size() {
		return 0, io.EOF
	}
	length := min(uint64(len(buffer)), r.stream.Size()-uint64(offset))
	n, err := readretry.ReadAt(r.ctx, int(length), func() (int, error) { return r.stream.ReadAt(r.ctx, buffer[:length], uint64(offset)) })
	if readretry.IsTerminal(err) || readerr.IsPermanent(err) {
		r.err = err
	}
	if err == nil && n < len(buffer) {
		err = io.EOF
	}
	return n, err
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
		var run sparse.Run
		err := readretry.Do(ctx, func() error {
			var err error
			run, err = source.RunAt(offset, prefixSize-offset)
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return readerr.Mark(err, false)
			}
			return err
		})
		if err != nil {
			return nil, 0, err
		}
		if run == nil || run.Offset() != offset || run.End() <= offset || run.End() > prefixSize {
			return nil, 0, readerr.Mark(fmt.Errorf("invalid sparse run at offset %d", offset), false)
		}
		runs = append(runs, prefetchedSourceRun{offset: offset, end: run.End(), kind: run.Kind()})
		offset = run.End()
	}
	_, err := readretry.ReadAt(ctx, len(prefix), func() (int, error) { return source.ReadAt(ctx, prefix, 0) })
	if err != nil {
		return nil, 0, err
	}
	erofsSize, err := image.ReadEROFSSize(bytes.NewReader(prefix))
	if err != nil {
		return nil, 0, readerr.Mark(err, false)
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
	return (tailzip.Section{Source: s.owner.stream, Base: s.base, Length: s.size}).RunAt(offset, limit)
}
func (s *sectionStream) ReadAt(ctx context.Context, b []byte, offset uint64) (int, error) {
	return (tailzip.Section{Source: s.owner.stream, Base: s.base, Length: s.size}).ReadAt(ctx, b, offset)
}

type appendedStream struct {
	sparse.Source
	owner *streamOwner
}

func (s *appendedStream) Close() error { return s.owner.Close() }
func (s *appendedStream) PayloadCommitment() (uint64, [32]byte, bool) {
	if provider, ok := s.Source.(tarstream.IdentityProvider); ok {
		return provider.PayloadCommitment()
	}
	return s.Size(), [32]byte{}, false
}
func (s *appendedStream) TarStreamDigest(name string) ([32]byte, bool) {
	if provider, ok := s.Source.(tarstream.IdentityProvider); ok {
		return provider.TarStreamDigest(name)
	}
	return [32]byte{}, false
}
