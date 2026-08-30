package uffd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/codec"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"golang.org/x/sys/unix"
)

const unitCHVA = uint64(0x4000_0000)

func TestChunkRunBufferedTail(t *testing.T) {
	plain := bytes.Repeat([]byte{0x6c}, chunkFaultFillBytes)
	m := &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(plain)),
		Entries: []codec.ChunkEntry{{
			Offset:         0,
			Size:           uint32(len(plain)),
			CiphertextHash: sha256.Sum256(plain),
		}},
	}
	stream, getter := openSnapshotManifest(t, m, map[store.ContentKey][]byte{sha256.Sum256(plain): plain})
	source, err := NewStreamSnapshotSource(stream, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingSnapshotReader{source: source}
	h := newUnitHandler(t, uint64(len(plain)), counted)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 9}, make([]byte, PageSize))
	waitUnitTail(t, h)

	if got := getter.chunkCalls.Load(); got != 1 {
		t.Fatalf("chunk Get calls = %d, want 1", got)
	}
	if got := counted.calls.Load(); got != 1 {
		t.Fatalf("snapshot RunAt calls = %d, want 1", got)
	}
	if got := counted.lastLimit.Load(); got != ordinaryDataFaultFillBytes {
		t.Fatalf("snapshot RunAt limit = %d, want %d", got, ordinaryDataFaultFillBytes)
	}
	calls := ioctls.snapshot()
	if len(calls) != 2 || calls[0].kind != "copy" || calls[0].length != PageSize || calls[1].length != chunkNeighborTailBytes {
		t.Fatalf("ioctl calls = %+v, want urgent 4KiB then buffered ChunkRun tail", calls)
	}
	if !bytes.Equal(calls[0].data, plain[:PageSize]) || !bytes.Equal(calls[1].data, plain[PageSize:]) {
		t.Fatal("buffered ChunkRun data was not reused for the tail")
	}
	for page := uint64(0); page < chunkFaultFillBytes/PageSize; page++ {
		if got := h.state.Get(page); got != StateLoaded {
			t.Fatalf("page %d state = %v, want Loaded", page, got)
		}
	}
	stats := h.Stats()
	if stats["source_read_calls"] != 1 || stats["source_read_bytes"] != chunkFaultFillBytes {
		t.Fatalf("source metrics = %d calls/%d bytes", stats["source_read_calls"], stats["source_read_bytes"])
	}
	if stats["tail_buffered_data"] != 1 || stats["tail_pages_completed"] != chunkNeighborTailBytes/PageSize || stats["pages_copied"] != chunkFaultFillBytes/PageSize {
		t.Fatalf("tail/copy metrics = %#v", stats)
	}
}

func TestChunkRunBusyReadsOnlyUrgentPage(t *testing.T) {
	plain := bytes.Repeat([]byte{0x2d}, 3*PageSize)
	m := &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(plain)),
		Entries: []codec.ChunkEntry{{
			Offset:         0,
			Size:           uint32(len(plain)),
			CiphertextHash: sha256.Sum256(plain),
		}},
	}
	stream, getter := openSnapshotManifest(t, m, map[store.ContentKey][]byte{sha256.Sum256(plain): plain})
	source, err := NewStreamSnapshotSource(stream, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	h := newUnitHandler(t, uint64(len(plain)), source)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()
	h.tailBusy.Store(true)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 9}, make([]byte, PageSize))
	h.tailBusy.Store(false)

	if got := getter.chunkCalls.Load(); got != 1 {
		t.Fatalf("chunk Get calls = %d, want 1", got)
	}
	stats := h.Stats()
	if stats["source_read_bytes"] != PageSize || stats["tail_dropped_busy"] != 1 || stats["tail_submitted"] != 0 {
		t.Fatalf("busy-path metrics = %#v", stats)
	}
	if h.state.Get(0) != StateLoaded || h.state.Get(1) != StateAbsent {
		t.Fatalf("states = %v/%v, want Loaded/Absent", h.state.Get(0), h.state.Get(1))
	}
	if calls := ioctls.snapshot(); len(calls) != 1 || calls[0].length != PageSize {
		t.Fatalf("ioctl calls = %+v", calls)
	}
}

func TestChunkRunFillsWholeVisibleChunkAroundFault(t *testing.T) {
	plain := make([]byte, chunkFaultFillBytes)
	for page := range uint64(chunkFaultFillBytes / PageSize) {
		start := page * PageSize
		for i := start; i < start+PageSize; i++ {
			plain[i] = byte(page)
		}
	}
	hash := sha256.Sum256(plain)
	m := &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(plain)),
		Entries: []codec.ChunkEntry{{
			Offset:         0,
			Size:           uint32(len(plain)),
			CiphertextHash: hash,
		}},
	}
	stream, getter := openSnapshotManifest(t, m, map[store.ContentKey][]byte{hash: plain})
	source, err := NewStreamSnapshotSource(stream, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	h := newUnitHandler(t, uint64(len(plain)), source)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	const faultPage = uint64(128)
	h.handleFault(faultEvent{address: unitCHVA + faultPage*PageSize, uffdFD: 9}, make([]byte, PageSize))
	waitUnitTail(t, h)

	if got := getter.chunkCalls.Load(); got != 1 {
		t.Fatalf("chunk Get calls = %d, want 1", got)
	}
	calls := ioctls.snapshot()
	if len(calls) != 3 {
		t.Fatalf("ioctl call count = %d, want urgent, suffix, prefix", len(calls))
	}
	wantSuffix := uint64(chunkFaultFillBytes) - (faultPage+1)*PageSize
	wantPrefix := faultPage * PageSize
	if calls[0].kind != "copy" || calls[0].dst != unitCHVA+faultPage*PageSize || calls[0].length != PageSize {
		t.Fatalf("urgent call = kind %s dst 0x%x length %d", calls[0].kind, calls[0].dst, calls[0].length)
	}
	if calls[1].kind != "copy" || calls[1].dst != unitCHVA+(faultPage+1)*PageSize || calls[1].length != wantSuffix {
		t.Fatalf("suffix call = kind %s dst 0x%x length %d, want dst 0x%x length %d", calls[1].kind, calls[1].dst, calls[1].length, unitCHVA+(faultPage+1)*PageSize, wantSuffix)
	}
	if calls[2].kind != "copy" || calls[2].dst != unitCHVA || calls[2].length != wantPrefix {
		t.Fatalf("prefix call = kind %s dst 0x%x length %d, want dst 0x%x length %d", calls[2].kind, calls[2].dst, calls[2].length, unitCHVA, wantPrefix)
	}
	if !bytes.Equal(calls[0].data, plain[faultPage*PageSize:(faultPage+1)*PageSize]) ||
		!bytes.Equal(calls[1].data, plain[(faultPage+1)*PageSize:]) ||
		!bytes.Equal(calls[2].data, plain[:faultPage*PageSize]) {
		t.Fatal("urgent/suffix/prefix calls did not reuse the correct slices of the one chunk buffer")
	}
	for page := uint64(0); page < chunkFaultFillBytes/PageSize; page++ {
		if got := h.state.Get(page); got != StateLoaded {
			t.Fatalf("page %d state = %v, want Loaded", page, got)
		}
	}
	stats := h.Stats()
	if stats["source_read_calls"] != 1 || stats["source_read_bytes"] != chunkFaultFillBytes {
		t.Fatalf("source metrics = %d calls/%d bytes, want 1/%d", stats["source_read_calls"], stats["source_read_bytes"], chunkFaultFillBytes)
	}
	if stats["tail_pages_completed"] != chunkNeighborTailBytes/PageSize || stats["pages_copied"] != chunkFaultFillBytes/PageSize {
		t.Fatalf("bidirectional tail metrics = %#v", stats)
	}
}

