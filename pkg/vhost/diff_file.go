package vhost

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"golang.org/x/crypto/xts"
	"golang.org/x/sys/unix"
)

const (
	diffPrefixSize       = 16
	diffHeaderPlainSize  = 128
	diffHeaderRegionSize = int64(4096)
	diffDataUnitSize     = int64(512)
	diffXTSKeySize       = 64
	diffHeaderNonceSize  = 12
	diffHeaderTagSize    = 16
	diffHeaderSealedSize = diffHeaderNonceSize + diffHeaderPlainSize + diffHeaderTagSize
	diffVersion          = uint16(1)
	maxDiffScratchSize   = 1 << 20
	ext4MagicOffset      = int64(1024 + 0x38)
)

var (
	diffMagic           = [8]byte{0x89, 'K', 'D', 'X', 'T', 'S', '1', '\n'}
	diffHeaderKeyDomain = []byte("kuasar/diff/header-key/aes-gcm/v1\x00")
	diffHeaderAADDomain = []byte("kuasar/diff/header/v1\x00")
	diffScratchPool     sync.Pool

	ErrDiffEncryptionRequired = errors.New("vhost: diff encryption required")
	ErrDiffPlaintextForbidden = errors.New("vhost: plaintext diff forbidden")
	ErrDiffAuthentication     = errors.New("vhost: diff authentication failed")
)

type diffScratch struct {
	buffer []byte
}

// DiffInit describes how OpenBlockCOW obtains its active diff. Existing
// non-empty targets always win and ignore the other fields. A fresh target is
// either seeded from TemplatePath's logical sparse plaintext view or created
// at CreateSize when no template is present.
type DiffInit struct {
	Existing     bool
	TemplatePath string
	CreateSize   int64
}

// BlockCOWOption configures active-diff storage without changing the COW
// policy above it. The option set is intentionally closed to this package.
type BlockCOWOption interface {
	applyBlockCOW(*blockCOWOptions) error
}

type blockCOWOptions struct {
	encryption    *diffEncryption
	required      bool
	encryptionSet bool
	cache         *COWCache
}

type diffEncryptionBlockCOWOption struct {
	customerKey [32]byte
	required    bool
}

func (o diffEncryptionBlockCOWOption) applyBlockCOW(options *blockCOWOptions) error {
	if options.encryptionSet {
		return fmt.Errorf("vhost: duplicate WithDiffEncryption option")
	}
	encryption, err := newDiffEncryption(o.customerKey)
	clear(o.customerKey[:])
	if err != nil {
		return err
	}
	options.encryption, options.required, options.encryptionSet = encryption, o.required, true
	return nil
}

// WithDiffEncryption makes active-diff reads policy-aware and makes every fresh
// diff an encrypted v1 file using customerKey. required rejects existing
// plaintext active diffs; it does not reject a plaintext template used only as
// a provisioning input.
func WithDiffEncryption(customerKey [32]byte, required bool) BlockCOWOption {
	return diffEncryptionBlockCOWOption{customerKey: customerKey, required: required}
}

func parseBlockCOWOptions(raw []BlockCOWOption) (blockCOWOptions, error) {
	var options blockCOWOptions
	for _, option := range raw {
		if option == nil {
			return blockCOWOptions{}, fmt.Errorf("vhost: nil BlockCOW option")
		}
		if err := option.applyBlockCOW(&options); err != nil {
			return blockCOWOptions{}, err
		}
	}
	return options, nil
}

// diffFile is the only physical active-diff encoding layer. Plaintext and
// encrypted files share the BlockCOW logic; encrypted body offsets are shifted
// by one fixed header region and transformed in fixed 512-byte XTS units.
type diffFile struct {
	f           *os.File
	bodyIO      diffBodyIO
	encrypted   bool
	logicalSize int64
	bodyOffset  int64
	xts         *xts.Cipher
	direct      *directWorkspace
	syncFile    func() error
}

type diffBodyIO interface {
	io.ReaderAt
	io.WriterAt
}

func openBlockCOWDiff(path string, init DiffInit, options blockCOWOptions) (*diffFile, error) {
	if path == "" {
		return nil, fmt.Errorf("vhost: empty diff path")
	}
	if init.Existing {
		return openExistingDiffFile(path, options.encryption, options.required, false)
	}
	return createFreshDiffFile(path, init, options.encryption)
}

