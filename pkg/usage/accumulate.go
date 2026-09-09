package usage

import (
	"errors"
	"time"
)

const (
	OK          = "ok"
	Missing     = "missing"
	Unsupported = "unsupported"
	Invalid     = "invalid"
	Paused      = "paused"
	MaxCounters = 1027
	MaxGauges   = 69 // Guest memory, 64 managed filesystems, four process RSS items.
)

// Window summarizes only intervals and observations assigned to this record.
// Positions are monotonic nanoseconds relative to Snapshot.RunEpoch, never UTC.
type Window struct {
	Area          Uint128 `json:"area_byte_ns"`
	Span          uint64  `json:"span_ns,string"`
	Covered       uint64  `json:"covered_ns,string"`
	Peak          uint64  `json:"peak_bytes,string"`
	PeakAt        int64   `json:"peak_at_ns,string"`
	PeakKnown     bool    `json:"peak_known"`
	Samples       uint64  `json:"samples,string"`
	MaxReadWindow uint64  `json:"max_read_window_ns,string"`
}

func (w *Window) peak(value uint64, at int64) {
	if !w.PeakKnown || value > w.Peak {
		w.Peak, w.PeakAt, w.PeakKnown = value, at, true
	}
}

func mergeWindow(a, b Window) (Window, error) {
	out := b
	var err error
	if out.Area, err = a.Area.Add(b.Area); err != nil {
		return Window{}, err
	}
	if out.Span, err = add64(a.Span, b.Span); err != nil {
		return Window{}, err
	}
	if out.Covered, err = add64(a.Covered, b.Covered); err != nil {
		return Window{}, err
	}
	if out.Samples, err = add64(a.Samples, b.Samples); err != nil {
		return Window{}, err
	}
	if a.PeakKnown {
		out.peak(a.Peak, a.PeakAt)
	}
	if a.MaxReadWindow > out.MaxReadWindow {
		out.MaxReadWindow = a.MaxReadWindow
	}
	return out, nil
}

// Gauge holds cumulative totals and the last observation boundary. LastAt is
// the last accepted attempt (including a failure); LastValueAt identifies the
// last valid value. This single time domain counts a failed interval once.
type Gauge struct {
	Name          string  `json:"name"`
	Source        string  `json:"source"`
	IntegralTotal Uint128 `json:"integral_total_byte_ns"`
	SpanTotal     uint64  `json:"span_total_ns,string"`
	CoveredTotal  uint64  `json:"covered_total_ns,string"`
	LastValue     uint64  `json:"last_value_bytes,string"`
	LastValueAt   int64   `json:"last_value_at_ns,string"`
	LastAt        int64   `json:"last_at_ns,string"`
	LastRequest   uint64  `json:"last_request_id,string"`
	PositionKnown bool    `json:"position_known"`
	ValueKnown    bool    `json:"value_known"`
	Continuous    bool    `json:"continuous"`
	Status        string  `json:"status"`
	Window        Window  `json:"window"`
}

// Observe accepts an ordered attempt. A failure breaks continuity. Duplicate
// identities are ignored, while a later request with a nonincreasing time
// explicitly invalidates the baseline. Arithmetic failures are transactional.
func (g *Gauge) Observe(source string, request uint64, at int64, value uint64, status string, width, interval time.Duration) error {
	if request == 0 || request <= g.LastRequest {
		return nil
	}
	if interval <= 0 || interval > time.Duration(1<<62-1) {
		return errors.New("usage: invalid sample interval")
	}
	n := *g
	n.LastRequest = request
	if n.PositionKnown && at <= n.LastAt {
		n.Continuous, n.Status = false, Invalid
		*g = n
		return nil
	}
	if width < 0 || width > interval {
		status = Invalid
	}
	valid := status == OK
	if n.PositionKnown && source == n.Source {
		dt := uint64(at - n.LastAt)
		var err error
		if n.SpanTotal, err = add64(n.SpanTotal, dt); err != nil {
			return err
		}
		if n.Window.Span, err = add64(n.Window.Span, dt); err != nil {
			return err
		}
		if n.Continuous && valid && dt <= uint64(2*interval) {
			area := product(n.LastValue, dt)
			if n.IntegralTotal, err = n.IntegralTotal.Add(area); err != nil {
				return err
			}
			if n.Window.Area, err = n.Window.Area.Add(area); err != nil {
				return err
			}
			if n.CoveredTotal, err = add64(n.CoveredTotal, dt); err != nil {
				return err
			}
			if n.Window.Covered, err = add64(n.Window.Covered, dt); err != nil {
				return err
			}
			// A save can fall between the two endpoints. Assign the left value
			// to this window only when its interval actually closes here.
			n.Window.peak(n.LastValue, n.LastValueAt)
		}
	}
	n.LastAt, n.PositionKnown, n.Source, n.Status = at, true, source, status
	n.Continuous = valid
	if valid {
		n.LastValue, n.LastValueAt, n.ValueKnown = value, at, true
		n.Window.peak(value, at)
		var err error
		if n.Window.Samples, err = add64(n.Window.Samples, 1); err != nil {
			return err
		}
		if uint64(width) > n.Window.MaxReadWindow {
			n.Window.MaxReadWindow = uint64(width)
		}
	}
	*g = n
	return nil
}