func TestChunkRunStopsAtStateAndCHRegionBoundaries(t *testing.T) {
	plain := bytes.Repeat([]byte{0x3c}, chunkFaultFillBytes)
	hash := sha256.Sum256(plain)
	const boundaryPages = uint64(10)

	for _, tt := range []struct {
		name              string
		configure         func(*testing.T, *Handler)
		wantBoundaryState PageState
	}{
		{
			name: "page state",
			configure: func(t *testing.T, h *Handler) {
				t.Helper()
				if err := h.addrMap.RegisterVMA(ProcessCH, unitCHVA, chunkFaultFillBytes, 0); err != nil {
					t.Fatal(err)
				}
				h.state.Set(boundaryPages, StateLoaded)
			},
			wantBoundaryState: StateLoaded,
		},
		{
			name: "CH region",
			configure: func(t *testing.T, h *Handler) {
				t.Helper()
				firstBytes := boundaryPages * PageSize
				if err := h.addrMap.RegisterVMA(ProcessCH, unitCHVA, firstBytes, 0); err != nil {
					t.Fatal(err)
				}
				if err := h.addrMap.RegisterVMA(ProcessCH, unitCHVA+0x20_0000, chunkFaultFillBytes-firstBytes, firstBytes); err != nil {
					t.Fatal(err)
				}
			},
			wantBoundaryState: StateAbsent,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := &codec.Manifest{
				Version:   codec.Version1,
				ImageSize: uint64(len(plain)),
				Entries: []codec.ChunkEntry{{
					Offset:         0,
					Size:           uint32(len(plain)),
					CiphertextHash: hash,
				}},
			}
			stream, getter := openSnapshotManifest(t, m, map[store.ContentKey][]byte{hash: plain})
			source, err := NewStreamSnapshotSource(stream, uint64(len(plain)))
			if err != nil {
				t.Fatal(err)
			}
			h := newUnitHandlerWithoutMap(t, uint64(len(plain)), source)
			tt.configure(t, h)
			ioctls := newFakeIoctls()
			h.ops = ioctls.ops()
			startUnitTail(t, h)

			h.handleFault(faultEvent{address: unitCHVA, uffdFD: 9}, make([]byte, PageSize))
			waitUnitTail(t, h)

			if got := getter.chunkCalls.Load(); got != 1 {
				t.Fatalf("chunk Get calls = %d, want 1", got)
			}
			wantBytes := boundaryPages * PageSize
			calls := ioctls.snapshot()
			if len(calls) != 2 || calls[0].length != PageSize || calls[1].length != wantBytes-PageSize {
				t.Fatalf("ioctl calls = %+v, want fill clipped at %d bytes", calls, wantBytes)
			}
			if got := h.Stats()["source_read_bytes"]; got != chunkFaultFillBytes {
				t.Fatalf("source read bytes = %d, want whole %d-byte chunk", got, chunkFaultFillBytes)
			}
			if got := h.state.Get(boundaryPages); got != tt.wantBoundaryState {
				t.Fatalf("boundary page state = %v, want %v", got, tt.wantBoundaryState)
			}
			if got := h.state.Get(boundaryPages + 1); got != StateAbsent {
				t.Fatalf("page after boundary = %v, want Absent", got)
			}
		})
	}
}

func TestChunkRunBidirectionalTailStopsAtBothBoundaries(t *testing.T) {
	plain := bytes.Repeat([]byte{0x58}, chunkFaultFillBytes)
	hash := sha256.Sum256(plain)
	const (
		faultPage = uint64(128)
		leftPage  = uint64(96)
		rightPage = uint64(160)
	)

	for _, tt := range []struct {
		name       string
		configure  func(*testing.T, *Handler) uint64
		fillStart  uint64
		fillEnd    uint64
		leftState  PageState
		rightState PageState
	}{
		{
			name: "page state",
			configure: func(t *testing.T, h *Handler) uint64 {
				t.Helper()
				if err := h.addrMap.RegisterVMA(ProcessCH, unitCHVA, chunkFaultFillBytes, 0); err != nil {
					t.Fatal(err)
				}
				h.state.Set(leftPage, StateReleased)
				h.state.Set(rightPage, StateLoaded)
				return unitCHVA + faultPage*PageSize
			},
			fillStart:  leftPage + 1,
			fillEnd:    rightPage,
			leftState:  StateReleased,
			rightState: StateLoaded,
		},
		{
			name: "CH region",
			configure: func(t *testing.T, h *Handler) uint64 {
				t.Helper()
				if err := h.addrMap.RegisterVMA(ProcessCH, unitCHVA, (rightPage-leftPage)*PageSize, leftPage*PageSize); err != nil {
					t.Fatal(err)
				}
				return unitCHVA + (faultPage-leftPage)*PageSize
			},
			fillStart:  leftPage,
			fillEnd:    rightPage,
			leftState:  StateLoaded,
			rightState: StateAbsent,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stream, getter := openSnapshotManifest(t, &codec.Manifest{
				Version:   codec.Version1,
				ImageSize: uint64(len(plain)),
				Entries: []codec.ChunkEntry{{
					Offset:         0,
					Size:           uint32(len(plain)),
					CiphertextHash: hash,
				}},
			}, map[store.ContentKey][]byte{hash: plain})
			source, err := NewStreamSnapshotSource(stream, uint64(len(plain)))
			if err != nil {
				t.Fatal(err)
			}
			h := newUnitHandlerWithoutMap(t, uint64(len(plain)), source)
			faultVA := tt.configure(t, h)
			ioctls := newFakeIoctls()
			h.ops = ioctls.ops()
			startUnitTail(t, h)

			h.handleFault(faultEvent{address: faultVA, uffdFD: 9}, make([]byte, PageSize))
			waitUnitTail(t, h)

			if got := getter.chunkCalls.Load(); got != 1 {
				t.Fatalf("chunk Get calls = %d, want 1", got)
			}
			calls := ioctls.snapshot()
			if len(calls) != 3 {
				t.Fatalf("ioctl call count = %d, want urgent, suffix, prefix", len(calls))
			}
			wantSuffix := (tt.fillEnd - faultPage - 1) * PageSize
			wantPrefix := (faultPage - tt.fillStart) * PageSize
			if calls[0].dst != faultVA || calls[0].length != PageSize {
				t.Fatalf("urgent call dst/length = 0x%x/%d, want 0x%x/%d", calls[0].dst, calls[0].length, faultVA, PageSize)
			}
			if calls[1].dst != faultVA+PageSize || calls[1].length != wantSuffix {
				t.Fatalf("suffix call dst/length = 0x%x/%d, want 0x%x/%d", calls[1].dst, calls[1].length, faultVA+PageSize, wantSuffix)
			}
			if calls[2].dst != faultVA-wantPrefix || calls[2].length != wantPrefix {
				t.Fatalf("prefix call dst/length = 0x%x/%d, want 0x%x/%d", calls[2].dst, calls[2].length, faultVA-wantPrefix, wantPrefix)
			}
			for page := tt.fillStart; page < tt.fillEnd; page++ {
				if got := h.state.Get(page); got != StateLoaded {
					t.Fatalf("page %d state = %v, want Loaded", page, got)
				}
			}
			if got := h.state.Get(leftPage); got != tt.leftState {
				t.Fatalf("left boundary state = %v, want %v", got, tt.leftState)
			}
			if got := h.state.Get(rightPage); got != tt.rightState {
				t.Fatalf("right boundary state = %v, want %v", got, tt.rightState)
			}
			if got := h.Stats()["source_read_bytes"]; got != chunkFaultFillBytes {
				t.Fatalf("source read bytes = %d, want whole %d-byte chunk", got, chunkFaultFillBytes)
			}
		})
	}
}

