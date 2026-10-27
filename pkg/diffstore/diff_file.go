package diffstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/presence"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/versioned"
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
	diffVersion          = uint16(3)
	maxDiffScratchSize   = 1 << 20
	ext4MagicOffset      = int64(1024 + 0x38)
)

var (
	diffMagic           = [8]byte{0x89, 'K', 'D', 'X', 'T', 'S', '1', '\n'}
	diffHeaderKeyDomain = []byte("kuasar/diff/header-key/aes-gcm/v3\x00")
	diffHeaderAADDomain = []byte("kuasar/diff/header/v3\x00")
	diffScratchPool     sync.Pool

	ErrDiffEncryptionRequired = errors.New("diffstore: diff encryption required")
	ErrDiffPlaintextForbidden = errors.New("diffstore: plaintext diff forbidden")
	ErrDiffAuthentication     = errors.New("diffstore: diff authentication failed")
)

type diffScratch struct {
	buffer []byte
}

// DiffInit describes how OpenDisk obtains its active diff. Existing
// non-empty targets always win and ignore the other fields. A fresh target is
// either seeded from TemplatePath's logical sparse plaintext view or created
// at CreateSize when no template is present.
type DiffInit struct {
	Existing     bool
	TemplatePath string
	CreateSize   int64
}

// DiskOption configures active-diff storage without changing the COW
// policy above it. The option set is intentionally closed to this package.
type DiskOption interface {
	applyDisk(*diskOptions) error
}

type diskOptions struct {
	encryption     *diffEncryption
	required       bool
	encryptionSet  bool
	cache          *Cache
	discardOnClose bool
	checkpoint     *checkpointDiskOption
	checkpointKey  *[32]byte
}

type diffEncryptionDiskOption struct {
	customerKey [32]byte
	required    bool
}

func (o diffEncryptionDiskOption) applyDisk(options *diskOptions) error {
	if options.encryptionSet {
		return fmt.Errorf("diffstore: duplicate WithDiffEncryption option")
	}
	encryption, err := newDiffEncryption(o.customerKey)
	if err != nil {
		return err
	}
	key := o.customerKey
	options.checkpointKey = &key
	options.encryption, options.required, options.encryptionSet = encryption, o.required, true
	return nil
}

// WithDiffEncryption makes active-diff reads policy-aware and encrypts every
// fresh diff using customerKey: v1 by default, or v2 with WithCheckpoint.
// required rejects existing
// plaintext active diffs; it does not reject a plaintext template used only as
// a provisioning input.
func WithDiffEncryption(customerKey [32]byte, required bool) DiskOption {
	return diffEncryptionDiskOption{customerKey: customerKey, required: required}
}

type discardOnCloseDiskOption struct{}

func (discardOnCloseDiskOption) applyDisk(options *diskOptions) error {
	options.discardOnClose = true
	return nil
}

// WithDiscardOnClose allows an auto-owned ephemeral diff to cancel unfinished
// template materialization when its runtime is being destroyed.
func WithDiscardOnClose() DiskOption { return discardOnCloseDiskOption{} }
func parseDiskOptions(raw []DiskOption) (diskOptions, error) {
	var options diskOptions
	for _, option := range raw {
		if option == nil {
			return diskOptions{}, fmt.Errorf("diffstore: nil Disk option")
		}
		if err := option.applyDisk(&options); err != nil {
			return diskOptions{}, err
		}
	}
	return options, nil
}

// diffFile implements single-layer v3 storage. Presence and payload allocation
// jointly distinguish Hole, Zero and Data. Payload uses 512-byte XTS units when
// encrypted; neither headers nor bitmaps belong to the logical data region.
type diffFile struct {
	path                string
	f                   *os.File
	bodyIO              diffBodyIO
	encrypted           bool
	logicalSize         int64
	bodyOffset          int64
	xts                 *xts.Cipher
	direct              *directWorkspace
	syncFile            func() error
	stateMu             sync.RWMutex
	bitmap              []byte
	payload             []uint64
	beforePresenceWrite func(int) error
}

type diffBodyIO interface {
	io.ReaderAt
	io.WriterAt
}

func openSingleDiff(path string, init DiffInit, options diskOptions) (*diffFile, error) {
	if path == "" {
		return nil, fmt.Errorf("diffstore: empty diff path")
	}
	if init.Existing {
		return openExistingDiffFile(path, options.encryption, options.required, false)
	}
	if init.TemplatePath != "" {
		return nil, fmt.Errorf("diffstore: templated fresh diff must be opened through OpenDisk")
	}
	fresh, err := createFreshDiffFile(path, init, options.encryption)
	if err != nil {
		return nil, err
	}
	defer fresh.abort()
	if err := fresh.diff.Sync(); err != nil {
		return nil, fmt.Errorf("diffstore: sync fresh diff: %w", err)
	}
	if err := fresh.diff.Close(); err != nil {
		fresh.diff = nil
		return nil, fmt.Errorf("diffstore: close fresh diff: %w", err)
	}
	fresh.diff = nil
	if err := fresh.commit(); err != nil {
		return nil, err
	}
	return openExistingDiffFile(path, options.encryption, options.encryption != nil, false)
}

