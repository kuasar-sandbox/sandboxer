package usage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"unicode/utf8"
)

const (
	FormatVersion    = 1
	AlgorithmVersion = 1
	MaxRecordBytes   = 512 * 1024
	frameHeader      = 16
	frameFooter      = 16
	maxString        = 256
)

var (
	fileMagic  = []byte("KUUSAGE1")
	tailMagic  = []byte("USAGEEND")
	crcTable   = crc32.MakeTable(crc32.Castagnoli)
	ErrCorrupt = errors.New("usage: corrupt record")
)

type encoder struct {
	b   []byte
	err error
}

func (e *encoder) u(v uint64) { e.b = binary.AppendUvarint(e.b, v) }
func (e *encoder) i(v int64)  { e.b = binary.AppendVarint(e.b, v) }
func (e *encoder) flag(v bool) {
	if v {
		e.u(1)
	} else {
		e.u(0)
	}
}
func (e *encoder) text(s string) {
	if !validText(s) {
		e.err = errors.New("usage: invalid string")
	}
	e.u(uint64(len(s)))
	e.b = append(e.b, s...)
}

func validText(s string) bool { return len(s) <= maxString && utf8.ValidString(s) }

// These bounds mirror EncodeRecord, reserving full-width varints so accepting
// an identity cannot make a later numeric update unencodable. Gauge status is
// caller supplied; counter status is set internally. No sample is serialized
// to check capacity, and no budget state is maintained alongside S/F/A.
func counterSizeBound(c Counter) int {
	status := max(len(c.Status), len(OK), len(Missing), len(Invalid))
	return 5*binary.MaxVarintLen64 + 2 + 3*2 + len(c.Name) + len(c.Source) + status
}

func gaugeSizeBound(g Gauge) int {
	// Eight endpoint and eight window varints, plus four boolean flags.
	return 16*binary.MaxVarintLen64 + 4 + 3*2 + len(g.Name) + len(g.Source) + maxString
}

func recordSizeBound(s Snapshot) int {
	// Sequence, saved/start times, two intervals, two array counts, closed.
	n := frameHeader + frameFooter + 7*binary.MaxVarintLen64 + 1 + 2*2 + len(s.SandboxID) + len(s.RunEpoch)
	for _, c := range s.Counters {
		n += counterSizeBound(c)
	}
	for _, g := range s.Gauges {
		n += gaugeSizeBound(g)
	}
	return n
}
func (e *encoder) wide(v Uint128) { e.u(v.Hi); e.u(v.Lo) }
func (e *encoder) window(w Window) {
	e.wide(w.Area)
	e.u(w.Span)
	e.u(w.Covered)
	e.u(w.Peak)
	e.i(w.PeakAt)
	e.flag(w.PeakKnown)
	e.u(w.Samples)
	e.u(w.MaxReadWindow)
}

// EncodeRecord emits explicit little-endian framing and unsigned/signed
// varints. CRC32C covers the complete header and payload, including identities.
func EncodeRecord(r Record) ([]byte, error) {
	if err := validateRecord(r); err != nil {
		return nil, err
	}
	e := encoder{b: make([]byte, frameHeader, 1024)}
	e.u(r.Sequence)
	e.i(r.SavedUTC)
	s := r.Snapshot
	e.text(s.SandboxID)
	e.text(s.RunEpoch)
	e.i(s.StartedUTC)
	e.i(s.SampleInterval)
	e.i(s.FlushInterval)
	e.flag(s.Closed)
	e.u(uint64(len(s.Counters)))
	for _, c := range s.Counters {
		e.text(c.Name)
		e.text(c.Source)
		e.wide(c.KnownTotal)
		e.u(c.LastRaw)
		e.u(c.Hertz)
		e.u(c.Remainder)
		e.flag(c.SourceKnown)
		e.flag(c.Complete)
		e.text(c.Status)
	}
	e.u(uint64(len(s.Gauges)))
	for _, g := range s.Gauges {
		e.text(g.Name)
		e.text(g.Source)
		e.wide(g.IntegralTotal)
		e.u(g.SpanTotal)
		e.u(g.CoveredTotal)
		e.u(g.LastValue)
		e.i(g.LastValueAt)
		e.i(g.LastAt)
		e.u(g.LastRequest)
		e.flag(g.PositionKnown)
		e.flag(g.ValueKnown)
		e.flag(g.Continuous)
		e.text(g.Status)
		e.window(g.Window)
	}
	if e.err != nil {
		return nil, e.err
	}
	length := len(e.b) + frameFooter
	if length > MaxRecordBytes {
		return nil, errors.New("usage: record too large")
	}
	copy(e.b, fileMagic)
	binary.LittleEndian.PutUint16(e.b[8:10], FormatVersion)
	binary.LittleEndian.PutUint16(e.b[10:12], AlgorithmVersion)
	binary.LittleEndian.PutUint32(e.b[12:16], uint32(length))
	e.b = binary.LittleEndian.AppendUint32(e.b, crc32.Checksum(e.b, crcTable))
	e.b = binary.LittleEndian.AppendUint32(e.b, uint32(length))
	e.b = append(e.b, tailMagic...)
	return e.b, nil
}