func TestChunkRunBidirectionalWindowRespectsLayeredVisibility(t *testing.T) {
	plain := bytes.Repeat([]byte{0x46}, chunkFaultFillBytes)
	hash := sha256.Sum256(plain)
	lower, getter := openSnapshotManifest(t, &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(plain)),
		Entries: []codec.ChunkEntry{{
			Offset:         0,
			Size:           uint32(len(plain)),
			CiphertextHash: hash,
		}},
	}, map[store.ContentKey][]byte{hash: plain})
	const (
		visibleStartPage = uint64(32)
		visibleEndPage   = uint64(224)
		faultPage        = uint64(128)
	)
	upper, err := sparse.NewSource(
		bytes.NewReader(bytes.Repeat([]byte{0x99}, chunkFaultFillBytes)),
		chunkFaultFillBytes,
		[]sparse.Extent{{
			Offset: visibleStartPage * PageSize,
			Size:   (visibleEndPage - visibleStartPage) * PageSize,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	root := fetch.NewLayered(testStream{Source: upper}, lower)
	source, err := NewStreamSnapshotSource(root, chunkFaultFillBytes)
	if err != nil {
		t.Fatal(err)
	}
	h := newUnitHandler(t, chunkFaultFillBytes, source)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA + faultPage*PageSize, uffdFD: 9}, make([]byte, PageSize))
	waitUnitTail(t, h)

	if got := getter.chunkCalls.Load(); got != 1 {
		t.Fatalf("lower chunk Get calls = %d, want 1", got)
	}
	calls := ioctls.snapshot()
	if len(calls) != 3 {
		t.Fatalf("ioctl call count = %d, want urgent, suffix, prefix", len(calls))
	}
	if calls[1].length != (visibleEndPage-faultPage-1)*PageSize || calls[2].dst != unitCHVA+visibleStartPage*PageSize || calls[2].length != (faultPage-visibleStartPage)*PageSize {
		t.Fatalf("layer-clipped suffix/prefix = dst 0x%x len %d / dst 0x%x len %d", calls[1].dst, calls[1].length, calls[2].dst, calls[2].length)
	}
	for page := visibleStartPage; page < visibleEndPage; page++ {
		if got := h.state.Get(page); got != StateLoaded {
			t.Fatalf("visible lower page %d state = %v, want Loaded", page, got)
		}
	}
	if h.state.Get(visibleStartPage-1) != StateAbsent || h.state.Get(visibleEndPage) != StateAbsent {
		t.Fatal("ChunkRun population crossed an opaque upper-layer boundary")
	}
	wantRead := (visibleEndPage - visibleStartPage) * PageSize
	if got := h.Stats()["source_read_bytes"]; got != wantRead {
		t.Fatalf("source read bytes = %d, want final-visible window %d", got, wantRead)
	}
}

func TestChunkRunBidirectionalWindowPopulatesOnlyFullPages(t *testing.T) {
	const (
		chunkStart = PageSize / 2
		chunkSize  = 4 * PageSize
		imageSize  = 5 * PageSize
		faultPage  = uint64(2)
	)
	plain := bytes.Repeat([]byte{0x3b}, chunkSize)
	hash := sha256.Sum256(plain)
	stream, getter := openSnapshotManifest(t, &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: imageSize,
		Entries: []codec.ChunkEntry{{
			Offset:         chunkStart,
			Size:           uint32(chunkSize),
			CiphertextHash: hash,
		}},
		Holes: []sparse.Extent{
			{Offset: 0, Size: chunkStart},
			{Offset: chunkStart + chunkSize, Size: imageSize - chunkStart - chunkSize},
		},
	}, map[store.ContentKey][]byte{hash: plain})
	source, err := NewStreamSnapshotSource(stream, imageSize)
	if err != nil {
		t.Fatal(err)
	}
	h := newUnitHandler(t, imageSize, source)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA + faultPage*PageSize, uffdFD: 9}, make([]byte, PageSize))
	waitUnitTail(t, h)

	if got := getter.chunkCalls.Load(); got != 1 {
		t.Fatalf("chunk Get calls = %d, want 1", got)
	}
	calls := ioctls.snapshot()
	if len(calls) != 3 {
		t.Fatalf("unaligned chunk ioctl call count = %d, want 3", len(calls))
	}
	if calls[0].dst != unitCHVA+2*PageSize || calls[1].dst != unitCHVA+3*PageSize || calls[1].length != PageSize || calls[2].dst != unitCHVA+PageSize || calls[2].length != PageSize {
		t.Fatalf("unaligned chunk calls: count=%d urgent=(0x%x,%d) suffix=(0x%x,%d) prefix=(0x%x,%d)", len(calls), calls[0].dst, calls[0].length, calls[1].dst, calls[1].length, calls[2].dst, calls[2].length)
	}
	if h.state.Get(0) != StateAbsent || h.state.Get(1) != StateLoaded || h.state.Get(2) != StateLoaded || h.state.Get(3) != StateLoaded || h.state.Get(4) != StateAbsent {
		t.Fatalf("unaligned chunk states = %v/%v/%v/%v/%v", h.state.Get(0), h.state.Get(1), h.state.Get(2), h.state.Get(3), h.state.Get(4))
	}
	if got := h.Stats()["source_read_bytes"]; got != chunkSize {
		t.Fatalf("source read bytes = %d, want exact %d-byte physical chunk", got, chunkSize)
	}
}