func openExistingDiffFile(path string, encryption *diffEncryption, required, readOnly bool) (*diffFile, error) {
	flags := os.O_RDWR
	if readOnly {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, fmt.Errorf("vhost: open diff: %w", err)
	}
	fail := func(err error) (*diffFile, error) {
		_ = f.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return fail(fmt.Errorf("vhost: stat diff: %w", err))
	}
	if !info.Mode().IsRegular() {
		return fail(fmt.Errorf("vhost: diff is not a regular file"))
	}
	if info.Size() <= 0 {
		return fail(fmt.Errorf("vhost: existing diff is empty"))
	}

	var magic [len(diffMagic)]byte
	if info.Size() >= int64(len(magic)) {
		if err := readFullAt(f, magic[:], 0); err != nil {
			return fail(fmt.Errorf("vhost: read diff magic: %w", err))
		}
	}
	if magic == diffMagic {
		if encryption == nil {
			return fail(ErrDiffEncryptionRequired)
		}
		diff, err := openEncryptedDiffFile(f, info.Size(), encryption)
		if err != nil {
			return fail(err)
		}
		if !readOnly {
			if err := diff.enableDirect(); err != nil {
				return fail(err)
			}
		}
		return diff, nil
	}
	if required {
		return fail(ErrDiffPlaintextForbidden)
	}
	if err := validateDiffLogicalSize(info.Size()); err != nil {
		return fail(fmt.Errorf("vhost: plaintext diff: %w", err))
	}
	diff := &diffFile{f: f, bodyIO: f, logicalSize: info.Size()}
	if !readOnly {
		if err := diff.enableDirect(); err != nil {
			return fail(err)
		}
	}
	return diff, nil
}

func openEncryptedDiffFile(f *os.File, physicalSize int64, encryption *diffEncryption) (*diffFile, error) {
	if physicalSize < diffHeaderRegionSize {
		return nil, fmt.Errorf("vhost: encrypted diff header is truncated")
	}
	region := make([]byte, diffHeaderRegionSize)
	defer clear(region)
	if err := readFullAt(f, region, 0); err != nil {
		return nil, fmt.Errorf("vhost: read encrypted diff header: %w", err)
	}
	prefix := region[:diffPrefixSize]
	if !bytes.Equal(prefix[:len(diffMagic)], diffMagic[:]) {
		return nil, fmt.Errorf("vhost: encrypted diff magic changed during open")
	}
	if version := binary.BigEndian.Uint16(prefix[8:10]); version != diffVersion {
		return nil, fmt.Errorf("vhost: encrypted diff version %d is unsupported", version)
	}
	if size := binary.BigEndian.Uint16(prefix[10:12]); size != diffPrefixSize {
		return nil, fmt.Errorf("vhost: encrypted diff prefix size %d is invalid", size)
	}
	if flags := binary.BigEndian.Uint32(prefix[12:16]); flags != 0 {
		return nil, fmt.Errorf("vhost: encrypted diff flags 0x%x are unsupported", flags)
	}
	wrappedEnd := diffPrefixSize + diffHeaderSealedSize
	if !allZero(region[wrappedEnd:]) {
		return nil, fmt.Errorf("vhost: encrypted diff header padding is not zero")
	}
	nonceEnd := diffPrefixSize + diffHeaderNonceSize
	plaintext, err := encryption.header.Open(
		region[nonceEnd:nonceEnd],
		region[diffPrefixSize:nonceEnd],
		region[nonceEnd:wrappedEnd],
		diffHeaderAAD(prefix),
	)
	if err != nil {
		return nil, ErrDiffAuthentication
	}
	if len(plaintext) != diffHeaderPlainSize {
		return nil, fmt.Errorf("vhost: encrypted diff header plaintext size is invalid")
	}

	logicalRaw := binary.BigEndian.Uint64(plaintext[0:8])
	if logicalRaw > uint64(^uint64(0)>>1) {
		return nil, fmt.Errorf("vhost: encrypted diff logical size overflows int64")
	}
	logicalSize := int64(logicalRaw)
	if err := validateDiffLogicalSize(logicalSize); err != nil {
		return nil, fmt.Errorf("vhost: encrypted diff: %w", err)
	}
	if blockSize := binary.BigEndian.Uint32(plaintext[8:12]); blockSize != cowBlockSize {
		return nil, fmt.Errorf("vhost: encrypted diff block size %d is invalid", blockSize)
	}
	if unitSize := binary.BigEndian.Uint32(plaintext[12:16]); unitSize != uint32(diffDataUnitSize) {
		return nil, fmt.Errorf("vhost: encrypted diff data-unit size %d is invalid", unitSize)
	}
	if bodyOffset := binary.BigEndian.Uint32(plaintext[16:20]); bodyOffset != uint32(diffHeaderRegionSize) {
		return nil, fmt.Errorf("vhost: encrypted diff body offset %d is invalid", bodyOffset)
	}
	if keySize := binary.BigEndian.Uint32(plaintext[20:24]); keySize != diffXTSKeySize {
		return nil, fmt.Errorf("vhost: encrypted diff XTS key size %d is invalid", keySize)
	}
	if !allZero(plaintext[88:]) {
		return nil, fmt.Errorf("vhost: encrypted diff reserved header bytes are not zero")
	}
	if logicalSize > int64(^uint64(0)>>1)-diffHeaderRegionSize || physicalSize != diffHeaderRegionSize+logicalSize {
		return nil, fmt.Errorf("vhost: encrypted diff physical size does not match its header")
	}

	var rawKey [diffXTSKeySize]byte
	copy(rawKey[:], plaintext[24:88])
	cipher, err := xts.NewCipher(aes.NewCipher, rawKey[:])
	clear(rawKey[:])
	if err != nil {
		return nil, fmt.Errorf("vhost: encrypted diff XTS cipher: %w", err)
	}
	return &diffFile{
		f: f, bodyIO: f, encrypted: true, logicalSize: logicalSize,
		bodyOffset: diffHeaderRegionSize, xts: cipher,
	}, nil
}

