package versioned

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/presence"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/sectorcrypt"
	"golang.org/x/sys/unix"
)

type record struct {
	page uint64
	data []byte
}
type dataWrite struct {
	slot, block uint64
	data        []byte
}

var errReservation = errors.New("diffstore: space reservation failed")

type transaction struct {
	store          *Store
	ctrl           control
	pages          map[uint64][]byte
	nodes          map[uint64]node
	protectedKnown bool
	protectedValue uint64
	fresh          map[uint64]bool
	allocated      []uint64
	data           []dataWrite
	committed      bool
}

func (s *Store) mutate(ctx context.Context, fn func(*transaction) error) error {
	for attempt := 0; ; attempt++ {
		err := s.mutateOnce(ctx, fn)
		if !errors.Is(err, errReservation) {
			return err
		}
		if attempt >= 2 {
			return s.fail(err)
		}
		// Reserved redo lets GC run even when new payload allocation fails.
		// Bound foreground assistance; retained pages are never sacrificed.
		var freed uint64
		for i := 0; i < 16 && (s.ctrl.Garbage != 0 || s.ctrl.Pending != 0); i++ {
			result, gcErr := s.gcLocked(ctx, MaxBatchPages)
			if gcErr != nil {
				return gcErr
			}
			freed += result.FreedSlots
			if result.State != More {
				break
			}
		}
		if freed == 0 {
			return s.fail(err)
		}
	}
}
func (s *Store) mutateOnce(ctx context.Context, fn func(*transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ofd(s.lock, unix.F_WRLCK, metadataByte, true); err != nil {
		return s.fail(err)
	}
	defer ofd(s.lock, unix.F_UNLCK, metadataByte, false)
	if err := ctx.Err(); err != nil {
		return err
	}
	t := &transaction{store: s, ctrl: s.ctrl, pages: make(map[uint64][]byte), fresh: make(map[uint64]bool), nodes: make(map[uint64]node)}
	if err := fn(t); err != nil {
		// Missing IDs and cancellation are ordinary request failures. Corruption,
		// lock errors and metadata/GC I/O errors make the owner unhealthy just as
		// failures in commit do, even if no transaction has been published yet.
		if errors.Is(err, ErrNotExist) || errors.Is(err, ErrExists) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return s.fail(err)
	}
	// Preparation has not published this transaction's allocations or mappings.
	// Cancel here so commit errors retain their storage-error classification,
	// including the private-overwrite path that needs no metadata transaction.
	if err := ctx.Err(); err != nil {
		return err
	}
	err := t.commit()
	if err != nil {
		// Before publishing redo, all allocated slots remain unreachable. Once the
		// commit write is attempted its result is uncertain: retain them for replay.
		if !t.committed {
			for _, slot := range t.allocated {
				_ = punch(s.file, slot)
				s.cache.remove(slotPage(slot))
			}
		}
		if errors.Is(err, errReservation) {
			return err
		}
		return s.fail(err)
	}
	return nil
}
func (t *transaction) read(page uint64) ([]byte, error) {
	if b, ok := t.pages[page]; ok {
		return b, nil
	}
	return t.store.readPage(page)
}
func (t *transaction) edit(page uint64) ([]byte, error) {
	if b, ok := t.pages[page]; ok {
		return b, nil
	}
	b, err := t.read(page)
	if err != nil {
		return nil, err
	}
	copyB := append([]byte(nil), b...)
	t.pages[page] = copyB
	return copyB, nil
}
func (t *transaction) desc(slot uint64) (descriptor, error) {
	if slot == 0 || slot >= t.ctrl.Next {
		return descriptor{}, fmt.Errorf("diffstore: slot %d out of bounds", slot)
	}
	b, err := t.read(descriptorPage(slot))
	if err != nil {
		return descriptor{}, err
	}
	if err = checkPage(b); err != nil {
		return descriptor{}, err
	}
	o := descriptorOffset(slot)
	return descriptor{le.Uint64(b[o:]), le.Uint64(b[o+8:]), le.Uint64(b[o+16:])}, nil
}
func (t *transaction) setDesc(slot uint64, d descriptor) error {
	p := descriptorPage(slot)
	b, err := t.edit(p)
	if err != nil {
		return err
	}
	o := descriptorOffset(slot)
	le.PutUint64(b[o:], d.Birth)
	le.PutUint64(b[o+8:], d.Next)
	le.PutUint64(b[o+16:], d.Kind)
	sealPage(b)
	return nil
}
func (t *transaction) allocate(kind uint64) (uint64, error) {
	var slot uint64
	if t.ctrl.Free != 0 {
		slot = t.ctrl.Free
		d, err := t.desc(slot)
		if err != nil {
			return 0, err
		}
		if d.Kind != kindFree {
			return 0, fmt.Errorf("diffstore: free chain contains allocated slot")
		}
		t.ctrl.Free = d.Next
		t.store.stats.Reused++
	} else {
		slot = t.ctrl.Next
		if slot > uint64((int64(^uint64(0)>>1)/PageSize-slotAreaPage)/(slotsPerGroup+1)*slotsPerGroup) {
			return 0, fmt.Errorf("diffstore: physical address exhausted")
		}
		t.ctrl.Next++
		if (slot-1)%slotsPerGroup == 0 {
			b := make([]byte, PageSize)
			copy(b, "KDDESC4")
			sealPage(b)
			t.pages[descriptorPage(slot)] = b
			t.fresh[descriptorPage(slot)] = true
		}
	}
	if err := t.setDesc(slot, descriptor{Birth: t.ctrl.Epoch, Kind: kind}); err != nil {
		return 0, err
	}
	t.fresh[slotPage(slot)] = true
	t.allocated = append(t.allocated, slot)
	t.store.cache.remove(slotPage(slot))
	t.store.stats.Allocated++
	return slot, nil
}
func (t *transaction) putCheckpoint(slot uint64, cp checkpoint) error {
	b, err := encodeJSON("KDCP4", cp)
	if err == nil {
		t.pages[slotPage(slot)] = b
	}
	return err
}
func (t *transaction) protectedEpoch() (uint64, error) {
	if t.protectedKnown {
		return t.protectedValue, nil
	}
	if t.ctrl.Protected == 0 {
		t.protectedKnown = true
		return 0, nil
	}
	cp, err := t.store.readCheckpoint(t.ctrl.Protected, t)
	if err == nil {
		t.protectedKnown = true
		t.protectedValue = cp.Epoch
	}
	return cp.Epoch, err
}
func (t *transaction) private(slot uint64) (bool, error) {
	d, err := t.desc(slot)
	if err != nil {
		return false, err
	}
	e, err := t.protectedEpoch()
	return d.Birth > e, err
}
func (t *transaction) retire(slot uint64) error {
	if slot == 0 {
		return nil
	}
	d, err := t.desc(slot)
	if err != nil {
		return err
	}
	if d.Kind == kindGarbage || d.Kind == kindFree {
		return fmt.Errorf("diffstore: double retirement of slot %d", slot)
	}
	if d.Kind == kindIndex {
		delete(t.nodes, slot)
		delete(t.pages, slotPage(slot))
	}
	e, err := t.protectedEpoch()
	if err != nil {
		return err
	}
	if t.ctrl.Protected != 0 && d.Birth <= e {
		cp, err := t.store.readCheckpoint(t.ctrl.Protected, t)
		if err != nil {
			return err
		}
		d.Next = cp.Garbage
		cp.Garbage = slot
		if err = t.putCheckpoint(t.ctrl.Protected, cp); err != nil {
			return err
		}
	} else {
		d.Next = t.ctrl.Garbage
		t.ctrl.Garbage = slot
	}
	d.Kind = kindGarbage
	return t.setDesc(slot, d)
}

// commit finishes the current transaction without consulting caller cancellation.
// The caller checks cancellation before entry; later batches check it again.
func (t *transaction) commit() error {
	s := t.store
	for slot, n := range t.nodes {
		t.pages[slotPage(slot)] = encodeNode(n)
	}
	if len(t.pages) == 0 && t.ctrl == s.ctrl {
		return t.writeData()
	}
	t.ctrl.Tx++
	ctrl, err := encodeJSON("KDCONT4", t.ctrl)
	if err != nil {
		return err
	}
	t.pages[1] = ctrl
	var existing, newPages []record
	for p, b := range t.pages {
		if t.fresh[p] {
			newPages = append(newPages, record{p, b})
		} else {
			existing = append(existing, record{p, b})
		}
	}
	// This is checked before *any* data or metadata write. No partial structure
	// transaction may be split to fit the bounded redo area.
	if len(existing) > maxLogPages {
		return fmt.Errorf("diffstore: transaction exceeds redo budget (%d)", len(existing))
	}
	sort.Slice(newPages, func(i, j int) bool { return newPages[i].page < newPages[j].page })
	sort.Slice(existing, func(i, j int) bool { return existing[i].page < existing[j].page })
	if err = t.reserve(newPages); err != nil {
		return err
	}
	if err = t.writeData(); err != nil {
		return err
	}
	for _, r := range newPages {
		if err = s.hook("new-index"); err != nil {
			return err
		}
		if err = writeFull(s.file, r.data, r.page); err != nil {
			return err
		}
		s.stats.IndexWritten += PageSize
	}
	if err = s.hook("redo-reset"); err != nil {
		return err
	}
	if err = writeFull(s.file, make([]byte, PageSize), logCommitPage); err != nil {
		return err
	}
	s.stats.RedoWritten += PageSize
	index := make([]byte, logIndexPages*PageSize)
	hash := sha256.New()
	for i, r := range existing {
		le.PutUint64(index[i*8:], r.page)
	}
	hash.Write(index)
	if err = s.hook("redo-index"); err != nil {
		return err
	}
	if err = writeFull(s.file, index, logIndexPage); err != nil {
		return err
	}
	s.stats.RedoWritten += uint64(len(index))
	// A single aligned aggregate keeps redo writeback from degenerating to one
	// syscall per metadata page. New unreachable pages are never duplicated here.
	payload := make([]byte, len(existing)*PageSize)
	for i, r := range existing {
		copy(payload[i*PageSize:], r.data)
		hash.Write(r.data)
	}
	if err = s.hook("redo-images"); err != nil {
		return err
	}
	if err = writeFull(s.file, payload, logImagesPage); err != nil {
		return err
	}
	s.stats.RedoWritten += uint64(len(payload))
	commit := make([]byte, PageSize)
	copy(commit, "KDREDO4")
	le.PutUint64(commit[8:], t.ctrl.Tx)
	le.PutUint64(commit[16:], uint64(len(existing)))
	copy(commit[24:], hash.Sum(nil))
	sealPage(commit)
	if err = s.hook("redo-commit"); err != nil {
		return err
	}
	t.committed = true
	if err = writeFull(s.file, commit, logCommitPage); err != nil {
		return err
	}
	s.stats.RedoWritten += PageSize
	for _, r := range existing {
		if err = s.hook("apply"); err != nil {
			return err
		}
		if err = writeFull(s.file, r.data, r.page); err != nil {
			return err
		}
		s.stats.IndexWritten += PageSize
	}
	if err = s.hook("redo-clear"); err != nil {
		return err
	}
	if err = writeFull(s.file, make([]byte, PageSize), logCommitPage); err != nil {
		return err
	}
	s.stats.RedoWritten += PageSize
	s.ctrl = t.ctrl
	s.currentFlag.Store(t.ctrl.Current)
	s.retiredFlag.Store(t.ctrl.Retired)
	for p, b := range t.pages {
		if presencePage(p, s.Size()) {
			i := int(p - presenceAreaPage)
			n := min(presence.PayloadSize, len(s.presence)-i*presence.PayloadSize)
			copy(s.presence[i*presence.PayloadSize:], b[presence.HeaderSize:presence.HeaderSize+n])
		}
		s.cache.put(p, b)
	}
	s.stats.Commits++
	return nil
}

func (t *transaction) reserve(newPages []record) error {
	pages := make([]uint64, 0, len(t.allocated)+len(newPages))
	for _, slot := range t.allocated {
		pages = append(pages, slotPage(slot))
	}
	for _, r := range newPages {
		pages = append(pages, r.page)
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i] < pages[j] })
	for start := 0; start < len(pages); {
		end := start + 1
		last := pages[start]
		for end < len(pages) && pages[end] <= last+1 {
			last = pages[end]
			end++
		}
		if err := t.store.hook("reserve"); err != nil {
			return errors.Join(errReservation, err)
		}
		err := unix.Fallocate(int(t.store.file.Fd()), unix.FALLOC_FL_KEEP_SIZE, int64(pages[start])*PageSize, int64(last-pages[start]+1)*PageSize)
		if err != nil {
			if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
				return errors.Join(errReservation, err)
			}
			return err
		}
		start = end
	}
	return nil
}

