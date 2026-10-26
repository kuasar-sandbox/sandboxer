package versioned

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/presence"
	"golang.org/x/crypto/xts"
	"golang.org/x/sys/unix"
)

var magic = [8]byte{0x89, 'K', 'D', 'X', 'T', 'S', '1', '\n'}
var le = binary.LittleEndian

const (
	logCommitPage  = 2
	logIndexPage   = 3
	logIndexPages  = 3
	logImagesPage  = logIndexPage + logIndexPages
	maxLogPages    = logIndexPages * PageSize / 8
	slotAreaPage   = 2048
	slotsPerGroup  = 128
	descriptorSize = 24
	cachePages     = 512
)

type header struct {
	Info    Info   `json:"info"`
	Wrapped []byte `json:"wrapped,omitempty"`
}
type control struct {
	Root      uint64
	Next      uint64
	Free      uint64
	Garbage   uint64
	Epoch     uint64
	Current   uint64
	Head      uint64
	Tail      uint64
	Protected uint64
	Pending   uint64 // deleted versions still needing pin release or chain processing
	GCScan    uint64
	Retired   bool
	Tx        uint64
}
type checkpoint struct {
	ID         string
	Epoch      uint64
	Root       uint64
	Next       uint64
	Previous   uint64
	Garbage    uint64
	Visible    bool
	Retained   bool
	Pending    bool
	Bitmap     uint64 // head of the exclusive presence-copy chain
	BitmapDone uint64 // durable GC cursor; live checkpoints always have zero
}

const (
	kindFree uint64 = iota
	kindData
	kindIndex
	kindCheckpoint
	kindGarbage
	kindBitmap
)

type descriptor struct{ Birth, Next, Kind uint64 }

