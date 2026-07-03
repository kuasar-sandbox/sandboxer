package restore

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/util"
	"gopkg.in/yaml.v3"
)

// UploadLocal promotes a LOCAL snapshot (the <sid>.snapshot tarstream bundle
// plus every local artifact its snapshot.cfg references) to a fully-REMOTE
// manifest:// snapshot WITHOUT booting a sandbox. It:
//
//  1. parses the bundle's snapshot.cfg + its lower chain;
//  2. validates every LOWER layer (from_refs / base_from_refs) is remote,
//     present, and sealed under the current MANIFEST_KEY — manifest blob
//     only, NO chunk download (Config.CheckManifest). A lower file:// layer
//     is rejected: the only local layers permitted are the TOP artifacts
//     (the local-layer invariant, docs §3.5);
//  3. AUTO-UPLOADS every local file:// ARTIFACT the cfg references — the
//     root/disk base images (base_ref, digest-verified) and the captured
//     top overlays — re-rendering their refs to manifest://. runtime_ref is
//     deliberately left alone: it is a node boot artifact pinned by digest
//     and distributed with the platform, not a tenant artifact;
//  4. ingests the memory bundle with the re-rendered snapshot.cfg and
//     returns the uploaded snapshot's identity = manifest://<memKey>.
//
// Local artifacts resolve as siblings of the bundle (file:// refs carry
// basenames); a base image's @sha256 digest is verified against the file
// before upload. Ingest is content-addressed, so re-uploading shared
// artifacts (the same base image across templates) dedups to the same key.
//
// The result is restorable via `sandbox-ctl run --restore=manifest://<memKey>`.
func UploadLocal(ctx context.Context, snapshotPath string, mcfg *manifest.Config, logf func(string, ...any)) (string, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	bundle, err := fetch.OpenTarStream(snapshotPath)
	if err != nil {
		return "", fmt.Errorf("open snapshot: %w", err)
	}
	defer bundle.Close()
	bundleSize := int64(bundle.Size())
	bundleDir := filepath.Dir(snapshotPath)

	// 1. Trailing-ZIP members (config.json / state.json / snapshot.cfg).
	zr, err := zip.NewReader(fetch.NewReaderAt(ctx, bundle, bundleSize), bundleSize)
	if err != nil {
		return "", fmt.Errorf("read snapshot ZIP trailer: %w", err)
	}
	ent := map[string][]byte{}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			return "", fmt.Errorf("zip open %s: %w", zf.Name, err)
		}
		b, rerr := io.ReadAll(rc)
		rc.Close()
		if rerr != nil {
			return "", fmt.Errorf("zip read %s: %w", zf.Name, rerr)
		}
		ent[zf.Name] = b
	}
	for _, w := range []string{"config.json", "state.json", "snapshot.cfg"} {
		if _, ok := ent[w]; !ok {
			return "", fmt.Errorf("snapshot bundle missing %s (produced by old sandbox-ctl?)", w)
		}
	}
	parsed, err := ParseSnapshotCfg(ent["snapshot.cfg"])
	if err != nil {
		return "", err
	}

	// 2. Validate every LOWER layer (memory + disk chains): remote, present,
	//    key-consistent. Reject buried local layers (invariant).
	lower := append([]string{}, parsed.FromRefs...)
	lower = append(lower, parsed.Boot.Root.BaseFromRefs...)
	if parsed.Boot.Root.Overlay != nil {
		lower = append(lower, parsed.Boot.Root.Overlay.BaseFromRefs...)
	}
	for i := range parsed.Boot.Disks {
		n := &parsed.Boot.Disks[i]
		lower = append(lower, n.BaseFromRefs...)
		if n.Overlay != nil {
			lower = append(lower, n.Overlay.BaseFromRefs...)
		}
	}
	for _, ref := range lower {
		sc, val, ok := config.SchemeAndPath(ref)
		if !ok {
			return "", fmt.Errorf("upload-snapshot: malformed lower ref %q", ref)
		}
		switch sc {
		case "file":
			return "", fmt.Errorf("upload-snapshot: lower layer %q is a local file:// — re-export locally to flatten it first (local-layer invariant, docs §3.5)", ref)
		case "manifest":
			for _, part := range strings.Split(val, ":") { // manifest://k1:k2 → per-key check
				key, perr := parseManifestKey(part)
				if perr != nil {
					return "", fmt.Errorf("upload-snapshot: lower ref %q: %w", ref, perr)
				}
				if cerr := mcfg.CheckManifest(ctx, key); cerr != nil {
					return "", fmt.Errorf("upload-snapshot: lower layer %q: %w", ref, cerr)
				}
			}
		default:
			return "", fmt.Errorf("upload-snapshot: lower ref %q: unknown scheme %q", ref, sc)
		}
	}
	logf("upload-snapshot: %d lower layer(s) verified present + key-consistent", len(lower))

	// 3. Auto-upload the local artifacts the cfg references, rewriting refs.
	ing, err := mcfg.NewIngester(mcfg.IngestKeyFunc(), nil)
	if err != nil {
		return "", fmt.Errorf("ingester: %w", err)
	}
	defer ing.Close()
	up := &artifactUploader{ctx: ctx, ing: ing, dir: bundleDir, logf: logf}

	if parsed.Boot.Root.BaseRef, err = up.uploadRef("root base", parsed.Boot.Root.BaseRef, true); err != nil {
		return "", err
	}
	if parsed.Boot.Root.Base, err = up.uploadRef("root top layer", parsed.Boot.Root.Base, false); err != nil {
		return "", err
	}
	if parsed.Boot.Root.Overlay != nil {
		if parsed.Boot.Root.Overlay.Base, err = up.uploadRef("root overlay", parsed.Boot.Root.Overlay.Base, false); err != nil {
			return "", err
		}
	}
	for i := range parsed.Boot.Disks {
		n := &parsed.Boot.Disks[i]
		lbl := fmt.Sprintf("disk %d", i)
		if n.BaseRef, err = up.uploadRef(lbl+" base", n.BaseRef, true); err != nil {
			return "", err
		}
		if n.Base, err = up.uploadRef(lbl+" top layer", n.Base, false); err != nil {
			return "", err
		}
		if n.Overlay != nil {
			if n.Overlay.Base, err = up.uploadRef(lbl+" overlay", n.Overlay.Base, false); err != nil {
				return "", err
			}
		}
	}

	// 4. Re-render snapshot.cfg, rebuild the ZIP, ingest [memory][new ZIP].
	newCfg, err := yaml.Marshal(&parsed)
	if err != nil {
		return "", fmt.Errorf("render snapshot.cfg: %w", err)
	}
	zipBytes, err := snapshot.BuildZIP(map[string][]byte{
		"config.json":  ent["config.json"],
		"state.json":   ent["state.json"],
		"snapshot.cfg": newCfg,
	})
	if err != nil {
		return "", fmt.Errorf("build zip: %w", err)
	}

	capBytes, err := util.ParseSize(parsed.Resources.Capacity.Memory)
	if err != nil {
		return "", fmt.Errorf("capacity.memory: %w", err)
	}
	if int64(capBytes) > bundleSize {
		return "", fmt.Errorf("snapshot %s too small (%d) for its memory section (%d)", snapshotPath, bundleSize, capBytes)
	}
	src := &memZipSource{bundle: bundle, memSize: capBytes, zip: zipBytes}
	res, err := ing.Ingest(ctx, src, ingest.IngestOption{OnProgress: up.progress("memory section")})
	if err != nil {
		return "", fmt.Errorf("ingest bundle: %w", err)
	}
	logf("upload-snapshot: memory stored=%d dedup=%d zero=%d", res.StoredChunks, res.DedupChunks, res.ZeroChunks)
	// The documented identity is the full ref — callers (orchestrator
	// promote, scripts) branch on the manifest:// scheme to tell remote
	// from local, so a bare key here reads as a local path downstream.
	return "manifest://" + snapshot.HexKey(res.ManifestKey), nil
}

