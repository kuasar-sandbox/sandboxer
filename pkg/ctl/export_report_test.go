package ctl

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestExportReportResponsePreservesLargeCompleteMetadata(t *testing.T) {
	root := "manifest://" + strings.Repeat("f", 64)
	for _, size := range []int{0, 80 * 1024, (1 << 20) - 200} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			// A selected carrier from Bundle.Refs can exceed the portable scalar
			// limit; repeated selectors still name their actual physical carrier.
			carrier := "file://parent.bundle@manifest:" + strings.Repeat("a", 64) + "@location:" + strings.Repeat("l", size)
			want := Response{Type: TypeExportDone, SandboxRef: root, SandboxManifestKey: strings.Repeat("f", 64),
				ExportReferences: &ExportReferences{SandboxRefs: []string{root}, RemovedRefs: []string{carrier, carrier + "b"}}}
			var wire bytes.Buffer
			if err := WriteExportReportResponse(&wire, want); err != nil {
				t.Fatal(err)
			}
			encoded := append([]byte(nil), wire.Bytes()...)
			for len(encoded) > 0 {
				if len(encoded) < 4 {
					t.Fatal("truncated frame")
				}
				n := int(binary.LittleEndian.Uint32(encoded))
				if n > MaxMessageBytes || n > len(encoded)-4 {
					t.Fatalf("invalid frame size %d", n)
				}
				encoded = encoded[4+n:]
			}
			got, err := ReadExportReportResponse(&wire)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("report changed: err=%v, root=%q", err, got.SandboxRef)
			}
		})
	}
}

func TestExportReportResponseRejectsIncompleteOrMalformedStream(t *testing.T) {
	body, err := json.Marshal(Response{Type: TypeExportDone, SandboxRef: "manifest://" + strings.Repeat("a", 64),
		ExportReferences: &ExportReferences{SandboxRefs: []string{}, RemovedRefs: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		frames []exportReportFrame
	}{
		{"empty-header", []exportReportFrame{{Type: typeExportReportStart}}},
		{"wrong-header", []exportReportFrame{{Type: typeExportReportDone, BodyBytes: len(body)}}},
		{"disconnect", []exportReportFrame{{Type: typeExportReportStart, BodyBytes: len(body)}, {Type: typeExportReportChunk, Data: body}}},
		{"short", []exportReportFrame{{Type: typeExportReportStart, BodyBytes: len(body)}, {Type: typeExportReportChunk, Data: body[:len(body)-1]}, {Type: typeExportReportDone, BodyBytes: len(body)}}},
		{"overflow", []exportReportFrame{{Type: typeExportReportStart, BodyBytes: len(body) - 1}, {Type: typeExportReportChunk, Data: body}}},
		{"empty-chunk", []exportReportFrame{{Type: typeExportReportStart, BodyBytes: len(body)}, {Type: typeExportReportChunk}}},
		{"wrong-count", []exportReportFrame{{Type: typeExportReportStart, BodyBytes: len(body)}, {Type: typeExportReportChunk, Data: body}, {Type: typeExportReportDone, BodyBytes: len(body) + 1}}},
		{"wrong-frame", []exportReportFrame{{Type: typeExportReportStart, BodyBytes: len(body)}, {Type: TypeSnapshotDone}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire bytes.Buffer
			for _, frame := range tc.frames {
				if err := WriteMessage(&wire, frame); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ReadExportReportResponse(&wire); err == nil {
				t.Fatal("accepted an incomplete report")
			}
		})
	}
}

func TestExportReportResponsePreservesOldOwnerError(t *testing.T) {
	want := Response{Type: TypeError, Msg: "unknown type: " + TypeExportReportRequest}
	var wire bytes.Buffer
	if err := WriteMessage(&wire, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadExportReportResponse(&wire)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("old owner error lost: %+v %v", got, err)
	}
}

type failedExportReportWriter struct{ written int }

func (w *failedExportReportWriter) Write(p []byte) (int, error) {
	if w.written > 0 {
		return 0, io.ErrClosedPipe
	}
	w.written += len(p)
	return len(p), nil
}

func TestExportReportResponseStopsAtWriteFailure(t *testing.T) {
	err := WriteExportReportResponse(&failedExportReportWriter{}, Response{Type: TypeExportDone, ExportReferences: &ExportReferences{SandboxRefs: []string{"x"}, RemovedRefs: []string{}}})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write error lost: %v", err)
	}
}

func TestExportReportDisconnectStillRunsCaptureRetirement(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	finished := make(chan error, 1)
	go func() {
		var first exportReportFrame
		err := ReadMessage(client, &first)
		_ = client.Close()
		finished <- err
	}()
	retired := 0
	response := Response{Type: TypeExportDone,
		ExportReferences: &ExportReferences{SandboxRefs: []string{"x"}, RemovedRefs: []string{}},
		AfterWrite:       func() { retired++ },
	}
	if err := writeCaptureResponseWith(server, response, WriteExportReportResponse); err == nil {
		t.Fatal("disconnected peer accepted a complete report")
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if retired != 1 {
		t.Fatalf("capture retirement calls=%d, want 1", retired)
	}
}
