// memorybaseline emits a same-tail legacy full-coverage artifact for E2E size
// accounting. The caller must prove M (or historical P) covers all RAM; this
// fixture rejects sparse historical inputs and never extrapolates coverage.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/internal/buildversion"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

type stream struct {
	sparse.Source
	f *os.File
}

func (s stream) Close() error { return s.f.Close() }
func open(ctx context.Context, path string) (*snapshotfile.Root, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	src, _, err := tarstream.SourceAt(f, st.Size(), "")
	if err != nil {
		return nil, err
	}
	return snapshotfile.Open(ctx, stream{src, f})
}
func run() error {
	if len(os.Args) != 5 && len(os.Args) != 6 {
		return fmt.Errorf("usage: memorybaseline dense|history NEW-S RAM.bin|OLD-W OUTPUT [REBUILT-H-REF]")
	}
	ctx := context.Background()
	root, err := open(ctx, os.Args[2])
	if err != nil {
		return err
	}
	defer root.Close()
	var memory sparse.Source
	switch os.Args[1] {
	case "dense":
		f, err := os.Open(os.Args[3])
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		memory, err = sparse.NewSource(f, uint64(st.Size()), nil)
		if err != nil {
			return err
		}
	case "history":
		parent, err := open(ctx, os.Args[3])
		if err != nil {
			return err
		}
		defer parent.Close()
		memory = parent.Memory
	default:
		return fmt.Errorf("invalid mode")
	}
	if memory.Size() != root.Memory.Size() {
		return fmt.Errorf("size mismatch")
	}
	for off := uint64(0); off < memory.Size(); {
		r, err := memory.RunAt(off, memory.Size()-off)
		if err != nil {
			return err
		}
		if r.Kind() != sparse.Data {
			return fmt.Errorf("baseline proof requires full Data coverage")
		}
		off = r.End()
	}
	cfg := root.SnapshotConfig
	if len(os.Args) == 6 {
		parsed, err := snapshot.ParseConfig(cfg)
		if err != nil {
			return err
		}
		if len(parsed.FromRefs) != 1 {
			return fmt.Errorf("baseline replacement requires exactly one local history")
		}
		parsed.FromRefs[0] = os.Args[5]
		cfg, err = snapshot.MarshalConfig(parsed)
		if err != nil {
			return err
		}
	}
	source, err := snapshotfile.BuildSource(memory, root.ConfigJSON, root.StateJSON, cfg)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(os.Args[4], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	scheme, digest, err := tarstream.WriteTo(ctx, out, "snapshot", source)
	if err != nil {
		return err
	}
	st, err := out.Stat()
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"identity": scheme + ":" + digest, "Data": memory.Size(), "Bitmap": 0, "ZIP": source.Size() - memory.Size(), "Physical": st.Size(), "tail_source": os.Args[2], "bytes_source": os.Args[3], "mode": os.Args[1]})
}
func main() {
	buildversion.ExitIfRequested()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
