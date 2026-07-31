package restore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// prefetchTask owns the lifetime of one best-effort snapshot prefetch. Stop
// must run before any Stream used by the task is closed.
type prefetchTask struct {
	cancel context.CancelFunc
	done   <-chan struct{}
	stop   sync.Once
}

// startMemoryPrefetch warms only the current top snapshot bundle. Parent
// memory layers and disk streams are deliberately absent from this API, which
// keeps their potentially cold historical data out of the speculative working
// set.
//
// Prefetch is best-effort: eligibility failures and I/O failures never fail the
// restore. A non-nil task owns a goroutine that Stop cancels and joins.
func startMemoryPrefetch(
	ctx context.Context,
	mode config.PrefetchMode,
	snapshotManifestKey string,
	selfStream fetch.Stream,
	parentLayers int,
	logf func(string, ...any),
) *prefetchTask {
	if mode != config.PrefetchMemory {
		return nil
	}
	backend := "file"
	completion := "advised"
	identity := fmt.Sprintf("backend=%s parent_layers=%d", backend, parentLayers)
	if snapshotManifestKey != "" {
		backend = "manifest"
		completion = "completed"
		identity = fmt.Sprintf("backend=%s parent_layers=%d key=%s", backend, parentLayers, snapshotManifestKey)
	}
	prefetcher, ok := selfStream.(fetch.Prefetcher)
	if !ok {
		logPrefetch(logf, "memory prefetch skipped reason=no_capability %s", identity)
		return nil
	}

	prefetchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	startedAt := time.Now()
	logPrefetch(logf, "memory prefetch started %s", identity)
	go func() {
		defer close(done)
		if err := prefetcher.Prefetch(prefetchCtx); err != nil {
			duration := time.Since(startedAt)
			if errors.Is(err, context.Canceled) {
				logPrefetch(logf, "memory prefetch canceled %s duration=%s", identity, duration)
			} else {
				logPrefetch(logf, "memory prefetch failed %s duration=%s error=%v restore_continues=on_demand", identity, duration, err)
			}
			return
		}
		logPrefetch(logf, "memory prefetch %s %s duration=%s", completion, identity, time.Since(startedAt))
	}()
	return &prefetchTask{cancel: cancel, done: done}
}

// Stop idempotently cancels and joins the asynchronous operation. A nil task
// represents a disabled or ineligible prefetch and is also safe to stop.
func (t *prefetchTask) Stop() {
	if t == nil {
		return
	}
	t.stop.Do(func() {
		t.cancel()
		<-t.done
	})
}

func logPrefetch(logf func(string, ...any), format string, args ...any) {
	if logf != nil {
		logf(format, args...)
	}
}