func slotPage(slot uint64) uint64 {
	n := slot - 1
	return slotAreaPage + (n/slotsPerGroup)*(slotsPerGroup+1) + 1 + n%slotsPerGroup
}
func descriptorPage(slot uint64) uint64 {
	return slotAreaPage + ((slot-1)/slotsPerGroup)*(slotsPerGroup+1)
}
func descriptorOffset(slot uint64) int { return 16 + int((slot-1)%slotsPerGroup)*descriptorSize }
func writeFull(f *os.File, b []byte, page uint64) error {
	if len(b)%PageSize != 0 {
		panic("diffstore: internal unaligned write")
	}
	return directWrite(f, b, int64(page)*PageSize)
}
func readFull(f *os.File, page uint64) ([]byte, error) {
	return directRead(f, PageSize, int64(page)*PageSize)
}
func sealPage(b []byte) { le.PutUint32(b[PageSize-4:], crc32.ChecksumIEEE(b[:PageSize-4])) }
func checkPage(b []byte) error {
	if len(b) != PageSize || le.Uint32(b[PageSize-4:]) != crc32.ChecksumIEEE(b[:PageSize-4]) {
		return fmt.Errorf("diffstore: metadata checksum mismatch")
	}
	return nil
}
func encodeJSON(kind string, v any) ([]byte, error) {
	p, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(p) > PageSize-20 {
		return nil, fmt.Errorf("diffstore: metadata exceeds a page")
	}
	b := make([]byte, PageSize)
	copy(b, kind)
	le.PutUint32(b[8:], uint32(len(p)))
	copy(b[12:], p)
	sealPage(b)
	return b, nil
}
func decodeJSON(b []byte, kind string, v any) error {
	if err := checkPage(b); err != nil {
		return err
	}
	if !bytes.Equal(b[:len(kind)], []byte(kind)) {
		return fmt.Errorf("diffstore: invalid %s page", kind)
	}
	n := int(le.Uint32(b[8:]))
	if n > PageSize-20 {
		return fmt.Errorf("diffstore: invalid metadata length")
	}
	return json.Unmarshal(b[12:12+n], v)
}
func wrapCipher(key *[32]byte) (cipher.AEAD, error) {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("kuasar/diff/header-key/aes-gcm/v4\x00"))
	raw := mac.Sum(nil)
	defer clear(raw)
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func createHeader(o Options) ([]byte, *xts.Cipher, error) {
	if o.DiffID == "" {
		o.DiffID = NewDiffID()
	}
	if err := validateInfo(Info{DiffID: o.DiffID, BaseID: o.BaseID, LogicalSize: o.LogicalSize}); err != nil {
		return nil, nil, err
	}
	h := header{Info: Info{DiffID: o.DiffID, BaseID: o.BaseID, LogicalSize: o.LogicalSize, Encrypted: o.Key != nil}}
	var xc *xts.Cipher
	if o.Key != nil {
		raw := make([]byte, 64)
		defer clear(raw)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		var err error
		xc, err = xts.NewCipher(aes.NewCipher, raw)
		if err != nil {
			return nil, nil, err
		}
		aead, err := wrapCipher(o.Key)
		if err != nil {
			return nil, nil, err
		}
		nonce := make([]byte, aead.NonceSize())
		if _, err = rand.Read(nonce); err != nil {
			return nil, nil, err
		}
		aad, _ := json.Marshal(h.Info)
		h.Wrapped = append(nonce, aead.Seal(nil, nonce, raw, aad)...)
	}
	p, err := json.Marshal(h)
	if err != nil {
		return nil, nil, err
	}
	if len(p) > PageSize-24 {
		return nil, nil, fmt.Errorf("diffstore: header too large")
	}
	b := make([]byte, PageSize)
	copy(b, magic[:])
	binary.BigEndian.PutUint16(b[8:], 4)
	binary.BigEndian.PutUint16(b[10:], 16)
	if o.Key != nil {
		binary.BigEndian.PutUint32(b[12:], 1)
	}
	le.PutUint32(b[16:], uint32(len(p)))
	copy(b[20:], p)
	sealPage(b)
	return b, xc, nil
}
func validateInfo(i Info) error {
	if err := ValidID(i.DiffID); err != nil {
		return fmt.Errorf("diffstore: invalid DIFF identity: %w", err)
	}
	if len(i.DiffID) == 0 || len(i.DiffID) > 128 || filepath.Base(i.DiffID) != i.DiffID || i.DiffID == "." || i.DiffID == ".." {
		return fmt.Errorf("diffstore: invalid DIFF identity")
	}
	if err := presence.ValidateSize(i.LogicalSize); err != nil {
		return err
	}
	if len(i.BaseID) > 2048 {
		return fmt.Errorf("diffstore: base identity too long")
	}
	return nil
}
func parseHeader(b []byte, key *[32]byte, metadata bool) (Info, *xts.Cipher, error) {
	if len(b) < 16 || !bytes.Equal(b[:8], magic[:]) || binary.BigEndian.Uint16(b[8:]) != 4 {
		return Info{}, nil, ErrFormat
	}
	if err := checkPage(b); err != nil {
		return Info{}, nil, err
	}
	if binary.BigEndian.Uint16(b[10:]) != 16 || binary.BigEndian.Uint32(b[12:]) > 1 {
		return Info{}, nil, fmt.Errorf("diffstore: invalid v4 prefix")
	}
	n := int(le.Uint32(b[16:]))
	if n > PageSize-24 {
		return Info{}, nil, fmt.Errorf("diffstore: invalid header length")
	}
	var h header
	if err := json.Unmarshal(b[20:20+n], &h); err != nil {
		return Info{}, nil, err
	}
	if err := validateInfo(h.Info); err != nil {
		return Info{}, nil, err
	}
	if h.Info.Encrypted != (binary.BigEndian.Uint32(b[12:]) == 1) {
		return Info{}, nil, fmt.Errorf("diffstore: encryption flags disagree")
	}
	if !h.Info.Encrypted {
		if len(h.Wrapped) != 0 {
			return Info{}, nil, fmt.Errorf("diffstore: plaintext key envelope")
		}
		return h.Info, nil, nil
	}
	if metadata {
		return h.Info, nil, nil
	}
	if key == nil {
		return Info{}, nil, ErrEncryptionRequired
	}
	aead, err := wrapCipher(key)
	if err != nil {
		return Info{}, nil, err
	}
	if len(h.Wrapped) != aead.NonceSize()+64+aead.Overhead() {
		return Info{}, nil, ErrAuthentication
	}
	aad, _ := json.Marshal(h.Info)
	raw, err := aead.Open(nil, h.Wrapped[:aead.NonceSize()], h.Wrapped[aead.NonceSize():], aad)
	if err != nil {
		return Info{}, nil, ErrAuthentication
	}
	defer clear(raw)
	xc, err := xts.NewCipher(aes.NewCipher, raw)
	return h.Info, xc, err
}

