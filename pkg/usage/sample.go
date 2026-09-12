package usage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
)

// Clock is the narrow scheduling seam; raw source functions are separately
// injectable in package tests. Production uses the Go monotonic clock.
type Clock interface {
	Now() time.Time
	Ticker(time.Duration) (<-chan time.Time, func())
}
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Ticker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

type sampleResult struct {
	source int
	apply  func()
}
type sampleSlot struct{ busy atomic.Bool }

func (s *sampleSlot) start(index int, out chan<- sampleResult, read func() func()) bool {
	if !s.busy.CompareAndSwap(false, true) {
		return false
	}
	go func() { result := read(); s.busy.Store(false); out <- sampleResult{index, result} }()
	return true
}

// Sampler is one Host scheduler with three fixed execution slots: CH proc,
// sandbox-ctl proc and the Guest/balloon round. A blocked source never receives
// a replacement goroutine. The Manager's writer has its own independent slot.
type Sampler struct {
	m               *Manager
	clock           Clock
	start           time.Time
	interval, flush time.Duration
	epoch           string
	pid             int
	disks           []string
	proc            *procReader
	guest           *guestlink.UsageClient
	balloon         *resctl.BalloonController
	slots           [3]sampleSlot
	request         atomic.Uint64
	paused          atomic.Bool
	ready           atomic.Bool
	mu              sync.Mutex
	cancelRound     context.CancelFunc
	cancel          context.CancelFunc
	done            chan struct{}
	wake            chan struct{}
	generation      uint64
	sourceMu        sync.Mutex
	sources         map[string]string
}

// NewSampler takes ownership of a freshly opened Manager, before it is shared
// or starts saving. Construction failure only releases its pinned descriptors;
// it must not seal a run that never sampled. On success Stop owns cleanup.
func NewSampler(m *Manager, vcpus, disks int, host *guestlink.HostClient, balloon *resctl.BalloonController, clock Clock) (*Sampler, error) {
	return newSampler(m, vcpus, disks, host, balloon, clock, newProcReader)
}

// The proc bootstrap seam exercises failures before sampling without changing
// the process-wide /proc mount or adding a runtime provider abstraction.
func newSampler(m *Manager, vcpus, disks int, host *guestlink.HostClient, balloon *resctl.BalloonController, clock Clock, openProc func(int) (*procReader, error)) (_ *Sampler, err error) {
	defer func() {
		if err != nil && m != nil {
			m.closeFiles()
		}
	}()
	if m == nil {
		return nil, errors.New("usage: manager unavailable")
	}
	if vcpus < 1 || vcpus > MaxCounters-3 || disks < 1 || disks > proto.MaxUsageFilesystems {
		return nil, errors.New("usage: unsupported resource count")
	}
	p, err := openProc(vcpus)
	if err != nil {
		return nil, err
	}
	if clock == nil {
		clock = realClock{}
	}
	v := m.View()
	s := &Sampler{m: m, clock: clock, start: clock.Now(), interval: time.Duration(v.Live.SampleInterval), flush: time.Duration(v.Live.FlushInterval),
		epoch: v.Live.RunEpoch, proc: p, guest: &guestlink.UsageClient{Host: host}, balloon: balloon, wake: make(chan struct{}, 1), sources: make(map[string]string)}
	if !m.runStart.IsZero() {
		s.start = m.runStart
	}
	s.disks = append(s.disks, "root")
	for i := 1; i < disks; i++ {
		s.disks = append(s.disks, fmt.Sprintf("disk-%d", i-1))
	}
	return s, nil
}

func (s *Sampler) Start(parent context.Context, pid int) {
	s.pid = pid
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	s.cancel = cancel
	s.done = make(chan struct{})
	s.mu.Unlock()
	go s.loop(ctx)
}

func (s *Sampler) Ready() {
	s.ready.Store(true)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Sampler) loop(ctx context.Context) {
	defer close(s.done)
	samples, stopSamples := s.clock.Ticker(s.interval)
	defer stopSamples()
	flushes, stopFlushes := s.clock.Ticker(s.flush)
	defer stopFlushes()
	s.round(ctx)
	var firstTick time.Time
	var lastSlot int64
	for {
		select {
		case <-ctx.Done():
			return
		case tick := <-samples:
			if firstTick.IsZero() {
				firstTick = tick
			}
			slot := tickSlot(tick.Sub(firstTick), s.interval)
			s.mu.Lock()
			// FinalCH can fence admission while the sole reaper is still
			// draining output. Fenced ticks must not erase final RSS endpoints.
			if !s.paused.Load() && slot > lastSlot+1 {
				s.m.discontinue()
			}
			s.mu.Unlock()
			lastSlot = slot
			if !s.paused.Load() {
				s.round(ctx)
			}
		case <-s.wake:
			if !s.paused.Load() {
				s.round(ctx)
			}
		case <-flushes:
			s.m.Save(s.clock.Now())
		}
	}
}