// Break ends a source's observed time domain without extending its last value.
// Paused/unsupported time does not become measured span or covered time.
func (g *Gauge) Break(status string) {
	g.PositionKnown, g.Continuous, g.ValueKnown = false, false, false
	g.Status = status
}

// Counter keeps raw native ticks, their scale and conversion remainder. Known
// totals include missed polls while the same native source remains alive.
type Counter struct {
	Name        string  `json:"name"`
	Source      string  `json:"source"`
	KnownTotal  Uint128 `json:"known_total_ns"`
	LastRaw     uint64  `json:"last_raw_ticks,string"`
	Hertz       uint64  `json:"ticks_per_second,string"`
	Remainder   uint64  `json:"conversion_remainder,string"`
	SourceKnown bool    `json:"source_known"`
	Complete    bool    `json:"complete"`
	Status      string  `json:"status"`
}

func (c *Counter) Observe(source string, raw, hertz uint64, created bool) error {
	if source == "" || hertz == 0 {
		return errors.New("usage: invalid counter source/scale")
	}
	n := *c
	var delta uint64
	if n.SourceKnown && n.Source == source {
		if hertz != n.Hertz || raw < n.LastRaw {
			c.Complete, c.Status = false, Invalid
			return errors.New("usage: counter regression or scale change")
		}
		delta = raw - n.LastRaw
	} else {
		if !n.SourceKnown {
			n.Complete = created
		} else if !created {
			n.Complete = false
		}
		if created {
			delta = raw
		}
		n.Remainder = 0
	}
	quantity, err := product(delta, uint64(time.Second)).Add(Uint128{Lo: n.Remainder})
	if err != nil {
		return err
	}
	amount, remainder := quantity.div(hertz)
	if n.KnownTotal, err = n.KnownTotal.Add(amount); err != nil {
		return err
	}
	n.Source, n.LastRaw, n.Hertz, n.Remainder = source, raw, hertz, remainder
	n.SourceKnown, n.Status = true, OK
	*c = n
	return nil
}

// Snapshot contains cumulative endpoints for one logical sandbox. Monotonic
// positions belong only to RunEpoch; UTC is for external correlation.
type Snapshot struct {
	SandboxID      string    `json:"sandbox_id"`
	RunEpoch       string    `json:"run_epoch"`
	StartedUTC     int64     `json:"started_utc_ns,string"`
	SampleInterval int64     `json:"sample_interval_ns,string"`
	FlushInterval  int64     `json:"flush_interval_ns,string"`
	Counters       []Counter `json:"counters"`
	Gauges         []Gauge   `json:"gauges"`
	Closed         bool      `json:"closed"`
}

func (s Snapshot) clone() Snapshot {
	s.Counters = append([]Counter(nil), s.Counters...)
	s.Gauges = append([]Gauge(nil), s.Gauges...)
	return s
}

func (s *Snapshot) newRun(epoch string, start time.Time, sample, flush time.Duration) {
	s.RunEpoch, s.StartedUTC = epoch, start.UnixNano()
	s.SampleInterval, s.FlushInterval, s.Closed = int64(sample), int64(flush), false
	for i := range s.Gauges {
		s.Gauges[i].Break(Missing)
		s.Gauges[i].LastRequest = 0
		s.Gauges[i].Window = Window{}
	}
}

// Record is one self-contained cumulative checkpoint. Sequence is a file
// position identity, not a consumption acknowledgement.
type Record struct {
	Sequence uint64   `json:"sequence,string"`
	SavedUTC int64    `json:"saved_utc_ns,string"`
	Snapshot Snapshot `json:"snapshot"`
}
