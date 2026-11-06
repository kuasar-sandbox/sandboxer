package versioned

import (
	"context"
	"errors"
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/presence"
	"hash/crc32"
	"io"
	"sort"
)

const maxEntries = 255
const maxDepth = 8

// MaxDiscardPages matches the existing 4 MiB guest DISCARD request bound.
// The metadata preflight may split a request further without changing the
// independent WritePages and GC batch limits.
const MaxDiscardPages = 1024

type entry struct{ key, slot uint64 }
type node struct {
	leaf    bool
	entries []entry
}

func encodeNode(n node) []byte {
	b := make([]byte, PageSize)
	copy(b, "KDBTREE4")
	le.PutUint16(b[8:], uint16(len(n.entries)))
	if n.leaf {
		b[10] = 1
	}
	for i, e := range n.entries {
		off := 16 + i*16
		le.PutUint64(b[off:], e.key)
		le.PutUint64(b[off+8:], e.slot)
	}
	h := crc32.NewIEEE()
	h.Write(b[:12])
	h.Write(b[16:])
	le.PutUint32(b[12:], h.Sum32())
	return b
}
func decodeNode(b []byte) (node, error) {
	if len(b) != PageSize || string(b[:8]) != "KDBTREE4" {
		return node{}, fmt.Errorf("diffstore: invalid B+ page")
	}
	h := crc32.NewIEEE()
	h.Write(b[:12])
	h.Write(b[16:])
	if h.Sum32() != le.Uint32(b[12:]) {
		return node{}, fmt.Errorf("diffstore: B+ checksum mismatch")
	}
	count := int(le.Uint16(b[8:]))
	if count == 0 || count > maxEntries || b[10] > 1 {
		return node{}, fmt.Errorf("diffstore: invalid B+ occupancy")
	}
	n := node{leaf: b[10] != 0, entries: make([]entry, count)}
	for i := range n.entries {
		off := 16 + i*16
		n.entries[i] = entry{le.Uint64(b[off:]), le.Uint64(b[off+8:])}
		if n.entries[i].slot == 0 || i > 0 && n.entries[i-1].key >= n.entries[i].key {
			return node{}, fmt.Errorf("diffstore: invalid B+ ordering")
		}
	}
	return n, nil
}

type pageReader interface{ read(uint64) ([]byte, error) }
type storeReader struct{ s *Store }

func (r storeReader) read(p uint64) ([]byte, error) { return r.s.readPage(p) }
func readNode(r pageReader, slot uint64) (node, error) {
	n, err := inspectNode(r, slot)
	if err == nil {
		n.entries = append([]entry(nil), n.entries...)
	}
	return n, err
}
func inspectNode(r pageReader, slot uint64) (node, error) {
	var cache *pageCache
	var stats *Stats
	var next, total uint64
	switch source := r.(type) {
	case storeReader:
		cache = source.s.cache
		stats = &source.s.stats
		next, total = source.s.ctrl.Next, uint64(source.s.Size()/PageSize)
	case *View:
		cache = source.cache
		next, total = source.next, source.Size()/PageSize
	case *transaction:
		if n, ok := source.nodes[slot]; ok {
			return n, nil
		}
		cache = source.store.cache
		stats = &source.store.stats
		next, total = source.ctrl.Next, uint64(source.store.Size()/PageSize)
	}
	if next != 0 && (slot == 0 || slot >= next) {
		return node{}, fmt.Errorf("diffstore: B+ slot outside allocation")
	}
	if cache != nil {
		if n, ok := cache.getNode(slotPage(slot)); ok {
			if stats != nil {
				stats.CacheHits++
			}
			return n, nil
		}
	}
	b, err := r.read(slotPage(slot))
	if err != nil {
		return node{}, err
	}
	n, err := decodeNode(b)
	if err == nil && next != 0 {
		// Validate once before caching the decoded node. A mapped page must
		// not bypass the capacity/allocation checks merely because P is set.
		for _, e := range n.entries {
			if e.key >= total || e.slot >= next {
				return node{}, fmt.Errorf("diffstore: B+ entry outside capacity or allocation")
			}
		}
	}
	if err == nil && cache != nil {
		cache.putNode(slotPage(slot), n)
	}
	return n, err
}
func lookup(r pageReader, root, key uint64) (uint64, error) {
	for depth := 0; root != 0; depth++ {
		if depth >= maxDepth {
			return 0, fmt.Errorf("diffstore: B+ depth exceeded")
		}
		n, err := inspectNode(r, root)
		if err != nil {
			return 0, err
		}
		i := sort.Search(len(n.entries), func(i int) bool { return n.entries[i].key >= key })
		if i == len(n.entries) {
			return 0, nil
		}
		e := n.entries[i]
		if n.leaf {
			if e.key == key {
				return e.slot, nil
			}
			return 0, nil
		}
		root = e.slot
	}
	return 0, nil
}