func TestChunkRunSuffixFailureAbandonsPrefix(t *testing.T) {
	plain := bytes.Repeat([]byte{0x2a}, chunkFaultFillBytes)
	hash := sha256.Sum256(plain)
	stream, _ := openSnapshotManifest(t, &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(plain)),
		Entries: []codec.ChunkEntry{{
			Offset:         0,
			Size:           uint32(len(plain)),
			CiphertextHash: hash,
		}},
	}, map[store.ContentKey][]byte{hash: plain})
	source, err := NewStreamSnapshotSource(stream, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	h := newUnitHandler(t, uint64(len(plain)), source)
	const faultPage = uint64(128)
	ioctls := newFakeIoctls()
	ioctls.copyHook = func(_ int, dst uint64, data []byte) (int64, error) {
		if dst == unitCHVA+(faultPage+1)*PageSize {
			return 2 * PageSize, unix.EAGAIN
		}
		return int64(len(data)), nil
	}
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA + faultPage*PageSize, uffdFD: 9}, make([]byte, PageSize))
	waitUnitTail(t, h)

	if calls := ioctls.snapshot(); len(calls) != 2 {
		t.Fatalf("ioctl call count = %d, want urgent and failed suffix only", len(calls))
	}
	if h.state.Get(faultPage) != StateLoaded || h.state.Get(faultPage+1) != StateLoaded || h.state.Get(faultPage+2) != StateLoaded || h.state.Get(faultPage+3) != StateAbsent {
		t.Fatal("suffix partial completion was not committed exactly")
	}
	if h.state.Get(faultPage-1) != StateAbsent {
		t.Fatal("prefix was attempted after suffix failure")
	}
	stats := h.Stats()
	if stats["tail_partial"] != 1 || stats["tail_conflicts"] != 1 || stats["tail_pages_completed"] != 2 {
		t.Fatalf("failed suffix metrics = %#v", stats)
	}
}

func TestChunkRunPrefixStateConflictKeepsAdjacentPages(t *testing.T) {
	plain := bytes.Repeat([]byte{0x6e}, chunkFaultFillBytes)
	hash := sha256.Sum256(plain)
	stream, _ := openSnapshotManifest(t, &codec.Manifest{
		Version:   codec.Version1,
		ImageSize: uint64(len(plain)),
		Entries: []codec.ChunkEntry{{
			Offset:         0,
			Size:           uint32(len(plain)),
			CiphertextHash: hash,
		}},
	}, map[store.ContentKey][]byte{hash: plain})
	source, err := NewStreamSnapshotSource(stream, uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	h := newUnitHandler(t, uint64(len(plain)), source)
	const (
		faultPage    = uint64(128)
		conflictPage = uint64(100)
	)
	ioctls := newFakeIoctls()
	ioctls.copyHook = func(_ int, dst uint64, data []byte) (int64, error) {
		if dst == unitCHVA+(faultPage+1)*PageSize {
			h.state.Set(conflictPage, StateLoaded)
		}
		return int64(len(data)), nil
	}
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA + faultPage*PageSize, uffdFD: 9}, make([]byte, PageSize))
	waitUnitTail(t, h)

	calls := ioctls.snapshot()
	if len(calls) != 3 {
		t.Fatalf("ioctl call count = %d, want urgent, suffix, shortened prefix", len(calls))
	}
	wantPrefixStart := conflictPage + 1
	if calls[2].dst != unitCHVA+wantPrefixStart*PageSize || calls[2].length != (faultPage-wantPrefixStart)*PageSize {
		t.Fatalf("shortened prefix dst/length = 0x%x/%d, want 0x%x/%d", calls[2].dst, calls[2].length, unitCHVA+wantPrefixStart*PageSize, (faultPage-wantPrefixStart)*PageSize)
	}
	if h.state.Get(conflictPage-1) != StateAbsent || h.state.Get(conflictPage) != StateLoaded || h.state.Get(conflictPage+1) != StateLoaded || h.state.Get(faultPage-1) != StateLoaded {
		t.Fatal("prefix state conflict did not preserve the fault-adjacent success range")
	}
	if h.Stats()["tail_conflicts"] != 1 {
		t.Fatalf("tail conflicts = %d, want 1", h.Stats()["tail_conflicts"])
	}
}

func TestOrdinaryDataFaultUsesOnePlusFifteenPages(t *testing.T) {
	const pages = chunkFaultFillBytes/PageSize + 4
	source := newRecordingSnapshot(pages*PageSize, sparse.Data, 0x91)
	h := newUnitHandler(t, pages*PageSize, source)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 7}, make([]byte, PageSize))
	if got := source.readLengths(); !equalUint64s(got, []uint64{PageSize}) {
		t.Fatalf("foreground Run.ReadAt lengths = %v, want [4096]", got)
	}
	if got := source.runLimits(); !equalUint64s(got, []uint64{ordinaryDataFaultFillBytes}) {
		t.Fatalf("metadata RunAt limits = %v, want [%d]", got, ordinaryDataFaultFillBytes)
	}
	if !h.tailBusy.Load() || len(h.tailQ) != 1 {
		t.Fatalf("deferred tail reservation: busy=%v queued=%d", h.tailBusy.Load(), len(h.tailQ))
	}

	startUnitTail(t, h)
	waitUnitTail(t, h)
	if got := source.readLengths(); !equalUint64s(got, []uint64{PageSize, ordinaryDataNeighborTailBytes}) {
		t.Fatalf("all Run.ReadAt lengths = %v", got)
	}
	if calls := ioctls.snapshot(); len(calls) != 2 || calls[0].length != PageSize || calls[1].length != ordinaryDataNeighborTailBytes {
		t.Fatalf("ioctl calls = %+v", calls)
	}
	for page := uint64(0); page < ordinaryDataFaultFillBytes/PageSize; page++ {
		if got := h.state.Get(page); got != StateLoaded {
			t.Fatalf("page %d state = %v, want Loaded", page, got)
		}
	}
	if page := uint64(ordinaryDataFaultFillBytes / PageSize); h.state.Get(page) != StateAbsent {
		t.Fatalf("page %d state = %v, want Absent", page, h.state.Get(page))
	}
}

func TestZeroTailStopsAtStateBoundary(t *testing.T) {
	const pages = 20
	h := newUnitHandler(t, pages*PageSize, ZeroSource{})
	h.state.Set(8, StateLoaded)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 5}, make([]byte, PageSize))
	startUnitTail(t, h)
	waitUnitTail(t, h)

	calls := ioctls.snapshot()
	if len(calls) != 2 || calls[0].length != PageSize || calls[1].length != 7*PageSize {
		t.Fatalf("zero calls = %+v, want urgent page then seven-page tail", calls)
	}
	for page := uint64(0); page <= 8; page++ {
		if got := h.state.Get(page); got != StateLoaded {
			t.Fatalf("page %d state = %v, want Loaded", page, got)
		}
	}
	if got := h.state.Get(9); got != StateAbsent {
		t.Fatalf("page 9 state = %v, want Absent", got)
	}
}

