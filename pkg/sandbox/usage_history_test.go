package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/usage"
)

func TestUsageHistoryBudgetStopsBeforeReadingWholePage(t *testing.T) {
	for _, escaped := range []bool{false, true} {
		t.Run(fmt.Sprint(escaped), func(t *testing.T) {
			r := usage.Record{Sequence: 1, Snapshot: usage.Snapshot{SandboxID: "test", RunEpoch: "epoch", SampleInterval: 1, FlushInterval: 1}}
			for i := 0; i < 192; i++ {
				text := strings.Repeat("x", 240)
				if escaped {
					text = strings.Repeat("\x00<&", 80)
				}
				r.Snapshot.Counters = append(r.Snapshot.Counters, usage.Counter{Name: fmt.Sprintf("%04d%s", i, text), Source: text})
			}
			if b, err := usage.EncodeRecord(r); err != nil || len(b) > usage.MaxRecordBytes {
				t.Fatalf("fixture is not a legal record: %d %v", len(b), err)
			}
			for _, limit := range []int{20, 100} {
				calls := 0
				history := func(cursor int64, n int) ([]usage.Record, int64, error) {
					calls++
					if n != 1 {
						t.Fatalf("read batch before budget: %d", n)
					}
					r.Sequence = uint64(cursor + 1)
					return []usage.Record{r}, cursor + 1, nil
				}
				body, err := marshalUsageHistory(history, 100, 0, limit)
				if err == nil || !strings.Contains(err.Error(), "reduce history limit") || body != nil {
					t.Fatalf("page limit=%d: body=%d err=%v", limit, len(body), err)
				}
				if calls > 10 || (escaped && calls != 2) {
					t.Fatalf("kept reading after JSON budget: escaped=%v limit=%d calls=%d", escaped, limit, calls)
				}
			}
		})
	}
}

func TestUsageHistoryWireShapeAndSelectedSavedPrefix(t *testing.T) {
	for _, cursor := range []int64{0, 3} {
		var calls []int64
		history := func(offset int64, n int) ([]usage.Record, int64, error) {
			calls = append(calls, offset)
			// Simulate a newer committed S. No record past selected end=3
			// may be included or preloaded as part of the response page.
			return []usage.Record{{Sequence: uint64(offset + 1)}}, offset + 1, nil
		}
		body, err := marshalUsageHistory(history, 3, cursor, 100)
		if err != nil {
			t.Fatal(err)
		}
		var wire bytes.Buffer
		if err := ctl.WriteUsageResponse(&wire, ctl.Response{Type: ctl.TypeUsageResponse, Usage: body}); err != nil {
			t.Fatal(err)
		}
		response, err := ctl.ReadUsageResponse(&wire)
		if err != nil || response.Type != ctl.TypeUsageResponse {
			t.Fatalf("%+v %v", response, err)
		}
		var page struct {
			Records []usage.Record `json:"records"`
			Next    int64          `json:"next_cursor,string"`
		}
		if err := json.Unmarshal(response.Usage, &page); err != nil || page.Next != 3 || len(page.Records) != int(3-cursor) || page.Records == nil {
			t.Fatalf("%+v err=%v", page, err)
		}
		if len(calls) != max(1, int(3-cursor)) {
			t.Fatalf("read beyond selected S: %v", calls)
		}
	}
	for _, args := range [][3]int64{{-1, 0, 1}, {0, -1, 1}, {0, 1, 1}, {1, 0, 0}, {1, 0, 101}} {
		_, err := marshalUsageHistory(func(int64, int) ([]usage.Record, int64, error) {
			t.Fatal("read invalid range")
			return nil, 0, nil
		}, args[0], args[1], int(args[2]))
		if err == nil {
			t.Fatal("invalid range accepted")
		}
	}
	want := errors.New("bad predecessor")
	if _, err := marshalUsageHistory(func(int64, int) ([]usage.Record, int64, error) { return nil, 3, want }, 3, 3, 1); !errors.Is(err, want) {
		t.Fatalf("terminal cursor validation hidden: %v", err)
	}
}