func (t *transaction) writeData() error {
	s := t.store
	sort.Slice(t.data, func(i, j int) bool { return t.data[i].slot < t.data[j].slot })
	for start := 0; start < len(t.data); {
		end := start + 1
		for end < len(t.data) && slotPage(t.data[end].slot) == slotPage(t.data[end-1].slot)+1 {
			end++
		}
		b := make([]byte, (end-start)*PageSize)
		for i := start; i < end; i++ {
			dst := b[(i-start)*PageSize : (i-start+1)*PageSize]
			copy(dst, t.data[i].data)
			s.crypt(dst, t.data[i].block, true)
		}
		if err := s.hook("data"); err != nil {
			return err
		}
		if err := writeFull(s.file, b, slotPage(t.data[start].slot)); err != nil {
			return err
		}
		s.stats.DataWritten += uint64(len(b))
		clear(b)
		start = end
	}
	return nil
}
func (s *Store) crypt(b []byte, block uint64, encrypt bool) {
	sectorcrypt.Transform(s.cipher, b[:PageSize], block*(PageSize/sectorcrypt.UnitSize), encrypt)
}

func readRedo(f *os.File, stats *Stats, size int64) ([]record, error) {
	read := func(page uint64, n int) ([]byte, error) {
		b, err := directRead(f, n, int64(page)*PageSize)
		if err != nil {
			return nil, err
		}
		if stats != nil {
			stats.RedoRead += uint64(n)
		}
		return b, nil
	}
	commit, err := read(logCommitPage, PageSize)
	if err != nil {
		return nil, err
	}
	if isZero(commit) {
		return nil, nil
	}
	// A torn commit descriptor is an unpublished transaction. Its home pages
	// were not touched until the whole descriptor write completed.
	if checkPage(commit) != nil || !bytes.Equal(commit[:7], []byte("KDREDO4")) {
		return nil, nil
	}
	count := le.Uint64(commit[16:])
	if count == 0 || count > maxLogPages {
		return nil, fmt.Errorf("diffstore: invalid redo count")
	}
	index, err := read(logIndexPage, logIndexPages*PageSize)
	if err != nil {
		return nil, err
	}
	body, err := read(logImagesPage, int(count)*PageSize)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	hash.Write(index)
	hash.Write(body)
	if !bytes.Equal(hash.Sum(nil), commit[24:56]) {
		return nil, fmt.Errorf("diffstore: committed redo checksum mismatch")
	}
	out := make([]record, count)
	seen := make(map[uint64]bool, len(out))
	for i := range out {
		p := le.Uint64(index[i*8:])
		if p == 0 || (p >= logCommitPage && p < slotAreaPage && !presencePage(p, size)) || seen[p] {
			return nil, fmt.Errorf("diffstore: invalid redo destination")
		}
		seen[p] = true
		out[i] = record{p, append([]byte(nil), body[i*PageSize:(i+1)*PageSize]...)}
	}
	return out, nil
}
