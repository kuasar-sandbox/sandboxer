package ctl

import (
	"bytes"
	"encoding/json"
	"errors"
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
