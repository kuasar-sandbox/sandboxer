package main

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
	"strings"
	"testing"

	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

const pageTestHighGPA = uint64(1 << 32)

type pageTestRegion struct {
	Slot      uint32  `json:"slot"`
	GPA       uint64  `json:"gpa"`
	Size      uint64  `json:"size"`
	Offset    *uint64 `json:"file_offset,omitempty"`
	Zone      string  `json:"zone_id"`
	VirtioMem bool    `json:"virtio_mem"`
}

func pageTestRegions() []pageTestRegion {
	low, high := uint64(0), uint64(8192)
	return []pageTestRegion{
		{Slot: 0, GPA: 0, Size: 8192, Offset: &low, Zone: "ram0"},
		{Slot: 1, GPA: pageTestHighGPA, Size: 8192, Offset: &high, Zone: "ram0"},
	}
}

func pageTestState(t *testing.T, regions []pageTestRegion) []byte {
	t.Helper()
	inner, err := json.Marshal(map[string]any{"guest_ram_mappings": regions, "memory_ranges": map[string]any{"data": []any{}}})
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(map[string]any{"snapshots": map[string]any{"memory-manager": map[string]any{"snapshot_data": map[string]any{"state": string(inner)}}}})
	if err != nil {
		t.Fatal(err)
	}
	return outer
}

type pageTestMemory struct {
	body  []byte
	kinds [4]sparse.RunKind
}

func newPageTestMemory(kind sparse.RunKind) *pageTestMemory {
	return &pageTestMemory{body: bytes.Repeat([]byte{0xff}, 16384), kinds: [4]sparse.RunKind{sparse.Data, sparse.Data, kind, sparse.Data}}
}

func (s *pageTestMemory) Size() uint64 { return uint64(len(s.body)) }

func (s *pageTestMemory) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.Size() {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, errors.New("empty test run")
	}
	end := min(s.Size(), (offset/memoryPageSize+1)*memoryPageSize, offset+limit)
	return pageTestRun{source: s, offset: offset, end: end}, nil
}

func (s *pageTestMemory) ReadAt(ctx context.Context, buf []byte, offset uint64) (int, error) {
	var done int
	for done < len(buf) {
		run, err := s.RunAt(offset+uint64(done), uint64(len(buf)-done))
		if err != nil {
			return done, err
		}
		n, err := run.ReadAt(ctx, buf[done:done+int(run.End()-run.Offset())], 0)
		done += n
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

type pageTestRun struct {
	source      *pageTestMemory
	offset, end uint64
}

func (r pageTestRun) Offset() uint64       { return r.offset }
func (r pageTestRun) End() uint64          { return r.end }
func (r pageTestRun) Kind() sparse.RunKind { return r.source.kinds[r.offset/memoryPageSize] }
func (r pageTestRun) ReadAt(ctx context.Context, buf []byte, inner uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if inner > r.end-r.offset || uint64(len(buf)) > r.end-r.offset-inner {
		return 0, io.EOF
	}
	if r.Kind() == sparse.Data {
		copy(buf, r.source.body[r.offset+inner:r.offset+inner+uint64(len(buf))])
	} else {
		clear(buf)
	}
	return len(buf), nil
}

func pageTestConfig(t *testing.T, dir, policy string, key byte) (string, tarstream.Codec) {
	t.Helper()
	var secret [32]byte
	secret[0] = key
	path := filepath.Join(dir, "manifest-"+policy+fmt.Sprint(key)+".yaml")
	contents := fmt.Sprintf("manifest:\n  key: %s\ncrypto:\n  local: %s\nstore:\n  endpoint: /must-not-connect/store.sock\ncache:\n  endpoint: /must-not-connect/cache.sock\n", hex.EncodeToString(secret[:]), policy)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	codec, err := manifestcrypto.NewTarStreamCodec(secret)
	if err != nil {
		t.Fatal(err)
	}
	return path, codec
}

func pageTestSnapshot(t *testing.T, dir, name string, memory sparse.Source, state []byte, codec tarstream.Codec, refs ...string) string {
	t.Helper()
	cfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: "file://unused.sandbox@digest:" + strings.Repeat("a", 64), FromRefs: refs})
	if err != nil {
		t.Fatal(err)
	}
	source, err := snapshotfile.BuildSource(memory, []byte(`{"memory":{"size":0}}`), state, cfg)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	var opts []tarstream.WriteOption
	if codec != nil {
		opts = append(opts, tarstream.WithCodec(codec, true))
	}
	scheme, digest, err := tarstream.WriteTo(context.Background(), file, "snapshot", source, opts...)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	return "file://" + name + "@" + scheme + ":" + digest
}