func openExistingDiffFile(path string, encryption *diffEncryption, required, readOnly bool) (*diffFile, error) {
	flags := os.O_RDWR
	if readOnly {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, fmt.Errorf("diffstore: open diff: %w", err)
	}
	fail := func(err error) (*diffFile, error) {
		_ = f.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return fail(fmt.Errorf("diffstore: stat diff: %w", err))
	}
	if !info.Mode().IsRegular() {
		return fail(fmt.Errorf("diffstore: diff is not a regular file"))
	}
	if info.Size() <= 0 {
		return fail(fmt.Errorf("diffstore: existing diff is empty"))
	}

	diff, err := readSingleHeader(f, info.Size(), encryption, required)
	if err != nil {
		return fail(err)
	}
	if err := diff.enableDirect(); err != nil {
		return fail(err)
	}
	return diff, nil
}

func createFreshDiffFile(path string, init DiffInit, encryption *diffEncryption) (*freshDiffInit, error) {
	return createFreshDiff(path, init, diskOptions{encryption: encryption})
}

// initializeFreshDiffFile preserves the historical synchronous helper used by
// focused diff tests. OpenDisk uses the asynchronous fresh path instead.
func initializeFreshDiffFile(path string, logicalSize int64, encryption *diffEncryption, template diffTemplateSource) (*diffFile, error) {
	init := DiffInit{CreateSize: logicalSize}
	fresh, err := createFreshDiffFile(path, init, encryption)
	if err != nil {
		return nil, err
	}
	defer fresh.abort()
	if template != nil {
		if err := seedDiffTemplate(context.Background(), fresh.diff, template); err != nil {
			return nil, fmt.Errorf("diffstore: seed diff template: %w", err)
		}
	}
	if err := fresh.diff.Sync(); err != nil {
		return nil, fmt.Errorf("diffstore: sync fresh diff: %w", err)
	}
	if err := fresh.diff.Close(); err != nil {
		fresh.diff = nil
		return nil, fmt.Errorf("diffstore: close fresh diff: %w", err)
	}
	fresh.diff = nil
	if err := fresh.commit(); err != nil {
		return nil, err
	}
	return openExistingDiffFile(path, encryption, encryption != nil, false)
}

func diffHeaderAAD(prefix []byte) []byte {
	aad := make([]byte, 0, len(diffHeaderAADDomain)+len(prefix))
	aad = append(aad, diffHeaderAADDomain...)
	aad = append(aad, prefix...)
	return aad
}

type diffEncryption struct {
	header         cipher.AEAD
	templateHeader cipher.AEAD
}

func newDiffEncryption(customerKey [32]byte) (*diffEncryption, error) {
	header, err := newDiffHeaderCipher(customerKey, diffHeaderKeyDomain)
	if err != nil {
		return nil, err
	}
	templateHeader, err := newDiffHeaderCipher(customerKey, []byte("kuasar/diff/header-key/aes-gcm/v1\x00"))
	if err != nil {
		return nil, err
	}
	return &diffEncryption{header: header, templateHeader: templateHeader}, nil
}

func newDiffHeaderCipher(customerKey [32]byte, domain []byte) (cipher.AEAD, error) {
	mac := hmac.New(sha256.New, customerKey[:])
	_, _ = mac.Write(domain)
	var headerKey [sha256.Size]byte
	mac.Sum(headerKey[:0])
	defer clear(headerKey[:])
	block, err := aes.NewCipher(headerKey[:])
	if err != nil {
		return nil, fmt.Errorf("diffstore: create diff header cipher: %w", err)
	}
	header, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("diffstore: create diff header GCM: %w", err)
	}
	return header, nil
}

// MaxWritableSize bounds a DIFF's logical address space, not its history, a
// read-only disk, guest RAM or an artifact's dense tail.
const MaxWritableSize int64 = presence.MaxSize

// ValidateWritableSize is shared by device planning and both storage formats.
func ValidateWritableSize(size int64) error    { return presence.ValidateSize(size) }
func validateDiffLogicalSize(size int64) error { return ValidateWritableSize(size) }

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
	read, err := d.directReadAt(buf[:n], offset)
	if err != nil {
		return read, err
	}
	return read, eof
}

