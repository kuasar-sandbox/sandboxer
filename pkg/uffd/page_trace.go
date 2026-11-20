package uffd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"golang.org/x/sys/unix"
)

// Temporary, opt-in investigation of one memfd page. No memory is read through
// the guest/backend mapping: COPY summaries use the already available source
// buffer. Records are buffered here and emitted only after Handler.Close joins
// workers and users, never while installMu is held.
const pageTraceEnv = "SANDBOX_UFFD_TRACE_MEMFD_PAGE"
const pageTraceLimit = 32
const pageTracePrefix = "uffd-page-trace "

type pageTraceRecord struct {
	Version     int     `json:"v"`
	Page        uint64  `json:"page"`
	Sequence    uint64  `json:"seq"`
	Event       string  `json:"event"`
	Elapsed     uint64  `json:"elapsed_ns"`
	PID         int     `json:"pid"`
	Handler     uint64  `json:"handler"`
	Start       *uint64 `json:"start,omitempty"`
	Length      *uint64 `json:"length,omitempty"`
	State       string  `json:"state,omitempty"`
	Kind        string  `json:"kind,omitempty"`
	SHA256      string  `json:"sha256,omitempty"`
	NonFF       *int    `json:"non_ff,omitempty"`
	Done        *int64  `json:"done,omitempty"`
	Errno       *int    `json:"errno,omitempty"`
	Recorded    *int    `json:"recorded,omitempty"`
	Dropped     *uint64 `json:"dropped,omitempty"`
	KernelEvent string  `json:"kernel_event,omitempty"`

	UffdFD        *int    `json:"uffd_fd,omitempty"`
	BackendVA     *uint64 `json:"backend_va,omitempty"`
	MemfdFD       *int    `json:"memfd_fd,omitempty"`
	MemfdDev      *uint64 `json:"memfd_dev,omitempty"`
	MemfdIno      *uint64 `json:"memfd_ino,omitempty"`
	MemfdSize     *uint64 `json:"memfd_size,omitempty"`
	KernelRelease *string `json:"kernel_release,omitempty"`
	CHVA          *uint64 `json:"ch_va,omitempty"`
	CHPID         *int    `json:"ch_pid,omitempty"`
	PeerErrno     *int    `json:"peer_errno,omitempty"`
	MemfdErrno    *int    `json:"memfd_errno,omitempty"`
	KernelErrno   *int    `json:"kernel_errno,omitempty"`
	MapsErrno     *int    `json:"maps_errno,omitempty"`
	MapsVA        *uint64 `json:"maps_va,omitempty"`
	MapsSize      *uint64 `json:"maps_size,omitempty"`
	MapsOffset    *uint64 `json:"maps_offset,omitempty"`
	MapsDev       *uint64 `json:"maps_dev,omitempty"`
	MapsIno       *uint64 `json:"maps_ino,omitempty"`
	MapsMatch     *bool   `json:"maps_match,omitempty"`

	Phase          string `json:"phase,omitempty"`
	Resident       *bool  `json:"resident,omitempty"`
	MincoreErrno   *int   `json:"mincore_errno,omitempty"`
	ExtentData     *bool  `json:"extent_data,omitempty"`
	SeekDataOffset *int64 `json:"seek_data_offset,omitempty"`
	SeekHoleOffset *int64 `json:"seek_hole_offset,omitempty"`
	SeekDataErrno  *int   `json:"seek_data_errno,omitempty"`
	SeekHoleErrno  *int   `json:"seek_hole_errno,omitempty"`
}

type pageTrace struct {
	page    uint64
	started time.Time
	pid     int
	id      uint64
	mu      sync.Mutex
	records []pageTraceRecord
	dropped uint64
}

var nextPageTraceID atomic.Uint64