func TestZeroAndReleasedFaultsUseSerialZeroTail(t *testing.T) {
	for _, tt := range []struct {
		name     string
		expected PageState
	}{
		{name: "snapshot zero", expected: StateAbsent},
		{name: "released", expected: StateReleased},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const pages = 20
			h := newUnitHandler(t, pages*PageSize, ZeroSource{})
			if tt.expected == StateReleased {
				h.state.SetRange(0, pages, StateReleased)
			}
			ioctls := newFakeIoctls()
			h.ops = ioctls.ops()

			h.handleFault(faultEvent{address: unitCHVA, uffdFD: 5}, make([]byte, PageSize))
			calls := ioctls.snapshot()
			if len(calls) != 1 || calls[0].kind != "zero" || calls[0].length != PageSize {
				t.Fatalf("urgent calls = %+v", calls)
			}
			startUnitTail(t, h)
			waitUnitTail(t, h)
			calls = ioctls.snapshot()
			if len(calls) != 2 || calls[1].kind != "zero" || calls[1].length != zeroNeighborTailBytes {
				t.Fatalf("zero calls = %+v", calls)
			}
			if h.Stats()["source_read_calls"] != 0 {
				t.Fatal("zero path read source")
			}
			for page := uint64(0); page < zeroFaultFillBytes/PageSize; page++ {
				if h.state.Get(page) != StateLoaded {
					t.Fatalf("page %d was not populated", page)
				}
			}
			if page := uint64(zeroFaultFillBytes / PageSize); h.state.Get(page) != tt.expected {
				t.Fatalf("page %d = %v, want %v", page, h.state.Get(page), tt.expected)
			}
		})
	}
}

func TestResolvedHoleAndZeroRunsUseOnePlusFifteenPages(t *testing.T) {
	for _, kind := range []sparse.RunKind{sparse.Hole, sparse.Zero} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			const pages = 20
			source := newRecordingSnapshot(pages*PageSize, kind, 0)
			h := newUnitHandler(t, pages*PageSize, source)
			ioctls := newFakeIoctls()
			h.ops = ioctls.ops()
			startUnitTail(t, h)

			h.handleFault(faultEvent{address: unitCHVA, uffdFD: 5}, make([]byte, PageSize))
			waitUnitTail(t, h)

			calls := ioctls.snapshot()
			if len(calls) != 2 || calls[0].kind != "zero" || calls[0].length != PageSize || calls[1].kind != "zero" || calls[1].length != zeroNeighborTailBytes {
				t.Fatalf("zero-like calls = %+v, want urgent page then 15-page tail", calls)
			}
			if got := source.readLengths(); len(got) != 0 {
				t.Fatalf("zero-like source payload reads = %v, want none", got)
			}
			if got := source.runLimits(); !equalUint64s(got, []uint64{ordinaryDataFaultFillBytes}) {
				t.Fatalf("metadata RunAt limits = %v, want one %d-byte query", got, ordinaryDataFaultFillBytes)
			}
			for page := uint64(0); page < zeroFaultFillBytes/PageSize; page++ {
				if got := h.state.Get(page); got != StateLoaded {
					t.Fatalf("page %d state = %v, want Loaded", page, got)
				}
			}
			if page := uint64(zeroFaultFillBytes / PageSize); h.state.Get(page) != StateAbsent {
				t.Fatalf("page %d state = %v, want Absent", page, h.state.Get(page))
			}
		})
	}
}

func TestConcurrentFaultsKeepSingleTailReservation(t *testing.T) {
	const pages = 64
	source := newRecordingSnapshot(pages*PageSize, sparse.Data, 0x4a)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	source.readHook = func(ctx context.Context, _ uint64, _ []byte) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h := newUnitHandler(t, pages*PageSize, source)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()

	var wg sync.WaitGroup
	for _, page := range []uint64{0, 32} {
		wg.Add(1)
		go func(page uint64) {
			defer wg.Done()
			h.handleFault(faultEvent{address: unitCHVA + page*PageSize, uffdFD: 3}, make([]byte, PageSize))
		}(page)
	}
	receiveN(t, entered, 2)
	close(release)
	wg.Wait()
	if !h.tailBusy.Load() || len(h.tailQ) != 1 {
		t.Fatalf("tail reservation busy=%v queue=%d", h.tailBusy.Load(), len(h.tailQ))
	}
	stats := h.Stats()
	if stats["tail_submitted"] != 1 || stats["tail_dropped_busy"] != 1 {
		t.Fatalf("tail reservation metrics = %#v", stats)
	}
	if stats["fault_inflight_hwm"] != 2 {
		t.Fatalf("fault inflight HWM = %d, want 2", stats["fault_inflight_hwm"])
	}
	startUnitTail(t, h)
	waitUnitTail(t, h)
}

func TestFaultQueueWaitDepthAndInflightMetrics(t *testing.T) {
	h := newUnitHandler(t, PageSize, newRecordingSnapshot(PageSize, sparse.Data, 0x17))
	h.queue = []chan faultEvent{make(chan faultEvent, 4)}
	ioctls := newFakeIoctls()
	var completed atomic.Int32
	done := make(chan struct{})
	record := func() {
		if completed.Add(1) == 3 {
			close(done)
		}
	}
	ioctls.copyHook = func(_ int, _ uint64, data []byte) (int64, error) {
		record()
		return int64(len(data)), nil
	}
	ioctls.zeroHook = func(_ int, _ uint64, length uint64) (int64, error) {
		record()
		return int64(length), nil
	}
	h.ops = ioctls.ops()

	var msg uffdMsg
	msg.Event = uffdEventPagefault
	binary.LittleEndian.PutUint64(msg.Arg[8:16], unitCHVA)
	for range 3 {
		h.dispatch(&msg, 21)
	}
	if got := h.Stats()["fault_queue_depth_hwm"]; got != 3 {
		t.Fatalf("queue depth HWM before worker = %d, want 3", got)
	}
	h.wg.Add(1)
	go h.runWorker(0)
	receiveSignal(t, done)
	close(h.stop)
	h.wg.Wait()

	stats := h.Stats()
	if stats["fault_queue_wait_ns"] == 0 || stats["fault_queue_wait_p50"] == 0 || stats["fault_queue_wait_p95"] == 0 || stats["fault_queue_wait_p99"] == 0 {
		t.Fatalf("queue wait metrics = %#v", stats)
	}
	if stats["fault_inflight_hwm"] != 1 || stats["fault_queue_depth"] != 0 {
		t.Fatalf("queue/inflight gauges = %#v", stats)
	}
}

func TestUrgentConflictConvergence(t *testing.T) {
	for _, tt := range []struct {
		name      string
		err       error
		wantState PageState
	}{
		{name: "EEXIST", err: unix.EEXIST, wantState: StateLoaded},
		{name: "EAGAIN", err: unix.EAGAIN, wantState: StateAbsent},
		{name: "ENOENT", err: unix.ENOENT, wantState: StateAbsent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := newRecordingSnapshot(2*PageSize, sparse.Data, 0x38)
			h := newUnitHandler(t, 2*PageSize, source)
			ioctls := newFakeIoctls()
			ioctls.copyHook = func(int, uint64, []byte) (int64, error) { return 0, tt.err }
			h.ops = ioctls.ops()
			h.handleFault(faultEvent{address: unitCHVA, uffdFD: 4}, make([]byte, PageSize))
			if got := h.state.Get(0); got != tt.wantState {
				t.Fatalf("state = %v, want %v", got, tt.wantState)
			}
			stats := h.Stats()
			if stats["wakes"] != 1 || stats["errors"] != 0 || stats["pages_copied"] != 0 {
				t.Fatalf("conflict metrics = %#v", stats)
			}
			if h.tailBusy.Load() {
				t.Fatal("urgent conflict left tail reserved")
			}
		})
	}
}

