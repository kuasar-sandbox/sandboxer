package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

type lazyStatsBackend struct {
	*bytes.Reader
	extraCalls int
}

func (*lazyStatsBackend) WriteAt([]byte, int64) (int, error) { return 0, vhost.ErrReadOnly }
func (*lazyStatsBackend) Flush() error                     { return nil }
func (*lazyStatsBackend) Discard(int64, int64) error        { return nil }
func (*lazyStatsBackend) SupportsDiscard() bool            { return false }
func (*lazyStatsBackend) ReadOnly() bool                    { return true }
func (b *lazyStatsBackend) BackendStats() map[string]any {
	b.extraCalls++
	return map[string]any{"test_extra": "kept"}
}

func TestLazyStatsTickerSkipsBackendExtras(t *testing.T) {
	backend := &lazyStatsBackend{Reader: bytes.NewReader(make([]byte, 4096))}
	server := vhost.NewServer("", backend, nil)
	server.EnableStats("blk-test", "test://backend")
	server.Stats().Record(vhost.BlkTypeIn, 4096, uint64(2*time.Millisecond), true)
	server.Stats().MarkRead(0, 4096)

	// Run the real polling path. Its first activity log ends the loop; the
	// deadline only bounds a broken ticker, rather than measuring performance.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var line string
	lazyStatsTicker(ctx, time.Millisecond, func() *uffd.Handler { return nil }, []*vhost.Server{server}, func(format string, args ...any) {
		line = fmt.Sprintf(format, args...)
		cancel()
	})
	if !strings.Contains(line, " | blk-test ") || !strings.Contains(line, "p99=2.0ms") {
		t.Fatalf("periodic log lost read activity or latency: %q", line)
	}
	if backend.extraCalls != 0 {
		t.Fatalf("periodic polling collected backend extras %d times", backend.extraCalls)
	}

	// Final summaries still include backend-specific metrics in both formats.
	var text bytes.Buffer
	if _, err := server.WriteStatsTo(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "test_extra=kept") {
		t.Fatalf("final text omitted backend extras: %s", text.String())
	}
	path := filepath.Join(t.TempDir(), "stats.json")
	if err := writeStatsJSON(path, statsBundle{Servers: []*vhost.Server{server}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report statsReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Backends) != 1 {
		t.Fatalf("final JSON backends = %d, want 1", len(report.Backends))
	}
	got := report.Backends[0]
	if got.Extra["test_extra"] != "kept" || got.Read.Count != 1 || got.Read.Bytes != 4096 || got.LoadedBlocks != 1 {
		t.Fatalf("final JSON lost backend extras or request statistics: %+v", got)
	}
}