func newPageTrace(value string, size uint64) *pageTrace {
	// Parse decimal or explicit hexadecimal only. In particular, do not accept
	// signs, whitespace, underscores, implicit octal, or an incomplete page.
	if value == "" {
		return nil
	}
	base, digits := 10, value
	if len(value) >= 2 && value[0] == '0' && (value[1] == 'x' || value[1] == 'X') {
		base, digits = 16, value[2:]
	}
	if digits == "" {
		return nil
	}
	for _, c := range digits {
		if c >= '0' && c <= '9' || base == 16 && (c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			continue
		}
		return nil
	}
	page, err := strconv.ParseUint(digits, base, 64)
	if err != nil || page%PageSize != 0 || page > size || PageSize > size-page {
		return nil
	}
	return &pageTrace{page: page, started: time.Now(), pid: os.Getpid(), id: nextPageTraceID.Add(1), records: make([]pageTraceRecord, 0, pageTraceLimit)}
}

func (t *pageTrace) covers(start, length uint64) bool {
	return start <= t.page && length >= PageSize && t.page-start <= length-PageSize
}

func pageTraceState(state PageState) string {
	switch state {
	case StateLoaded:
		return "loaded"
	case StateReleased:
		return "released"
	default:
		return "absent"
	}
}

func (t *pageTrace) record(event string, start, length uint64, state PageState, fill func(*pageTraceRecord)) {
	if !t.covers(start, length) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.records) == pageTraceLimit {
		t.dropped++
		return
	}
	r := pageTraceRecord{
		Version: 2, Page: t.page, Sequence: uint64(len(t.records)) + 1,
		Event: event, Start: &start, Length: &length, State: pageTraceState(state),
		Elapsed: uint64(time.Since(t.started)), PID: t.pid, Handler: t.id,
	}
	if fill != nil {
		fill(&r)
	}
	t.records = append(t.records, r)
}

func (h *Handler) traceSource(run sparse.Run) {
	state := h.state.Get(h.pageTrace.page / PageSize)
	h.pageTrace.record("source", run.Offset(), run.End()-run.Offset(), state, func(r *pageTraceRecord) {
		switch run.Kind() {
		case sparse.Data:
			r.Kind = "data"
		case sparse.Zero:
			r.Kind = "zero"
		case sparse.Hole:
			r.Kind = "hole"
		}
	})
}

func (h *Handler) traceIO(event string, dst, length uint64, data []byte, done int64, err error) {
	off, ok := h.addrMap.Locate(dst)
	if !ok {
		return
	}
	state := h.state.Get(h.pageTrace.page / PageSize)
	h.pageTrace.record(event, off, length, state, func(r *pageTraceRecord) {
		// All recorded ranges cover the complete target page. A nil buffer is
		// ZEROPAGE's known input, not a read of the existing destination folio.
		page := zeroScratch[:PageSize]
		if data != nil {
			inner := h.pageTrace.page - off
			page = data[inner : inner+PageSize]
		}
		digest := sha256.Sum256(page)
		r.SHA256 = hex.EncodeToString(digest[:])
		nonFF := 0
		for _, value := range page {
			if value != 0xff {
				nonFF++
			}
		}
		errno := 0
		if err != nil {
			var number unix.Errno
			if errors.As(err, &number) {
				errno = int(number)
			} else {
				errno = -1
			}
		}
		r.NonFF, r.Done, r.Errno = &nonFF, &done, &errno
	})
}

func (t *pageTrace) emit(logf func(string, ...any)) {
	t.mu.Lock()
	records := append([]pageTraceRecord(nil), t.records...)
	recorded, dropped := len(records), t.dropped
	t.mu.Unlock()
	records = append(records, pageTraceRecord{
		Version: 2, Page: t.page, Sequence: uint64(recorded) + dropped + 1,
		Event: "summary", Recorded: &recorded, Dropped: &dropped,
		Elapsed: uint64(time.Since(t.started)), PID: t.pid, Handler: t.id,
	})
	for _, record := range records {
		body, err := json.Marshal(record)
		if err == nil && len(pageTracePrefix)+len(body) < 2048 {
			logf("%s%s", pageTracePrefix, body)
		}
	}
}