func TestStaleAbsentFaultConvergesWithoutError(t *testing.T) {
	source := newRecordingSnapshot(2*PageSize, sparse.Data, 0x28)
	h := newUnitHandler(t, 2*PageSize, source)
	ioctls := newFakeIoctls()
	h.ops = ioctls.ops()

	// handleAbsentFault is entered only after handleFault observed Absent.
	// Model a tail completion changing the page before stateHardEnd scans it.
	h.state.Set(0, StateLoaded)
	h.handleAbsentFault(7, unitCHVA, 0, 0, make([]byte, PageSize))

	stats := h.Stats()
	if stats["errors"] != 0 || stats["wakes"] != 1 {
		t.Fatalf("stale fault metrics = %#v, want one wake and no error", stats)
	}
	if got := source.runLimits(); len(got) != 0 {
		t.Fatalf("stale fault queried source with limits %v", got)
	}
	if calls := ioctls.snapshot(); len(calls) != 0 {
		t.Fatalf("stale fault issued data ioctl calls: %+v", calls)
	}
	if got := h.state.Get(0); got != StateLoaded {
		t.Fatalf("stale fault changed state to %v", got)
	}
}

func TestTailNoCompletionLeavesNeighborAbsent(t *testing.T) {
	source := newRecordingSnapshot(4*PageSize, sparse.Data, 0x59)
	h := newUnitHandler(t, 4*PageSize, source)
	ioctls := newFakeIoctls()
	ioctls.copyHook = func(_ int, dst uint64, data []byte) (int64, error) {
		length := uint64(len(data))
		if dst == unitCHVA+PageSize {
			return 0, unix.EAGAIN
		}
		return int64(length), nil
	}
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 6}, make([]byte, PageSize))
	waitUnitTail(t, h)
	if h.state.Get(0) != StateLoaded || h.state.Get(1) != StateAbsent || h.state.Get(2) != StateAbsent {
		t.Fatalf("states = %v/%v/%v, want Loaded/Absent/Absent", h.state.Get(0), h.state.Get(1), h.state.Get(2))
	}
	stats := h.Stats()
	if stats["pages_copied"] != 1 || stats["tail_pages_completed"] != 0 || stats["tail_partial"] != 1 || stats["tail_conflicts"] != 1 || stats["errors"] != 0 {
		t.Fatalf("tail completion metrics = %#v", stats)
	}
}

func TestZeroTailNoCompletionLeavesNeighborAbsent(t *testing.T) {
	h := newUnitHandler(t, 4*PageSize, ZeroSource{})
	ioctls := newFakeIoctls()
	ioctls.zeroHook = func(_ int, dst uint64, length uint64) (int64, error) {
		if dst == unitCHVA+PageSize {
			return 0, unix.EAGAIN
		}
		return int64(length), nil
	}
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 6}, make([]byte, PageSize))
	waitUnitTail(t, h)
	if h.state.Get(0) != StateLoaded || h.state.Get(1) != StateAbsent || h.state.Get(2) != StateAbsent {
		t.Fatalf("zero states = %v/%v/%v, want Loaded/Absent/Absent", h.state.Get(0), h.state.Get(1), h.state.Get(2))
	}
	stats := h.Stats()
	if stats["pages_zeroed"] != 1 || stats["tail_pages_completed"] != 0 || stats["tail_partial"] != 1 || stats["tail_conflicts"] != 1 || stats["errors"] != 0 {
		t.Fatalf("zero tail completion metrics = %#v", stats)
	}
}

func TestDataTailCommitsPositivePartialPrefix(t *testing.T) {
	const pages = 20
	source := newRecordingSnapshot(pages*PageSize, sparse.Data, 0x43)
	h := newUnitHandler(t, pages*PageSize, source)
	ioctls := newFakeIoctls()
	ioctls.copyHook = func(_ int, dst uint64, data []byte) (int64, error) {
		if dst == unitCHVA+PageSize {
			return 3 * PageSize, unix.EAGAIN
		}
		return int64(len(data)), nil
	}
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 6}, make([]byte, PageSize))
	waitUnitTail(t, h)

	calls := ioctls.snapshot()
	if len(calls) != 2 || calls[1].length != ordinaryDataNeighborTailBytes {
		t.Fatalf("ioctl calls = %+v, want one %d-byte batch tail", calls, ordinaryDataNeighborTailBytes)
	}
	for page := uint64(0); page < 4; page++ {
		if got := h.state.Get(page); got != StateLoaded {
			t.Fatalf("completed page %d = %v, want Loaded", page, got)
		}
	}
	if got := h.state.Get(4); got != StateAbsent {
		t.Fatalf("first uncompleted page = %v, want Absent", got)
	}
	stats := h.Stats()
	if stats["pages_copied"] != 4 || stats["tail_pages_completed"] != 3 || stats["tail_partial"] != 1 || stats["tail_conflicts"] != 1 || stats["errors"] != 0 {
		t.Fatalf("partial data-tail metrics = %#v", stats)
	}
}

func TestZeroTailCommitsPositivePartialPrefix(t *testing.T) {
	const pages = 20
	h := newUnitHandler(t, pages*PageSize, ZeroSource{})
	ioctls := newFakeIoctls()
	ioctls.zeroHook = func(_ int, dst, length uint64) (int64, error) {
		if dst == unitCHVA+PageSize {
			return 5 * PageSize, unix.EAGAIN
		}
		return int64(length), nil
	}
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 6}, make([]byte, PageSize))
	waitUnitTail(t, h)

	calls := ioctls.snapshot()
	if len(calls) != 2 || calls[1].length != zeroNeighborTailBytes {
		t.Fatalf("ioctl calls = %+v, want one %d-byte batch zero tail", calls, zeroNeighborTailBytes)
	}
	for page := uint64(0); page < 6; page++ {
		if got := h.state.Get(page); got != StateLoaded {
			t.Fatalf("completed page %d = %v, want Loaded", page, got)
		}
	}
	if got := h.state.Get(6); got != StateAbsent {
		t.Fatalf("first uncompleted page = %v, want Absent", got)
	}
	stats := h.Stats()
	if stats["pages_zeroed"] != 6 || stats["tail_pages_completed"] != 5 || stats["tail_partial"] != 1 || stats["tail_conflicts"] != 1 || stats["errors"] != 0 {
		t.Fatalf("partial zero-tail metrics = %#v", stats)
	}
}

func TestFaultAndTailAddNoAllocationsAboveSource(t *testing.T) {
	for _, tt := range []struct {
		name   string
		source SnapshotReader
	}{
		{name: "zero", source: ZeroSource{}},
		{name: "ordinary data", source: newNoAllocDataSource(20 * PageSize)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const pages = 20
			h := newUnitHandler(t, pages*PageSize, tt.source)
			h.logf = func(string, ...any) {}
			h.ops = noAllocUffdOps()
			pageBuf := make([]byte, PageSize)

			allocs := testing.AllocsPerRun(100, func() {
				h.state.SetRange(0, pages, StateAbsent)
				h.handleFault(faultEvent{address: unitCHVA, uffdFD: 6}, pageBuf)
				if h.tailBusy.Load() {
					task := <-h.tailQ
					h.processTail(task)
					h.releaseTail()
				}
			})
			if allocs != 0 {
				t.Fatalf("UFFD fault+tail allocations = %.2f, want 0 above source", allocs)
			}
		})
	}
}

