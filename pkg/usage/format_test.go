package usage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"reflect"
	"testing"
)

func sampleRecord() Record {
	return Record{Sequence: 1, SavedUTC: 99, Snapshot: Snapshot{SandboxID: "test", RunEpoch: "epoch", StartedUTC: 1,
		SampleInterval: 1e9, FlushInterval: 300e9,
		Counters: []Counter{{Name: "guest/cpu", Source: "boot/pid/start", KnownTotal: Uint128{4, 5}, LastRaw: 456, Hertz: 100, SourceKnown: true, Complete: true, Status: OK}},
		Gauges: []Gauge{{Name: "guest/memory", IntegralTotal: Uint128{8, 7}, Source: "ram", SpanTotal: 10, CoveredTotal: 9,
			LastValue: 6, LastAt: 10, LastValueAt: 10, ValueKnown: true, PositionKnown: true, Continuous: true, LastRequest: 2, Status: OK,
			Window: Window{Area: Uint128{3, 2}, Span: 10, Covered: 9, Peak: 6, PeakAt: 10, PeakKnown: true, Samples: 2}}}}}
}

func TestRecordAndEveryTruncation(t *testing.T) {
	r := sampleRecord()
	b, err := EncodeRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRecord(b, "test")
	if err != nil || !reflect.DeepEqual(r, decoded) {
		t.Fatalf("%v\n%+v\n%+v", err, r, decoded)
	}
	for cut := 0; cut < len(b); cut++ {
		input := append(append([]byte(nil), b...), b[:cut]...)
		recovery, err := Recover(bytes.NewReader(input), int64(len(input)), "test")
		if err != nil || recovery.Record == nil || recovery.End != int64(len(b)) || recovery.IncompleteTail != (cut > 0) {
			t.Fatalf("cut=%d recovery=%+v err=%v", cut, recovery, err)
		}
		initial, err := Recover(bytes.NewReader(b[:cut]), int64(cut), "test")
		if err != nil || initial.Record != nil || initial.End != 0 {
			t.Fatalf("first cut=%d: %+v %v", cut, initial, err)
		}
	}
}

func TestCorruptionIdentityAndHistory(t *testing.T) {
	r := sampleRecord()
	b, _ := EncodeRecord(r)
	for i := range b {
		bad := append([]byte(nil), b...)
		bad[i] ^= 0x80
		if _, err := DecodeRecord(bad, "test"); err == nil {
			t.Fatalf("corruption accepted at %d", i)
		}
		if _, err := Recover(bytes.NewReader(bad), int64(len(bad)), "test"); err == nil {
			t.Fatalf("damaged full record recovered at %d", i)
		}
	}
	if _, err := DecodeRecord(b, "different"); err == nil {
		t.Fatal("identity accepted")
	}
	bad := append([]byte(nil), b...)
	bad[40] ^= 1
	r.Sequence = 2
	last, _ := EncodeRecord(r)
	file := append(bad, last...)
	got, err := Recover(bytes.NewReader(file), int64(len(file)), "test")
	if err != nil || got.Record.Sequence != 2 {
		t.Fatalf("fast recovery: %+v %v", got, err)
	}
	if _, _, err := ReadHistory(bytes.NewReader(file), int64(len(file)), 0, 10, "test"); err == nil {
		t.Fatal("history corruption hidden")
	}
}

func historyFile(t *testing.T, sequences ...uint64) ([]byte, []int64) {
	t.Helper()
	var data []byte
	offsets := []int64{0}
	for _, sequence := range sequences {
		r := sampleRecord()
		r.Sequence = sequence
		b, err := EncodeRecord(r)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, b...)
		offsets = append(offsets, int64(len(data)))
	}
	return data, offsets
}

func TestHistorySequenceAcrossEveryPageBoundary(t *testing.T) {
	for _, sequences := range [][]uint64{{1, 3}, {1, 1}, {1, 3, 2}, {2}, {1, ^uint64(0), 1}} {
		data, _ := historyFile(t, sequences...)
		for _, limit := range []int{1, 2, 100} {
			cursor, end := int64(0), int64(len(data))
			var err error
			for cursor < end {
				_, cursor, err = ReadHistory(bytes.NewReader(data), end, cursor, limit, "test")
				if err != nil {
					break
				}
			}
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("sequences=%v limit=%d accepted or wrong error: %v", sequences, limit, err)
			}
		}
	}
	data, offsets := historyFile(t, 1, 2, 3)
	for i, cursor := range offsets {
		records, next, err := ReadHistory(bytes.NewReader(data), int64(len(data)), cursor, 1, "test")
		if err != nil {
			t.Fatal(err)
		}
		if i == 3 {
			if len(records) != 0 || next != cursor {
				t.Fatalf("empty terminal page: %+v %d", records, next)
			}
		} else if len(records) != 1 || records[0].Sequence != uint64(i+1) || next != offsets[i+1] {
			t.Fatalf("page %d: %+v next=%d", i, records, next)
		}
	}
}

