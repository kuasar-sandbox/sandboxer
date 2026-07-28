// Package runtimebundle inspects the virtio-pmem runtime artifact without
// reading its EROFS prefix. The bundle format is raw EROFS + padding followed
// by one trailing ZIP entry whose empty filename carries the SHA256 identity.
package runtimebundle

import (
	"archive/zip"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

const pmemAlignment = int64(2 << 20)

// Info is the metadata needed by lifecycle and restore paths.
type Info struct {
	Size   int64
	Digest string // "sha256:<64-lowercase-hex>"
}

// Inspect validates the bundle envelope and reads its declared identity. It
// reads only ZIP metadata at EOF and the empty marker; it never scans EROFS.
func Inspect(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	if !st.Mode().IsRegular() {
		return Info{}, fmt.Errorf("runtime bundle %s is not a regular file", path)
	}
	if st.Size() == 0 || st.Size()%pmemAlignment != 0 {
		return Info{}, fmt.Errorf("runtime bundle %s size %d is not 2 MiB aligned", path, st.Size())
	}
	var eocd [22]byte
	if _, err := f.ReadAt(eocd[:], st.Size()-int64(len(eocd))); err != nil {
		return Info{}, fmt.Errorf("runtime bundle %s: read ZIP footer: %w", path, err)
	}
	if binary.LittleEndian.Uint32(eocd[:4]) != 0x06054b50 || binary.LittleEndian.Uint16(eocd[20:]) != 0 {
		return Info{}, fmt.Errorf("runtime bundle %s: ZIP end record is not at EOF", path)
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		return Info{}, fmt.Errorf("runtime bundle %s: read digest ZIP: %w", path, err)
	}
	if len(zr.File) != 1 {
		return Info{}, fmt.Errorf("runtime bundle %s: digest ZIP has %d entries, want 1", path, len(zr.File))
	}
	zf := zr.File[0]
	if zf.Method != zip.Store || zf.CompressedSize64 != 0 || zf.UncompressedSize64 != 0 {
		return Info{}, fmt.Errorf("runtime bundle %s: digest marker %q is not empty", path, zf.Name)
	}
	hexDigest := strings.TrimPrefix(zf.Name, tarstream.SHA256MarkerPrefix)
	if hexDigest == zf.Name || len(hexDigest) != 64 || strings.ToLower(hexDigest) != hexDigest {
		return Info{}, fmt.Errorf("runtime bundle %s: invalid digest marker %q", path, zf.Name)
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return Info{}, fmt.Errorf("runtime bundle %s: invalid digest marker %q", path, zf.Name)
	}
	r, err := zf.Open()
	if err != nil {
		return Info{}, fmt.Errorf("runtime bundle %s: open digest marker: %w", path, err)
	}
	n, copyErr := io.Copy(io.Discard, r)
	closeErr := r.Close()
	if copyErr != nil {
		return Info{}, fmt.Errorf("runtime bundle %s: verify digest marker: %w", path, copyErr)
	}
	if closeErr != nil {
		return Info{}, fmt.Errorf("runtime bundle %s: close digest marker: %w", path, closeErr)
	}
	if n != 0 {
		return Info{}, fmt.Errorf("runtime bundle %s: digest marker %q is not empty", path, zf.Name)
	}
	return Info{Size: st.Size(), Digest: "sha256:" + hexDigest}, nil
}