func createFreshDiffFile(path string, init DiffInit, encryption *diffEncryption) (*diffFile, error) {
	var template diffTemplateSource
	logicalSize := init.CreateSize
	if init.TemplatePath != "" {
		var err error
		template, err = openDiffTemplate(init.TemplatePath, encryption)
		if err != nil {
			return nil, fmt.Errorf("vhost: open diff template: %w", err)
		}
		defer template.Close()
		if template.Size() > uint64(^uint64(0)>>1) {
			return nil, fmt.Errorf("vhost: diff template size overflows int64")
		}
		logicalSize = int64(template.Size())
	}
	if err := validateDiffLogicalSize(logicalSize); err != nil {
		return nil, fmt.Errorf("vhost: fresh diff: %w", err)
	}
	return initializeFreshDiffFile(path, logicalSize, encryption, template)
}

func initializeFreshDiffFile(path string, logicalSize int64, encryption *diffEncryption, template diffTemplateSource) (*diffFile, error) {
	directory := filepath.Dir(path)
	tmp, err := os.CreateTemp(directory, "."+filepath.Base(path)+".*.partial")
	if err != nil {
		return nil, fmt.Errorf("vhost: create diff temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	var target *diffFile
	if encryption == nil {
		if err := tmp.Truncate(logicalSize); err != nil {
			return nil, fmt.Errorf("vhost: size plaintext diff: %w", err)
		}
		target = &diffFile{f: tmp, bodyIO: tmp, logicalSize: logicalSize}
	} else {
		target, err = createEncryptedDiffFile(tmp, logicalSize, encryption, cryptorand.Reader)
		if err != nil {
			return nil, err
		}
		if err := target.validateFreshEncryptedBodySparse(); err != nil {
			return nil, err
		}
	}
	defer target.Close()
	if err := target.enableDirect(); err != nil {
		return nil, err
	}
	if template != nil {
		if err := seedDiffTemplate(context.Background(), target, template); err != nil {
			return nil, fmt.Errorf("vhost: seed diff template: %w", err)
		}
	}
	if err := target.Sync(); err != nil {
		return nil, fmt.Errorf("vhost: sync fresh diff: %w", err)
	}
	if err := target.Close(); err != nil {
		return nil, fmt.Errorf("vhost: close fresh diff: %w", err)
	}
	if err := commitFreshDiff(tmpPath, path); err != nil {
		return nil, err
	}
	committed = true
	if err := syncDirectory(directory); err != nil {
		return nil, err
	}
	// An encryption-backed writer always produced encrypted v1, even in auto mode.
	return openExistingDiffFile(path, encryption, encryption != nil, false)
}

func createEncryptedDiffFile(f *os.File, logicalSize int64, encryption *diffEncryption, random io.Reader) (*diffFile, error) {
	var rawKey [diffXTSKeySize]byte
	var header [diffHeaderPlainSize]byte
	defer clear(rawKey[:])
	defer clear(header[:])
	if _, err := io.ReadFull(random, rawKey[:]); err != nil {
		return nil, fmt.Errorf("vhost: generate XTS key: %w", err)
	}
	cipher, err := xts.NewCipher(aes.NewCipher, rawKey[:])
	if err != nil {
		return nil, fmt.Errorf("vhost: create XTS cipher: %w", err)
	}

	prefix := marshalDiffPrefix()
	binary.BigEndian.PutUint64(header[0:8], uint64(logicalSize))
	binary.BigEndian.PutUint32(header[8:12], cowBlockSize)
	binary.BigEndian.PutUint32(header[12:16], uint32(diffDataUnitSize))
	binary.BigEndian.PutUint32(header[16:20], uint32(diffHeaderRegionSize))
	binary.BigEndian.PutUint32(header[20:24], diffXTSKeySize)
	copy(header[24:88], rawKey[:])
	region := make([]byte, diffHeaderRegionSize)
	copy(region, prefix[:])
	nonceEnd := diffPrefixSize + diffHeaderNonceSize
	if _, err := io.ReadFull(random, region[diffPrefixSize:nonceEnd]); err != nil {
		clear(region)
		return nil, fmt.Errorf("vhost: generate diff header nonce: %w", err)
	}
	sealed := encryption.header.Seal(
		region[nonceEnd:nonceEnd],
		region[diffPrefixSize:nonceEnd],
		header[:],
		diffHeaderAAD(prefix[:]),
	)
	if len(sealed) != diffHeaderPlainSize+diffHeaderTagSize {
		panic("vhost: AES-GCM returned an unexpected diff header size")
	}
	if err := writeFullAt(f, region, 0); err != nil {
		clear(region)
		return nil, fmt.Errorf("vhost: write encrypted diff header: %w", err)
	}
	clear(region)
	if logicalSize > int64(^uint64(0)>>1)-diffHeaderRegionSize {
		return nil, fmt.Errorf("vhost: encrypted diff physical size overflow")
	}
	if err := f.Truncate(diffHeaderRegionSize + logicalSize); err != nil {
		return nil, fmt.Errorf("vhost: size encrypted diff: %w", err)
	}
	return &diffFile{
		f: f, bodyIO: f, encrypted: true, logicalSize: logicalSize,
		bodyOffset: diffHeaderRegionSize, xts: cipher,
	}, nil
}

func marshalDiffPrefix() [diffPrefixSize]byte {
	var prefix [diffPrefixSize]byte
	copy(prefix[:8], diffMagic[:])
	binary.BigEndian.PutUint16(prefix[8:10], diffVersion)
	binary.BigEndian.PutUint16(prefix[10:12], diffPrefixSize)
	return prefix
}

func diffHeaderAAD(prefix []byte) []byte {
	aad := make([]byte, 0, len(diffHeaderAADDomain)+len(prefix))
	aad = append(aad, diffHeaderAADDomain...)
	aad = append(aad, prefix...)
	return aad
}

type diffEncryption struct {
	header cipher.AEAD
}

func newDiffEncryption(customerKey [32]byte) (*diffEncryption, error) {
	mac := hmac.New(sha256.New, customerKey[:])
	_, _ = mac.Write(diffHeaderKeyDomain)
	var headerKey [sha256.Size]byte
	mac.Sum(headerKey[:0])
	defer clear(headerKey[:])
	block, err := aes.NewCipher(headerKey[:])
	if err != nil {
		return nil, fmt.Errorf("vhost: create diff header cipher: %w", err)
	}
	header, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vhost: create diff header GCM: %w", err)
	}
	return &diffEncryption{header: header}, nil
}

func validateDiffLogicalSize(size int64) error {
	if size <= 0 {
		return fmt.Errorf("logical size must be > 0")
	}
	if size%cowBlockSize != 0 {
		return fmt.Errorf("logical size %d is not aligned to %d", size, cowBlockSize)
	}
	if size > int64(^uint64(0)>>1)-diffHeaderRegionSize {
		return fmt.Errorf("logical size is too large")
	}
	return nil
}

func (d *diffFile) ReadAt(buf []byte, offset int64) (int, error) {
	if len(buf) == 0 {
		if offset < 0 || offset > d.logicalSize {
			return 0, io.EOF
		}
		return 0, nil
	}
	if offset < 0 || offset >= d.logicalSize {
		return 0, io.EOF
	}
	n := len(buf)
	var eof error
	if int64(n) > d.logicalSize-offset {
		n = int(d.logicalSize - offset)
		eof = io.EOF
	}
	if d.direct != nil {
		read, err := d.directReadAt(buf[:n], offset)
		if err != nil {
			return read, err
		}
		return read, eof
	}
	if !d.encrypted {
		read, err := d.bodyIO.ReadAt(buf[:n], offset)
		if err != nil && !(errors.Is(err, io.EOF) && read == n) {
			return read, err
		}
		if read != n {
			return read, io.ErrUnexpectedEOF
		}
		return n, eof
	}

	alignedStart := offset / diffDataUnitSize * diffDataUnitSize
	alignedEnd := alignUp(offset+int64(n), diffDataUnitSize)
	scratch := acquireDiffScratch(int(alignedEnd - alignedStart))
	defer releaseDiffScratch(scratch)
	buffer := scratch.buffer
	if err := readFullAt(d.bodyIO, buffer, d.bodyOffset+alignedStart); err != nil {
		return 0, err
	}
	for sectorOffset := int64(0); sectorOffset < int64(len(buffer)); sectorOffset += diffDataUnitSize {
		sector := buffer[sectorOffset : sectorOffset+diffDataUnitSize]
		d.xts.Decrypt(sector, sector, uint64((alignedStart+sectorOffset)/diffDataUnitSize))
	}
	copy(buf[:n], buffer[offset-alignedStart:offset-alignedStart+int64(n)])
	return n, eof
}

func (d *diffFile) WriteAt(buf []byte, offset int64) (int, error) {
	if offset < 0 || offset > d.logicalSize || int64(len(buf)) > d.logicalSize-offset {
		return 0, fmt.Errorf("vhost: diff write out of bounds")
	}
	if len(buf) == 0 {
		return 0, nil
	}
	if d.direct != nil {
		return d.directWriteAt(buf, offset)
	}
	if !d.encrypted {
		written, err := d.bodyIO.WriteAt(buf, offset)
		if err == nil && written != len(buf) {
			err = io.ErrShortWrite
		}
		return written, err
	}

	end := offset + int64(len(buf))
	alignedStart := offset / diffDataUnitSize * diffDataUnitSize
	alignedEnd := alignUp(end, diffDataUnitSize)
	scratch := acquireDiffScratch(int(alignedEnd - alignedStart))
	defer releaseDiffScratch(scratch)
	buffer := scratch.buffer
	if alignedStart != offset || alignedEnd != end {
		if err := readFullAt(d.bodyIO, buffer, d.bodyOffset+alignedStart); err != nil {
			return 0, err
		}
		for sectorOffset := int64(0); sectorOffset < int64(len(buffer)); sectorOffset += diffDataUnitSize {
			sector := buffer[sectorOffset : sectorOffset+diffDataUnitSize]
			d.xts.Decrypt(sector, sector, uint64((alignedStart+sectorOffset)/diffDataUnitSize))
		}
	}
	copy(buffer[offset-alignedStart:end-alignedStart], buf)
	for sectorOffset := int64(0); sectorOffset < int64(len(buffer)); sectorOffset += diffDataUnitSize {
		sector := buffer[sectorOffset : sectorOffset+diffDataUnitSize]
		d.xts.Encrypt(sector, sector, uint64((alignedStart+sectorOffset)/diffDataUnitSize))
	}
	if err := writeFullAt(d.bodyIO, buffer, d.bodyOffset+alignedStart); err != nil {
		return 0, err
	}
	return len(buf), nil
}

func (d *diffFile) scanDirtyBlocks() ([]uint64, error) {
	numBlocks := d.logicalSize / cowBlockSize
	words := (numBlocks + 63) / 64
	if words > int64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("vhost: diff bitmap is too large")
	}
	bitmap := make([]uint64, int(words))
	bodyEnd := d.bodyOffset + d.logicalSize
	for physical := d.bodyOffset; physical < bodyEnd; {
		dataOffset, err := unix.Seek(int(d.f.Fd()), physical, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				return bitmap, nil
			}
			return nil, fmt.Errorf("SEEK_DATA at %d: %w", physical, err)
		}
		if dataOffset >= bodyEnd {
			return bitmap, nil
		}
		holeOffset, err := unix.Seek(int(d.f.Fd()), dataOffset, unix.SEEK_HOLE)
		if err != nil {
			return nil, fmt.Errorf("SEEK_HOLE at %d: %w", dataOffset, err)
		}
		if holeOffset <= dataOffset {
			return nil, fmt.Errorf("invalid sparse extent [%d,%d)", dataOffset, holeOffset)
		}
		if holeOffset > bodyEnd {
			holeOffset = bodyEnd
		}
		startBlock := (dataOffset - d.bodyOffset) / cowBlockSize
		endBlock := alignUp(holeOffset-d.bodyOffset, cowBlockSize) / cowBlockSize
		for block := startBlock; block < endBlock; block++ {
			bitmap[block/64] |= 1 << (uint64(block) % 64)
		}
		physical = holeOffset
	}
	return bitmap, nil
}