func pageTestArgs(ref, dir, cfg string) []string {
	return []string{"--source-ref", ref, "--source-dir", dir, "--manifest-config", cfg,
		"--gpa", fmt.Sprint(pageTestHighGPA), "--start", "5", "--length", "64"}
}

func TestPageLocalSnapshotLayers(t *testing.T) {
	t.Setenv("MANIFEST_KEY", "")
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			for _, kind := range []sparse.RunKind{sparse.Data, sparse.Hole, sparse.Zero} {
				t.Run(fmt.Sprint(kind), func(t *testing.T) {
					dir := t.TempDir()
					policy := "off"
					if encrypted {
						policy = "required"
					}
					cfg, codec := pageTestConfig(t, dir, policy, 0x51)
					if !encrypted {
						codec = nil
					}
					state := pageTestState(t, pageTestRegions())
					bottom := newPageTestMemory(sparse.Data)
					bottom.body[8192+7], bottom.body[8192+20] = 0, 0x31
					base := pageTestSnapshot(t, dir, "base.snapshot", bottom, state, codec)
					middle := pageTestSnapshot(t, dir, "middle.snapshot", newPageTestMemory(kind), state, codec, base)
					top := pageTestSnapshot(t, dir, "top.snapshot", newPageTestMemory(sparse.Hole), state, codec, middle, base)
					var out bytes.Buffer
					if err := runPage(pageTestArgs(top, dir, cfg), &out); err != nil {
						t.Fatal(err)
					}
					if out.Len() > 16<<10 {
						t.Fatalf("unbounded diagnostic: %d bytes", out.Len())
					}
					var got pageReport
					if err := json.Unmarshal(out.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
					want := bytes.Repeat([]byte{0xff}, 64)
					wantKinds := []string{"Hole", "Data", "Data"}
					switch kind {
					case sparse.Hole:
						copy(want, bottom.body[8192+5:8192+69])
						wantKinds[1] = "Hole"
					case sparse.Zero:
						clear(want)
						wantKinds[1] = "Zero"
					}
					hash := sha256.Sum256(want)
					if got.GPA != pageTestHighGPA || got.MemoryOffset != 8192 || got.Start != 5 || got.Length != 64 || got.SHA256 != hex.EncodeToString(hash[:]) {
						t.Fatalf("wrong page result: %+v", got)
					}
					if len(got.Layers) != 3 || got.SourceIdentity != got.Layers[0].Identity {
						t.Fatalf("wrong layer identities: %+v", got)
					}
					for i, layer := range got.Layers {
						if layer.Kind != wantKinds[i] || len(strings.SplitN(layer.Identity, ":", 2)[1]) != 64 {
							t.Fatalf("layer %d: %+v", i, layer)
						}
					}
					var count uint64
					var points []pageBadPoint
					for i, value := range want {
						if value != 0xff {
							count++
							if len(points) < 8 {
								points = append(points, pageBadPoint{Offset: uint64(5 + i), Actual: value})
							}
						}
					}
					if got.NonFF != count || len(got.BadPoints) != len(points) {
						t.Fatalf("wrong non-FF evidence: %+v", got)
					}
					for i := range points {
						if got.BadPoints[i] != points[i] {
							t.Fatalf("bad point %d = %+v, want %+v", i, got.BadPoints[i], points[i])
						}
					}
				})
			}
		})
	}
}