// Ticker timestamps carry timer-delivery jitter. Quantize against one fixed
// schedule phase, not the preceding (also jittered) timestamp. A +1 ns interval
// is not a missing round; jumping over a whole scheduled slot is.
func tickSlot(elapsed, interval time.Duration) int64 {
	slot, remainder := elapsed/interval, elapsed%interval
	if remainder >= (interval/2 + interval%2) {
		slot++
	}
	return int64(slot)
}

func (s *Sampler) acceptResult(ctx context.Context, generation, id uint64, result sampleResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paused.Load() || s.generation != generation {
		return
	}
	if ctx.Err() != nil {
		// select may choose an already finished result over an already expired
		// deadline. This rejected observation is still a continuity break.
		s.missing(result.source, id, s.clock.Now())
		return
	}
	result.apply()
}

func (s *Sampler) round(parent context.Context) {
	budget := s.interval
	if budget > time.Second {
		budget = time.Second
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	s.mu.Lock()
	if s.paused.Load() {
		s.mu.Unlock()
		return
	}
	s.cancelRound = cancel
	generation := s.generation
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.cancelRound = nil; s.mu.Unlock() }()
	id := s.request.Add(1)
	if id == 0 {
		s.Pause()
		return
	}
	out := make(chan sampleResult, len(s.slots))
	pending := [3]bool{}
	for i := range s.slots {
		s.mu.Lock()
		if s.paused.Load() || s.generation != generation {
			s.mu.Unlock()
			return
		}
		index := i
		if index == 2 && !s.ready.Load() {
			s.mu.Unlock()
			continue
		}
		pending[index] = s.slots[index].start(index, out, func() func() {
			if index == 2 {
				return s.readGuest(ctx, id)
			}
			pid, name := s.pid, "ch"
			if index == 1 {
				pid, name = os.Getpid(), "sandbox_ctl"
			}
			return s.readProcess(pid, name, id, false)
		})
		if !pending[index] {
			s.missing(index, id, s.clock.Now())
		}
		s.mu.Unlock()
	}
	for pending[0] || pending[1] || pending[2] {
		select {
		case result := <-out:
			pending[result.source] = false
			s.acceptResult(ctx, generation, id, result)
		case <-ctx.Done():
			s.mu.Lock()
			if !s.paused.Load() && s.generation == generation {
				for i, waiting := range pending {
					if waiting {
						s.missing(i, id, s.clock.Now())
					}
				}
			}
			s.mu.Unlock()
			return
		}
	}
}

func (s *Sampler) missing(source int, id uint64, at time.Time) {
	if source == 2 {
		_ = s.m.Gauge("guest.memory", s.source("guest.memory", ""), id, at.Sub(s.start).Nanoseconds(), 0, Missing, 0)
		for _, disk := range s.disks {
			_ = s.m.Gauge("filesystem."+disk, s.source("filesystem."+disk, ""), id, at.Sub(s.start).Nanoseconds(), 0, Missing, 0)
		}
		return
	}
	name := "ch"
	if source == 1 {
		name = "sandbox_ctl"
	}
	s.processMissing(name, false)
	for _, field := range []string{"rss_anon", "rss_file"} {
		_ = s.m.Gauge(name+"."+field, s.source(name+"."+field, ""), id, at.Sub(s.start).Nanoseconds(), 0, Missing, 0)
	}
}

func (s *Sampler) processMissing(name string, final bool) {
	s.m.CounterMissing(name+".cpu", final)
	if name == "ch" {
		s.m.CounterMissing("guest.cpu", final)
		for cpu := 0; cpu < s.proc.vcpuCount; cpu++ {
			s.m.counterMissing(fmt.Sprintf("guest.vcpu.%d", cpu), final, true)
		}
	}
}

