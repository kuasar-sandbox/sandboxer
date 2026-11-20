package vhost

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"golang.org/x/sys/unix"
)

func fakeIOClock(c *IOController) (*atomic.Int64, time.Time) {
	var ns atomic.Int64
	base := time.Unix(1000, 0)
	c.now = func() time.Time { return base.Add(time.Duration(ns.Load())) }
	return &ns, base
}

func TestQoSJointAdmissionDoesNotReserveGlobalForWaitingDisk(t *testing.T) {
	c := NewIOController(IOLimits{ReadBandwidth: 100, ReadIOPS: 10}, IOLimits{ReadBandwidth: 40, ReadIOPS: 2})
	ns, _ := fakeIOClock(c)
	a, b := c.NewDisk(), c.NewDisk()
	ctx := context.Background()
	if delay, err := a.try(ctx, false, 20, nil, nil); err != nil || delay != 0 {
		t.Fatal(delay, err)
	}
	ns.Store(int64(200 * time.Millisecond))
	if delay, err := a.try(ctx, false, 20, nil, nil); err != nil || delay != 300*time.Millisecond {
		t.Fatal(delay, err)
	}
	if delay, err := b.try(ctx, false, 20, nil, nil); err != nil || delay != 0 {
		t.Fatalf("slow disk reserved global capacity: %v %v", delay, err)
	}
	if delay, err := a.try(ctx, true, math.MaxUint64, nil, nil); err != nil || delay != 0 {
		t.Fatalf("read quota blocked unlimited write: %v %v", delay, err)
	}
	ns.Store(int64(500 * time.Millisecond))
	if delay, err := a.try(ctx, false, 20, nil, nil); err != nil || delay != 0 {
		t.Fatal(delay, err)
	}
}

func TestQoSRateArithmeticAndLargeRequest(t *testing.T) {
	base := time.Unix(1000, 0)
	m := ioMeter{rate: 3, debt: 1, last: base}
	for _, step := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond} {
		if d := m.delay(base.Add(step)); d <= 0 {
			t.Fatalf("early admit at %v", step)
		}
	}
	if d := m.delay(base.Add(333333333 * time.Nanosecond)); d != time.Nanosecond {
		t.Fatalf("fraction lost: %v", d)
	}
	if d := m.delay(base.Add(333333334 * time.Nanosecond)); d != 0 {
		t.Fatal(d)
	}
	big := ioMeter{rate: 1, debt: math.MaxUint64, last: base}
	if d := big.delay(base); d != time.Duration(math.MaxInt64) {
		t.Fatalf("overflowed delay: %v", d)
	}
	_ = big.delay(base.Add(time.Second))
	if big.debt != math.MaxUint64-1 {
		t.Fatalf("incorrect huge debt %d", big.debt)
	}
	c := NewIOController(IOLimits{ReadBandwidth: 1 << 20}, IOLimits{})
	fakeIOClock(c)
	d := c.NewDisk()
	if delay, err := d.try(context.Background(), false, 128<<20, nil, nil); err != nil || delay != 0 {
		t.Fatal(delay, err)
	}
	if delay, err := d.try(context.Background(), false, 1, nil, nil); err != nil || delay != 128*time.Second {
		t.Fatal(delay, err)
	}
}

func TestQoSLogicalDiskSharesQuotaAcrossDevices(t *testing.T) {
	c := NewIOController(IOLimits{}, IOLimits{ReadIOPS: 1})
	fakeIOClock(c)
	root := c.NewDisk()
	lower, upper := NewServer("", &memBackend{}, nil), NewServer("", &memBackend{}, nil)
	lower.SetIOBudget(root)
	upper.SetIOBudget(root)
	if delay, err := lower.ioBudget.try(context.Background(), false, 4096, nil, nil); err != nil || delay != 0 {
		t.Fatal(delay, err)
	}
	if delay, err := upper.ioBudget.try(context.Background(), false, 4096, nil, nil); err != nil || delay != time.Second {
		t.Fatal(delay, err)
	}
	if delay, err := c.NewDisk().try(context.Background(), false, 4096, nil, nil); err != nil || delay != 0 {
		t.Fatal(delay, err)
	}
}

