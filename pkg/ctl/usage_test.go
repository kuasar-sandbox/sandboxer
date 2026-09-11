package ctl

import (
	"bytes"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
)

type usageFailedWriter struct{ calls int }

func (w *usageFailedWriter) Write(b []byte) (int, error) {
	w.calls++
	return 1, errors.New("partial write")
}

func TestUsageLargePageAndPartialWrite(t *testing.T) {
	var buf bytes.Buffer
	raw := json.RawMessage(`"` + strings.Repeat("x", MaxUsageResponseBytes) + `"`)
	if err := WriteUsageResponse(&buf, Response{Type: TypeUsageResponse, Usage: raw}); err != nil {
		t.Fatal(err)
	}
	r, err := ReadUsageResponse(&buf)
	if err != nil || r.Type != TypeError || !strings.Contains(r.Msg, "reduce history limit") {
		t.Fatalf("%+v %v", r, err)
	}
	w := &usageFailedWriter{}
	if err := WriteUsageResponse(w, Response{Type: TypeUsageResponse, Usage: json.RawMessage(`{}`)}); err == nil || w.calls != 1 {
		t.Fatalf("calls=%d err=%v", w.calls, err)
	}
}

func TestUsageOversizedRawPageRejectedBeforeEncoding(t *testing.T) {
	// Allocation is measured only after constructing the caller-owned raw
	// input. This is a local serializer regression, not VM performance data.
	raw := json.RawMessage(`"` + strings.Repeat("x", 16*MaxUsageResponseBytes) + `"`)
	var buf bytes.Buffer
	if err := WriteUsageResponse(&buf, Response{Type: TypeUsageResponse, Usage: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := WriteUsageResponse(&buf, Response{Type: TypeUsageResponse, Usage: raw}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 2*MaxUsageResponseBytes {
		t.Fatalf("oversized raw page encoded before rejection: allocated=%d", n)
	}
	response, err := ReadUsageResponse(&buf)
	if err != nil || response.Type != TypeError || !strings.Contains(response.Msg, "reduce history limit") {
		t.Fatalf("%+v %v", response, err)
	}
}
