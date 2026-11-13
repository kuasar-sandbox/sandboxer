package usage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
)

// The Guest service exercises these byte boundaries separately in
// guest-runtime/cmd/sandbox-init.TestUsageRunEpochContract. Keeping each endpoint
// checked against the shared wire limit avoids a Guest dependency on Host code.
func TestUsageRunEpochContract(t *testing.T) {
	if proto.MaxUsageRunEpochBytes != 128 {
		t.Fatalf("run epoch wire limit changed: %d", proto.MaxUsageRunEpochBytes)
	}
	for _, epoch := range []string{"", "one", strings.Repeat("x", 128), strings.Repeat("x", 129), strings.Repeat("x", 256), strings.Repeat("😀", 32), strings.Repeat("😀", 33)} {
		t.Run(epoch, func(t *testing.T) {
			m, err := Open(t.TempDir(), "test", epoch, time.Now(), time.Second, time.Minute)
			if m != nil {
				defer func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					m.Close(ctx, time.Now())
				}()
			}
			want := epoch != "" && len(epoch) <= proto.MaxUsageRunEpochBytes
			if (err == nil) != want {
				t.Fatalf("epoch bytes=%d: Open=%v, want accepted=%t", len(epoch), err, want)
			}
		})
	}
}