func TestPageImplicitZeroAndPolicyFailures(t *testing.T) {
	t.Setenv("MANIFEST_KEY", "")
	dir := t.TempDir()
	off, codec := pageTestConfig(t, dir, "off", 0x51)
	required, _ := pageTestConfig(t, dir, "required", 0x51)
	wrongKey, _ := pageTestConfig(t, dir, "required", 0x52)
	state := pageTestState(t, pageTestRegions())
	plain := pageTestSnapshot(t, dir, "plain.snapshot", newPageTestMemory(sparse.Hole), state, nil)
	encrypted := pageTestSnapshot(t, dir, "encrypted.snapshot", newPageTestMemory(sparse.Data), state, codec)
	var out bytes.Buffer
	args := pageTestArgs(plain, dir, off)
	args[9], args[11] = "0", "4096"
	if err := runPage(args, &out); err != nil {
		t.Fatal(err)
	}
	var got pageReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(make([]byte, 4096))
	if got.SHA256 != hex.EncodeToString(hash[:]) || got.NonFF != 4096 || got.Layers[0].Kind != "Hole" || len(got.BadPoints) != 8 || out.Len() > 1024 {
		t.Fatalf("final Hole must zero-fill: %+v", got)
	}
	for _, tc := range []struct{ name, ref, cfg string }{
		{"required-rejects-plain", plain, required},
		{"wrong-key", encrypted, wrongKey},
		{"encrypted-with-policy-off", encrypted, off},
		{"missing-config", plain, filepath.Join(dir, "missing.yaml")},
		{"wrong-identity", plain[:strings.LastIndex(plain, ":")+1] + strings.Repeat("0", 64), off},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := runPage(pageTestArgs(tc.ref, dir, tc.cfg), &output); err == nil || output.Len() != 0 {
				t.Fatalf("expected failure without success JSON: err=%v output=%s", err, output.String())
			}
		})
	}
}