// artifactUploader ingests local file:// artifacts referenced by a
// snapshot.cfg and rewrites their refs to manifest://.
type artifactUploader struct {
	ctx  context.Context
	ing  ingest.Ingester
	dir  string // bundle dir: file:// refs resolve as siblings
	logf func(string, ...any)
}

// uploadRef resolves ref: manifest:// (or empty) passes through; file://
// uploads the local artifact and returns its manifest:// ref. digested
// selects the base_ref form (file://<name>@sha256:<hex>, digest verified
// against the file) vs the plain captured-layer form (file://<basename>).
func (u *artifactUploader) uploadRef(label, ref string, digested bool) (string, error) {
	if ref == "" {
		return ref, nil
	}
	sc, _, ok := config.SchemeAndPath(ref)
	if !ok {
		return "", fmt.Errorf("upload-snapshot: %s: malformed ref %q", label, ref)
	}
	if sc != "file" {
		return ref, nil // already remote
	}

	name := strings.TrimPrefix(ref, "file://")
	wantDigest := ""
	if digested {
		r, err := ParseRef(ref)
		if err != nil {
			return "", fmt.Errorf("upload-snapshot: %s: %w", label, err)
		}
		name, wantDigest = r.Basename, r.Digest
	}
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(u.dir, name)
	}
	if wantDigest != "" {
		got, err := fileSHA256(path)
		if err != nil {
			return "", fmt.Errorf("upload-snapshot: %s: hash %s: %w", label, path, err)
		}
		if got != wantDigest {
			return "", fmt.Errorf("upload-snapshot: %s: %s digest %s does not match ref %s", label, path, got, wantDigest)
		}
	}

	st, err := fetch.OpenTarStream(path)
	if err != nil {
		return "", fmt.Errorf("upload-snapshot: %s: %w", label, err)
	}
	defer st.Close()
	res, err := u.ing.Ingest(u.ctx, st, ingest.IngestOption{OnProgress: u.progress(label)})
	if err != nil {
		return "", fmt.Errorf("upload-snapshot: ingest %s: %w", label, err)
	}
	key := snapshot.HexKey(res.ManifestKey)
	u.logf("upload-snapshot: %s %s → manifest://%s (stored=%d dedup=%d)",
		label, name, key, res.StoredChunks, res.DedupChunks)
	return "manifest://" + key, nil
}

