package uffd

import "sync"

// PageState packed in a byte.
type PageState uint8

const (
	StateAbsent   PageState = 0
	StateLoaded   PageState = 1
	StateReleased PageState = 2
)

// PageStateMap holds one byte per page. Concurrent access is guarded by
// a single RWMutex — Get/Set/CAS hold the lock briefly. With per-page
// hashing in the worker dispatcher (same page idx routes to same
// worker), same-page contention is rare; cross-page concurrent updates
// are common but each worker's update is microseconds.
type PageStateMap struct {
	mu    sync.RWMutex
	pages []uint8
}

// NewPageStateMap allocates a state table for n pages, all StateAbsent.
func NewPageStateMap(n int) *PageStateMap {
	return &PageStateMap{pages: make([]uint8, n)}
}

// Get returns the current state of pageIdx.
func (m *PageStateMap) Get(pageIdx uint64) PageState {
	if pageIdx >= uint64(len(m.pages)) {
		return StateAbsent
	}
	m.mu.RLock()
	v := m.pages[pageIdx]
	m.mu.RUnlock()
	return PageState(v)
}

// Set unconditionally writes the new state.
func (m *PageStateMap) Set(pageIdx uint64, s PageState) {
	if pageIdx >= uint64(len(m.pages)) {
		return
	}
	m.mu.Lock()
	m.pages[pageIdx] = uint8(s)
	m.mu.Unlock()
}

// CompareAndSwap returns true if the old state was old and the new
// value was stored.
func (m *PageStateMap) CompareAndSwap(pageIdx uint64, old, new PageState) bool {
	if pageIdx >= uint64(len(m.pages)) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pages[pageIdx] != uint8(old) {
		return false
	}
	m.pages[pageIdx] = uint8(new)
	return true
}

// SetRange writes state to every page in [startIdx, endIdx).
func (m *PageStateMap) SetRange(startIdx, endIdx uint64, s PageState) {
	if startIdx >= uint64(len(m.pages)) || startIdx >= endIdx {
		return
	}
	if endIdx > uint64(len(m.pages)) {
		endIdx = uint64(len(m.pages))
	}
	v := uint8(s)
	m.mu.Lock()
	for i := startIdx; i < endIdx; i++ {
		m.pages[i] = v
	}
	m.mu.Unlock()
}

// RunLength returns the number of consecutive pages equal to want beginning at
// startIdx, capped at max. The whole scan holds one read lock.
func (m *PageStateMap) RunLength(startIdx, max uint64, want PageState) uint64 {
	if max == 0 || startIdx >= uint64(len(m.pages)) {
		return 0
	}
	endIdx := uint64(len(m.pages))
	if max < endIdx-startIdx {
		endIdx = startIdx + max
	}
	wantByte := uint8(want)
	m.mu.RLock()
	i := startIdx
	for i < endIdx && m.pages[i] == wantByte {
		i++
	}
	m.mu.RUnlock()
	return i - startIdx
}

// RunBounds returns the maximal consecutive range [startIdx,endIdx) equal to
// want, containing anchorIdx and bounded by [minIdx,maxIdx). Both directions
// are scanned under one read lock so the ChunkRun fault path does not acquire a
// lock once per candidate page. If anchorIdx is out of range or does not equal
// want, the returned range is empty at anchorIdx.
func (m *PageStateMap) RunBounds(anchorIdx, minIdx, maxIdx uint64, want PageState) (startIdx, endIdx uint64) {
	pageCount := uint64(len(m.pages))
	if maxIdx > pageCount {
		maxIdx = pageCount
	}
	if minIdx > anchorIdx || anchorIdx >= maxIdx || anchorIdx >= pageCount {
		return anchorIdx, anchorIdx
	}
	wantByte := uint8(want)
	m.mu.RLock()
	if m.pages[anchorIdx] != wantByte {
		m.mu.RUnlock()
		return anchorIdx, anchorIdx
	}
	startIdx = anchorIdx
	for startIdx > minIdx && m.pages[startIdx-1] == wantByte {
		startIdx--
	}
	endIdx = anchorIdx + 1
	for endIdx < maxIdx && m.pages[endIdx] == wantByte {
		endIdx++
	}
	m.mu.RUnlock()
	return startIdx, endIdx
}

// SetRangeIf changes each page in [startIdx,endIdx) that still equals old to
// new and returns the number of pages changed. The conditional commit holds one
// write lock, preventing a stale tail completion from overwriting a concurrent
// EVENT_REMOVE transition to StateReleased.
func (m *PageStateMap) SetRangeIf(startIdx, endIdx uint64, old, new PageState) uint64 {
	if startIdx >= uint64(len(m.pages)) || startIdx >= endIdx {
		return 0
	}
	if endIdx > uint64(len(m.pages)) {
		endIdx = uint64(len(m.pages))
	}
	oldByte := uint8(old)
	newByte := uint8(new)
	var changed uint64
	m.mu.Lock()
	for i := startIdx; i < endIdx; i++ {
		if m.pages[i] == oldByte {
			m.pages[i] = newByte
			changed++
		}
	}
	m.mu.Unlock()
	return changed
}

// Len returns the number of tracked pages.
func (m *PageStateMap) Len() int { return len(m.pages) }