func TestBufferedChunkTailAddsNoAllocations(t *testing.T) {
	const (
		pages     = uint64(chunkFaultFillBytes / PageSize)
		faultPage = uint64(128)
	)
	h := newUnitHandler(t, chunkFaultFillBytes, newNoAllocDataSource(chunkFaultFillBytes))
	h.logf = func(string, ...any) {}
	h.ops = noAllocUffdOps()
	task := tailTask{
		kind:        tailBufferedData,
		uffdFD:      6,
		dstVA:       unitCHVA + (faultPage+1)*PageSize,
		pageIdx:     faultPage + 1,
		start:       (faultPage + 1) * PageSize,
		end:         chunkFaultFillBytes,
		bufferStart: 0,
		prefixStart: 0,
		expected:    StateAbsent,
	}

	allocs := testing.AllocsPerRun(100, func() {
		h.state.SetRange(0, pages, StateAbsent)
		h.state.Set(faultPage, StateLoaded)
		h.processBufferedTail(task)
	})
	if allocs != 0 {
		t.Fatalf("buffered ChunkRun tail allocations = %.2f, want 0", allocs)
	}
}

func TestUrgentRejectsInvalidIoctlCompletion(t *testing.T) {
	for _, completed := range []int64{-1, 1, 2 * PageSize} {
		t.Run(fmt.Sprint(completed), func(t *testing.T) {
			h := newUnitHandler(t, PageSize, newRecordingSnapshot(PageSize, sparse.Data, 0x29))
			ioctls := newFakeIoctls()
			ioctls.copyHook = func(int, uint64, []byte) (int64, error) { return completed, nil }
			h.ops = ioctls.ops()
			h.handleFault(faultEvent{address: unitCHVA, uffdFD: 17}, make([]byte, PageSize))
			if got := h.state.Get(0); got != StateAbsent {
				t.Fatalf("invalid completion changed state to %v", got)
			}
			if h.Stats()["errors"] != 1 || h.Stats()["pages_copied"] != 0 {
				t.Fatalf("invalid completion metrics = %#v", h.Stats())
			}
		})
	}
}

func TestEventRemoveWinsTailConditionalCommit(t *testing.T) {
	source := newRecordingSnapshot(4*PageSize, sparse.Data, 0x77)
	h := newUnitHandler(t, 4*PageSize, source)
	ioctls := newFakeIoctls()
	ioctls.copyHook = func(_ int, dst uint64, data []byte) (int64, error) {
		length := uint64(len(data))
		if dst == unitCHVA+PageSize {
			h.state.SetRange(1, 4, StateReleased)
		}
		return int64(length), nil
	}
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 6}, make([]byte, PageSize))
	waitUnitTail(t, h)
	for page := uint64(1); page < 4; page++ {
		if got := h.state.Get(page); got != StateReleased {
			t.Fatalf("stale tail changed page %d to %v", page, got)
		}
	}
	if h.Stats()["tail_conflicts"] == 0 {
		t.Fatal("EVENT_REMOVE race did not record tail conflict")
	}
}

func TestUrgentWinsRunningTail(t *testing.T) {
	const pages = 20
	source := newRecordingSnapshot(pages*PageSize, sparse.Data, 0x45)
	h := newUnitHandler(t, pages*PageSize, source)
	tailEntered := make(chan struct{})
	releaseTail := make(chan struct{})
	var tailBlocked atomic.Bool
	ioctls := newFakeIoctls()
	ioctls.copyHook = func(_ int, dst uint64, data []byte) (int64, error) {
		length := uint64(len(data))
		if dst == unitCHVA+PageSize && tailBlocked.CompareAndSwap(false, true) {
			close(tailEntered)
			<-releaseTail
			return 0, unix.EEXIST
		}
		return int64(length), nil
	}
	h.ops = ioctls.ops()
	startUnitTail(t, h)

	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 8}, make([]byte, PageSize))
	receiveSignal(t, tailEntered)
	h.handleFault(faultEvent{address: unitCHVA + PageSize, uffdFD: 8}, make([]byte, PageSize))
	close(releaseTail)
	waitUnitTail(t, h)
	if h.state.Get(1) != StateLoaded {
		t.Fatalf("urgent competitor state = %v", h.state.Get(1))
	}
	stats := h.Stats()
	if stats["tail_conflicts"] == 0 || stats["tail_dropped_busy"] == 0 || stats["errors"] != 0 {
		t.Fatalf("urgent/tail race metrics = %#v", stats)
	}
}

func TestFaultRunBoundStopsAtCHRegion(t *testing.T) {
	for _, kind := range []sparse.RunKind{sparse.Data, sparse.Hole, sparse.Zero} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			const pages = 4
			source := newRecordingSnapshot(pages*PageSize, kind, 0x63)
			h := newUnitHandlerWithoutMap(t, pages*PageSize, source)
			if err := h.addrMap.RegisterVMA(ProcessCH, unitCHVA, 2*PageSize, 0); err != nil {
				t.Fatal(err)
			}
			if err := h.addrMap.RegisterVMA(ProcessCH, unitCHVA+0x10_0000, 2*PageSize, 2*PageSize); err != nil {
				t.Fatal(err)
			}
			ioctls := newFakeIoctls()
			h.ops = ioctls.ops()

			h.handleFault(faultEvent{address: unitCHVA + PageSize, uffdFD: 11}, make([]byte, PageSize))
			if got := source.runLimits(); !equalUint64s(got, []uint64{PageSize}) {
				t.Fatalf("RunAt limits = %v, want one page at split", got)
			}
			if h.tailBusy.Load() || len(h.tailQ) != 0 {
				t.Fatal("fault crossed CH region into a tail")
			}
		})
	}
}

