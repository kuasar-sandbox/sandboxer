package sandboxfile

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

const (
	sandboxTestSHA  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	sandboxTestSHA2 = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

func livePortableBytes(t *testing.T) []byte {
	t.Helper()
	cfg := &config.PortableSandboxConfig{
		Version: config.PortableSandboxConfigVersion,
		Resources: config.PortableResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 2, Memory: "1GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1, Memory: "512MiB"},
		},
		Boot: config.PortableBootConfig{
			Kernel:  "file://vmlinux@sha256:" + sandboxTestSHA,
			Runtime: "file://sandbox-runtime.bundle@sha256:" + sandboxTestSHA2,
			Root: config.PortableRootConfig{
				Base:    "file://root.erofs@sha256:" + sandboxTestSHA,
				Overlay: &config.PortableOverlayConfig{Base: "self"},
			},
		},
		Launch: config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"},
	}
	raw, err := config.MarshalPortableSandboxConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func erofsPortableBytes(t *testing.T) []byte {
	t.Helper()
	cfg, err := config.ParsePortableSandboxConfig(livePortableBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Boot.Root = config.PortableRootConfig{Base: "self", Overlay: &config.PortableOverlayConfig{}}
	raw, err := config.MarshalPortableSandboxConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type closeStream struct {
	sparse.Source
	closes int
}

func (s *closeStream) Close() error {
	s.closes++
	return nil
}

func dataSource(t *testing.T, body []byte) sparse.Source {
	t.Helper()
	source, err := sparse.NewSource(bytes.NewReader(body), uint64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func sourceBytes(t *testing.T, source sparse.Source) []byte {
	t.Helper()
	body := make([]byte, source.Size())
	if len(body) == 0 {
		return body
	}
	n, err := source.ReadAt(context.Background(), body, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if n != len(body) {
		t.Fatalf("source read = %d, want %d", n, len(body))
	}
	return body
}

func TestLiveSandboxLayoutAndPayloadSection(t *testing.T) {
	payload := make([]byte, 8192)
	copy(payload, "live sparse delta without an ext4 superblock")
	sparsePayload, err := sparse.NewSource(bytes.NewReader(payload), uint64(len(payload)), []sparse.Extent{{Offset: 4096, Size: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	logical, err := BuildSource(sparsePayload, nil, livePortableBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	stream := &closeStream{Source: logical}
	root, err := Open(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	if root.ArchiveBase != uint64(len(payload)) || root.Payload.Size() != uint64(len(payload)) {
		t.Fatalf("archive base/payload size = %d/%d, want %d", root.ArchiveBase, root.Payload.Size(), len(payload))
	}
	if root.FullStream.Size() <= root.Payload.Size() {
		t.Fatal("full stream does not include ZIP tail")
	}
	run, err := root.Payload.RunAt(4096, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if run.Kind() != sparse.Hole {
		t.Fatalf("payload hole became %v", run.Kind())
	}
	if _, err := root.Payload.RunAt(root.Payload.Size(), 1); !errors.Is(err, io.EOF) {
		t.Fatalf("payload exposed ZIP tail: %v", err)
	}
	if !bytes.Equal(root.RuntimeConfig, livePortableBytes(t)) {
		t.Fatal("runtime config bytes changed")
	}
	if len(root.ImageConfig) != 0 {
		t.Fatalf("live layout unexpectedly has config.json")
	}
	if err := root.Payload.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if stream.closes != 1 {
		t.Fatalf("carrier close count = %d, want 1", stream.closes)
	}
}

func fakeEROFS() []byte {
	body := make([]byte, 4096)
	binary.LittleEndian.PutUint32(body[1024:1028], 0xE0F5E1E2)
	body[1024+12] = 12
	binary.LittleEndian.PutUint32(body[1024+36:1024+40], 1)
	return body
}

func TestEROFSLayoutUsesOneStrictZIPAndPreservesConfigBytes(t *testing.T) {
	payload := fakeEROFS()
	imageConfig := []byte(`{"Architecture":"amd64","Os":"linux","Env":["B=2","A=1"]}`)
	logical, err := BuildSource(dataSource(t, payload), imageConfig, erofsPortableBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	root, err := Open(context.Background(), &closeStream{Source: logical})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if root.ArchiveBase != uint64(len(payload)) {
		t.Fatalf("archive base = %d, want %d", root.ArchiveBase, len(payload))
	}
	if !bytes.Equal(root.ImageConfig, imageConfig) {
		t.Fatalf("config.json changed:\n%s\n%s", root.ImageConfig, imageConfig)
	}
	archive, err := parseStrictArchive(context.Background(), root.FullStream)
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.entries) != 2 || archive.entries[0].name != ImageConfigName || archive.entries[1].name != config.SandboxRuntimeConfigName {
		t.Fatalf("entries = %#v", archive.entries)
	}
}

func TestOfflineFlattenedEROFSRebuildPreservesConfigAndRemovesOldZIP(t *testing.T) {
	payload := fakeEROFS()
	imageConfig := []byte(`{"Architecture":"arm64","Os":"linux","Cmd":["/app"]}`)
	legacyZIP := legacyConfigZIP(t, imageConfig)
	flattenedBytes := append(append([]byte(nil), payload...), legacyZIP...)
	flattened := &closeStream{Source: dataSource(t, flattenedBytes)}
	opened, err := OpenFlattenedEROFS(context.Background(), flattened)
	if err != nil {
		t.Fatal(err)
	}
	if opened.ArchiveBase != uint64(len(payload)) {
		t.Fatalf("flattened archive base = %d, want %d", opened.ArchiveBase, len(payload))
	}
	logical, err := BuildSource(opened.Payload, opened.ImageConfig, erofsPortableBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := Open(context.Background(), &closeStream{Source: logical})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ArchiveBase != uint64(len(payload)) {
		t.Fatalf("rebuilt payload retained old ZIP: archive base=%d want=%d", rebuilt.ArchiveBase, len(payload))
	}
	if !bytes.Equal(rebuilt.ImageConfig, imageConfig) {
		t.Fatal("offline conversion changed config.json bytes")
	}
	if err := rebuilt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if flattened.closes != 1 {
		t.Fatalf("flattened close count = %d", flattened.closes)
	}
}

func TestOpenEROFSArtifactAcceptsBarePayloadAndStripsFlattenedZIP(t *testing.T) {
	payload := fakeEROFS()
	bare, err := OpenEROFSArtifact(context.Background(), &closeStream{Source: dataSource(t, payload)})
	if err != nil {
		t.Fatal(err)
	}
	if bare.Payload.Size() != uint64(len(payload)) || bare.ImageConfig != nil {
		t.Fatalf("bare EROFS = size %d config %q", bare.Payload.Size(), bare.ImageConfig)
	}
	if err := bare.Close(); err != nil {
		t.Fatal(err)
	}

	imageConfig := []byte(`{"Architecture":"amd64","Os":"linux","Cmd":["/app"]}`)
	flattenedBytes := append(append([]byte(nil), payload...), legacyConfigZIP(t, imageConfig)...)
	flattened, err := OpenEROFSArtifact(context.Background(), &closeStream{Source: dataSource(t, flattenedBytes)})
	if err != nil {
		t.Fatal(err)
	}
	defer flattened.Close()
	if flattened.Payload.Size() != uint64(len(payload)) || !bytes.Equal(flattened.ImageConfig, imageConfig) {
		t.Fatalf("flattened EROFS = size %d config %q", flattened.Payload.Size(), flattened.ImageConfig)
	}

	malformed := append(append([]byte(nil), payload...), []byte("not-a-config-zip")...)
	if _, err := OpenEROFSArtifact(context.Background(), &closeStream{Source: dataSource(t, malformed)}); err == nil {
		t.Fatal("malformed EROFS tail was accepted")
	}
}

func TestSandboxRejectsDoubleZIPAndBadEROFS(t *testing.T) {
	imageConfig := []byte(`{"Architecture":"amd64","Os":"linux"}`)
	payload := fakeEROFS()
	legacy := append(append([]byte(nil), payload...), legacyConfigZIP(t, imageConfig)...)
	tail, err := BuildZIP(map[string][]byte{
		ImageConfigName:                 imageConfig,
		config.SandboxRuntimeConfigName: erofsPortableBytes(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	double := append(legacy, tail...)
	if _, err := Open(context.Background(), &closeStream{Source: dataSource(t, double)}); err == nil || !stringsContains(err.Error(), "does not equal ZIP archive base") {
		t.Fatalf("double ZIP error = %v", err)
	}

	bad := fakeEROFS()
	bad[1024] ^= 0xff
	if _, err := BuildSource(dataSource(t, bad), imageConfig, erofsPortableBytes(t)); err == nil || !stringsContains(err.Error(), "bad magic") {
		t.Fatalf("bad EROFS error = %v", err)
	}
}

func TestSandboxStrictZIPRejectsMalformedInputs(t *testing.T) {
	payload := []byte("root-payload-without-filesystem-magic")
	logical, err := BuildSource(dataSource(t, payload), nil, livePortableBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	valid := sourceBytes(t, logical)
	base, central, _, _, err := parseEOCD(context.Background(), &closeStream{Source: dataSource(t, valid)})
	if err != nil {
		t.Fatal(err)
	}
	eocd := len(valid) - eocdSize
	tests := []struct {
		name    string
		mutate  func([]byte) []byte
		wantErr string
	}{
		{name: "truncated", mutate: func(in []byte) []byte { return in[:len(in)-1] }, wantErr: "EOCD"},
		{name: "trailing byte", mutate: func(in []byte) []byte { return append(in, 0) }, wantErr: "EOCD"},
		{name: "comment", mutate: func(in []byte) []byte { binary.LittleEndian.PutUint16(in[eocd+20:eocd+22], 1); return in }, wantErr: "comment"},
		{name: "multi disk", mutate: func(in []byte) []byte { binary.LittleEndian.PutUint16(in[eocd+4:eocd+6], 1); return in }, wantErr: "multi-disk"},
		{name: "zip64", mutate: func(in []byte) []byte { binary.LittleEndian.PutUint16(in[eocd+10:eocd+12], 0xffff); return in }, wantErr: "ZIP64"},
		{name: "flags", mutate: func(in []byte) []byte {
			binary.LittleEndian.PutUint16(in[int(base)+6:int(base)+8], 8)
			binary.LittleEndian.PutUint16(in[int(central)+8:int(central)+10], 8)
			return in
		}, wantErr: "unsupported flags"},
		{name: "local central mismatch", mutate: func(in []byte) []byte { in[int(base)+14] ^= 1; return in }, wantErr: "local/central header mismatch"},
		{name: "crc", mutate: func(in []byte) []byte {
			nameLength := int(binary.LittleEndian.Uint16(in[int(base)+26 : int(base)+28]))
			in[int(base)+localHeaderSize+nameLength] ^= 1
			return in
		}, wantErr: "CRC mismatch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := tc.mutate(append([]byte(nil), valid...))
			_, err := Open(context.Background(), &closeStream{Source: dataSource(t, input)})
			if err == nil || !stringsContains(err.Error(), tc.wantErr) {
				t.Fatalf("Open error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestSandboxStrictZIPRejectsUnknownDuplicateExtraAndNonCanonicalConfig(t *testing.T) {
	payload := []byte("payload")
	nonCanonical := bytes.Replace(livePortableBytes(t), []byte("version: 1\n"), []byte("version: !!int 1\n"), 1)
	tests := []struct {
		name    string
		entries []rawEntry
		wantErr string
	}{
		{name: "unknown", entries: []rawEntry{{name: "unknown", body: []byte("x")}}, wantErr: "unknown entry"},
		{name: "duplicate", entries: []rawEntry{
			{name: config.SandboxRuntimeConfigName, body: livePortableBytes(t)},
			{name: config.SandboxRuntimeConfigName, body: livePortableBytes(t)},
		}, wantErr: "duplicate entry"},
		{name: "extra", entries: []rawEntry{{name: config.SandboxRuntimeConfigName, body: livePortableBytes(t), extra: []byte{1, 2}}}, wantErr: "extra data"},
		{name: "deflate", entries: []rawEntry{{name: config.SandboxRuntimeConfigName, body: livePortableBytes(t), method: zip.Deflate}}, wantErr: "method"},
		{name: "noncanonical runtime", entries: []rawEntry{{name: config.SandboxRuntimeConfigName, body: nonCanonical}}, wantErr: "not canonically encoded"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			archive := rawZIP(t, tc.entries)
			input := append(append([]byte(nil), payload...), archive...)
			_, err := Open(context.Background(), &closeStream{Source: dataSource(t, input)})
			if err == nil || !stringsContains(err.Error(), tc.wantErr) {
				t.Fatalf("Open error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestPayloadSectionPreservesHoleZeroDataKinds(t *testing.T) {
	payload := &kindSource{size: 12, data: []byte("DATA" + "\x00\x00\x00\x00" + "\x00\x00\x00\x00")}
	logical, err := BuildSource(payload, nil, livePortableBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	root, err := Open(context.Background(), &closeStream{Source: logical})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, tc := range []struct {
		offset uint64
		kind   sparse.RunKind
	}{{0, sparse.Data}, {4, sparse.Hole}, {8, sparse.Zero}} {
		run, err := root.Payload.RunAt(tc.offset, 4)
		if err != nil {
			t.Fatal(err)
		}
		if run.Kind() != tc.kind {
			t.Fatalf("RunAt(%d) kind = %v, want %v", tc.offset, run.Kind(), tc.kind)
		}
	}
}

func legacyConfigZIP(t *testing.T, body []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	header := &zip.FileHeader{Name: ImageConfigName, Method: zip.Store}
	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

type rawEntry struct {
	name   string
	body   []byte
	extra  []byte
	method uint16
}

func rawZIP(t *testing.T, entries []rawEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, item := range entries {
		method := item.method
		if method == 0 {
			method = zip.Store
		}
		header := &zip.FileHeader{
			Name: item.name, Method: method, Extra: item.extra,
			CreatorVersion: 20, ReaderVersion: 20,
			CRC32: crc32.ChecksumIEEE(item.body), CompressedSize64: uint64(len(item.body)),
			UncompressedSize64: uint64(len(item.body)), ModifiedDate: zipEpochDate,
		}
		entry, err := writer.CreateRaw(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(item.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func stringsContains(value, substring string) bool {
	return bytes.Contains([]byte(value), []byte(substring))
}

type kindSource struct {
	size uint64
	data []byte
}

func (s *kindSource) Size() uint64 { return s.size }

func (s *kindSource) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.size {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, errors.New("zero limit")
	}
	boundary := ((offset / 4) + 1) * 4
	if max := offset + limit; boundary > max {
		boundary = max
	}
	if boundary > s.size {
		boundary = s.size
	}
	kind := sparse.Data
	if offset >= 8 {
		kind = sparse.Zero
	} else if offset >= 4 {
		kind = sparse.Hole
	}
	return kindRun{source: s, offset: offset, end: boundary, kind: kind}, nil
}

func (s *kindSource) ReadAt(_ context.Context, buffer []byte, offset uint64) (int, error) {
	if offset >= s.size {
		return 0, io.EOF
	}
	n := len(buffer)
	var eof error
	if uint64(n) > s.size-offset {
		n = int(s.size - offset)
		eof = io.EOF
	}
	copy(buffer[:n], s.data[offset:offset+uint64(n)])
	return n, eof
}

type kindRun struct {
	source *kindSource
	offset uint64
	end    uint64
	kind   sparse.RunKind
}

func (r kindRun) Offset() uint64       { return r.offset }
func (r kindRun) End() uint64          { return r.end }
func (r kindRun) Kind() sparse.RunKind { return r.kind }
func (r kindRun) ReadAt(ctx context.Context, buffer []byte, inner uint64) (int, error) {
	if inner > r.end-r.offset || uint64(len(buffer)) > r.end-r.offset-inner {
		return 0, errors.New("out of bounds")
	}
	if r.kind != sparse.Data {
		clear(buffer)
		return len(buffer), nil
	}
	return r.source.ReadAt(ctx, buffer, r.offset+inner)
}
