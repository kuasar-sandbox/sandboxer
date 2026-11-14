package diffstore

import (
	"context"
	"crypto/aes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/sectorcrypt"
	"golang.org/x/crypto/xts"
	"golang.org/x/sys/unix"
)

// templateImage is an immutable provisioning input, not a writable backend.
// Raw images and the existing encrypted template envelope keep their original
// sparse-file semantics. No work-file upgrade or mutation occurs on this path.
type templateImage struct {
	f          *os.File
	size, body int64
	xts        *xts.Cipher
}

// OpenProvisioningTemplate opens the same immutable sparse input used when
// creating a private writer. The returned view preserves holes and explicit
// zeros and must be closed by the caller.
func OpenProvisioningTemplate(path string, opts ...DiskOption) (ExportView, error) {
	options, err := parseDiskOptions(opts)
	if err != nil {
		return nil, err
	}
	return openProvisioningTemplate(path, options.encryption)
}

func openProvisioningTemplate(path string, encryption *diffEncryption) (diffTemplateSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (diffTemplateSource, error) { _ = f.Close(); return nil, err }
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return fail(fmt.Errorf("diffstore: template must be a nonempty regular file"))
	}
	var prefix [diffPrefixSize]byte
	if err := readFullAt(f, prefix[:], 0); err != nil {
		return fail(err)
	}
	image := &templateImage{f: f, size: info.Size()}
	if string(prefix[:8]) == string(diffMagic[:]) {
		switch binary.BigEndian.Uint16(prefix[8:10]) {
		case diffVersion:
			// A caller may explicitly provide a current single-layer file as
			// an immutable source. Its Zero state must survive materialization.
			diff, err := readSingleHeader(f, info.Size(), encryption, false)
			if err != nil {
				return fail(err)
			}
			if err := diff.enableDirect(); err != nil {
				return fail(err)
			}
			return &fileDiffTemplate{diff: diff}, nil
		case 1:
			if encryption == nil {
				return fail(ErrDiffEncryptionRequired)
			}
			if err := image.readEncryptedHeader(encryption); err != nil {
				return fail(err)
			}
		default:
			return fail(ErrFormat)
		}
	}
	if err := validateDiffLogicalSize(image.size); err != nil {
		return fail(err)
	}
	return image, nil
}

func (s *templateImage) readEncryptedHeader(encryption *diffEncryption) error {
	var region [diffHeaderRegionSize]byte
	defer clear(region[:])
	if err := readFullAt(s.f, region[:], 0); err != nil {
		return err
	}
	end := diffPrefixSize + diffHeaderSealedSize
	if binary.BigEndian.Uint16(region[10:12]) != diffPrefixSize || binary.BigEndian.Uint32(region[12:16]) != 0 || !allZero(region[end:]) {
		return fmt.Errorf("diffstore: invalid encrypted template header")
	}
	aad := append([]byte("kuasar/diff/header/v1\x00"), region[:diffPrefixSize]...)
	start := diffPrefixSize + diffHeaderNonceSize
	plain, err := encryption.templateHeader.Open(nil, region[diffPrefixSize:start], region[start:end], aad)
	if err != nil {
		return ErrDiffAuthentication
	}
	defer clear(plain)
	if len(plain) != diffHeaderPlainSize || binary.BigEndian.Uint64(plain[:8]) > uint64(MaxWritableSize) ||
		binary.BigEndian.Uint32(plain[8:12]) != cowBlockSize || binary.BigEndian.Uint32(plain[12:16]) != uint32(diffDataUnitSize) ||
		binary.BigEndian.Uint32(plain[16:20]) != uint32(diffHeaderRegionSize) || binary.BigEndian.Uint32(plain[20:24]) != diffXTSKeySize || !allZero(plain[88:]) {
		return fmt.Errorf("diffstore: invalid encrypted template layout")
	}
	s.size, s.body = int64(binary.BigEndian.Uint64(plain[:8])), diffHeaderRegionSize
	info, err := s.f.Stat()
	if err != nil {
		return err
	}
	if info.Size() != s.body+s.size {
		return fmt.Errorf("diffstore: encrypted template size does not match header")
	}
	s.xts, err = xts.NewCipher(aes.NewCipher, plain[24:88])
	return err
}