func TestCheckedCompletion(t *testing.T) {
	for _, tt := range []struct {
		name      string
		completed int64
		requested uint64
		want      uint64
		wantErr   bool
	}{
		{name: "zero", completed: 0, requested: PageSize},
		{name: "full", completed: PageSize, requested: PageSize, want: PageSize},
		{name: "partial pages", completed: PageSize, requested: 2 * PageSize, want: PageSize},
		{name: "negative", completed: -1, requested: PageSize, wantErr: true},
		{name: "too large", completed: 2 * PageSize, requested: PageSize, wantErr: true},
		{name: "unaligned", completed: 1, requested: PageSize, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checkedCompletion(tt.completed, tt.requested)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("checkedCompletion = (%d,%v), want (%d, err=%v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

type recordingSnapshot struct {
	size uint64
	kind sparse.RunKind
	fill byte

	mu       sync.Mutex
	reads    []uint64
	limits   []uint64
	readHook func(context.Context, uint64, []byte) error
	readErr  error
}

type countingSnapshotReader struct {
	source    SnapshotReader
	calls     atomic.Uint64
	lastLimit atomic.Uint64
}

func (s *countingSnapshotReader) RunAt(offset, limit uint64) (sparse.Run, error) {
	s.calls.Add(1)
	s.lastLimit.Store(limit)
	return s.source.RunAt(offset, limit)
}

func (s *countingSnapshotReader) resolveChunkWindow(anchor fetch.ChunkRun, maxBytes uint64) (fetch.ChunkRun, error) {
	resolver, ok := s.source.(chunkWindowSource)
	if !ok {
		return anchor, nil
	}
	return resolver.resolveChunkWindow(anchor, maxBytes)
}

type noAllocDataSource struct {
	size uint64
	run  noAllocDataRun
}

type noAllocDataRun struct {
	offset uint64
	end    uint64
}

func newNoAllocDataSource(size uint64) *noAllocDataSource {
	return &noAllocDataSource{size: size, run: noAllocDataRun{end: size}}
}

func (s *noAllocDataSource) RunAt(offset, limit uint64) (sparse.Run, error) {
	s.run.offset = offset
	s.run.end = offset + min(limit, s.size-offset)
	return &s.run, nil
}

func (r *noAllocDataRun) Offset() uint64     { return r.offset }
func (r *noAllocDataRun) End() uint64        { return r.end }
func (*noAllocDataRun) Kind() sparse.RunKind { return sparse.Data }
func (r *noAllocDataRun) ReadAt(ctx context.Context, buf []byte, innerOffset uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if innerOffset > r.end-r.offset || uint64(len(buf)) > r.end-r.offset-innerOffset {
		return 0, io.ErrUnexpectedEOF
	}
	for i := range buf {
		buf[i] = 0x5d
	}
	return len(buf), nil
}

func noAllocUffdOps() uffdOps {
	return uffdOps{
		copy: func(_ int, _ uint64, src []byte) (int64, error) {
			return int64(len(src)), nil
		},
		zeropage: func(_ int, _, length uint64) (int64, error) {
			return int64(length), nil
		},
		wake: func(int, uint64, uint64) error { return nil },
	}
}

func newRecordingSnapshot(size uint64, kind sparse.RunKind, fill byte) *recordingSnapshot {
	return &recordingSnapshot{size: size, kind: kind, fill: fill}
}

func (s *recordingSnapshot) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.size {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, errors.New("zero limit")
	}
	s.mu.Lock()
	s.limits = append(s.limits, limit)
	s.mu.Unlock()
	end := s.size
	if limit < s.size-offset {
		end = offset + limit
	}
	return recordingRun{source: s, offset: offset, end: end, kind: s.kind}, nil
}

func (s *recordingSnapshot) readLengths() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.reads...)
}

func (s *recordingSnapshot) runLimits() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.limits...)
}

type recordingRun struct {
	source *recordingSnapshot
	offset uint64
	end    uint64
	kind   sparse.RunKind
}

func (r recordingRun) Offset() uint64       { return r.offset }
func (r recordingRun) End() uint64          { return r.end }
func (r recordingRun) Kind() sparse.RunKind { return r.kind }

func (r recordingRun) ReadAt(ctx context.Context, buf []byte, innerOffset uint64) (int, error) {
	length := r.end - r.offset
	if innerOffset > length || uint64(len(buf)) > length-innerOffset {
		return 0, fmt.Errorf("recording Run read outside [0,%d)", length)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.kind != sparse.Data {
		clear(buf)
		return len(buf), nil
	}
	r.source.mu.Lock()
	r.source.reads = append(r.source.reads, uint64(len(buf)))
	hook := r.source.readHook
	err := r.source.readErr
	r.source.mu.Unlock()
	if hook != nil {
		if hookErr := hook(ctx, r.offset+innerOffset, buf); hookErr != nil {
			return 0, hookErr
		}
	}
	if err != nil {
		return 0, err
	}
	for i := range buf {
		buf[i] = r.source.fill
	}
	return len(buf), nil
}

type fakeIoctlCall struct {
	kind   string
	fd     int
	dst    uint64
	length uint64
	data   []byte
}

type fakeIoctls struct {
	mu       sync.Mutex
	calls    []fakeIoctlCall
	wakes    []fakeIoctlCall
	copyHook func(int, uint64, []byte) (int64, error)
	zeroHook func(int, uint64, uint64) (int64, error)
}

func newFakeIoctls() *fakeIoctls { return &fakeIoctls{} }

func (f *fakeIoctls) ops() uffdOps {
	return uffdOps{
		copy: func(fd int, dst uint64, src []byte) (int64, error) {
			data := append([]byte(nil), src...)
			length := uint64(len(src))
			f.mu.Lock()
			f.calls = append(f.calls, fakeIoctlCall{kind: "copy", fd: fd, dst: dst, length: length, data: data})
			hook := f.copyHook
			f.mu.Unlock()
			if hook != nil {
				return hook(fd, dst, src)
			}
			return int64(length), nil
		},
		zeropage: func(fd int, dst, length uint64) (int64, error) {
			f.mu.Lock()
			f.calls = append(f.calls, fakeIoctlCall{kind: "zero", fd: fd, dst: dst, length: length})
			hook := f.zeroHook
			f.mu.Unlock()
			if hook != nil {
				return hook(fd, dst, length)
			}
			return int64(length), nil
		},
		wake: func(fd int, start, length uint64) error {
			f.mu.Lock()
			f.wakes = append(f.wakes, fakeIoctlCall{kind: "wake", fd: fd, dst: start, length: length})
			f.mu.Unlock()
			return nil
		},
	}
}

func (f *fakeIoctls) snapshot() []fakeIoctlCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeIoctlCall(nil), f.calls...)
}

func newUnitHandler(t testing.TB, size uint64, source SnapshotReader) *Handler {
	t.Helper()
	h := newUnitHandlerWithoutMap(t, size, source)
	if err := h.addrMap.RegisterVMA(ProcessCH, unitCHVA, size, 0); err != nil {
		t.Fatal(err)
	}
	return h
}

func newUnitHandlerWithoutMap(t testing.TB, size uint64, source SnapshotReader) *Handler {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var tailBuf []byte
	if !isZeroSource(source) {
		tailBuf = make([]byte, chunkFaultFillBytes)
	}
	h := &Handler{
		cfg: Config{
			Size:   int(size),
			Source: source,
		},
		addrMap:  NewAddressMap(size),
		state:    NewPageStateMap(int(size / PageSize)),
		logf:     t.Logf,
		stop:     make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
		tailQ:    make(chan tailTask, 1),
		tailBuf:  tailBuf,
		tailIdle: make(chan struct{}, 1),
	}
	return h
}

func startUnitTail(t *testing.T, h *Handler) {
	t.Helper()
	h.tailWG.Add(1)
	go h.runTailWorker()
	t.Cleanup(func() {
		h.closing.Store(true)
		h.tailSubmit.Lock()
		h.cancel()
		h.tailSubmit.Unlock()
		h.waitTailIdle()
		h.tailWG.Wait()
	})
}

func waitUnitTail(t *testing.T, h *Handler) {
	t.Helper()
	if !h.tailBusy.Load() {
		return
	}
	select {
	case <-h.tailIdle:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tail reservation release")
	}
	if h.tailBusy.Load() {
		t.Fatal("tail idle notification arrived while still busy")
	}
}

func receiveSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for signal")
	}
}

func receiveN(t *testing.T, ch <-chan struct{}, n int) {
	t.Helper()
	for range n {
		receiveSignal(t, ch)
	}
}

func equalUint64s(left, right []uint64) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