func TestQoSFullRequestChargedOnceAcrossReadWindows(t *testing.T) {
	sizes := []int{512, maxReadWindow + 4096, 512}
	f := newWindowFixture(t, sizes, true)
	c := NewIOController(IOLimits{ReadBandwidth: 1, ReadIOPS: 1}, IOLimits{ReadBandwidth: 1, ReadIOPS: 1})
	d := c.NewDisk()
	f.server.SetIOBudget(d)
	if err := f.server.processQueue(f.queue); err != nil {
		t.Fatal(err)
	}
	n := uint64(sizes[0] + sizes[1] + sizes[2])
	f.check(t, BlkStatusOK, int(n), false)
	if len(f.backend.calls) < 2 {
		t.Fatal("fixture did not split request")
	}
	if c.global[0][0].debt != n || c.global[0][1].debt != 1 || d.disk[0][0].debt != n || d.disk[0][1].debt != 1 {
		t.Fatalf("wrong logical charge: %+v %+v", c.global[0], d.disk[0])
	}
}

func TestQoSFullWriteChargedOnceAcrossSegments(t *testing.T) {
	f := newWindowFixture(t, []int{4096, 4096}, false)
	binary.LittleEndian.PutUint32(f.header, BlkTypeOut)
	mem := f.server.memTable.regions[0].mmapBytes
	for _, i := range []int{1, 2} {
		binary.LittleEndian.PutUint16(mem[i*16+12:], descFlagNext)
	}
	c := NewIOController(IOLimits{WriteBandwidth: 1, WriteIOPS: 1}, IOLimits{WriteBandwidth: 1, WriteIOPS: 1})
	d := c.NewDisk()
	f.server.SetIOBudget(d)
	if err := f.server.processQueue(f.queue); err != nil {
		t.Fatal(err)
	}
	if *f.status != BlkStatusOK || f.queue.baseIdx != 1 {
		t.Fatal("write not completed")
	}
	if c.global[1][0].debt != 8192 || c.global[1][1].debt != 1 || d.disk[1][0].debt != 8192 || d.disk[1][1].debt != 1 {
		t.Fatalf("wrong write charge: %+v %+v", c.global[1], d.disk[1])
	}
}

func TestQoSBatchYieldsControlAndUnboundRequests(t *testing.T) {
	for _, gate := range []string{"pause", "disable"} {
		for _, op := range []string{"flush", "discard", "unbound"} {
			t.Run(gate+"/"+op, func(t *testing.T) {
				f := newWindowFixture(t, []int{4096}, true)
				c := NewIOController(IOLimits{ReadIOPS: 1}, IOLimits{})
				f.server.SetIOBudget(c.NewDisk())
				mem := f.server.memTable.regions[0].mmapBytes
				const secondHeader, secondStatus = 4000, 4024
				le := binary.LittleEndian
				typ := uint32(BlkTypeFlush)
				if op == "discard" {
					typ = BlkTypeDiscard
				}
				if op == "unbound" {
					typ = BlkTypeIn
				}
				le.PutUint32(mem[secondHeader:], typ)
				mem[secondStatus] = sentinel
				le.PutUint64(mem[10*16:], secondHeader)
				le.PutUint32(mem[10*16+8:], 16)
				le.PutUint16(mem[10*16+12:], descFlagNext)
				le.PutUint16(mem[10*16+14:], 11)
				le.PutUint64(mem[11*16:], secondStatus)
				le.PutUint32(mem[11*16+8:], 1)
				le.PutUint16(mem[11*16+12:], descFlagWrite)
				le.PutUint16(mem[128*16+2:], 2)
				le.PutUint16(mem[128*16+6:], 10)
				f.backend.fault = func(p []byte, off int64) (int, error) {
					if gate == "pause" {
						f.server.ioPause.set(true)
					} else {
						f.queue.ioDisabled.set(true)
					}
					if op == "unbound" {
						f.server.backend = &unboundBackend{}
					}
					return f.backend.memBackend.ReadAt(p, off)
				}
				if err := f.server.processQueue(f.queue); !errors.Is(err, errIOAdmissionYield) {
					t.Fatalf("did not yield: %v", err)
				}
				if f.queue.baseIdx != 1 || le.Uint16(f.used[2:]) != 1 || mem[secondStatus] != sentinel {
					t.Fatal("unstarted control/unbound request acknowledged")
				}
				f.server.ioPause.set(false)
				f.queue.ioDisabled.set(false)
				if err := f.server.processQueue(f.queue); err != nil {
					t.Fatal(err)
				}
				if f.queue.baseIdx != 2 || le.Uint16(f.used[2:]) != 2 || mem[secondStatus] == sentinel {
					t.Fatal("pending descriptor did not complete once")
				}
				if c.global[0][1].debt != 1 {
					t.Fatal("control/unbound request charged quota")
				}
			})
		}
	}
}