// validateFreshEncryptedBodySparse verifies the filesystem contract before a
// template can allocate legitimate body blocks. A filesystem whose allocation
// granularity lets the header's data extent cross bodyOffset cannot safely
// reconstruct the dirty bitmap after reopen, so creation fails closed.
func (d *diffFile) validateFreshEncryptedBodySparse() error {
	if !d.encrypted {
		return nil
	}
	if err := d.Sync(); err != nil {
		return fmt.Errorf("vhost: sync encrypted diff header: %w", err)
	}
	bitmap, err := d.scanDirtyBlocks()
	if err != nil {
		return fmt.Errorf("vhost: validate encrypted diff sparse body: %w", err)
	}
	for _, word := range bitmap {
		if word != 0 {
			return fmt.Errorf("vhost: filesystem does not preserve the encrypted diff's 4 KiB header/body sparse boundary")
		}
	}
	return nil
}

func (d *diffFile) punchHole(offset, length int64) error {
	return unix.Fallocate(int(d.f.Fd()),
		unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE,
		d.bodyOffset+offset, length)
}

func (d *diffFile) Sync() error {
	if d.syncFile != nil {
		return d.syncFile()
	}
	return d.f.Sync()
}
func (d *diffFile) Close() error {
	// Owners join all operations before Close; the locks protect direct users too.
	if d.direct == nil {
		return d.f.Close()
	}
	d.direct.readMu.Lock()
	defer d.direct.readMu.Unlock()
	d.direct.writeMu.Lock()
	defer d.direct.writeMu.Unlock()
	return errors.Join(d.direct.read.close(), d.direct.write.close(), d.f.Close())
}

