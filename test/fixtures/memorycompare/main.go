// memorycompare verifies complete logical RAM against a paused CH observation.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/buildversion"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

type localStream struct {
	sparse.Source
	file *os.File
}

func (s *localStream) Close() error { return s.file.Close() }

type layerStat struct {
	Path                    string
	Identity                string
	Physical                int64
	Data, Zero, Bitmap, ZIP uint64
	DirtyPresent            bool
}

func run() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("usage: memorycompare SNAPSHOT RAM.bin")
	}
	ctx := context.Background()
	topPath, err := filepath.Abs(os.Args[1])
	if err != nil {
		return err
	}
	raw, err := os.Open(os.Args[2])
	if err != nil {
		return err
	}
	defer raw.Close()
	var layers []fetch.Stream
	var stats []layerStat
	seen := map[string]bool{}
	var reachable int64
	refs := []string{topPath}
	for i := 0; i < len(refs); i++ {
		path := refs[i]
		var expected *manifest.Ref
		if i != 0 {
			r, err := manifest.ParseRef(path)
			if err != nil {
				return err
			}
			if r.Scheme != manifest.RefSchemeFile || r.Location != "" {
				return fmt.Errorf("comparison fixture expects local FileSink chain: %s", path)
			}
			expected = &r
			path = r.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(topPath), path)
			}
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil {
			return err
		}
		var opts []tarstream.ReadOption
		if expected != nil {
			opts = append(opts, tarstream.WithExpectedDigest(expected.DigestScheme, expected.Digest))
		}
		source, _, err := tarstream.SourceAt(file, info.Size(), "", opts...)
		if err != nil {
			return err
		}
		root, err := snapshotfile.Open(ctx, &localStream{Source: source, file: file})
		if err != nil {
			return err
		}
		defer root.Close()
		if i == 0 {
			cfg, err := snapshot.ParseConfig(root.SnapshotConfig)
			if err != nil {
				return err
			}
			refs = append(refs, cfg.FromRefs...)
		}
		identity, ok := source.(interface{ Digest() (string, string) })
		if !ok {
			return fmt.Errorf("carrier has no content identity")
		}
		scheme, digest := identity.Digest()
		id := scheme + ":" + digest
		stat := layerStat{Path: path, Identity: id, Physical: info.Size(), Bitmap: uint64(len(root.MemoryDirty)), ZIP: source.Size() - root.Memory.Size(), DirtyPresent: root.MemoryDirty != nil}
		for off := uint64(0); off < root.Memory.Size(); {
			r, err := root.Memory.RunAt(off, root.Memory.Size()-off)
			if err != nil {
				return err
			}
			switch r.Kind() {
			case sparse.Data:
				stat.Data += r.End() - off
			case sparse.Zero:
				stat.Zero += r.End() - off
			}
			off = r.End()
		}
		if !seen[id] {
			reachable += stat.Physical
			seen[id] = true
		}
		stats = append(stats, stat)
		layers = append(layers, root.Memory)
	}
	full := fetch.NewLayered(layers...)
	info, err := raw.Stat()
	if err != nil {
		return err
	}
	if uint64(info.Size()) != full.Size() {
		return fmt.Errorf("logical size mismatch %d/%d", info.Size(), full.Size())
	}
	got, want := make([]byte, 1<<20), make([]byte, 1<<20)
	hash := sha256.New()
	for off := uint64(0); off < full.Size(); {
		n := min(uint64(len(got)), full.Size()-off)
		if _, err := io.ReadFull(raw, want[:n]); err != nil {
			return err
		}
		if _, err := full.ReadAt(ctx, got[:n], off); err != nil {
			return err
		}
		if !bytes.Equal(got[:n], want[:n]) {
			for i := uint64(0); i < n; i++ {
				if got[i] != want[i] {
					return fmt.Errorf("RAM differs at offset 0x%x: artifact=%02x CH=%02x", off+i, got[i], want[i])
				}
			}
		}
		hash.Write(got[:n])
		off += n
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"logical_bytes": full.Size(), "sha256": hex.EncodeToString(hash.Sum(nil)), "layers": stats, "identity_deduplicated_local_bytes": reachable, "complete_comparison": "equal"})
}
func main() {
	buildversion.ExitIfRequested()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