func openData(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECT, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
func Probe(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	b := make([]byte, PageSize)
	n, err := f.ReadAt(b, 0)
	if n < 16 {
		return Info{}, ErrFormat
	}
	if err != nil && err != io.EOF {
		return Info{}, err
	}
	info, _, err := parseHeader(b, nil, true)
	if err != nil {
		return Info{}, err
	}
	// Current checkpoint identity must be read under the same short metadata lock
	// as recovery interpretation. Probe never opens a writer or mutates a file.
	return probeCurrent(path, f, info)
}

func Create(path string, o Options) (*Store, error) {
	if o.RequireEncryption && o.Key == nil {
		return nil, ErrEncryptionRequired
	}
	h, xc, err := createHeader(o)
	if err != nil {
		return nil, err
	}
	lockPath := o.LockPath
	if lockPath == "" {
		lockPath = path + ".lock"
	}
	lock, err := openLock(lockPath)
	if err != nil {
		return nil, err
	}
	if err = ofd(lock, unix.F_WRLCK, ownerByte, false); err != nil {
		lock.Close()
		return nil, err
	}
	f, err := openData(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		lock.Close()
		return nil, err
	}
	fail := func(err error) (*Store, error) { f.Close(); lock.Close(); os.Remove(path); return nil, err }
	if err = f.Truncate(slotAreaPage * PageSize); err != nil {
		return fail(err)
	}
	// Redo and existing control home pages must remain writable even when guest
	// data fills the filesystem. This fixed, bounded reservation is not payload.
	if err = unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, (logImagesPage+maxLogPages)*PageSize); err != nil {
		return fail(err)
	}
	if err = writeFull(f, h, 0); err != nil {
		return fail(err)
	}
	ctrl := control{Next: 1, Epoch: 1}
	b, _ := encodeJSON("KDCONT4", ctrl)
	if err = writeFull(f, b, 1); err != nil {
		return fail(err)
	}
	if err = writeFull(f, make([]byte, PageSize), logCommitPage); err != nil {
		return fail(err)
	}
	bitmap := make([]byte, presence.Bytes(o.LogicalSize))
	if err = unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, int64(presenceAreaPage)*PageSize, int64(presence.Pages(o.LogicalSize))*PageSize); err != nil {
		return fail(err)
	}
	if err = writeInitialPresence(f, bitmap, o.LogicalSize); err != nil {
		return fail(err)
	}
	info, _, _ := parseHeader(h, nil, true)
	s := &Store{presence: bitmap, file: f, lock: lock, lockPath: lockPath, info: info, cipher: xc, ctrl: ctrl, cache: newCache(cachePages)}
	s.startGC()
	return s, nil
}
func Open(path string, o Options) (*Store, error) { return openStore(path, o, false) }
func OpenMetadata(path string) (*Store, error)    { return openStore(path, Options{}, true) }
func openStore(path string, o Options, metadata bool) (*Store, error) {
	lockPath := o.LockPath
	if lockPath == "" {
		lockPath = path + ".lock"
	}
	lock, err := openLock(lockPath)
	if err != nil {
		return nil, err
	}
	if err = ofd(lock, unix.F_WRLCK, ownerByte, false); err != nil {
		lock.Close()
		return nil, err
	}
	f, err := openData(path, unix.O_RDWR)
	if err != nil {
		lock.Close()
		return nil, err
	}
	fail := func(err error) (*Store, error) { f.Close(); lock.Close(); return nil, err }
	b, err := readFull(f, 0)
	if err != nil {
		return fail(err)
	}
	info, xc, err := parseHeader(b, o.Key, metadata)
	if err != nil {
		return fail(err)
	}
	if o.RequireEncryption && !info.Encrypted {
		return fail(ErrPlaintextForbidden)
	}
	if o.DiffID != "" && o.DiffID != info.DiffID || o.BaseID != "" && o.BaseID != info.BaseID || o.LogicalSize != 0 && o.LogicalSize != info.LogicalSize {
		return fail(fmt.Errorf("diffstore: identity, capacity or immutable base mismatch"))
	}
	if err = ofd(lock, unix.F_WRLCK, metadataByte, true); err != nil {
		return fail(err)
	}
	records, err := readRedo(f, nil, info.LogicalSize)
	if err == nil {
		for _, r := range records {
			if err = writeFull(f, r.data, r.page); err != nil {
				break
			}
		}
	}
	if err == nil && len(records) > 0 {
		err = writeFull(f, make([]byte, PageSize), logCommitPage)
	}
	if err == nil {
		b, err = readFull(f, 1)
	}
	var ctrl control
	if err == nil {
		err = decodeJSON(b, "KDCONT4", &ctrl)
	}
	unlock := ofd(lock, unix.F_UNLCK, metadataByte, false)
	if err == nil {
		err = unlock
	}
	if err != nil {
		return fail(err)
	}
	if ctrl.Retired && !metadata {
		return fail(ErrRetired)
	}
	if ctrl.Next == 0 || ctrl.Epoch == 0 {
		return fail(fmt.Errorf("diffstore: invalid control generation"))
	}
	s := &Store{file: f, lock: lock, lockPath: lockPath, info: info, cipher: xc, ctrl: ctrl, cache: newCache(cachePages), metadataOnly: metadata}
	s.presence, err = loadPresence(s.readPage, info.LogicalSize)
	if err != nil {
		return fail(err)
	}
	// Read only the root before exposing a data owner. Full presence discovery
	// is on demand; replay, identity and this minimum tree check still finish
	// synchronously. Metadata-only cleanup remains possible for a damaged tree.
	if !metadata && ctrl.Root != 0 {
		if _, err = s.activeNode(ctrl.Root); err != nil {
			return fail(err)
		}
	}
	s.currentFlag.Store(ctrl.Current)
	s.retiredFlag.Store(ctrl.Retired)
	if !metadata {
		s.startGC()
	}
	return s, nil
}

func (s *Store) readPage(page uint64) ([]byte, error) {
	if b, ok := s.cache.get(page); ok {
		s.stats.CacheHits++
		return b, nil
	}
	s.stats.CacheMisses++
	b, err := readFull(s.file, page)
	if err != nil {
		return nil, err
	}
	s.stats.IndexRead += PageSize
	s.cache.put(page, b)
	return b, nil
}
func (s *Store) readCheckpoint(slot uint64, t *transaction) (checkpoint, error) {
	var b []byte
	var err error
	if t != nil {
		b, err = t.read(slotPage(slot))
	} else {
		b, err = s.readPage(slotPage(slot))
	}
	if err != nil {
		return checkpoint{}, err
	}
	var cp checkpoint
	err = decodeJSON(b, "KDCP4", &cp)
	return cp, err
}

func punch(f *os.File, slot uint64) error {
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, int64(slotPage(slot))*PageSize, PageSize)
}
func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