// progress returns a throttled OnProgress logger.
func (u *artifactUploader) progress(label string) func(processed, total uint64) {
	const mib = 1 << 20
	last := time.Now()
	return func(processed, total uint64) {
		if time.Since(last) < 2*time.Second {
			return
		}
		last = time.Now()
		u.logf("upload: %s %d/%d MiB", label, processed/mib, total/mib)
	}
}

// memZipSource composes [0,memSize) of the bundle entry followed by the
// re-rendered ZIP trailer as one sparse.Source — the same [memory][ZIP]
// shape the live snapshot path ingests.
type memZipSource struct {
	bundle  sparse.Source
	memSize uint64
	zip     []byte
}

func (s *memZipSource) Size() uint64 { return s.memSize + uint64(len(s.zip)) }

func (s *memZipSource) RunAt(off, limit uint64) (sparse.RunKind, uint64, error) {
	total := s.Size()
	if off >= total {
		return 0, 0, io.EOF
	}
	if off < s.memSize {
		if limit > s.memSize-off {
			limit = s.memSize - off
		}
		return s.bundle.RunAt(off, limit)
	}
	end := off + limit
	if end > total {
		end = total
	}
	return sparse.Data, end, nil
}

func (s *memZipSource) ReadAt(ctx context.Context, buf []byte, off uint64) (int, error) {
	total := s.Size()
	if off >= total {
		return 0, io.EOF
	}
	n := len(buf)
	var eof error
	if off+uint64(n) > total {
		n = int(total - off)
		eof = io.EOF
	}
	p := buf[:n]
	done := 0
	if off < s.memSize {
		part := n
		if rest := s.memSize - off; uint64(part) > rest {
			part = int(rest)
		}
		if _, err := s.bundle.ReadAt(ctx, p[:part], off); err != nil && err != io.EOF {
			return 0, err
		}
		done += part
		off += uint64(part)
	}
	if done < n {
		copy(p[done:], s.zip[off-s.memSize:])
		done = n
	}
	return n, eof
}

// fileSHA256 hashes a file's bytes (the artifact content address).
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parseManifestKey decodes a 64-hex manifest key into a store.ContentKey.
func parseManifestKey(hexKey string) (store.ContentKey, error) {
	var k store.ContentKey
	b, err := hex.DecodeString(hexKey)
	if err != nil {
		return k, fmt.Errorf("manifest key %q: %w", hexKey, err)
	}
	if len(b) != len(k) {
		return k, fmt.Errorf("manifest key %q: want %d bytes, got %d", hexKey, len(k), len(b))
	}
	copy(k[:], b)
	return k, nil
}