func waitingFixture(t *testing.T) (*windowFixture, *IOController, *atomic.Int64, <-chan struct{}) {
	t.Helper()
	f := newWindowFixture(t, []int{4096}, true)
	c := NewIOController(IOLimits{ReadIOPS: 1}, IOLimits{})
	ns, _ := fakeIOClock(c)
	d := c.NewDisk()
	f.server.SetIOBudget(d)
	if delay, err := d.try(context.Background(), false, 4096, nil, nil); err != nil || delay != 0 {
		t.Fatal(delay, err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	c.waiting = func() { once.Do(func() { close(waiting) }) }
	f.server.queues[0] = f.queue
	return f, c, ns, waiting
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal(what)
	}
}

func TestQoSQuiesceInterruptsUnstartedRequestAndResumeRetriesOnce(t *testing.T) {
	f, _, ns, waiting := waitingFixture(t)
	done := make(chan error, 1)
	go func() { done <- f.server.runPending(f.queue) }()
	awaitSignal(t, waiting, "admission did not wait")
	paused := make(chan struct{})
	go func() { f.server.Quiesce(); close(paused) }()
	select {
	case <-paused:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pause waited for a one-second admission timer")
	}
	f.check(t, sentinel, 0, true)
	if len(f.backend.calls) != 0 {
		t.Fatal("unstarted request reached backend")
	}
	ns.Store(int64(time.Second))
	f.server.Resume()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("resume needed another KICK")
	}
	f.check(t, BlkStatusOK, 4096, false)
	if len(f.backend.calls) != 1 {
		t.Fatal("request replayed more than once")
	}
}