type decoder struct {
	b   []byte
	err error
}

func (d *decoder) u() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = ErrCorrupt
		return 0
	}
	d.b = d.b[n:]
	return v
}
func (d *decoder) i() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.err = ErrCorrupt
		return 0
	}
	d.b = d.b[n:]
	return v
}
func (d *decoder) flag() bool {
	v := d.u()
	if v > 1 {
		d.err = ErrCorrupt
	}
	return v == 1
}
func (d *decoder) text() string {
	n := d.u()
	if n > maxString || n > uint64(len(d.b)) {
		d.err = ErrCorrupt
		return ""
	}
	s := string(d.b[:int(n)])
	d.b = d.b[int(n):]
	if !utf8.ValidString(s) {
		d.err = ErrCorrupt
	}
	return s
}
func (d *decoder) wide() Uint128 { return Uint128{d.u(), d.u()} }
func (d *decoder) window() Window {
	return Window{Area: d.wide(), Span: d.u(), Covered: d.u(), Peak: d.u(), PeakAt: d.i(),
		PeakKnown: d.flag(), Samples: d.u(), MaxReadWindow: d.u()}
}

func DecodeRecord(b []byte, sandboxID string) (Record, error) {
	if len(b) < frameHeader+frameFooter || len(b) > MaxRecordBytes ||
		!bytes.Equal(b[:8], fileMagic) || !bytes.Equal(b[len(b)-8:], tailMagic) {
		return Record{}, ErrCorrupt
	}
	if binary.LittleEndian.Uint16(b[8:10]) != FormatVersion || binary.LittleEndian.Uint16(b[10:12]) != AlgorithmVersion {
		return Record{}, errors.New("usage: unsupported format/algorithm version")
	}
	if int(binary.LittleEndian.Uint32(b[12:16])) != len(b) || int(binary.LittleEndian.Uint32(b[len(b)-12:])) != len(b) ||
		crc32.Checksum(b[:len(b)-frameFooter], crcTable) != binary.LittleEndian.Uint32(b[len(b)-frameFooter:]) {
		return Record{}, ErrCorrupt
	}
	d := decoder{b: b[frameHeader : len(b)-frameFooter]}
	r := Record{Sequence: d.u(), SavedUTC: d.i()}
	s := &r.Snapshot
	s.SandboxID, s.RunEpoch, s.StartedUTC = d.text(), d.text(), d.i()
	s.SampleInterval, s.FlushInterval, s.Closed = d.i(), d.i(), d.flag()
	n := d.u()
	if n > MaxCounters {
		return Record{}, ErrCorrupt
	}
	s.Counters = make([]Counter, int(n))
	for i := range s.Counters {
		c := &s.Counters[i]
		c.Name, c.Source, c.KnownTotal = d.text(), d.text(), d.wide()
		c.LastRaw, c.Hertz, c.Remainder = d.u(), d.u(), d.u()
		c.SourceKnown, c.Complete, c.Status = d.flag(), d.flag(), d.text()
	}
	n = d.u()
	if n > MaxGauges {
		return Record{}, ErrCorrupt
	}
	s.Gauges = make([]Gauge, int(n))
	for i := range s.Gauges {
		g := &s.Gauges[i]
		g.Name, g.Source, g.IntegralTotal = d.text(), d.text(), d.wide()
		g.SpanTotal, g.CoveredTotal, g.LastValue = d.u(), d.u(), d.u()
		g.LastValueAt, g.LastAt, g.LastRequest = d.i(), d.i(), d.u()
		g.PositionKnown, g.ValueKnown, g.Continuous = d.flag(), d.flag(), d.flag()
		g.Status, g.Window = d.text(), d.window()
	}
	if d.err != nil || len(d.b) != 0 {
		return Record{}, ErrCorrupt
	}
	if s.SandboxID != sandboxID {
		return Record{}, errors.New("usage: sandbox identity mismatch")
	}
	if err := validateRecord(r); err != nil {
		return Record{}, err
	}
	return r, nil
}

