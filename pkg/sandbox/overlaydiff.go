package sandbox

import (
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// DefaultBaseRoot is the on-disk root for per-sandbox persistent state (the
// overlay diff). Distinct from the tmpfs run-dir (sockets / snap staging).
// Overridable via --base-root / SANDBOX_BASE_ROOT.
const DefaultBaseRoot = "/var/lib/sandbox"

// DefaultBaseDir returns the per-sandbox base directory under baseRoot.
func DefaultBaseDir(baseRoot, sandboxID string) string {
	return filepath.Join(baseRoot, sandboxID)
}

// DefaultDiffURI returns the auto-default overlay diff URI (on disk, under the
// base dir). Used when boot.root.overlay.diff is empty.
func DefaultDiffURI(baseRoot, sandboxID string) string {
	return "file://" + filepath.Join(DefaultBaseDir(baseRoot, sandboxID), sandboxID+".overlay.diff")
}

// PrepareDiff provisions the overlay diff before OpenBlockCOW and returns the
// createSize to pass it. It never truncates an existing diff. For a fresh
// (absent/empty) diff it enforces the cold-boot ext4-source rule:
//
//   - existing non-empty diff → kept as-is (size = its on-disk size)
//   - else diff_template set  → sparse-copy the template (its size wins)
//   - else base present       → blank diff sized to the base (vdb = base ext4)
//   - else                    → error (a blank diff is not a mountable ext4)
//
// baseSize is 0 when there is no overlay.base; templateURI is "" when unset.
func PrepareDiff(diffPath, templateURI string, baseSize, diffSize int64) (createSize int64, err error) {
	if st, statErr := os.Stat(diffPath); statErr == nil && st.Size() > 0 {
		return st.Size(), nil // existing (pre-formatted / prior run) — use as-is
	}
	switch {
	case templateURI != "":
		_, tpl, ok := config.SchemeAndPath(templateURI)
		if !ok {
			return 0, fmt.Errorf("diff_template invalid URI: %s", templateURI)
		}
		if err := sparseCopyFile(tpl, diffPath); err != nil {
			return 0, fmt.Errorf("seed diff from template %s: %w", tpl, err)
		}
		st, err := os.Stat(diffPath)
		if err != nil {
			return 0, err
		}
		return st.Size(), nil
	case baseSize > 0:
		// Blank diff over a formatted base: vdb content = base, mountable.
		return baseSize, nil
	default:
		return 0, fmt.Errorf("no ext4 source for a fresh overlay diff: set boot.root.overlay.diff_template or boot.root.overlay.base, or pre-format the diff at %s", diffPath)
	}
}

// sparseCopyFile copies src to dst preserving holes (SEEK_DATA/SEEK_HOLE), so
// a sparse ext4 template doesn't expand to its full logical size on disk.
func sparseCopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	size := st.Size()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := out.Truncate(size); err != nil { // set logical size; holes stay holes
		return err
	}

	fd := int(in.Fd())
	buf := make([]byte, 1<<20)
	var off int64
	for off < size {
		dataOff, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if err != nil { // ENXIO: no more data to EOF
			break
		}
		holeOff, err := unix.Seek(fd, dataOff, unix.SEEK_HOLE)
		if err != nil {
			holeOff = size
		}
		if _, err := in.Seek(dataOff, io.SeekStart); err != nil {
			return err
		}
		if _, err := out.Seek(dataOff, io.SeekStart); err != nil {
			return err
		}
		remaining := holeOff - dataOff
		for remaining > 0 {
			n := int64(len(buf))
			if n > remaining {
				n = remaining
			}
			r, err := io.ReadFull(in, buf[:n])
			if r > 0 {
				if _, werr := out.Write(buf[:r]); werr != nil {
					return werr
				}
				remaining -= int64(r)
			}
			if err != nil {
				return err
			}
		}
		off = holeOff
	}
	return out.Sync()
}
