package vhost

import (
	"context"
	"errors"
	"math"
	"math/bits"
	"sync"
	"time"
)

var errIOAdmissionYield = errors.New("vhost: retry unstarted request after admission interruption")

// A failed completion notification must leave the waiting request unstarted,
// but is not a terminal storage error or a reason to ACK that request.
type ioAdmissionNotifyError struct{ err error }

func (e *ioAdmissionNotifyError) Error() string { return e.err.Error() }
func (e *ioAdmissionNotifyError) Unwrap() error { return e.err }

// IOLimits bounds admitted logical requests. Bandwidth is in bytes/second;
// IOPS counts complete read or write requests. Zero leaves a limit disabled.
type IOLimits struct {
	ReadBandwidth, WriteBandwidth uint64
	ReadIOPS, WriteIOPS           uint64
}

// Each admitted request incurs debt, so idle time cannot accumulate an
// unbounded burst. A request larger than one second's quota is valid and
// delays subsequent admission by its complete cost. No future global quota
// is reserved by a request that is still waiting for its per-disk limit.
type ioMeter struct {
	rate, debt, fraction uint64
	last                 time.Time
}

func (m *ioMeter) delay(now time.Time) time.Duration {
	if m.rate == 0 || m.debt == 0 {
		m.last = now
		m.fraction = 0
		return 0
	}
	elapsed := now.Sub(m.last)
	if elapsed > 0 {
		hi, lo := bits.Mul64(uint64(elapsed), m.rate)
		lo, carry := bits.Add64(lo, m.fraction, 0)
		hi, _ = bits.Add64(hi, 0, carry)
		dhi, dlo := bits.Mul64(m.debt, uint64(time.Second))
		if hi > dhi || hi == dhi && lo >= dlo {
			m.debt, m.fraction = 0, 0
		} else {
			paid, fraction := bits.Div64(hi, lo, uint64(time.Second))
			m.debt -= paid
			m.fraction = fraction
		}
		m.last = now
	}
	if m.debt == 0 {
		return 0
	}
	hi, lo := bits.Mul64(m.debt, uint64(time.Second))
	lo, borrow := bits.Sub64(lo, m.fraction, 0)
	hi, _ = bits.Sub64(hi, 0, borrow)
	if hi >= m.rate {
		return time.Duration(math.MaxInt64)
	}
	ns, rem := bits.Div64(hi, lo, m.rate)
	if ns >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	if rem != 0 {
		ns++
	}
	return time.Duration(ns)
}

// IOController owns the aggregate and uniform per-disk rate policy for one
// runtime. It must not be copied after use. There is no minimum-share guarantee.
type IOController struct {
	mu      sync.Mutex
	global  [2][2]ioMeter
	per     IOLimits
	now     func() time.Time
	waiting func() // deterministic test hook, called without mu
}

// DiskIOBudget is shared by every frontend of one logical disk. In particular,
// an overlay base and its writable upper must not receive independent budgets.
type DiskIOBudget struct {
	owner *IOController
	disk  [2][2]ioMeter
}

func ioMeters(l IOLimits) [2][2]ioMeter {
	return [2][2]ioMeter{{{rate: l.ReadBandwidth}, {rate: l.ReadIOPS}}, {{rate: l.WriteBandwidth}, {rate: l.WriteIOPS}}}
}
func NewIOController(total, per IOLimits) *IOController {
	return &IOController{global: ioMeters(total), per: per, now: time.Now}
}
func (c *IOController) NewDisk() *DiskIOBudget {
	return &DiskIOBudget{owner: c, disk: ioMeters(c.per)}
}

func interrupted(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (d *DiskIOBudget) try(ctx context.Context, write bool, size uint64, a, b <-chan struct{}) (time.Duration, error) {
	c := d.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if interrupted(a) || interrupted(b) {
		return 0, errIOAdmissionYield
	}
	dir := 0
	if write {
		dir = 1
	}
	now := c.now()
	meters := []*ioMeter{&c.global[dir][0], &c.global[dir][1], &d.disk[dir][0], &d.disk[dir][1]}
	var delay time.Duration
	for _, m := range meters {
		delay = max(delay, m.delay(now))
	}
	if delay != 0 {
		return delay, nil
	}
	for i, m := range meters {
		if m.rate != 0 {
			m.debt = 1
			if i%2 == 0 {
				m.debt = size
			}
			m.last, m.fraction = now, 0
		}
	}
	return 0, nil
}

func (d *DiskIOBudget) wait(ctx context.Context, write bool, size uint64, a, b <-chan struct{}, beforeWait func() error) error {
	for {
		delay, err := d.try(ctx, write, size, a, b)
		if err != nil || delay == 0 {
			return err
		}
		if beforeWait != nil {
			if err := beforeWait(); err != nil {
				return err
			}
			beforeWait = nil
		}
		if d.owner.waiting != nil {
			d.owner.waiting()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-a:
			timer.Stop()
			return errIOAdmissionYield
		case <-b:
			timer.Stop()
			return errIOAdmissionYield
		case <-timer.C:
		}
	}
}

// An interruptible gate is separate from actual I/O cancellation. Yielding
// before backend execution leaves the descriptor and used ring untouched.
type ioAdmissionSwitch struct {
	mu      sync.Mutex
	blocked bool
	changed chan struct{}
}

func (g *ioAdmissionSwitch) snapshot() (bool, <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.changed == nil {
		g.changed = make(chan struct{})
	}
	return g.blocked, g.changed
}
func (g *ioAdmissionSwitch) set(blocked bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.changed == nil {
		g.changed = make(chan struct{})
	}
	if g.blocked != blocked {
		g.blocked = blocked
		close(g.changed)
		g.changed = make(chan struct{})
	}
}
func (g *ioAdmissionSwitch) waitOpen(ctx context.Context) error {
	for {
		blocked, changed := g.snapshot()
		if !blocked {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// SetIOBudget binds a fixed runtime policy before Listen or Serve starts.
// It must not be called concurrently with serving requests.
func (s *Server) SetIOBudget(d *DiskIOBudget) { s.ioBudget = d }

func (s *Server) admitIO(q *virtq, write bool, size uint64) error {
	if s.ioBudget == nil {
		return nil
	}
	paused, a := s.ioPause.snapshot()
	disabled, b := q.ioDisabled.snapshot()
	if paused || disabled {
		return errIOAdmissionYield
	}
	return s.ioBudget.wait(q.readContext(), write, size, a, b, func() error {
		// Rate waits may last arbitrarily longer than a normal batch. Tell
		// the guest about completed earlier chains before waiting, without
		// turning every admitted request into a separate interrupt.
		if q.notifyPending {
			if err := s.notifyCall(q); err != nil {
				return &ioAdmissionNotifyError{err}
			}
		}
		return nil
	})
}