func (d *diffFile) WriteAt(buf []byte, offset int64) (int, error) {
	if offset < 0 || offset > d.logicalSize || int64(len(buf)) > d.logicalSize-offset {
		return 0, fmt.Errorf("diffstore: diff write out of bounds")
	}
	if len(buf) == 0 {
		return 0, nil
	}
	return d.directWriteAt(buf, offset)
}

func (d *diffFile) scanPayload() ([]uint64, error) {
	numBlocks := d.logicalSize / cowBlockSize
	words := (numBlocks + 63) / 64
	if words > int64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("diffstore: diff bitmap is too large")
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
		setBitmapRange(bitmap, uint64(startBlock), uint64(endBlock))
		physical = holeOffset
	}
	return bitmap, nil
}

// validateFreshBodySparse verifies the filesystem contract before a
// template can allocate legitimate body blocks. A filesystem whose allocation
// granularity lets the header's data extent cross bodyOffset cannot safely
// reconstruct the dirty bitmap after reopen, so creation fails closed.
func (d *diffFile) validateFreshBodySparse() error {
	if err := d.Sync(); err != nil {
		return fmt.Errorf("diffstore: sync encrypted diff header: %w", err)
	}
	bodyEnd := d.bodyOffset + d.logicalSize
	dataOffset, err := unix.Seek(int(d.f.Fd()), d.bodyOffset, unix.SEEK_DATA)
	if err != nil {
		if errors.Is(err, unix.ENXIO) {
			return nil
		}
		return fmt.Errorf("diffstore: validate encrypted diff sparse body: %w", err)
	}
	if dataOffset < bodyEnd {
		return fmt.Errorf("diffstore: filesystem does not preserve the encrypted diff's 4 KiB header/body sparse boundary")
	}
	return nil
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
	return errors.Join(d.direct.read.Close(), d.direct.write.Close(), d.f.Close())
}

type diffTemplateSource interface {
	sparse.Source
	io.Closer
}

type freshDiffInit struct {
	diff          diffBackend
	template      diffTemplateSource
	tmpPath       string
	finalPath     string
	claim         *os.File
	claimPath     string
	committed     bool
	beforePublish func(string) error // deterministic I/O/crash boundaries; installed before attachment
}

func (f *freshDiffInit) publishHook(stage string) error {
	if f.beforePublish != nil {
		return f.beforePublish(stage)
	}
	return nil
}

func acquireFreshDiffClaim(path string) (*os.File, string, error) {
	claimPath := filepath.Join(filepath.Dir(path), ".claim-"+filepath.Base(path))
	claim, err := os.OpenFile(claimPath, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("diffstore: open fresh diff claim: %w", err)
	}
	if err := unix.Flock(int(claim.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = claim.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, "", unix.EEXIST
		}
		return nil, "", fmt.Errorf("diffstore: lock fresh diff claim: %w", err)
	}
	return claim, claimPath, nil
}

func (f *freshDiffInit) releaseClaim() error {
	if f == nil || f.claim == nil {
		return nil
	}
	var err error
	if f.claimPath != "" {
		err = errors.Join(err, os.Remove(f.claimPath))
	}
	err = errors.Join(err, unix.Flock(int(f.claim.Fd()), unix.LOCK_UN))
	err = errors.Join(err, f.claim.Close())
	f.claim = nil
	return err
}

func (f *freshDiffInit) abort() error {
	if f == nil {
		return nil
	}
	var err error
	if f.template != nil {
		err = errors.Join(err, f.template.Close())
		f.template = nil
	}
	if f.diff != nil {
		err = errors.Join(err, f.diff.Close())
		f.diff = nil
	}
	if !f.committed && f.tmpPath != "" {
		err = errors.Join(err, os.Remove(f.tmpPath))
	}
	err = errors.Join(err, f.releaseClaim())
	return err
}

func (f *freshDiffInit) failMaterialization() error {
	if f == nil {
		return nil
	}
	var err error
	if f.template != nil {
		err = errors.Join(err, f.template.Close())
		f.template = nil
	}
	if !f.committed && f.tmpPath != "" {
		err = errors.Join(err, os.Remove(f.tmpPath))
	}
	err = errors.Join(err, f.releaseClaim())
	return err
}

type fileDiffTemplate struct {
	diff *diffFile
}

func openLazyDiffTemplate(path string, encryption *diffEncryption) (diffTemplateSource, error) {
	return openProvisioningTemplate(path, encryption)
}

func openDiffTemplate(path string, encryption *diffEncryption) (diffTemplateSource, error) {
	return openProvisioningTemplate(path, encryption)
}

func (s *fileDiffTemplate) Size() uint64 { return uint64(s.diff.logicalSize) }