type diffTemplateSource interface {
	sparse.Source
	io.Closer
}

type fileDiffTemplate struct {
	diff   *diffFile
	bitmap []uint64
}

func openDiffTemplate(path string, encryption *diffEncryption) (diffTemplateSource, error) {
	diff, err := openExistingDiffFile(path, encryption, false, true)
	if err != nil {
		return nil, err
	}
	bitmap, err := diff.scanDirtyBlocks()
	if err != nil {
		_ = diff.Close()
		return nil, err
	}
	return &fileDiffTemplate{diff: diff, bitmap: bitmap}, nil
}

// ValidateExistingDiffExt4 verifies the effective filesystem seen through an
// existing active diff and its optional immutable base. It opens the diff
// read-only, applies the same local-encryption policy as OpenBlockCOW, and
// checks the ext4 superblock magic in the combined view without mutating the
// diff or inventing sparse data.
func ValidateExistingDiffExt4(ctx context.Context, path string, base BlockReader, rawOptions ...BlockCOWOption) error {
	options, err := parseBlockCOWOptions(rawOptions)
	if err != nil {
		return err
	}
	diff, err := openExistingDiffFile(path, options.encryption, options.required, true)
	if err != nil {
		return err
	}
	if err := diff.enableDirect(); err != nil {
		return errors.Join(err, diff.Close())
	}
	bitmap, err := diff.scanDirtyBlocks()
	if err != nil {
		return errors.Join(fmt.Errorf("vhost: scan existing diff: %w", err), diff.Close())
	}
	return validateDiffSourceExt4(ctx, &fileDiffTemplate{diff: diff, bitmap: bitmap}, base)
}

