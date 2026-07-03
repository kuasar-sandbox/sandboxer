package sandbox

import (
	"encoding/json"
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"os"
	"runtime"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

// statsReport is the JSON layout written by --stats-json. The bucket
// bounds are included once at the top so consumers can interpret each
// backend's lat_buckets array. The last element of lat_buckets is the
// >max-bucket-bound overflow (latency exceeded all named bounds).
type statsReport struct {
	BucketsNs []uint64                `json:"latency_bucket_bounds_ns"`
	Backends  []statsBackendJSON      `json:"backends"`
	Uffd      *uffdStatsJSON          `json:"uffd,omitempty"`
	Runtime   *runtimeStatsJSON       `json:"runtime,omitempty"`
	Ping      *guestlink.PingSnapshot `json:"ping,omitempty"`
	Wallclock wallclockJSON           `json:"wallclock"`
}

// uffdStatsJSON captures the page-fault handler counters plus a
// derived "lazy-load ratio" so consumers (perf bench) can read the
// resident-memory share without recomputing. Both file:// and
// manifest:// snapshot paths surface the same shape.
type uffdStatsJSON struct {
	FaultsAbsent        uint64 `json:"faults_absent"`
	FaultsReleased      uint64 `json:"faults_released"`
	FaultsLoaded        uint64 `json:"faults_loaded"`
	ZeropageCalls       uint64 `json:"zeropage_calls"`
	CopyCalls           uint64 `json:"copy_calls"`
	PagesZeroed         uint64 `json:"pages_zeroed"`
	PagesCopied         uint64 `json:"pages_copied"`
	Wakes               uint64 `json:"wakes"`
	RemoveEvents        uint64 `json:"remove_events"`
	RemoveQDropped      uint64 `json:"remove_q_dropped"`
	RemoveEventsBatched uint64 `json:"remove_events_batched"`
	MadviseCalls        uint64 `json:"madvise_calls"`
	MadviseBytes        uint64 `json:"madvise_bytes"`
	BackendLookupMiss   uint64 `json:"backend_lookup_miss"`
	Errors              uint64 `json:"errors"`
	BatchCalls          uint64 `json:"batch_calls"`
	BatchPagesTotal     uint64 `json:"batch_pages_total"`
	BatchAvgPages       uint64 `json:"batch_avg_pages"`
	BatchMaxPages       uint64 `json:"batch_max_pages"`
	// LazyLoadRatio = (pages_zeroed + pages_copied) / total_pages.
	// total_pages comes from RAMSize/PageSize. Cold-start tracks how
	// little of declared RAM the guest actually touches; restore tracks
	// how much of the snapshot was demand-paged before app rebooted.
	TotalPages    uint64  `json:"total_pages"`
	ResidentPages uint64  `json:"resident_pages"`
	LazyLoadRatio float64 `json:"lazy_load_ratio"`
}

// runtimeStatsJSON captures Go runtime memory + GC counters at
// sandbox-ctl shutdown. Useful to spot allocator regressions in the
// hot uffd / vhost paths without external profilers.
type runtimeStatsJSON struct {
	NumGoroutine    int    `json:"num_goroutine"`
	NumGC           uint32 `json:"num_gc"`
	GCPauseTotalNs  uint64 `json:"gc_pause_total_ns"`
	HeapAllocBytes  uint64 `json:"heap_alloc_bytes"`
	HeapInuseBytes  uint64 `json:"heap_inuse_bytes"`
	HeapSysBytes    uint64 `json:"heap_sys_bytes"`
	HeapObjects     uint64 `json:"heap_objects"`
	TotalAllocBytes uint64 `json:"total_alloc_bytes"`
	Mallocs         uint64 `json:"mallocs"`
	Frees           uint64 `json:"frees"`
	StackInuseBytes uint64 `json:"stack_inuse_bytes"`
}

type wallclockJSON struct {
	StartUnixNs int64 `json:"start_unix_ns"`
	EndUnixNs   int64 `json:"end_unix_ns"`
	DurationMs  int64 `json:"duration_ms"`
}

type statsBackendJSON struct {
	Name          string         `json:"name"`
	Path          string         `json:"path"`
	BlockBytes    uint64         `json:"block_bytes"`
	TotalBlocks   uint64         `json:"total_blocks"`
	LoadedBlocks  uint64         `json:"loaded_blocks"`
	WrittenBlocks uint64         `json:"written_blocks"`
	Read          reqJSON        `json:"read"`
	Write         reqJSON        `json:"write"`
	Flush         reqJSON        `json:"flush"`
	Discard       reqJSON        `json:"discard"`
	Extra         map[string]any `json:"extra,omitempty"`
}

type reqJSON struct {
	Count      uint64   `json:"count"`
	Bytes      uint64   `json:"bytes"`
	ErrCount   uint64   `json:"err_count"`
	LatSumNs   uint64   `json:"lat_sum_ns"`
	LatMaxNs   uint64   `json:"lat_max_ns"`
	LatBuckets []uint64 `json:"lat_buckets"`
	P50Ns      uint64   `json:"p50_ns"`
	P99Ns      uint64   `json:"p99_ns"`
}

func toReqJSON(r vhost.ReqSnapshot) reqJSON {
	buckets := make([]uint64, len(r.LatBuckets))
	copy(buckets, r.LatBuckets[:])
	return reqJSON{
		Count:      r.Count,
		Bytes:      r.Bytes,
		ErrCount:   r.ErrCount,
		LatSumNs:   r.LatSumNs,
		LatMaxNs:   r.LatMaxNs,
		LatBuckets: buckets,
		P50Ns:      r.P50(),
		P99Ns:      r.P99(),
	}
}

// statsBundle holds everything writeStatsJSON needs. Beats threading
// 6 separate args through callers — single struct, easy to extend.
type statsBundle struct {
	Servers     []*vhost.Server
	Uffd        map[string]uint64       // counter snapshot from uffd.Handler.Stats()
	UffdRAMSize int64                   // total RAM in bytes; 0 ⇒ skip lazy-ratio
	Ping        *guestlink.PingSnapshot // host→guest health probe stats; nil ⇒ omit
	StartUnixNs int64                   // sandbox.Run T0
	EndUnixNs   int64                   // sandbox.Run T_exit
}

// writeStatsJSON renders the bundle as a single JSON document at path.
func writeStatsJSON(path string, b statsBundle) error {
	report := statsReport{
		BucketsNs: vhost.LatencyBucketBoundsNs(),
		Wallclock: wallclockJSON{
			StartUnixNs: b.StartUnixNs,
			EndUnixNs:   b.EndUnixNs,
			DurationMs:  (b.EndUnixNs - b.StartUnixNs) / int64(time.Millisecond),
		},
	}
	for _, srv := range b.Servers {
		if srv == nil || srv.Stats() == nil {
			continue
		}
		snap := srv.SnapshotStats()
		report.Backends = append(report.Backends, statsBackendJSON{
			Name:          snap.Name,
			Path:          snap.Path,
			BlockBytes:    snap.BlockBytes,
			TotalBlocks:   snap.TotalBlocks,
			LoadedBlocks:  snap.LoadedBlocks,
			WrittenBlocks: snap.WrittenBlocks,
			Read:          toReqJSON(snap.Read),
			Write:         toReqJSON(snap.Write),
			Flush:         toReqJSON(snap.Flush),
			Discard:       toReqJSON(snap.Discard),
			Extra:         snap.Extra,
		})
	}
	if b.Uffd != nil {
		report.Uffd = buildUffdJSON(b.Uffd, b.UffdRAMSize)
	}
	if b.Ping != nil {
		report.Ping = b.Ping
	}
	report.Runtime = collectRuntimeStats()

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal stats: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// buildUffdJSON converts the uffd.Handler.Stats() counter map plus
// the declared RAM size into the typed JSON shape, computing the
// lazy-load ratio inline.
func buildUffdJSON(counters map[string]uint64, ramBytes int64) *uffdStatsJSON {
	zeroed := counters["pages_zeroed"]
	copied := counters["pages_copied"]
	resident := zeroed + copied
	var totalPages uint64
	var ratio float64
	if ramBytes > 0 {
		const pageSize = 4096
		totalPages = uint64(ramBytes) / pageSize
		if totalPages > 0 {
			ratio = float64(resident) / float64(totalPages)
		}
	}
	return &uffdStatsJSON{
		FaultsAbsent:        counters["faults_absent"],
		FaultsReleased:      counters["faults_released"],
		FaultsLoaded:        counters["faults_loaded"],
		ZeropageCalls:       counters["zeropage_calls"],
		CopyCalls:           counters["copy_calls"],
		PagesZeroed:         zeroed,
		PagesCopied:         copied,
		Wakes:               counters["wakes"],
		RemoveEvents:        counters["remove_events"],
		RemoveQDropped:      counters["remove_q_dropped"],
		RemoveEventsBatched: counters["remove_events_batched"],
		MadviseCalls:        counters["madvise_calls"],
		MadviseBytes:        counters["madvise_bytes"],
		BackendLookupMiss:   counters["backend_lookup_miss"],
		Errors:              counters["errors"],
		BatchCalls:          counters["batch_calls"],
		BatchPagesTotal:     counters["batch_pages_total"],
		BatchAvgPages:       counters["batch_avg_pages"],
		BatchMaxPages:       counters["batch_max_pages"],
		TotalPages:          totalPages,
		ResidentPages:       resident,
		LazyLoadRatio:       ratio,
	}
}

func collectRuntimeStats() *runtimeStatsJSON {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return &runtimeStatsJSON{
		NumGoroutine:    runtime.NumGoroutine(),
		NumGC:           ms.NumGC,
		GCPauseTotalNs:  ms.PauseTotalNs,
		HeapAllocBytes:  ms.HeapAlloc,
		HeapInuseBytes:  ms.HeapInuse,
		HeapSysBytes:    ms.HeapSys,
		HeapObjects:     ms.HeapObjects,
		TotalAllocBytes: ms.TotalAlloc,
		Mallocs:         ms.Mallocs,
		Frees:           ms.Frees,
		StackInuseBytes: ms.StackInuse,
	}
}
