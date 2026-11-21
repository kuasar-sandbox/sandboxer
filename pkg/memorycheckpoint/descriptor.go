// Package memorycheckpoint owns the process-local backing of a checkpoint's
// frozen memory. Descriptors are immutable; capture records describe the
// independently progressing runtime retirement and backing publication.
package memorycheckpoint

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
)

const Version = 1

// RecordVersion changes only the local lifecycle protocol. The immutable
// descriptor and data encodings keep Version. Old readers reject these records
// rather than silently ignoring a revoked checkpoint.
const RecordVersion = 2
const pageSize = 4096

type Descriptor struct {
	Version      int      `json:"version"`
	MemoryID     string   `json:"memory_id"`
	CheckpointID string   `json:"checkpoint_id"`
	Size         uint64   `json:"size"`
	MapRef       string   `json:"map_ref"`
	Parents      []string `json:"parents,omitempty"`
	WorkingSet   bool     `json:"working_set,omitempty"`
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// Checkpoint IDs are logical names shared with disk checkpoints, not paths.
// Preserve sandboxfile.ValidateCheckpointID's existing UTF-8 name contract.
func validCheckpointID(id string) bool {
	return len(id) > 0 && len(id) <= 128 && utf8.ValidString(id) && strings.IndexFunc(id, unicode.IsControl) < 0
}

func (d Descriptor) Validate() error {
	if d.Version != Version || !validID(d.MemoryID) || !validCheckpointID(d.CheckpointID) {
		return errors.New("memory checkpoint: invalid descriptor identity or version")
	}
	if d.Size == 0 || d.Size%pageSize != 0 || d.Size > 1<<40 {
		return errors.New("memory checkpoint: invalid memory geometry")
	}
	checkRef := func(raw string) (manifest.Ref, error) {
		ref, err := manifest.ParseRef(raw)
		if err != nil || ref.Scheme != manifest.RefSchemeFile || ref.DigestScheme == "" || ref.Digest == "" {
			return manifest.Ref{}, errors.New("memory checkpoint: content-identified local file ref required")
		}
		return ref, nil
	}
	mapRef, err := checkRef(d.MapRef)
	if err != nil {
		return err
	}
	if mapRef.Location != "" || filepath.Base(mapRef.Path) != mapRef.Path || strings.ContainsAny(mapRef.Path, `/\\`) {
		return errors.New("memory checkpoint: map ref must be a local basename")
	}
	// The previous root plus its existing maximum 64-entry memory suffix.
	if len(d.Parents) > 65 {
		return errors.New("memory checkpoint: too many direct parents")
	}
	seen := make(map[string]bool, len(d.Parents))
	for _, p := range d.Parents {
		ref, err := checkRef(p)
		if err != nil {
			return err
		}
		if seen[ref.String()] {
			return errors.New("memory checkpoint: duplicate parent ref")
		}
		seen[ref.String()] = true
	}
	return nil
}

// Map encoding is independent of the bounded snapshot ZIP. The fixed header
// carries the geometry; the three dense bitmaps retain exactly CH's M/D/R.
const mapHeader = 32

var mapMagic = [8]byte{'K', 'S', 'M', 'E', 'M', 'A', 'P', 1}

func mapBytes(size uint64) (int, error) {
	if size == 0 || size%pageSize != 0 || size > math.MaxInt64 {
		return 0, errors.New("memory checkpoint: invalid map geometry")
	}
	n := (size/pageSize + 7) / 8
	if n > uint64((math.MaxInt-mapHeader)/3) {
		return 0, errors.New("memory checkpoint: oversized map")
	}
	return int(n), nil
}

func validateMap(v *memory.CaptureView) error {
	if v == nil {
		return errors.New("memory checkpoint: nil map")
	}
	n, err := mapBytes(v.Size)
	if err != nil {
		return err
	}
	for _, b := range [][]byte{v.Materialized, v.Dirty, v.Released} {
		if len(b) != n {
			return fmt.Errorf("memory checkpoint: map length %d, expected %d", len(b), n)
		}
		if rem := v.Size / pageSize % 8; rem != 0 && b[n-1] & ^byte((1<<rem)-1) != 0 {
			return errors.New("memory checkpoint: map has bits outside memory")
		}
	}
	return nil
}

func EncodeMap(v *memory.CaptureView) ([]byte, error) {
	if err := validateMap(v); err != nil {
		return nil, err
	}
	n := len(v.Materialized)
	b := make([]byte, mapHeader+3*n)
	copy(b, mapMagic[:])
	binary.LittleEndian.PutUint64(b[8:16], v.Size)
	binary.LittleEndian.PutUint64(b[16:24], uint64(n))
	if v.WorkingSetOnly {
		b[24] = 1
	}
	copy(b[mapHeader:], v.Materialized)
	copy(b[mapHeader+n:], v.Dirty)
	copy(b[mapHeader+2*n:], v.Released)
	return b, nil
}

func DecodeMap(b []byte, expectedSize uint64) (*memory.CaptureView, error) {
	if len(b) < mapHeader || string(b[:8]) != string(mapMagic[:]) {
		return nil, errors.New("memory checkpoint: invalid map header")
	}
	size := binary.LittleEndian.Uint64(b[8:16])
	n, err := mapBytes(size)
	if err != nil {
		return nil, err
	}
	if size != expectedSize || binary.LittleEndian.Uint64(b[16:24]) != uint64(n) || len(b) != mapHeader+3*n || b[24] > 1 {
		return nil, errors.New("memory checkpoint: map geometry mismatch")
	}
	for _, c := range b[25:mapHeader] {
		if c != 0 {
			return nil, errors.New("memory checkpoint: invalid map flags")
		}
	}
	v := &memory.CaptureView{Size: size, WorkingSetOnly: b[24] != 0, Materialized: append([]byte(nil), b[mapHeader:mapHeader+n]...), Dirty: append([]byte(nil), b[mapHeader+n:mapHeader+2*n]...), Released: append([]byte(nil), b[mapHeader+2*n:]...)}
	if err := validateMap(v); err != nil {
		return nil, err
	}
	return v, nil
}