// successor traverses with a bounded recursion stack; there are no leaf links
// requiring additional CoW when adjacent leaves change.
func successor(r pageReader, root, key uint64, depth int) (entry, bool, error) {
	if root == 0 {
		return entry{}, false, nil
	}
	if depth >= maxDepth {
		return entry{}, false, fmt.Errorf("diffstore: B+ depth exceeded")
	}
	n, err := inspectNode(r, root)
	if err != nil {
		return entry{}, false, err
	}
	i := sort.Search(len(n.entries), func(i int) bool { return n.entries[i].key >= key })
	if n.leaf {
		if i == len(n.entries) {
			return entry{}, false, nil
		}
		return n.entries[i], true, nil
	}
	for ; i < len(n.entries); i++ {
		e, ok, err := successor(r, n.entries[i].slot, key, depth+1)
		if err != nil || ok {
			return e, ok, err
		}
	}
	return entry{}, false, nil
}
func walkTree(ctx context.Context, r pageReader, root uint64, visit func(entry) error, depth int) error {
	if root == 0 {
		return nil
	}
	if depth >= maxDepth {
		return fmt.Errorf("diffstore: B+ depth exceeded")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	n, err := inspectNode(r, root)
	if err != nil {
		return err
	}
	for _, e := range n.entries {
		if n.leaf {
			if err = visit(e); err != nil {
				return err
			}
		} else {
			if err = walkTree(ctx, r, e.slot, visit, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
func (t *transaction) writable(slot uint64, n node) (uint64, error) {
	private, err := t.private(slot)
	if err != nil {
		return 0, err
	}
	if private {
		return slot, nil
	}
	fresh, err := t.allocate(kindIndex)
	if err != nil {
		return 0, err
	}
	if err = t.retire(slot); err != nil {
		return 0, err
	}
	t.nodes[fresh] = n
	return fresh, nil
}
func (t *transaction) put(root, key, value uint64, depth int) ([]entry, error) {
	if depth >= maxDepth {
		return nil, fmt.Errorf("diffstore: B+ depth exceeded")
	}
	if root == 0 {
		slot, err := t.allocate(kindIndex)
		if err != nil {
			return nil, err
		}
		t.nodes[slot] = node{leaf: true, entries: []entry{{key, value}}}
		return []entry{{key, slot}}, nil
	}
	n, err := readNode(t, root)
	if err != nil {
		return nil, err
	}
	slot, err := t.writable(root, n)
	if err != nil {
		return nil, err
	}
	i := sort.Search(len(n.entries), func(i int) bool { return n.entries[i].key >= key })
	if n.leaf {
		if i < len(n.entries) && n.entries[i].key == key {
			n.entries[i].slot = value
		} else {
			n.entries = append(n.entries, entry{})
			copy(n.entries[i+1:], n.entries[i:])
			n.entries[i] = entry{key, value}
		}
	} else {
		if i == len(n.entries) {
			i--
		}
		children, err := t.put(n.entries[i].slot, key, value, depth+1)
		if err != nil {
			return nil, err
		}
		replacement := make([]entry, 0, len(n.entries)+1)
		replacement = append(replacement, n.entries[:i]...)
		replacement = append(replacement, children...)
		replacement = append(replacement, n.entries[i+1:]...)
		n.entries = replacement
	}
	if len(n.entries) <= maxEntries {
		t.nodes[slot] = n
		return []entry{{n.entries[len(n.entries)-1].key, slot}}, nil
	}
	mid := len(n.entries) / 2
	right, err := t.allocate(kindIndex)
	if err != nil {
		return nil, err
	}
	t.nodes[slot] = node{n.leaf, n.entries[:mid]}
	t.nodes[right] = node{n.leaf, n.entries[mid:]}
	return []entry{{n.entries[mid-1].key, slot}, {n.entries[len(n.entries)-1].key, right}}, nil
}
func (t *transaction) set(key, value uint64) error {
	children, err := t.put(t.ctrl.Root, key, value, 0)
	if err != nil {
		return err
	}
	if len(children) == 1 {
		t.ctrl.Root = children[0].slot
		return nil
	}
	root, err := t.allocate(kindIndex)
	if err != nil {
		return err
	}
	t.nodes[root] = node{entries: children}
	t.ctrl.Root = root
	return nil
}

// removeRange visits each intersecting subtree once. Entries returned for an
// unchanged subtree still borrow its original slot; all changed entry slices
// are private to this transaction. Payload retirement is deferred until the
// tree change is complete so descriptor pages can be updated together.
func (t *transaction) removeRange(root, start, end uint64, depth int, payloads *[]uint64) ([]entry, bool, error) {
	if root == 0 {
		return nil, false, nil
	}
	if depth >= maxDepth {
		return nil, false, fmt.Errorf("diffstore: B+ depth exceeded")
	}
	n, err := inspectNode(t, root)
	if err != nil {
		return nil, false, err
	}
	unchanged := func() ([]entry, bool, error) {
		return []entry{{n.entries[len(n.entries)-1].key, root}}, false, nil
	}
	first := sort.Search(len(n.entries), func(i int) bool { return n.entries[i].key >= start })
	if first == len(n.entries) {
		return unchanged()
	}
	var entries []entry
	if n.leaf {
		last := sort.Search(len(n.entries), func(i int) bool { return n.entries[i].key >= end })
		if first == last {
			return unchanged()
		}
		for _, e := range n.entries[first:last] {
			*payloads = append(*payloads, e.slot)
		}
		entries = make([]entry, 0, len(n.entries)-(last-first))
		entries = append(entries, n.entries[:first]...)
		entries = append(entries, n.entries[last:]...)
	} else {
		entries = make([]entry, 0, len(n.entries))
		entries = append(entries, n.entries[:first]...)
		changed, next := false, first
		for next < len(n.entries) {
			// Each separator is its child's maximum key. A later child's
			// lower bound is strictly greater than the preceding separator.
			if next > 0 && n.entries[next-1].key >= end-1 {
				break
			}
			children, childChanged, err := t.removeRange(n.entries[next].slot, start, end, depth+1, payloads)
			if err != nil {
				return nil, false, err
			}
			entries = append(entries, children...)
			changed = changed || childChanged
			next++
		}
		if !changed {
			return unchanged()
		}
		affectedEnd := len(entries)
		entries = append(entries, n.entries[next:]...)
		// Compact only the affected children and their two boundaries. As
		// in single-key removal, moving child pointers never retires them.
		for i := max(0, first-1); i+1 < len(entries) && i < affectedEnd; {
			left, err := inspectNode(t, entries[i].slot)
			if err != nil {
				return nil, false, err
			}
			right, err := inspectNode(t, entries[i+1].slot)
			if err != nil {
				return nil, false, err
			}
			if left.leaf != right.leaf {
				return nil, false, fmt.Errorf("diffstore: inconsistent B+ levels")
			}
			if len(left.entries)+len(right.entries) > maxEntries {
				i++
				continue
			}
			merged := node{leaf: left.leaf, entries: make([]entry, 0, len(left.entries)+len(right.entries))}
			merged.entries = append(merged.entries, left.entries...)
			merged.entries = append(merged.entries, right.entries...)
			slot, err := t.writable(entries[i].slot, merged)
			if err != nil {
				return nil, false, err
			}
			if err := t.retire(entries[i+1].slot); err != nil {
				return nil, false, err
			}
			t.nodes[slot] = merged
			entries[i] = entry{merged.entries[len(merged.entries)-1].key, slot}
			entries = append(entries[:i+1], entries[i+2:]...)
			affectedEnd--
		}
	}
	if len(entries) == 0 {
		// A fully removed node needs no writable copy. Its old contents
		// remain protected by the same epoch rule as any other retirement.
		if err := t.retire(root); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}
	n.entries = entries
	slot, err := t.writable(root, n)
	if err != nil {
		return nil, false, err
	}
	t.nodes[slot] = n
	return []entry{{entries[len(entries)-1].key, slot}}, true, nil
}

func (t *transaction) unsetRange(start, end uint64) error {
	if start >= end {
		return nil
	}
	var payloads []uint64
	children, changed, err := t.removeRange(t.ctrl.Root, start, end, 0, &payloads)
	if err != nil || !changed {
		return err
	}
	if len(children) == 0 {
		t.ctrl.Root = 0
	} else {
		t.ctrl.Root = children[0].slot
		// A range can remove several levels at once. Only the root may
		// collapse; internal single-child nodes keep their level.
		for t.ctrl.Root != 0 {
			n, err := inspectNode(t, t.ctrl.Root)
			if err != nil {
				return err
			}
			if n.leaf || len(n.entries) != 1 {
				break
			}
			old := t.ctrl.Root
			t.ctrl.Root = n.entries[0].slot
			if err := t.retire(old); err != nil {
				return err
			}
		}
	}
	return t.retirePayloads(payloads)
}

func (s *Store) WritePages(ctx context.Context, pages []Page) error {
	return s.writePages(ctx, pages, false)
}

func (s *Store) writePages(ctx context.Context, pages []Page, deferSpaceFailure bool) error {
	if err := WaitInitialized(s, ctx); err != nil {
		return err
	}
	if len(pages) == 0 {
		return ctx.Err()
	}
	if deferSpaceFailure && len(pages) > 4*MaxBatchPages {
		return fmt.Errorf("diffstore: cache Data window exceeds 256 pages")
	}
	for _, p := range pages {
		if len(p.Data) != PageSize || p.Block >= uint64(s.Size()/PageSize) {
			return fmt.Errorf("diffstore: invalid full page write")
		}
	}
	if err := s.MarkModified(ctx); err != nil {
		return err
	}
	for start := 0; start < len(pages); start += MaxBatchPages {
		part := pages[start:min(start+MaxBatchPages, len(pages))]
		s.mu.Lock()
		if err := s.healthy(ctx); err != nil {
			s.mu.Unlock()
			return err
		}
		if s.metadataOnly {
			s.mu.Unlock()
			return fmt.Errorf("diffstore: metadata-only handle cannot write payload")
		}
		// A direct Store caller may race Checkpoint between the optimistic
		// MarkModified and this write lock. Fence again before any data I/O.
		if s.ctrl.Current != 0 {
			if err := s.mutate(ctx, func(t *transaction) error { t.ctrl.Current = 0; return nil }); err != nil {
				s.mu.Unlock()
				return err
			}
		}
		err := s.mutateWithReservationPolicy(ctx, func(t *transaction) error {
			// Last write to a repeated block wins within this bounded batch.
			unique := make(map[uint64][]byte, len(part))
			order := make([]uint64, 0, len(part))
			for _, p := range part {
				if _, ok := unique[p.Block]; !ok {
					order = append(order, p.Block)
				}
				unique[p.Block] = p.Data
			}
			checked := append([]uint64(nil), order...)
			sort.Slice(checked, func(i, j int) bool { return checked[i] < checked[j] })
			for i := 0; i < len(checked); {
				j := i + 1
				for j < len(checked) && checked[j] == checked[j-1]+1 {
					j++
				}
				if err := s.validatePayloadRange(ctx, checked[i], checked[j-1]+1); err != nil {
					return err
				}
				i = j
			}
			// Reserve the batch's data slots before allocating any B+ pages. This
			// keeps unrelated logical pages physically adjacent whenever possible.
			var mappings []entry
			for _, block := range order {
				if err := t.setPresence(block); err != nil {
					return err
				}
				slot, err := lookup(t, t.ctrl.Root, block)
				if err != nil {
					return err
				}
				if slot != 0 {
					private, err := t.private(slot)
					if err != nil {
						return err
					}
					if private {
						t.data = append(t.data, dataWrite{slot, block, unique[block]})
						continue
					}
				}
				newSlot, err := t.allocate(kindData)
				if err != nil {
					return err
				}
				t.data = append(t.data, dataWrite{newSlot, block, unique[block]})
				if slot != 0 {
					if err = t.retire(slot); err != nil {
						return err
					}
				}
				mappings = append(mappings, entry{block, newSlot})
			}
			for _, mapping := range mappings {
				if err := t.set(mapping.key, mapping.slot); err != nil {
					return err
				}
			}
			return nil
		}, 0, deferSpaceFailure)
		if deferSpaceFailure && s.fatal == nil && spaceReservation(err) {
			err = &SpacePressureError{Err: err}
		}
		s.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) ReadAt(p []byte, off int64) (int, error) {
	if err := WaitInitialized(s, context.Background()); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthy(context.Background()); err != nil {
		return 0, err
	}
	if s.metadataOnly {
		return 0, fmt.Errorf("diffstore: metadata-only handle cannot read payload")
	}
	if off < 0 || off > s.Size() {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off == s.Size() {
		return 0, io.EOF
	}
	count := min(len(p), int(s.Size()-off))
	done := 0
	for done < count {
		pos := off + int64(done)
		block := uint64(pos / PageSize)
		inner := int(pos % PageSize)
		n := min(count-done, PageSize-inner)
		if !presence.Has(s.presence, block) {
			clear(p[done : done+n])
			done += n
			continue
		}
		slot, err := lookup(storeReader{s}, s.ctrl.Root, block)
		if err != nil {
			return done, s.fail(err)
		}
		if slot == 0 {
			// Only an absence answer needs the neighboring subtree checks.
			// Eagerly validating later pages would lose a readable prefix when
			// lookahead encounters corruption in a different leaf.
			if err := s.validatePayloadRange(context.Background(), block, block+1); err != nil {
				return done, err
			}
			clear(p[done : done+n])
			done += n
			continue
		}
		// Keep the active mapping and slot lifetimes protected through the
		// read and copy. Slots that differ by one need not be physical neighbors:
		// each allocator group has an intervening descriptor page.
		pages := 1
		var lookupErr error
		for pages < 256 && pages*PageSize-inner < count-done {
			if !presence.Has(s.presence, block+uint64(pages)) {
				break
			}
			var next uint64
			next, lookupErr = lookup(storeReader{s}, s.ctrl.Root, block+uint64(pages))
			if lookupErr != nil || next == 0 || slotPage(next) != slotPage(slot)+uint64(pages) {
				break
			}
			pages++
		}
		n, err = s.readData(p[done:count], slotPage(slot), block, pages, inner)
		done += n
		if err != nil {
			return done, s.fail(err)
		}
		// A lookahead failure must not discard the readable prefix collected
		// before it. No mapping beyond that prefix is consumed.
		if lookupErr != nil {
			return done, s.fail(lookupErr)
		}
	}
	if done < len(p) {
		return done, io.EOF
	}
	return done, nil
}
func (s *Store) Scan(ctx context.Context, visit func(uint64) error) error {
	if err := WaitInitialized(s, ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthy(ctx); err != nil {
		return err
	}
	if s.ctrl.Root == 0 {
		return nil
	}
	return s.scanActive(ctx, s.ctrl.Root, 0, uint64(s.Size()/PageSize)-1, 0, visit)
}

// Discard removes active data mappings and commits logical Zero in bounded
// batches. The range must contain aligned complete pages. Retained versions
// keep their protected records until independent deletion and GC can free them.
func (s *Store) Discard(ctx context.Context, off, length int64) error {
	return s.discardWithRedoBudget(ctx, off, length, maxLogPages)
}

// The budget argument lets deterministic tests exercise the same splitting
// path with small fixtures; production always uses the on-disk redo capacity.
func (s *Store) discardWithRedoBudget(ctx context.Context, off, length int64, redoBudget int) error {
	if redoBudget <= 0 || redoBudget > maxLogPages {
		return fmt.Errorf("diffstore: invalid discard redo budget")
	}
	if err := WaitInitialized(s, ctx); err != nil {
		return err
	}
	if off < 0 || length < 0 || off%PageSize != 0 || length%PageSize != 0 || off > s.Size() || length > s.Size()-off {
		return fmt.Errorf("diffstore: invalid discard range")
	}
	if length == 0 {
		return ctx.Err()
	}
	if err := s.MarkModified(ctx); err != nil {
		return err
	}
	pos := off
	end := min(off+length, pos+MaxDiscardPages*PageSize)
	for pos < off+length {
		s.mu.Lock()
		if err := s.healthy(ctx); err != nil {
			s.mu.Unlock()
			return err
		}
		if s.metadataOnly {
			s.mu.Unlock()
			return fmt.Errorf("diffstore: metadata-only handle cannot discard payload")
		}
		err := s.mutateWithRedoBudget(ctx, func(t *transaction) error {
			// A concurrent direct caller may checkpoint after the optimistic
			// MarkModified call. Publish invalidation with this mapping change.
			t.ctrl.Current = 0
			if err := s.validatePayloadRange(ctx, uint64(pos/PageSize), uint64(end/PageSize)); err != nil {
				return err
			}
			// mutate may retry after reservation pressure and foreground GC. Its
			// closure must not advance the caller's range before a commit succeeds.
			t.setPresenceRange(uint64(pos/PageSize), uint64(end/PageSize))
			return t.unsetRange(uint64(pos/PageSize), uint64(end/PageSize))
		}, redoBudget)
		s.mu.Unlock()
		var budget *discardRedoBudget
		if errors.As(err, &budget) && end-pos > PageSize {
			// Discard the uncommitted preparation and retry from the same
			// logical position. Reservation retries likewise rebuild it.
			pages := (end - pos) / PageSize
			end = pos + max(1, pages/2)*PageSize
			continue
		}
		if err != nil {
			return err
		}
		pos = end
		end = min(off+length, pos+MaxDiscardPages*PageSize)
	}
	return nil
}
