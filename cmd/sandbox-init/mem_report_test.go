package main

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestMemReportAttemptLogsRecoveryAfterTransientFailures(t *testing.T) {
	var logs []string
	notifyCalls := 0
	push := newMemReportAttempt(
		func() (uint64, uint64, error) { return 128 << 20, 256 << 20, nil },
		func(avail, total uint64) error {
			notifyCalls++
			if avail != 128<<20 || total != 256<<20 {
				t.Fatalf("notify(%d, %d)", avail, total)
			}
			if notifyCalls <= 2 {
				return errors.New("read: resource temporarily unavailable")
			}
			return nil
		},
		func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	)

	push()
	push()
	push()
	push()

	want := []string{
		"mem_report: read: resource temporarily unavailable",
		"mem_report: read: resource temporarily unavailable",
		"mem_report: recovered after 2 consecutive failures",
	}
	if !reflect.DeepEqual(logs, want) {
		t.Fatalf("logs = %#v, want %#v", logs, want)
	}
}
