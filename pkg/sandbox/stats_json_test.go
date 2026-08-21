package sandbox

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestUffdStatsExposeFaultFirstTailMetrics(t *testing.T) {
	counters := map[string]uint64{
		"pages_zeroed":          3,
		"pages_copied":          5,
		"fault_queue_wait_ns":   11,
		"fault_queue_wait_p50":  12,
		"fault_queue_wait_p95":  13,
		"fault_queue_wait_p99":  14,
		"fault_queue_depth_hwm": 15,
		"fault_inflight_hwm":    16,
		"source_read_calls":     17,
		"source_read_bytes":     18,
		"source_read_ns":        19,
		"urgent_copy_calls":     20,
		"urgent_copy_ns":        21,
		"urgent_zero_calls":     22,
		"urgent_zero_ns":        23,
		"tail_submitted":        24,
		"tail_dropped_busy":     25,
		"tail_canceled":         26,
		"tail_buffered_data":    27,
		"tail_deferred_data":    28,
		"tail_zero":             29,
		"tail_pages_planned":    30,
		"tail_pages_completed":  31,
		"tail_copy_ns":          32,
		"tail_zero_ns":          33,
		"tail_conflicts":        34,
		"tail_partial":          35,
	}
	report := buildUffdJSON(counters, 16*4096)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range counters {
		if value, ok := got[key]; !ok || uint64(value.(float64)) != want {
			t.Fatalf("JSON %q = %v, want %d", key, value, want)
		}
	}
	if _, ok := got["batch_calls"]; ok {
		t.Fatal("obsolete synchronous batch metric remains in JSON")
	}
	for _, key := range []string{"tail_window_current", "tail_window_grows", "tail_window_resets"} {
		if _, ok := got[key]; ok {
			t.Fatalf("obsolete adaptive-tail metric %q remains in JSON", key)
		}
	}
	if report.ResidentPages != 8 || report.LazyLoadRatio != 0.5 {
		t.Fatalf("resident ratio = %d/%v", report.ResidentPages, report.LazyLoadRatio)
	}
}

func TestWriteUffdStatsUsesFaultFirstTailKeys(t *testing.T) {
	var out bytes.Buffer
	writeUffdStats(&out, map[string]uint64{
		"source_read_calls": 7,
		"tail_submitted":    9,
	})
	text := out.String()
	for _, want := range []string{"source_read_calls", "tail_submitted", "fault_queue_wait_p95"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stats output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "batch_calls") {
		t.Fatalf("stats output contains obsolete batch metric:\n%s", text)
	}
}