// ValidateDiffTemplateExt4 verifies the effective filesystem produced by a
// provisioning template over an optional immutable base. Templates retain the
// existing policy that plaintext is accepted even when newly-created active
// diffs must be encrypted.
func ValidateDiffTemplateExt4(ctx context.Context, path string, base BlockReader, rawOptions ...BlockCOWOption) error {
	options, err := parseBlockCOWOptions(rawOptions)
	if err != nil {
		return err
	}
	source, err := openDiffTemplate(path, options.encryption)
	if err != nil {
		return err
	}
	return validateDiffSourceExt4(ctx, source, base)
}

func validateDiffSourceExt4(ctx context.Context, source diffTemplateSource, base BlockReader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	validationErr := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if source.Size() < uint64(ext4MagicOffset+2) {
			return fmt.Errorf("vhost: ext4 source is too small")
		}
		if base != nil && base.Size() > int64(source.Size()) {
			return fmt.Errorf("vhost: base size %d > diff size %d", base.Size(), source.Size())
		}
		var magic [2]byte
		run, err := source.RunAt(uint64(ext4MagicOffset), uint64(len(magic)))
		if err != nil {
			return fmt.Errorf("vhost: inspect ext4 superblock run: %w", err)
		}
		switch run.Kind() {
		case sparse.Hole, sparse.Zero:
			if base != nil && base.Size() >= ext4MagicOffset+int64(len(magic)) {
				n, readErr := base.ReadAt(magic[:], ext4MagicOffset)
				if readErr != nil && (readretry.IsTerminal(readErr) || readerr.IsPermanent(readErr) || !(errors.Is(readErr, io.EOF) && n == len(magic))) {
					return fmt.Errorf("vhost: read ext4 magic from base: %w", readErr)
				}
				if n != len(magic) {
					return io.ErrUnexpectedEOF
				}
			}
		default:
			n, readErr := source.ReadAt(ctx, magic[:], uint64(ext4MagicOffset))
			if readErr != nil && (readretry.IsTerminal(readErr) || readerr.IsPermanent(readErr) || !(errors.Is(readErr, io.EOF) && n == len(magic))) {
				return fmt.Errorf("vhost: read ext4 magic from diff: %w", readErr)
			}
			if n != len(magic) {
				return io.ErrUnexpectedEOF
			}
		}
		if magic != [2]byte{0x53, 0xef} {
			return fmt.Errorf("vhost: effective writable disk is not a formatted ext4 filesystem")
		}
		return nil
	}()
	return errors.Join(validationErr, source.Close())
}

