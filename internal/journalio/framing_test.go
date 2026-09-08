package journalio

import (
	"strings"
	"testing"
)

func TestWriterDoesNotTrimCarriageReturnAtSizeBoundary(t *testing.T) {
	var got strings.Builder
	w := mustWriter(t, Target{Tag: "app"}, nil, "", func(message string, _ map[string]string) error {
		got.WriteString(message)
		return nil
	})
	want := strings.Repeat("x", MaxLine-1) + "\rmore"
	_, _ = w.Write([]byte(want + "\n"))
	_ = w.Close()
	if got.String() != want {
		t.Fatal("forced size boundary discarded a data byte")
	}
}

func TestWriterClosePreservesUnterminatedCarriageReturn(t *testing.T) {
	var entries []entry
	w := mustWriter(t, Target{Tag: "app"}, nil, "", capture(&entries))
	_, _ = w.Write([]byte("tail\r"))
	_ = w.Close()
	if len(entries) != 1 || entries[0].message != "tail\r" {
		t.Fatalf("Close changed unterminated data: %#v", entries)
	}
}