func (s *Sampler) readProcess(pid int, name string, id uint64, final bool) func() {
	start := s.clock.Now()
	if name == "ch" && s.proc.incomplete == nil {
		s.proc.incomplete = make([]bool, s.proc.vcpuCount)
	}
	path := filepath.Join(s.proc.root, strconv.Itoa(pid))
	stat, err := s.proc.stat(filepath.Join(path, "stat"))
	if err != nil || stat.PID != pid {
		var incomplete []bool
		if name == "ch" {
			for cpu := 0; cpu < s.proc.vcpuCount; cpu++ {
				if _, known := s.proc.threads[cpu]; !known {
					s.proc.incomplete[cpu] = true
				}
			}
			incomplete = append([]bool(nil), s.proc.incomplete...)
		}
		return func() {
			s.processMissing(name, final)
			for cpu, lost := range incomplete {
				if lost {
					s.m.CounterMissing(fmt.Sprintf("guest.vcpu.%d", cpu), true)
				}
			}
			s.missing(map[bool]int{true: 0, false: 1}[name == "ch"], id, s.clock.Now())
		}
	}
	rawCPU, cpuErr := add64(stat.User, stat.System) // utime already contains guest_time.
	status, statusErr := s.proc.read(filepath.Join(path, "status"), 64*1024)
	anon, file, rssErr := parseRSS(status)
	if statusErr != nil {
		rssErr = statusErr
	}
	type threadValue struct {
		cpu  int
		stat procStat
		err  error
		gone bool
	}
	var threads []threadValue
	if name == "ch" {
		previous := s.proc.threads
		ambiguous := false
		if s.proc.threadCount != stat.Threads || len(s.proc.threads) != s.proc.vcpuCount {
			ambiguous = errors.Is(s.proc.discover(pid, stat.Threads), errAmbiguousVCPU)
		}
		for cpu := 0; cpu < s.proc.vcpuCount; cpu++ {
			entry, known := s.proc.threads[cpu]
			var ts procStat
			var te error
			gone := false
			if ambiguous {
				te, gone = errAmbiguousVCPU, true
			} else if !known {
				te = errors.New("vCPU thread unavailable")
				gone = true
			} else {
				if old, existed := previous[cpu]; existed && old != entry {
					gone = true
				}
				ts, te = s.proc.stat(filepath.Join(path, "task", strconv.Itoa(entry.tid), "stat"))
				currentGone := errors.Is(te, os.ErrNotExist)
				if te == nil && (ts.PID != entry.tid || ts.Start != entry.start || ts.Comm != "vcpu"+strconv.Itoa(cpu)) {
					te, currentGone = errors.New("vCPU identity changed"), true
				}
				if currentGone {
					delete(s.proc.threads, cpu)
				}
				gone = gone || currentGone
			}
			s.proc.incomplete[cpu] = s.proc.incomplete[cpu] || gone
			threads = append(threads, threadValue{cpu, ts, te, s.proc.incomplete[cpu]})
		}
	}
	end := s.clock.Now()
	at := start.Add(end.Sub(start) / 2).Sub(s.start).Nanoseconds()
	return func() {
		identity := s.proc.identity(stat)
		if cpuErr == nil {
			_ = s.m.Counter(name+".cpu", identity, rawCPU, s.proc.hertz, true)
		} else {
			s.m.CounterMissing(name+".cpu", true)
		}
		if name == "ch" {
			_ = s.m.Counter("guest.cpu", identity, stat.Guest, s.proc.hertz, true)
			for _, thread := range threads {
				key := fmt.Sprintf("guest.vcpu.%d", thread.cpu)
				if thread.err != nil {
					s.m.counterMissing(key, final || thread.gone, true)
				} else {
					if thread.gone {
						s.m.CounterMissing(key, true)
					}
					_ = s.m.Counter(key, s.proc.identity(thread.stat), thread.stat.Guest, s.proc.hertz, true)
				}
			}
		}
		state := OK
		if rssErr != nil {
			state = Missing
		}
		_ = s.m.Gauge(name+".rss_anon", s.source(name+".rss_anon", identity), id, at, anon, state, end.Sub(start))
		_ = s.m.Gauge(name+".rss_file", s.source(name+".rss_file", identity), id, at, file, state, end.Sub(start))
	}
}

