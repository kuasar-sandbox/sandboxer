package main

import (
	"errors"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestRunQuiesceWith(t *testing.T) {
	tests := []struct {
		name      string
		skip      bool
		writeErr  error
		want      proto.DropCachesResult
		wantWrite bool
	}{
		{name: "skipped", skip: true, want: proto.DropCachesSkipped},
		{name: "succeeded", want: proto.DropCachesSucceeded, wantWrite: true},
		{name: "failed", writeErr: errors.New("read-only"), want: proto.DropCachesFailed, wantWrite: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synced := false
			wrote := false
			got := runQuiesceWith(tt.skip, func() {
				synced = true
			}, func(path string, data []byte) error {
				wrote = true
				if path != "/proc/sys/vm/drop_caches" || string(data) != "3\n" {
					t.Fatalf("unexpected write %q %q", path, data)
				}
				return tt.writeErr
			})
			if !synced {
				t.Fatal("sync must run for every cache policy")
			}
			if wrote != tt.wantWrite {
				t.Fatalf("write=%v, want %v", wrote, tt.wantWrite)
			}
			if got != tt.want {
				t.Fatalf("result=%q, want %q", got, tt.want)
			}
		})
	}
}