func (s *fileDiffTemplate) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.Size() {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, fmt.Errorf("diffstore: diff template RunAt limit is zero")
	}
	kind, end := s.diff.classify(offset, limit)
	return fileDiffTemplateRun{source: s, offset: offset, end: end, kind: kind}, nil
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
		return 0, fmt.Errorf("diffstore: diff template Run read outside [0,%d)", length)
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
	return s.diff.ReadAt(buf, int64(offset))
}

func (s *fileDiffTemplate) Close() error { return s.diff.Close() }

func seedDiffTemplate(ctx context.Context, target diffBackend, source diffTemplateSource) error {
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
			for position := start; position < seedEnd; {
				if err := ctx.Err(); err != nil {
					return err
				}
				if kind == sparse.Zero && position >= offset && position+cowBlockSize <= end {
					zeroEnd := min(end/cowBlockSize*cowBlockSize, position+64*cowBlockSize)
					if err := target.discard(ctx, int64(position), int64(zeroEnd-position)); err != nil {
						return err
					}
					position = zeroEnd
					continue
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
				position += cowBlockSize
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
			return fmt.Errorf("diffstore: commit fresh diff without replacement: %w", err)
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
		return fmt.Errorf("diffstore: commit fresh diff without replacement: %w", err)
	}
	defer placeholder.Close()
	if lockErr := unix.Flock(int(placeholder.Fd()), unix.LOCK_EX); lockErr != nil {
		return fmt.Errorf("diffstore: lock empty diff placeholder: %w", lockErr)
	}
	defer unix.Flock(int(placeholder.Fd()), unix.LOCK_UN)
	placeholderInfo, statErr := placeholder.Stat()
	pathInfo, pathErr := os.Lstat(destination)
	if statErr != nil || pathErr != nil || !placeholderInfo.Mode().IsRegular() ||
		placeholderInfo.Size() != 0 || !os.SameFile(placeholderInfo, pathInfo) {
		return fmt.Errorf("diffstore: commit fresh diff without replacement: %w", err)
	}
	if exchangeErr := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_EXCHANGE); exchangeErr != nil {
		return fmt.Errorf("diffstore: exchange empty diff placeholder: %w", exchangeErr)
	}
	rollback := func(cause error) error {
		rollbackErr := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_EXCHANGE)
		if rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("diffstore: roll back empty diff placeholder exchange: %w", rollbackErr))
		}
		return cause
	}
	swappedInfo, swappedErr := os.Lstat(source)
	placeholderInfo, statErr = placeholder.Stat()
	if swappedErr != nil || statErr != nil || !os.SameFile(placeholderInfo, swappedInfo) || placeholderInfo.Size() != 0 {
		return rollback(fmt.Errorf("diffstore: empty diff placeholder changed during atomic exchange"))
	}
	if removeErr := os.Remove(source); removeErr != nil {
		return rollback(fmt.Errorf("diffstore: remove exchanged empty diff placeholder: %w", removeErr))
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("diffstore: open diff parent directory: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return fmt.Errorf("diffstore: sync diff parent directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("diffstore: close diff parent directory: %w", closeErr)
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

// DiffSourceCapacity authenticates and reads logical capacity without opening
// a writable backend or provisioning any files. Templates retain the existing plaintext-allowed provisioning policy.
func DiffSourceCapacity(path string, template bool, rawOptions ...DiskOption) (int64, error) {
	options, err := parseDiskOptions(rawOptions)
	if err != nil {
		return 0, err
	}
	if info, err := versioned.Probe(path); err == nil {
		if options.required && !template && !info.Encrypted {
			return 0, ErrDiffPlaintextForbidden
		}
		source, err := versioned.OpenExport(context.Background(), path, "", options.checkpointKey)
		if err != nil {
			return 0, err
		}
		return int64(source.Size()), source.Close()
	} else if !errors.Is(err, versioned.ErrFormat) {
		return 0, err
	}
	if template {
		source, err := openDiffTemplate(path, options.encryption)
		if err != nil {
			return 0, err
		}
		size := source.Size()
		return int64(size), source.Close()
	}
	file, err := openExistingDiffFile(path, options.encryption, options.required, true)
	if err != nil {
		return 0, err
	}
	size := file.logicalSize
	return size, file.Close()
}

// setBitmapRange marks a validated half-open range without visiting every page.
func setBitmapRange(bitmap []uint64, start, end uint64) {
	if start >= end {
		return
	}
	first, last := start/64, (end-1)/64
	left := ^uint64(0) << (start % 64)
	right := ^uint64(0) >> (63 - (end-1)%64)
	if first == last {
		bitmap[first] |= left & right
		return
	}
	bitmap[first] |= left
	for i := first + 1; i < last; i++ {
		bitmap[i] = ^uint64(0)
	}
	bitmap[last] |= right
}