func (s *Sampler) readGuest(ctx context.Context, id uint64) func() {
	w, err := s.guest.Read(ctx, s.epoch, id)
	if err != nil {
		return func() { s.missing(2, id, s.clock.Now()) }
	}
	at := w.Started.Add(w.Finished.Sub(w.Started) / 2).Sub(s.start).Nanoseconds()
	width := w.Finished.Sub(w.Started)
	var balloon uint64
	memoryStatus := w.Response.Memory.Status
	if s.balloon != nil {
		a := s.balloon.ActualObservation()
		joint, valid := jointWindow(w.Started, w.Finished, a, s.interval)
		if !valid {
			end, _ := ctx.Deadline()
			apiCtx, cancel := context.WithTimeout(ctx, time.Until(end)/2)
			a, err = s.balloon.TryObserveActual(apiCtx)
			cancel()
			joint, valid = jointWindow(w.Started, w.Finished, a, s.interval)
		}
		if !valid || err != nil {
			memoryStatus = Missing
		} else {
			balloon, width = a.Current, joint
		}
	}
	used, memErr := memoryUsed(w.Response.Memory, balloon)
	if memErr != nil && memoryStatus == proto.UsageOK {
		memoryStatus = Invalid
	}
	return func() {
		if memoryStatus == proto.UsageOK {
			memoryStatus = OK
		}
		memorySource := s.source("guest.memory", "")
		if memoryStatus == OK {
			var valid bool
			memorySource, valid = s.stableSource("guest.memory", w.Response.Memory.Domain)
			if !valid {
				memoryStatus = Invalid
			}
		}
		_ = s.m.Gauge("guest.memory", memorySource, id, at, used, memoryStatus, width)
		for _, disk := range s.disks {
			status, value := Missing, uint64(0)
			identity := s.source("filesystem."+disk, "")
			for _, fs := range w.Response.Filesystems {
				if fs.Disk == disk {
					status = fs.Status
					v, e := filesystemUsed(fs)
					if e == nil {
						value = v
						status = OK
						var valid bool
						identity, valid = s.stableSource("filesystem."+disk, fs.Incarnation)
						if !valid {
							status = Invalid
						}
					} else if status == proto.UsageOK {
						status = Invalid
					}
					break
				}
			}
			_ = s.m.Gauge("filesystem."+disk, identity, id, at, value, status, w.Finished.Sub(w.Started))
		}
	}
}

func (s *Sampler) source(name, identity string) string {
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	if identity != "" {
		s.sources[name] = s.epoch + "/" + identity
	}
	if value := s.sources[name]; value != "" {
		return value
	}
	return s.epoch + "/" + name
}

// RAM domains and pinned filesystems cannot change within a running VM.
// Reject a conflicting observation rather than silently starting a new domain.
func (s *Sampler) stableSource(name, identity string) (string, bool) {
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	previous := s.sources[name]
	current := s.epoch + "/" + identity
	if identity == "" || len(current) > maxString || (previous != "" && previous != current) {
		return previous, false
	}
	s.sources[name] = current
	return current, true
}

func (s *Sampler) Pause() {
	s.fence(true)
}

// Final process reads need the last accepted RSS endpoint. A snapshot pause
// instead ends every Gauge time domain; both paths fence periodic admission.
func (s *Sampler) fence(pauseGauges bool) {
	s.mu.Lock()
	s.paused.Store(true)
	s.generation++
	cancel := s.cancelRound
	s.cancelRound = nil // This round is invalidated once, not again by Stop.
	if pauseGauges {
		s.m.BreakGauges(Paused)
	} else if cancel != nil {
		s.m.discontinuePending(s.request.Load())
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.guest.Close()
}
func (s *Sampler) Resume() { s.paused.Store(false); s.Ready() }

// FinalCH is called by the one process reaper while WNOWAIT retains /proc.
// A stuck proc slot cannot stall cmd.Wait or create a second reader.
func (s *Sampler) FinalCH(ctx context.Context) {
	s.fence(false)
	id := s.request.Add(1)
	missing := func() {
		s.processMissing("ch", true)
		s.missing(0, id, s.clock.Now())
	}
	if ctx.Err() != nil {
		missing()
		return
	}
	out := make(chan sampleResult, 1)
	if !s.slots[0].start(0, out, func() func() { return s.readProcess(s.pid, "ch", id, true) }) {
		missing()
		return
	}
	select {
	case result := <-out:
		if ctx.Err() == nil {
			result.apply()
		} else {
			missing()
		}
	case <-ctx.Done():
		missing()
	}
}

func (s *Sampler) Stop(ctx context.Context) {
	s.fence(false)
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	// The control process remains alive. Sample its own known native counter
	// once more through the original slot, independently from Guest readiness.
	id := s.request.Add(1)
	out := make(chan sampleResult, 1)
	if ctx.Err() == nil && s.slots[1].start(1, out, func() func() { return s.readProcess(os.Getpid(), "sandbox_ctl", id, true) }) {
		select {
		case result := <-out:
			if ctx.Err() == nil {
				result.apply()
			} else {
				s.missing(1, id, s.clock.Now())
			}
		case <-ctx.Done():
			s.missing(1, id, s.clock.Now())
		}
	} else {
		s.missing(1, id, s.clock.Now())
	}
	// This process necessarily performs shutdown/save work after its last
	// self-observation; it cannot certify its own terminal CPU endpoint.
	s.m.CounterMissing("sandbox_ctl.cpu", true)
	s.m.Close(ctx, s.clock.Now())
}