func (s *templateImage) Size() uint64 { return uint64(s.size) }
func (s *templateImage) Close() error { return s.f.Close() }

// Resolve only the requested sparse extent. Startup does not scan an entire
// template, and all reads use explicit offsets rather than the seek cursor.
func (s *templateImage) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.Size() {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, fmt.Errorf("diffstore: template RunAt limit is zero")
	}
	end := offset + min(limit, s.Size()-offset)
	block := int64(offset) / cowBlockSize * cowBlockSize
	data, err := unix.Seek(int(s.f.Fd()), s.body+block, unix.SEEK_DATA)
	if errors.Is(err, unix.ENXIO) || err == nil && data >= s.body+s.size {
		return templateImageRun{s, offset, end, sparse.Hole}, nil
	}
	if err != nil {
		return nil, err
	}
	first := (data - s.body) / cowBlockSize * cowBlockSize
	if first > block {
		return templateImageRun{s, offset, min(end, uint64(first)), sparse.Hole}, nil
	}
	hole, err := unix.Seek(int(s.f.Fd()), data, unix.SEEK_HOLE)
	if err != nil {
		return nil, err
	}
	if hole <= data {
		return nil, fmt.Errorf("diffstore: invalid template sparse extent")
	}
	return templateImageRun{s, offset, min(end, uint64(alignUp(min(hole, s.body+s.size)-s.body, cowBlockSize))), sparse.Data}, nil
}

type templateImageRun struct {
	source      *templateImage
	offset, end uint64
	kind        sparse.RunKind
}

func (r templateImageRun) Offset() uint64       { return r.offset }
func (r templateImageRun) End() uint64          { return r.end }
func (r templateImageRun) Kind() sparse.RunKind { return r.kind }
func (r templateImageRun) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	if off > r.end-r.offset || uint64(len(p)) > r.end-r.offset-off {
		return 0, fmt.Errorf("diffstore: template read outside run")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.kind == sparse.Hole {
		clear(p)
		return len(p), nil
	}
	return r.source.readData(p, int64(r.offset+off))
}

func (s *templateImage) readData(p []byte, off int64) (int, error) {
	if s.xts == nil {
		return s.f.ReadAt(p, s.body+off)
	}
	done := 0
	for done < len(p) {
		pos := off + int64(done)
		start := pos / diffDataUnitSize * diffDataUnitSize
		n := min(len(p)-done, maxDiffScratchSize-int(pos-start))
		end := alignUp(pos+int64(n), diffDataUnitSize)
		scratch := acquireDiffScratch(int(end - start))
		err := readFullAt(s.f, scratch.buffer, s.body+start)
		if err == nil {
			sectorcrypt.Transform(s.xts, scratch.buffer, uint64(start/diffDataUnitSize), false)
			copy(p[done:done+n], scratch.buffer[pos-start:pos-start+int64(n)])
		}
		releaseDiffScratch(scratch)
		if err != nil {
			return done, err
		}
		done += n
	}
	return done, nil
}

func (s *templateImage) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	if off >= s.Size() {
		return 0, io.EOF
	}
	n := min(uint64(len(p)), s.Size()-off)
	done := 0
	for uint64(done) < n {
		r, err := s.RunAt(off+uint64(done), n-uint64(done))
		if err != nil {
			return done, err
		}
		count, err := r.ReadAt(ctx, p[done:done+int(r.End()-r.Offset())], 0)
		done += count
		if err != nil {
			return done, err
		}
	}
	if done < len(p) {
		return done, io.EOF
	}
	return done, nil
}