func TestQoSDisablePauseResumeEnableDoesNotAckPendingRequest(t *testing.T) {
	f, _, ns, waiting := waitingFixture(t)
	done := make(chan error, 1)
	go func() { done <- f.server.runPending(f.queue) }()
	awaitSignal(t, waiting, "admission did not wait")
	payload := make([]byte, 8)
	if err := f.server.handleSetVringEnable(&Message{Payload: payload}); err != nil {
		t.Fatal(err)
	}
	paused := make(chan struct{})
	go func() { f.server.Quiesce(); close(paused) }()
	select {
	case <-paused:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("disable did not yield admission")
	}
	f.check(t, sentinel, 0, true)
	ns.Store(int64(time.Second))
	f.server.Resume()
	select {
	case err := <-done:
		t.Fatalf("processed disabled queue: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	binary.LittleEndian.PutUint32(payload[4:], 1)
	if err := f.server.handleSetVringEnable(&Message{Payload: payload}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("enable needed another KICK")
	}
	f.check(t, BlkStatusOK, 4096, false)
}

func TestQoSStopCancelsAdmissionWithoutCompletion(t *testing.T) {
	f, _, _, waiting := waitingFixture(t)
	done := make(chan error, 1)
	go func() { done <- f.server.runPending(f.queue) }()
	awaitSignal(t, waiting, "admission did not wait")
	f.queue.stopReading()
	select {
	case err := <-done:
		if !readretry.IsTerminal(err) || !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("stop waited for admission")
	}
	f.check(t, sentinel, 0, true)
	if len(f.backend.calls) != 0 {
		t.Fatal("canceled request reached backend")
	}
}

func TestQoSQuiesceStillWaitsForStartedIO(t *testing.T) {
	f := newWindowFixture(t, []int{4096}, true)
	c := NewIOController(IOLimits{ReadIOPS: 1}, IOLimits{})
	f.server.SetIOBudget(c.NewDisk())
	entered, release := make(chan struct{}), make(chan struct{})
	f.backend.fault = func(p []byte, off int64) (int, error) {
		close(entered)
		<-release
		return f.backend.memBackend.ReadAt(p, off)
	}
	done := make(chan error, 1)
	go func() { done <- f.server.runPending(f.queue) }()
	awaitSignal(t, entered, "backend did not start")
	paused := make(chan struct{})
	go func() { f.server.Quiesce(); close(paused) }()
	select {
	case <-paused:
		t.Fatal("pause passed active backend IO")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	awaitSignal(t, paused, "pause did not drain completed IO")
	f.server.Resume()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.check(t, BlkStatusOK, 4096, false)
}

func TestQoSCanceledAdmissionDoesNotSpendQuota(t *testing.T) {
	c := NewIOController(IOLimits{ReadIOPS: 1}, IOLimits{ReadIOPS: 1})
	d := c.NewDisk()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.try(ctx, false, 4096, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if c.global[0][1].debt != 0 || d.disk[0][1].debt != 0 {
		t.Fatal("canceled request spent quota")
	}
}

func appendQoSReadChain(f *windowFixture) *byte {
	mem := f.server.memTable.regions[0].mmapBytes
	const header, payload, status = 3600, 3700, 4212
	le := binary.LittleEndian
	le.PutUint32(mem[header:], BlkTypeIn)
	le.PutUint64(mem[header+8:], readSector)
	mem[status] = sentinel
	le.PutUint64(mem[10*16:], header)
	le.PutUint32(mem[10*16+8:], 16)
	le.PutUint16(mem[10*16+12:], descFlagNext)
	le.PutUint16(mem[10*16+14:], 11)
	le.PutUint64(mem[11*16:], payload)
	le.PutUint32(mem[11*16+8:], 513) // 512-byte payload followed by status
	le.PutUint16(mem[11*16+12:], descFlagWrite)
	le.PutUint16(mem[128*16+2:], 2)
	le.PutUint16(mem[128*16+6:], 10)
	return &mem[status]
}

func TestQoSNotifiesCompletedRequestsBeforeWaiting(t *testing.T) {
	f := newWindowFixture(t, []int{4096}, true)
	secondStatus := appendQoSReadChain(f)
	if err := unix.SetNonblock(f.queue.callFd, true); err != nil {
		t.Fatal(err)
	}
	c := NewIOController(IOLimits{ReadIOPS: 1}, IOLimits{})
	ns, _ := fakeIOClock(c)
	f.server.SetIOBudget(c.NewDisk())
	waiting := make(chan struct{})
	var once sync.Once
	c.waiting = func() { once.Do(func() { close(waiting) }) }
	done := make(chan error, 1)
	go func() { done <- f.server.runPending(f.queue) }()
	awaitSignal(t, waiting, "second request did not wait")
	// Check before any pause/yield: those paths also notify completed entries
	// and would otherwise hide a notification delayed by the rate wait.
	var event [8]byte
	if n, err := unix.Read(f.queue.callFd, event[:]); err != nil || n != 8 || binary.LittleEndian.Uint64(event[:]) != 1 {
		t.Fatalf("completed first request was not notified: n=%d err=%v event=%v", n, err, event)
	}
	f.check(t, BlkStatusOK, 4096, false)
	if *secondStatus != sentinel || len(f.backend.calls) != 1 {
		t.Fatal("waiting second request started or completed early")
	}
	f.server.Quiesce()
	ns.Store(int64(time.Second))
	f.server.Resume()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second request did not resume")
	}
	if f.queue.baseIdx != 2 || binary.LittleEndian.Uint16(f.used[2:]) != 2 || *secondStatus != BlkStatusOK || len(f.backend.calls) != 2 {
		t.Fatal("second request did not complete exactly once")
	}
}

func TestQoSAdmissionRetainsBatchNotificationsWhenReady(t *testing.T) {
	f := newWindowFixture(t, []int{4096}, true)
	appendQoSReadChain(f)
	c := NewIOController(IOLimits{ReadIOPS: 1}, IOLimits{})
	ns, _ := fakeIOClock(c)
	f.server.SetIOBudget(c.NewDisk())
	f.backend.fault = func(p []byte, off int64) (int, error) {
		ns.Add(int64(time.Second))
		return f.backend.memBackend.ReadAt(p, off)
	}
	if err := f.server.processQueue(f.queue); err != nil {
		t.Fatal(err)
	}
	var event [8]byte
	if n, err := unix.Read(f.queue.callFd, event[:]); err != nil || n != 8 || binary.LittleEndian.Uint64(event[:]) != 1 {
		t.Fatalf("ready requests lost batch notification: n=%d err=%v event=%v", n, err, event)
	}
}

func TestQoSNotifyFailureDoesNotAckWaitingRequestOrBecomeStorageFatal(t *testing.T) {
	f := newWindowFixture(t, []int{4096}, true)
	secondStatus := appendQoSReadChain(f)
	_ = unix.Close(f.queue.callFd)
	f.queue.callFd = -1
	c := NewIOController(IOLimits{ReadIOPS: 1}, IOLimits{})
	fakeIOClock(c)
	f.server.SetIOBudget(c.NewDisk())
	fatal := false
	f.server.SetReadFatal(func(error) { fatal = true })
	err := f.server.runPending(f.queue)
	if !errors.Is(err, unix.EBADF) || readretry.IsTerminal(err) || fatal {
		t.Fatalf("notification failure changed storage semantics: %v fatal=%t", err, fatal)
	}
	f.check(t, BlkStatusOK, 4096, false)
	if *secondStatus != sentinel || len(f.backend.calls) != 1 || c.global[0][1].debt != 1 {
		t.Fatal("notification failure acknowledged or charged the waiting request")
	}
}
