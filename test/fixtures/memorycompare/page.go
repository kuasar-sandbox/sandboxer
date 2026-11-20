package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

const memoryPageSize = uint64(4096)

type pageRequest struct {
	sourceRef, sourceDir, manifestConfig string
	gpa, start, length                   uint64
}

type pageBadPoint struct {
	Offset uint64 `json:"offset"` // Offset within the 4 KiB physical page.
	Actual byte   `json:"actual"`
}

type pageLayer struct {
	Identity string `json:"identity"`
	Kind     string `json:"kind"`
}

type pageReport struct {
	SourceIdentity string         `json:"source_identity"`
	GPA            uint64         `json:"gpa"`
	MemoryOffset   uint64         `json:"memory_offset"` // Page start in the RAM memfd.
	Start          uint64         `json:"start"`
	Length         uint64         `json:"length"`
	SHA256         string         `json:"sha256"`
	NonFF          uint64         `json:"non_ff"`
	BadPoints      []pageBadPoint `json:"bad_points"`
	Layers         []pageLayer    `json:"layers"`
}

// page inspects a saved source after a failure; its GPA may come from a later
// restored guest. Page migration means this is evidence at that sampled GPA,
// not proof that the guest allocation occupied the same GPA at capture time.
func runPage(args []string, output io.Writer) error {
	var req pageRequest
	flags := flag.NewFlagSet("memorycompare page", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&req.sourceRef, "source-ref", "", "local Snapshot ref with content identity")
	flags.StringVar(&req.sourceDir, "source-dir", "", "checkpoint directory")
	flags.StringVar(&req.manifestConfig, "manifest-config", "", "actual artifact access configuration")
	flags.Uint64Var(&req.gpa, "gpa", 0, "4 KiB aligned guest physical address")
	flags.Uint64Var(&req.start, "start", 0, "first byte within the physical page")
	flags.Uint64Var(&req.length, "length", 0, "number of bytes within the physical page")
	if err := flags.Parse(args); err != nil {
		return err
	}
	provided := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	for _, name := range []string{"source-ref", "source-dir", "manifest-config", "gpa", "start", "length"} {
		if !provided[name] {
			return fmt.Errorf("memorycompare page: --%s is required", name)
		}
	}
	if flags.NArg() != 0 || req.sourceRef == "" || req.sourceDir == "" || req.manifestConfig == "" {
		return errors.New("memorycompare page: unexpected arguments or empty required path/ref")
	}
	if req.gpa%memoryPageSize != 0 || req.gpa > ^uint64(0)-memoryPageSize ||
		req.start >= memoryPageSize || req.length == 0 || req.length > memoryPageSize-req.start {
		return errors.New("memorycompare page: GPA must be page aligned and 0 < length <= 4096-start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	report, err := inspectPage(ctx, req)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}

// This borrowed fetcher makes the local-only restriction explicit even if a
// file changes format between the format check and ProcessStorage.OpenFile.
type noPageRemote struct{}

func (noPageRemote) OpenManifest(context.Context, store.ContentKey) (fetch.Stream, error) {
	return nil, errors.New("memorycompare page: remote Manifest access is unsupported")
}

func inspectPage(ctx context.Context, req pageRequest) (_ *pageReport, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(req.sourceDir)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("memorycompare page: source-dir is not a directory")
	}
	cfg, err := config.LoadManifestConfig(req.manifestConfig)
	if err != nil {
		return nil, err
	}
	storage, err := artifact.NewProcessStorage(cfg)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, storage.Close()) }()
	local := storage.BorrowWithFetcher(noPageRemote{})
	var roots []*snapshotfile.Root
	defer func() {
		for _, root := range roots {
			retErr = errors.Join(retErr, root.Close())
		}
	}()

	refs := []string{req.sourceRef}
	var layers []fetch.Stream
	report := &pageReport{GPA: req.gpa, Start: req.start, Length: req.length, BadPoints: []pageBadPoint{}}
	var memorySize uint64
	for i := 0; i < len(refs); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ref, path, err := localPageRef(dir, refs[i])
		if err != nil {
			return nil, fmt.Errorf("memorycompare page: layer %d: %w", i, err)
		}
		opened, err := local.OpenFile(ctx, path, ref)
		if err != nil {
			return nil, fmt.Errorf("memorycompare page: layer %d: %w", i, err)
		}
		if opened.Format() != artifact.FileFormatTarstream {
			return nil, errors.Join(errors.New("memorycompare page: Manifest Bundles are unsupported"), opened.Close())
		}
		scheme, digest := opened.Digest()
		identity := scheme + ":" + digest
		root, err := snapshotfile.Open(ctx, opened)
		if err != nil {
			return nil, fmt.Errorf("memorycompare page: layer %d Snapshot: %w", i, err)
		}
		roots = append(roots, root)
		snapshotCfg, err := snapshot.ParseConfig(root.SnapshotConfig)
		if err != nil {
			return nil, err
		}
		offset, err := pageMemfdOffset(root.StateJSON, root.Memory.Size(), req.gpa)
		if err != nil {
			return nil, fmt.Errorf("memorycompare page: layer %d mapping: %w", i, err)
		}
		if i == 0 {
			// FromRefs is already the complete, ordered flat memory chain.
			refs = append(refs, snapshotCfg.FromRefs...)
			if len(refs) > 64 {
				return nil, errors.New("memorycompare page: more than 64 memory layers are unsupported")
			}
			report.SourceIdentity, report.MemoryOffset = identity, offset
			memorySize = root.Memory.Size()
		} else if root.Memory.Size() != memorySize || offset != report.MemoryOffset {
			return nil, errors.New("memorycompare page: inconsistent memory layout across layers")
		}
		kind, err := pageKind(root.Memory, offset)
		if err != nil {
			return nil, err
		}
		report.Layers = append(report.Layers, pageLayer{Identity: identity, Kind: kind})
		layers = append(layers, root.Memory)
	}
	buf := make([]byte, req.length)
	n, err := fetch.NewLayered(layers...).ReadAt(ctx, buf, report.MemoryOffset+req.start)
	if err != nil || n != len(buf) {
		return nil, errors.Join(fmt.Errorf("memorycompare page: incomplete read %d/%d", n, len(buf)), err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hash := sha256.Sum256(buf)
	report.SHA256 = hex.EncodeToString(hash[:])
	for i, value := range buf {
		if value != 0xff {
			report.NonFF++
			if len(report.BadPoints) < 8 {
				report.BadPoints = append(report.BadPoints, pageBadPoint{Offset: req.start + uint64(i), Actual: value})
			}
		}
	}
	return report, nil
}

func localPageRef(dir, raw string) (manifest.Ref, string, error) {
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		return manifest.Ref{}, "", err
	}
	if ref.Scheme != manifest.RefSchemeFile || ref.Location != "" ||
		(ref.DigestScheme != "digest" && ref.DigestScheme != "hmac") ||
		ref.Path == "." || ref.Path == ".." || filepath.Base(ref.Path) != ref.Path || strings.ContainsAny(ref.Path, `/\`) {
		return manifest.Ref{}, "", errors.New("unsupported source: require a local basename ref with digest/hmac identity and no location")
	}
	path, err := filepath.EvalSymlinks(filepath.Join(dir, ref.Path))
	if err != nil {
		return manifest.Ref{}, "", err
	}
	if filepath.Dir(path) != dir {
		return manifest.Ref{}, "", errors.New("unsupported source: artifact escapes checkpoint directory")
	}
	info, err := os.Stat(path)
	if err != nil {
		return manifest.Ref{}, "", err
	}
	if !info.Mode().IsRegular() {
		return manifest.Ref{}, "", errors.New("unsupported source: artifact is not a regular file")
	}
	format, err := artifact.DetectFileFormat(path)
	if err != nil {
		return manifest.Ref{}, "", err
	}
	if format != artifact.FileFormatTarstream {
		return manifest.Ref{}, "", errors.New("unsupported source: Manifest Bundles are not local tarstream chains")
	}
	return ref, path, nil
}

// Match the ordered single-memfd mapping contract used by restore, including
// the GPA gap below high RAM. A GPA is never treated as a memfd byte offset.
func pageMemfdOffset(stateJSON []byte, capacity, gpa uint64) (uint64, error) {
	var tree struct {
		Snapshots map[string]struct {
			SnapshotData *struct {
				State string `json:"state"`
			} `json:"snapshot_data"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(stateJSON, &tree); err != nil {
		return 0, err
	}
	mm := tree.Snapshots["memory-manager"].SnapshotData
	if mm == nil {
		return 0, errors.New("missing memory-manager state")
	}
	var state struct {
		Ranges *struct {
			Data []json.RawMessage `json:"data"`
		} `json:"memory_ranges"`
		Regions []struct {
			Slot      uint32  `json:"slot"`
			GPA       uint64  `json:"gpa"`
			Size      uint64  `json:"size"`
			Offset    *uint64 `json:"file_offset"`
			Zone      string  `json:"zone_id"`
			VirtioMem bool    `json:"virtio_mem"`
		} `json:"guest_ram_mappings"`
	}
	if err := json.Unmarshal([]byte(mm.State), &state); err != nil {
		return 0, err
	}
	if state.Ranges == nil || len(state.Ranges.Data) != 0 || len(state.Regions) == 0 {
		return 0, errors.New("unsupported non-external or empty RAM layout")
	}
	seen := make(map[uint32]bool)
	var nextOffset, gpaEnd, result uint64
	found := false
	for _, region := range state.Regions {
		if seen[region.Slot] || region.Zone != "ram0" || region.VirtioMem || region.Offset == nil ||
			*region.Offset != nextOffset || region.Size == 0 || (region.GPA|region.Size|nextOffset)%memoryPageSize != 0 ||
			region.GPA < gpaEnd || region.GPA > ^uint64(0)-region.Size ||
			nextOffset > capacity || region.Size > capacity-nextOffset {
			return 0, fmt.Errorf("invalid ordered memfd mapping at slot %d", region.Slot)
		}
		if gpa >= region.GPA && gpa-region.GPA < region.Size {
			result, found = nextOffset+gpa-region.GPA, true
		}
		seen[region.Slot] = true
		nextOffset += region.Size
		gpaEnd = region.GPA + region.Size
	}
	if nextOffset != capacity || !found {
		return 0, errors.New("incomplete RAM mapping or GPA outside RAM")
	}
	return result, nil
}

func pageKind(source sparse.Source, offset uint64) (string, error) {
	var kind sparse.RunKind
	for at := offset; at < offset+memoryPageSize; {
		run, err := source.RunAt(at, offset+memoryPageSize-at)
		if err != nil {
			return "", err
		}
		if run == nil || run.Offset() != at || run.End() <= at || run.End() > offset+memoryPageSize {
			return "", errors.New("memorycompare page: invalid sparse page run")
		}
		if at == offset {
			kind = run.Kind()
		} else if run.Kind() != kind {
			return "", errors.New("memorycompare page: mixed sparse states within one page are unsupported")
		}
		at = run.End()
	}
	switch kind {
	case sparse.Data:
		return "Data", nil
	case sparse.Zero:
		return "Zero", nil
	case sparse.Hole:
		return "Hole", nil
	default:
		return "", errors.New("memorycompare page: unknown sparse state")
	}
}