func TestPageRejectsUnsupportedSources(t *testing.T) {
	t.Setenv("MANIFEST_KEY", "")
	dir := t.TempDir()
	cfg, _ := pageTestConfig(t, dir, "off", 0x51)
	state := pageTestState(t, pageTestRegions())
	plain := pageTestSnapshot(t, dir, "plain.snapshot", newPageTestMemory(sparse.Data), state, nil)
	identity := plain[strings.LastIndex(plain, "@"):]
	outside := t.TempDir()
	other := pageTestSnapshot(t, outside, "other.snapshot", newPageTestMemory(sparse.Data), state, nil)
	if err := os.Symlink(filepath.Join(outside, "other.snapshot"), filepath.Join(dir, "escape.snapshot")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle.snapshot"), []byte{'P', 'K', 3, 4, 0, 0, 0, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	remote := "manifest://" + strings.Repeat("b", 64)
	remoteParent := pageTestSnapshot(t, dir, "remote-parent.snapshot", newPageTestMemory(sparse.Hole), state, nil, remote)
	for _, ref := range []string{
		remote, plain + "@location:cache", "file://plain.snapshot", "file://../plain.snapshot" + identity,
		"file://escape.snapshot" + other[strings.LastIndex(other, "@"):], "file://bundle.snapshot" + identity, remoteParent,
	} {
		var out bytes.Buffer
		if err := runPage(pageTestArgs(ref, dir, cfg), &out); err == nil || out.Len() != 0 {
			t.Fatalf("unsupported source accepted: %s, err=%v", ref, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := inspectPage(ctx, pageRequest{sourceRef: plain, sourceDir: dir, manifestConfig: cfg}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled probe error = %v", err)
	}
}

func TestPageRejectsMappingAndRangeErrors(t *testing.T) {
	t.Setenv("MANIFEST_KEY", "")
	dir := t.TempDir()
	cfg, _ := pageTestConfig(t, dir, "off", 0x51)
	for _, tc := range []struct {
		name   string
		mutate func([]pageTestRegion)
	}{
		{"missing-offset", func(r []pageTestRegion) { r[1].Offset = nil }},
		{"duplicate-slot", func(r []pageTestRegion) { r[1].Slot = r[0].Slot }},
		{"gpa-overlap", func(r []pageTestRegion) { r[1].GPA = 4096 }},
		{"offset-gap", func(r []pageTestRegion) { *r[1].Offset = 12288 }},
		{"overflow", func(r []pageTestRegion) { r[1].GPA = ^uint64(0) - 4095 }},
		{"partial-capacity", func(r []pageTestRegion) { r[1].Size = 4096 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			regions := pageTestRegions()
			tc.mutate(regions)
			ref := pageTestSnapshot(t, dir, tc.name+".snapshot", newPageTestMemory(sparse.Data), pageTestState(t, regions), nil)
			if err := runPage(pageTestArgs(ref, dir, cfg), io.Discard); err == nil {
				t.Fatal("invalid mapping accepted")
			}
		})
	}
	good := pageTestSnapshot(t, dir, "good.snapshot", newPageTestMemory(sparse.Data), pageTestState(t, pageTestRegions()), nil)
	for _, values := range [][3]string{
		{"8192", "0", "1"}, // The GPA gap must not be mistaken for a memfd offset.
		{fmt.Sprint(pageTestHighGPA + 1), "0", "1"},
		{fmt.Sprint(pageTestHighGPA), "4095", "2"},
		{fmt.Sprint(pageTestHighGPA), "0", "0"},
		{fmt.Sprint(pageTestHighGPA), "0", "4097"},
	} {
		args := pageTestArgs(good, dir, cfg)
		args[7], args[9], args[11] = values[0], values[1], values[2]
		if err := runPage(args, io.Discard); err == nil {
			t.Fatalf("invalid address/range accepted: %v", values)
		}
	}
	if err := runPage(pageTestArgs(good, dir, cfg)[:10], io.Discard); err == nil {
		t.Fatal("missing explicit length accepted")
	}
	regions := pageTestRegions()
	regions[0].Size, regions[1].Size, *regions[1].Offset = 4096, 12288, 4096
	base := pageTestSnapshot(t, dir, "changed-layout.snapshot", newPageTestMemory(sparse.Data), pageTestState(t, regions), nil)
	top := pageTestSnapshot(t, dir, "changed-parent.snapshot", newPageTestMemory(sparse.Hole), pageTestState(t, pageTestRegions()), nil, base)
	if err := runPage(pageTestArgs(top, dir, cfg), io.Discard); err == nil || !strings.Contains(err.Error(), "inconsistent memory layout") {
		t.Fatalf("different GPA-to-memfd mapping in parent accepted: %v", err)
	}
}

func TestCompleteMemoryComparisonStillWorks(t *testing.T) {
	dir := t.TempDir()
	memory := newPageTestMemory(sparse.Data)
	_ = pageTestSnapshot(t, dir, "full.snapshot", memory, pageTestState(t, pageTestRegions()), nil)
	raw := filepath.Join(dir, "RAM.bin")
	if err := os.WriteFile(raw, memory.body, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(dir, "comparison.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	oldArgs, oldStdout := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = oldArgs, oldStdout }()
	os.Args, os.Stdout = []string{"memorycompare", filepath.Join(dir, "full.snapshot"), raw}, output
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.NewDecoder(output).Decode(&report); err != nil || report["complete_comparison"] != "equal" {
		t.Fatalf("old comparison result = %v, err=%v", report, err)
	}
	memory.body[8199] = 0
	if err := os.WriteFile(raw, memory.body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil || !strings.Contains(err.Error(), "RAM differs at offset 0x2007") {
		t.Fatalf("old comparison no longer detects mismatch: %v", err)
	}
}