func (s *fileDiffTemplate) Size() uint64 { return uint64(s.diff.logicalSize) }

func (s *fileDiffTemplate) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.Size() {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, fmt.Errorf("vhost: diff template RunAt limit is zero")
	}
	end := offset + limit
	if end < offset || end > s.Size() {
		end = s.Size()
	}
	block := int64(offset) / cowBlockSize
	dirty := bitmapBlockDirty(s.bitmap, block)
	runEnd := min(uint64((block+1)*cowBlockSize), end)
	for runEnd < end {
		nextBlock := int64(runEnd) / cowBlockSize
		if bitmapBlockDirty(s.bitmap, nextBlock) != dirty {
			break
		}
		runEnd = min(uint64((nextBlock+1)*cowBlockSize), end)
	}
	kind := sparse.Hole
	if dirty {
		kind = sparse.Data
	}
	return fileDiffTemplateRun{source: s, offset: offset, end: runEnd, kind: kind}, nil
}

type fileDiffTemplateRun struct {
	source *fileDiffTemplate
	offset uint64
	end    uint64
	kind   sparse.RunKind
}

func (r fileDiffTemplateRun) Offset() uint64       { return r.offset }
func (r fileDiffTemplateRun) End() uint64          { return r.end }
func (r fileDiffTemplateRun) Kind() sparse.RunKind { return r.kind }

func (r fileDiffTemplateRun) ReadAt(ctx context.Context, buf []byte, innerOffset uint64) (int, error) {
	length := r.end - r.offset
	if innerOffset > length || uint64(len(buf)) > length-innerOffset {
		return 0, fmt.Errorf("vhost: diff template Run read outside [0,%d)", length)
	}
	if len(buf) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.kind == sparse.Hole || r.kind == sparse.Zero {
		clear(buf)
		return len(buf), nil
	}
	n, err := r.source.ReadAt(ctx, buf, r.offset+innerOffset)
	if n == len(buf) && (err == nil || errors.Is(err, io.EOF)) {
		return n, nil
	}
	if err != nil {
		return n, err
	}
	return n, io.ErrUnexpectedEOF
}

func (s *fileDiffTemplate) ReadAt(ctx context.Context, buf []byte, offset uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if offset >= s.Size() {
		return 0, io.EOF
	}
	n := len(buf)
	var eof error
	if uint64(n) > s.Size()-offset {
		n = int(s.Size() - offset)
		eof = io.EOF
	}
	written := 0
	for written < n {
		position := int64(offset) + int64(written)
		block := position / cowBlockSize
		blockEnd := min((block+1)*cowBlockSize, int64(offset)+int64(n))
		chunk := buf[written : written+int(blockEnd-position)]
		if bitmapBlockDirty(s.bitmap, block) {
			read, err := s.diff.ReadAt(chunk, position)
			written += read
			if err != nil && !(errors.Is(err, io.EOF) && read == len(chunk)) {
				return written, err
			}
			if read != len(chunk) {
				return written, io.ErrUnexpectedEOF
			}
			continue
		}
		clear(chunk)
		written += len(chunk)
	}
	return written, eof
}

func (s *fileDiffTemplate) Close() error { return s.diff.Close() }

func seedDiffTemplate(ctx context.Context, target *diffFile, source diffTemplateSource) error {
	block := make([]byte, cowBlockSize)
	defer clear(block)
	var nextSeed uint64
	for offset := uint64(0); offset < source.Size(); {
		run, err := source.RunAt(offset, source.Size()-offset)
		if err != nil {
			return err
		}
		kind, end := run.Kind(), run.End()
		if end <= offset || end > source.Size() {
			return fmt.Errorf("invalid diff template sparse run")
		}
		if kind != sparse.Hole {
			start := uint64(int64(offset) / cowBlockSize * cowBlockSize)
			if start < nextSeed {
				start = nextSeed
			}
			seedEnd := uint64(alignUp(int64(end), cowBlockSize))
			for position := start; position < seedEnd; position += cowBlockSize {
				if err := ctx.Err(); err != nil {
					return err
				}
				read, err := source.ReadAt(ctx, block, position)
				if err != nil && !(errors.Is(err, io.EOF) && read == len(block)) {
					return err
				}
				if read != len(block) {
					return io.ErrUnexpectedEOF
				}
				written, err := target.WriteAt(block, int64(position))
				if err != nil {
					return err
				}
				if written != len(block) {
					return io.ErrShortWrite
				}
			}
			nextSeed = seedEnd
		}
		offset = end
	}
	return nil
}