type boundedHistoryReader struct {
	io.ReaderAt
	minimum, bytes int64
	calls          int
}

func (r *boundedHistoryReader) ReadAt(p []byte, off int64) (int, error) {
	if off < r.minimum {
		return 0, errors.New("read before immediate predecessor")
	}
	r.bytes += int64(len(p))
	r.calls++
	return r.ReaderAt.ReadAt(p, off)
}

func TestHistoryValidatesOnlyBoundedImmediatePredecessor(t *testing.T) {
	data, offsets := historyFile(t, 1, 2, 3)
	reader := &boundedHistoryReader{ReaderAt: bytes.NewReader(data), minimum: offsets[1]}
	if records, _, err := ReadHistory(reader, int64(len(data)), offsets[2], 1, "test"); err != nil || len(records) != 1 {
		t.Fatalf("%+v %v", records, err)
	}
	if reader.calls != 4 || reader.bytes > 2*MaxRecordBytes+frameHeader+frameFooter {
		t.Fatalf("unbounded predecessor check: %+v", reader)
	}
	// Every damaged predecessor byte is detected even with a valid next frame.
	for pos := offsets[1]; pos < offsets[2]; pos++ {
		bad := append([]byte(nil), data...)
		bad[pos] ^= 0x80
		if _, _, err := ReadHistory(bytes.NewReader(bad), int64(len(bad)), offsets[2], 1, "test"); err == nil {
			t.Fatalf("bad predecessor byte %d accepted", pos)
		}
	}
	for cursor := int64(1); cursor < offsets[1]; cursor++ {
		if _, _, err := ReadHistory(bytes.NewReader(data), int64(len(data)), cursor, 1, "test"); err == nil {
			t.Fatalf("partial cursor %d accepted", cursor)
		}
	}
	wrong := sampleRecord()
	wrong.Sequence, wrong.Snapshot.SandboxID = 2, "other"
	frame, err := EncodeRecord(wrong)
	if err != nil {
		t.Fatal(err)
	}
	bad := append(append(append([]byte(nil), data[:offsets[1]]...), frame...), data[offsets[2]:]...)
	if _, _, err := ReadHistory(bytes.NewReader(bad), int64(len(bad)), offsets[1]+int64(len(frame)), 1, "test"); err == nil {
		t.Fatal("wrong predecessor identity accepted")
	}
}

func FuzzHistoryCursor(f *testing.F) {
	b, _ := EncodeRecord(sampleRecord())
	f.Add(b, int64(0), 1)
	f.Add(b, int64(len(b)), 1)
	f.Fuzz(func(t *testing.T, b []byte, cursor int64, limit int) {
		if len(b) > 2*MaxRecordBytes {
			return
		}
		_, _, _ = ReadHistory(bytes.NewReader(b), int64(len(b)), cursor, limit, "test")
	})
}

func FuzzDecodeRecord(f *testing.F) {
	b, _ := EncodeRecord(sampleRecord())
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 2*MaxRecordBytes {
			return
		}
		r, err := DecodeRecord(b, "test")
		if err == nil {
			encoded, err := EncodeRecord(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeRecord(encoded, "test"); err != nil {
				t.Fatal(err)
			}
		}
		_, _ = Recover(bytes.NewReader(b), int64(len(b)), "test")
		// Also reach payload validation beyond CRC so length/array bombs
		// exercise the allocation guards in the decoder.
		if len(b) >= 32 && len(b) <= MaxRecordBytes {
			b = append([]byte(nil), b...)
			copy(b[:8], fileMagic)
			copy(b[len(b)-8:], tailMagic)
			binary.LittleEndian.PutUint16(b[8:10], FormatVersion)
			binary.LittleEndian.PutUint16(b[10:12], AlgorithmVersion)
			binary.LittleEndian.PutUint32(b[12:16], uint32(len(b)))
			binary.LittleEndian.PutUint32(b[len(b)-12:], uint32(len(b)))
			binary.LittleEndian.PutUint32(b[len(b)-16:], crc32.Checksum(b[:len(b)-16], crcTable))
			_, _ = DecodeRecord(b, "test")
		}
	})
}
