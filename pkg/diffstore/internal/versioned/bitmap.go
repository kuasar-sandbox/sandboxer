package versioned

import (
	"context"
	"fmt"
	"os"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/presence"
)

// The 259-page maximum bitmap fits between the existing redo area and record
// area. Only the pages required by this capacity are valid replay targets.
const presenceAreaPage = uint64(1600)

func writeInitialPresence(f *os.File, bitmap []byte, size int64) error {
	pages := presence.Pages(size)
	// Validated capacities are at most 32 GiB: all 259 encoded pages fit in
	// 1 MiB + 12 KiB, below the existing 8 MiB direct-I/O buffer limit.
	buf := make([]byte, pages*PageSize)
	for i := 0; i < pages; i++ {
		copy(buf[i*PageSize:], presence.Encode(bitmap, size, i))
	}
	return writeFull(f, buf, presenceAreaPage)
}

func presencePage(page uint64, size int64) bool {
	return page >= presenceAreaPage && page-presenceAreaPage < uint64(presence.Pages(size))
}

func loadPresence(read func(uint64) ([]byte, error), size int64) ([]byte, error) {
	bitmap := make([]byte, presence.Bytes(size))
	for i := 0; i < presence.Pages(size); i++ {
		b, err := read(presenceAreaPage + uint64(i))
		if err != nil {
			return nil, err
		}
		part, err := presence.Decode(b, size, i)
		if err != nil {
			return nil, err
		}
		copy(bitmap[i*presence.PayloadSize:], part)
	}
	return bitmap, nil
}

// PresenceBitmap returns a private complete logical-presence snapshot. Pending
// frontend writes are deliberately absent; only committed store state is read.
func (s *Store) PresenceBitmap(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthy(ctx); err != nil {
		return nil, err
	}
	return append([]byte(nil), s.presence...), nil
}

func (s *Store) Presence(ctx context.Context, start uint64, dst []bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthy(ctx); err != nil {
		return err
	}
	total := uint64(s.Size() / PageSize)
	if len(dst) > MaxPresencePages || start > total || uint64(len(dst)) > total-start {
		return fmt.Errorf("diffstore: invalid presence range")
	}
	for i := range dst {
		dst[i] = presence.Has(s.presence, start+uint64(i))
	}
	return nil
}

// setPresence merges only this operation's bit into the transaction page. It
// never copies an external frontend bitmap containing unflushed writes.
func (t *transaction) setPresence(block uint64) error {
	index := int(block/8) / presence.PayloadSize
	page := presenceAreaPage + uint64(index)
	b, ok := t.pages[page]
	if !ok {
		if presence.Has(t.store.presence, block) {
			return nil
		}
		b = presence.Encode(t.store.presence, t.store.Size(), index)
		t.pages[page] = b
	}
	part := b[presence.HeaderSize : PageSize-4]
	presence.Set(part, block-uint64(index*presence.PayloadSize)*8)
	sealPage(b)
	return nil
}

func (t *transaction) snapshotPresence() (uint64, error) {
	var head, previous uint64
	for i := 0; i < presence.Pages(t.store.Size()); i++ {
		slot, err := t.allocate(kindBitmap)
		if err != nil {
			return 0, err
		}
		t.pages[slotPage(slot)] = presence.Encode(t.store.presence, t.store.Size(), i)
		if previous == 0 {
			head = slot
		} else {
			d, err := t.desc(previous)
			if err != nil {
				return 0, err
			}
			d.Next = slot
			if err := t.setDesc(previous, d); err != nil {
				return 0, err
			}
		}
		previous = slot
	}
	return head, nil
}

func readSnapshotPresence(read func(uint64) ([]byte, error), cp checkpoint, size int64, next uint64) ([]byte, error) {
	if cp.BitmapDone != 0 {
		return nil, fmt.Errorf("diffstore: checkpoint bitmap is being reclaimed")
	}
	bitmap := make([]byte, presence.Bytes(size))
	slot := cp.Bitmap
	for i := 0; i < presence.Pages(size); i++ {
		if slot == 0 || slot >= next {
			return nil, fmt.Errorf("diffstore: invalid checkpoint bitmap link")
		}
		b, err := read(descriptorPage(slot))
		if err != nil {
			return nil, err
		}
		if err := checkPage(b); err != nil {
			return nil, err
		}
		o := descriptorOffset(slot)
		if le.Uint64(b[o:]) != cp.Epoch || le.Uint64(b[o+16:]) != kindBitmap {
			return nil, fmt.Errorf("diffstore: checkpoint bitmap ownership mismatch")
		}
		following := le.Uint64(b[o+8:])
		b, err = read(slotPage(slot))
		if err != nil {
			return nil, err
		}
		part, err := presence.Decode(b, size, i)
		if err != nil {
			return nil, err
		}
		copy(bitmap[i*presence.PayloadSize:], part)
		slot = following
	}
	if slot != 0 {
		return nil, fmt.Errorf("diffstore: excess checkpoint bitmap link")
	}
	return bitmap, nil
}

// releaseBitmap moves exclusively owned pages directly into global garbage.
// It never transfers them through the shared-version ownership algorithm.
func (t *transaction) releaseBitmap(cp *checkpoint, budget int) (int, error) {
	done := 0
	for cp.Bitmap != 0 && done < budget {
		if cp.BitmapDone >= uint64(presence.Pages(t.store.Size())) {
			return done, fmt.Errorf("diffstore: excess checkpoint bitmap")
		}
		slot := cp.Bitmap
		d, err := t.desc(slot)
		if err != nil {
			return done, err
		}
		if d.Kind != kindBitmap || d.Birth != cp.Epoch || d.Next == slot {
			return done, fmt.Errorf("diffstore: invalid checkpoint bitmap ownership")
		}
		b, err := t.read(slotPage(slot))
		if err != nil {
			return done, err
		}
		if _, err := presence.Decode(b, t.store.Size(), int(cp.BitmapDone)); err != nil {
			return done, err
		}
		cp.Bitmap, cp.BitmapDone = d.Next, cp.BitmapDone+1
		d.Kind, d.Next = kindGarbage, t.ctrl.Garbage
		t.ctrl.Garbage = slot
		if err := t.setDesc(slot, d); err != nil {
			return done, err
		}
		done++
	}
	if cp.Bitmap == 0 && cp.BitmapDone != uint64(presence.Pages(t.store.Size())) {
		return done, fmt.Errorf("diffstore: truncated checkpoint bitmap")
	}
	return done, nil
}