func commitFreshDiff(source, destination string) error {
	err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
	if !errors.Is(err, unix.EEXIST) {
		if err != nil {
			return fmt.Errorf("vhost: commit fresh diff without replacement: %w", err)
		}
		return nil
	}

	// The historical contract treats an existing empty regular file as a
	// provisioned placeholder. Serialize cooperating creators on its inode,
	// revalidate the path while holding that lock, then atomically exchange the
	// seeded temporary file with the placeholder. The post-exchange inode and
	// size checks catch a non-cooperating path replacement or writer before the
	// old empty inode is removed.
	placeholder, openErr := os.OpenFile(destination, os.O_RDWR|unix.O_NOFOLLOW, 0)
	if openErr != nil {
		return fmt.Errorf("vhost: commit fresh diff without replacement: %w", err)
	}
	defer placeholder.Close()
	if lockErr := unix.Flock(int(placeholder.Fd()), unix.LOCK_EX); lockErr != nil {
		return fmt.Errorf("vhost: lock empty diff placeholder: %w", lockErr)
	}
	defer unix.Flock(int(placeholder.Fd()), unix.LOCK_UN)
	placeholderInfo, statErr := placeholder.Stat()
	pathInfo, pathErr := os.Lstat(destination)
	if statErr != nil || pathErr != nil || !placeholderInfo.Mode().IsRegular() ||
		placeholderInfo.Size() != 0 || !os.SameFile(placeholderInfo, pathInfo) {
		return fmt.Errorf("vhost: commit fresh diff without replacement: %w", err)
	}
	if exchangeErr := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_EXCHANGE); exchangeErr != nil {
		return fmt.Errorf("vhost: exchange empty diff placeholder: %w", exchangeErr)
	}
	rollback := func(cause error) error {
		rollbackErr := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_EXCHANGE)
		if rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("vhost: roll back empty diff placeholder exchange: %w", rollbackErr))
		}
		return cause
	}
	swappedInfo, swappedErr := os.Lstat(source)
	placeholderInfo, statErr = placeholder.Stat()
	if swappedErr != nil || statErr != nil || !os.SameFile(placeholderInfo, swappedInfo) || placeholderInfo.Size() != 0 {
		return rollback(fmt.Errorf("vhost: empty diff placeholder changed during atomic exchange"))
	}
	if removeErr := os.Remove(source); removeErr != nil {
		return rollback(fmt.Errorf("vhost: remove exchanged empty diff placeholder: %w", removeErr))
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("vhost: open diff parent directory: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return fmt.Errorf("vhost: sync diff parent directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("vhost: close diff parent directory: %w", closeErr)
	}
	return nil
}

func readFullAt(reader io.ReaderAt, buf []byte, offset int64) error {
	n, err := reader.ReadAt(buf, offset)
	if err != nil && !(errors.Is(err, io.EOF) && n == len(buf)) {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	if n != len(buf) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func writeFullAt(writer io.WriterAt, buf []byte, offset int64) error {
	n, err := writer.WriteAt(buf, offset)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return io.ErrShortWrite
	}
	return nil
}

func alignUp(value, alignment int64) int64 {
	return (value + alignment - 1) / alignment * alignment
}

func allZero(buf []byte) bool {
	var aggregate byte
	for _, value := range buf {
		aggregate |= value
	}
	return aggregate == 0
}

func acquireDiffScratch(size int) *diffScratch {
	if size <= maxDiffScratchSize {
		if pooled := diffScratchPool.Get(); pooled != nil {
			scratch := pooled.(*diffScratch)
			if cap(scratch.buffer) >= size {
				scratch.buffer = scratch.buffer[:size]
				return scratch
			}
			scratch.buffer = make([]byte, size)
			return scratch
		}
	}
	return &diffScratch{buffer: make([]byte, size)}
}

func releaseDiffScratch(scratch *diffScratch) {
	clear(scratch.buffer)
	if cap(scratch.buffer) <= maxDiffScratchSize {
		scratch.buffer = scratch.buffer[:0]
		diffScratchPool.Put(scratch)
	}
}
