package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

// lazyStatsTicker periodically logs lazy-load progress so a slow remote store
// or cache (or a backed-up fault queue) is visible in real time. Each printed
// line covers ONLY the interval since the previous line — windowed rates and
// windowed page-in / block-read latency percentiles, never since-start.
//
// Cadence is adaptive: it polls at `fast` (min(base,2s)) while there is
// activity — so a busy or slow phase reports promptly — and backs off to `base`
// (the configured --stats-interval) after a couple of idle polls. A poll whose
// window saw no faults, no reads, and nothing in flight prints nothing, so a
// warm, fully-paged sandbox is silent. The first poll starts in fast mode to
// catch the cold-start / restore page-in burst. Stops when ctx is cancelled.
//
// getUffd returns the uffd handler or nil (constructed asynchronously on the
// first va_report); the ticker tolerates nil until then. Baselines advance
// every poll, so windows are contiguous and idle (zero-delta) gaps lose nothing.
func lazyStatsTicker(ctx context.Context, base time.Duration, getUffd func() *uffd.Handler, servers []*vhost.Server, logf func(string, ...any)) {
	fast := min(base, 2*time.Second)
	const idleBackoffPolls = 2 // consecutive idle polls before backing off to base

	sleep := fast
	var prevU uffd.LazyStats
	prev := make([]vhost.StatsSnapshot, len(servers))
	last := time.Now()
	idle := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		now := time.Now()
		elapsed := now.Sub(last).Seconds()
		if elapsed <= 0 {
			elapsed = sleep.Seconds()
		}

		// Sample every disk, then compute each window against the PREVIOUS
		// baseline before advancing it.
		cur := make([]vhost.StatsSnapshot, len(servers))
		anyDiskRead := false
		var diskSeg strings.Builder
		for i, s := range servers {
			cur[i] = s.SnapshotStats()
			d := cur[i].Read.Count - prev[i].Read.Count
			b := cur[i].Read.Bytes - prev[i].Read.Bytes
			w := cur[i].Read.Sub(prev[i].Read)
			if d > 0 {
				anyDiskRead = true
			}
			fmt.Fprintf(&diskSeg, " | %s %.0f rd/s %.1fMB/s p99=%s",
				cur[i].Name, float64(d)/elapsed, float64(b)/elapsed/1e6, fmtNs(w.P99()))
		}
		var u uffd.LazyStats
		if h := getUffd(); h != nil {
			u = h.LazyStats()
		}

		faultDelta := (u.FaultsAbsent + u.FaultsReleased + u.FaultsLoaded) -
			(prevU.FaultsAbsent + prevU.FaultsReleased + prevU.FaultsLoaded)
		pageDelta := (u.PagesCopied + u.PagesZeroed) - (prevU.PagesCopied + prevU.PagesZeroed)
		win := u.PageIn.Sub(prevU.PageIn)
		active := faultDelta > 0 || anyDiskRead || u.Inflight > 0 || u.QueueDepth > 0

		prevU, last = u, now
		copy(prev, cur)

		if !active {
			idle++
			if idle >= idleBackoffPolls {
				sleep = base
			}
			continue
		}
		idle, sleep = 0, fast
		logf("[lazy] uffd %.0f fault/s %.0f pgin/s inflight=%d queued=%d fetch_p50=%s p99=%s max=%s%s",
			float64(faultDelta)/elapsed, float64(pageDelta)/elapsed, u.Inflight, u.QueueDepth,
			fmtNs(win.P50()), fmtNs(win.P99()), fmtNs(win.MaxNs), diskSeg.String())
	}
}

// fmtNs renders a nanosecond latency in the largest unit ≤ the value.
func fmtNs(ns uint64) string {
	switch {
	case ns == 0:
		return "0"
	case ns < 1_000:
		return fmt.Sprintf("%dns", ns)
	case ns < 1_000_000:
		return fmt.Sprintf("%.0fµs", float64(ns)/1e3)
	case ns < 1_000_000_000:
		return fmt.Sprintf("%.1fms", float64(ns)/1e6)
	default:
		return fmt.Sprintf("%.2fs", float64(ns)/1e9)
	}
}