func validateRecord(r Record) error {
	s := r.Snapshot
	if r.Sequence == 0 || s.SandboxID == "" || s.RunEpoch == "" || s.SampleInterval <= 0 ||
		s.SampleInterval > 1<<62-1 || s.FlushInterval < s.SampleInterval || len(s.Counters) > MaxCounters || len(s.Gauges) > MaxGauges {
		return ErrCorrupt
	}
	names := make(map[string]bool, len(s.Counters)+len(s.Gauges))
	for _, c := range s.Counters {
		if c.Name == "" || names[c.Name] || (c.SourceKnown && (c.Source == "" || c.Hertz == 0 || c.Remainder >= c.Hertz)) {
			return ErrCorrupt
		}
		names[c.Name] = true
	}
	for _, g := range s.Gauges {
		if g.Name == "" || names[g.Name] || g.CoveredTotal > g.SpanTotal || g.Window.Covered > g.Window.Span ||
			g.Window.Covered > g.CoveredTotal || g.Window.Span > g.SpanTotal {
			return ErrCorrupt
		}
		names[g.Name] = true
	}
	return nil
}

// Recovery reads at most two maximum-sized records from the tail. It validates
// the last surviving record and an explicitly incomplete append, not history.
type Recovery struct {
	Record         *Record
	End            int64
	IncompleteTail bool
}

func Recover(reader io.ReaderAt, size int64, sandboxID string) (Recovery, error) {
	if size < 0 {
		return Recovery{}, ErrCorrupt
	}
	if size == 0 {
		return Recovery{}, nil
	}
	start := size - 2*MaxRecordBytes
	if start < 0 {
		start = 0
	}
	b := make([]byte, int(size-start))
	if _, err := reader.ReadAt(b, start); err != nil {
		return Recovery{}, err
	}
	search := len(b)
	for search >= len(tailMagic) {
		pos := bytes.LastIndex(b[:search], tailMagic)
		if pos < 0 {
			break
		}
		end := pos + len(tailMagic)
		search = pos
		if pos < 8 {
			continue
		}
		n := int(binary.LittleEndian.Uint32(b[pos-4 : pos]))
		if n < frameHeader+frameFooter || n > MaxRecordBytes || n > end {
			continue
		}
		r, err := DecodeRecord(b[end-n:end], sandboxID)
		if err != nil {
			// A complete-looking final frame with a damaged checksum is not
			// an incomplete append and must never silently roll back to zero.
			if end == len(b) {
				return Recovery{}, err
			}
			continue
		}
		if err := checkIncomplete(b[end:]); err != nil {
			return Recovery{}, err
		}
		return Recovery{Record: &r, End: start + int64(end), IncompleteTail: end != len(b)}, nil
	}
	if start != 0 {
		return Recovery{}, ErrCorrupt
	}
	if err := checkIncomplete(b); err != nil {
		return Recovery{}, err
	}
	return Recovery{IncompleteTail: true}, nil
}

func checkIncomplete(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	n := len(b)
	if n < len(fileMagic) {
		if !bytes.Equal(b, fileMagic[:n]) {
			return ErrCorrupt
		}
		return nil
	}
	if !bytes.Equal(b[:8], fileMagic) {
		return ErrCorrupt
	}
	if n < frameHeader {
		return nil
	}
	if binary.LittleEndian.Uint16(b[8:10]) != FormatVersion || binary.LittleEndian.Uint16(b[10:12]) != AlgorithmVersion {
		return ErrCorrupt
	}
	length := int(binary.LittleEndian.Uint32(b[12:16]))
	if length < frameHeader+frameFooter || length > MaxRecordBytes || n >= length {
		return ErrCorrupt
	}
	return nil
}

// ReadHistory reads complete records within an already selected saved prefix.
// A cursor is a byte offset. Any encountered corruption is returned explicitly.
func ReadHistory(reader io.ReaderAt, end, cursor int64, limit int, sandboxID string) ([]Record, int64, error) {
	if cursor < 0 || cursor > end || limit < 1 || limit > 100 {
		return nil, cursor, errors.New("usage: invalid history range")
	}
	out := make([]Record, 0, limit)
	for cursor < end && len(out) < limit {
		var header [frameHeader]byte
		if end-cursor < frameHeader {
			return nil, cursor, ErrCorrupt
		}
		if _, err := reader.ReadAt(header[:], cursor); err != nil {
			return nil, cursor, err
		}
		n := int64(binary.LittleEndian.Uint32(header[12:]))
		if n < frameHeader+frameFooter || n > MaxRecordBytes || n > end-cursor {
			return nil, cursor, ErrCorrupt
		}
		b := make([]byte, int(n))
		if _, err := reader.ReadAt(b, cursor); err != nil {
			return nil, cursor, err
		}
		r, err := DecodeRecord(b, sandboxID)
		if err != nil {
			return nil, cursor, fmt.Errorf("usage offset %d: %w", cursor, err)
		}
		if len(out) > 0 && r.Sequence != out[len(out)-1].Sequence+1 {
			return nil, cursor, ErrCorrupt
		}
		out = append(out, r)
		cursor += n
	}
	return out, cursor, nil
}
