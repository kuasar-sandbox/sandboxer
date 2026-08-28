package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

func TestMemReportNotifyDeadlineFitsRetryInterval(t *testing.T) {
	if memReportNotifyDeadline <= 0 {
		t.Fatalf("memReportNotifyDeadline = %v, want positive", memReportNotifyDeadline)
	}
	if memReportNotifyDeadline >= memReportInterval {
		t.Fatalf("memReportNotifyDeadline = %v, want less than interval %v", memReportNotifyDeadline, memReportInterval)
	}
}

func TestExchangeMemReportAllowsPressureDelayedAck(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	serverDone := make(chan error, 1)
	report := proto.MemReport{
		Epoch:             7,
		Seq:               9,
		MemTotalBytes:     256 << 20,
		MemAvailableBytes: 128 << 20,
	}
	go func() {
		req, err := proto.ReadMessage(server)
		if err != nil {
			serverDone <- fmt.Errorf("read report: %w", err)
			return
		}
		if req.Type != proto.TypeMemReport {
			serverDone <- fmt.Errorf("message type = %q, want %q", req.Type, proto.TypeMemReport)
			return
		}
		if req.MemReport == nil || !reflect.DeepEqual(*req.MemReport, report) {
			serverDone <- fmt.Errorf("report = %+v, want %+v", req.MemReport, report)
			return
		}
		time.Sleep(proto.DeadlineAppNotify + 50*time.Millisecond)
		serverDone <- proto.WriteMessage(server, &proto.Message{Type: proto.TypeMemReportAck})
	}()

	if err := exchangeMemReport(client, report, memReportNotifyDeadline); err != nil {
		t.Fatalf("exchangeMemReport with pressure-delayed ACK: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestExchangeMemReportBoundsTrickledAck(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	client := &vsockConn{fd: fds[0]}
	server := &vsockConn{fd: fds[1]}
	defer client.Close()
	defer server.Close()

	serverDone := make(chan error, 1)
	go func() {
		req, err := proto.ReadMessage(server)
		if err != nil {
			serverDone <- fmt.Errorf("read report: %w", err)
			return
		}
		if req.Type != proto.TypeMemReport {
			serverDone <- fmt.Errorf("message type = %q, want %q", req.Type, proto.TypeMemReport)
			return
		}

		var ack bytes.Buffer
		if err := proto.WriteMessage(&ack, &proto.Message{Type: proto.TypeMemReportAck}); err != nil {
			serverDone <- fmt.Errorf("encode ACK: %w", err)
			return
		}
		for _, b := range ack.Bytes() {
			time.Sleep(15 * time.Millisecond)
			if _, err := server.Write([]byte{b}); err != nil {
				serverDone <- nil
				return
			}
		}
		serverDone <- errors.New("trickled ACK completed beyond the exchange budget")
	}()

	const budget = 100 * time.Millisecond
	started := time.Now()
	if err := exchangeMemReport(client, proto.MemReport{Epoch: 1, Seq: 1, MemTotalBytes: 256 << 20}, budget); err == nil {
		t.Fatal("exchangeMemReport accepted an ACK that exceeded the wall-clock budget")
	}
	if elapsed := time.Since(started); elapsed > 3*budget {
		t.Fatalf("exchangeMemReport returned after %v, want within %v", elapsed, 3*budget)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("trickled ACK server did not observe deadline close")
	}
}

func TestMemReportStreamRetriesSameObservationAndProgresses(t *testing.T) {
	var logs []string
	var reports []proto.MemReport
	reads := 0
	stream := &memReportStream{epoch: 1}
	read := func() (proto.MemReport, error) {
		reads++
		return proto.MemReport{
			MemTotalBytes:     256 << 20,
			MemAvailableBytes: uint64(128+reads) << 20,
		}, nil
	}
	notify := func(report proto.MemReport) error {
		reports = append(reports, report)
		if len(reports) <= 2 {
			return errors.New("read: resource temporarily unavailable")
		}
		return nil
	}
	log := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	stream.attempt(read, notify, log)
	stream.attempt(read, notify, log)
	stream.attempt(read, notify, log)
	stream.attempt(read, notify, log)

	if reads != 2 {
		t.Fatalf("read calls = %d, want 2", reads)
	}
	if len(reports) != 4 {
		t.Fatalf("notify calls = %d, want 4", len(reports))
	}
	for i := 0; i < 3; i++ {
		if !reflect.DeepEqual(reports[i], reports[0]) {
			t.Fatalf("retry report[%d] = %+v, want identical to %+v", i, reports[i], reports[0])
		}
	}
	if reports[0].Epoch != 1 || reports[0].Seq != 1 || reports[3].Epoch != 1 || reports[3].Seq != 2 {
		t.Fatalf("report sequence = %+v then %+v", reports[0], reports[3])
	}
	if reports[3].MemAvailableBytes == reports[0].MemAvailableBytes {
		t.Fatal("successful retry did not allow the following attempt to sample fresh data")
	}

	want := []string{
		"mem_report: read: resource temporarily unavailable",
		"mem_report: read: resource temporarily unavailable",
		"mem_report: recovered after 2 consecutive failures",
	}
	if !reflect.DeepEqual(logs, want) {
		t.Fatalf("logs = %#v, want %#v", logs, want)
	}
}

func TestMemReportStreamAdvanceEpochPausesAndClearsPending(t *testing.T) {
	stream := &memReportStream{epoch: 9}
	readCalls := 0
	read := func() (proto.MemReport, error) {
		readCalls++
		return proto.MemReport{MemTotalBytes: 512 << 20, MemAvailableBytes: 64 << 20}, nil
	}
	stream.attempt(read, func(proto.MemReport) error { return errors.New("timeout") }, func(string, ...any) {})
	if readCalls != 1 {
		t.Fatalf("initial read calls = %d, want 1", readCalls)
	}

	epoch, err := stream.advanceEpochAndPause()
	if err != nil {
		t.Fatal(err)
	}
	if epoch != 10 {
		t.Fatalf("epoch = %d, want 10", epoch)
	}
	var got proto.MemReport
	notifyCalls := 0
	stream.attempt(read, func(report proto.MemReport) error {
		notifyCalls++
		got = report
		return nil
	}, func(string, ...any) {})
	if readCalls != 1 || notifyCalls != 0 || got != (proto.MemReport{}) {
		t.Fatalf("paused epoch sampled or sent report: reads=%d notifies=%d report=%+v", readCalls, notifyCalls, got)
	}
	stream.resumeEpoch()
	stream.attempt(read, func(report proto.MemReport) error {
		notifyCalls++
		got = report
		return nil
	}, func(string, ...any) {})
	if readCalls != 2 || notifyCalls != 1 {
		t.Fatalf("resumed epoch calls: reads=%d notifies=%d, want 2/1", readCalls, notifyCalls)
	}
	if got.Epoch != 10 || got.Seq != 1 {
		t.Fatalf("new epoch report = %+v, want epoch=10 seq=1", got)
	}
}

func TestMemReportStreamAdvanceWaitsForInflightReport(t *testing.T) {
	stream := &memReportStream{epoch: 1}
	started := make(chan struct{})
	release := make(chan struct{})
	attemptDone := make(chan struct{})
	go func() {
		stream.attempt(
			func() (proto.MemReport, error) { return proto.MemReport{MemTotalBytes: 1}, nil },
			func(proto.MemReport) error {
				close(started)
				<-release
				return nil
			},
			func(string, ...any) {},
		)
		close(attemptDone)
	}()
	<-started

	advanced := make(chan uint64, 1)
	go func() {
		epoch, err := stream.advanceEpochAndPause()
		if err != nil {
			advanced <- 0
			return
		}
		advanced <- epoch
	}()
	select {
	case epoch := <-advanced:
		t.Fatalf("advanceEpochAndPause returned %d before the in-flight report completed", epoch)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-attemptDone
	if epoch := <-advanced; epoch != 2 {
		t.Fatalf("advanced epoch = %d, want 2", epoch)
	}
}

func TestMemReportStreamPauseAndDrainWaitsForInflightReport(t *testing.T) {
	stream := &memReportStream{epoch: 1}
	started := make(chan struct{})
	release := make(chan struct{})
	attemptDone := make(chan struct{})
	go func() {
		stream.attempt(
			func() (proto.MemReport, error) { return proto.MemReport{MemTotalBytes: 1}, nil },
			func(proto.MemReport) error {
				close(started)
				<-release
				return nil
			},
			func(string, ...any) {},
		)
		close(attemptDone)
	}()
	<-started

	paused := make(chan struct{})
	go func() {
		if err := quiesceExecAndMemoryReports(newExecRegistry(), stream); err != nil {
			t.Errorf("quiesceExecAndMemoryReports: %v", err)
		}
		close(paused)
	}()
	gateObserved := make(chan struct{})
	go func() {
		for !stream.isPaused() {
			runtime.Gosched()
		}
		close(gateObserved)
	}()
	select {
	case <-gateObserved:
	case <-time.After(time.Second):
		t.Fatal("pauseAndDrain did not gate new reports")
	}

	readCalls := 0
	rejectedDone := make(chan struct{})
	go func() {
		stream.attempt(
			func() (proto.MemReport, error) {
				readCalls++
				return proto.MemReport{MemTotalBytes: 1}, nil
			},
			func(proto.MemReport) error { return nil },
			func(string, ...any) {},
		)
		close(rejectedDone)
	}()
	select {
	case <-rejectedDone:
	case <-time.After(time.Second):
		t.Fatal("new report blocked behind the in-flight report after the pause gate closed")
	}
	if readCalls != 0 {
		t.Fatalf("pause gate sampled %d new report(s), want 0", readCalls)
	}
	select {
	case <-paused:
		t.Fatal("pauseAndDrain returned while the admitted report was still in flight")
	default:
	}

	close(release)
	<-attemptDone
	<-paused

	stream.attempt(
		func() (proto.MemReport, error) {
			readCalls++
			return proto.MemReport{MemTotalBytes: 1}, nil
		},
		func(proto.MemReport) error { return nil },
		func(string, ...any) {},
	)
	if readCalls != 0 {
		t.Fatalf("paused stream sampled %d report(s), want 0", readCalls)
	}

	stream.resumeEpoch()
	stream.attempt(
		func() (proto.MemReport, error) {
			readCalls++
			return proto.MemReport{MemTotalBytes: 1}, nil
		},
		func(proto.MemReport) error { return nil },
		func(string, ...any) {},
	)
	if readCalls != 1 {
		t.Fatalf("resumed stream sampled %d report(s), want 1", readCalls)
	}
}

func TestMemReportStreamLiveAttachPreservesInflightAdmission(t *testing.T) {
	stream := &memReportStream{epoch: 1}
	started := make(chan struct{})
	release := make(chan struct{})
	attemptDone := make(chan any, 1)
	go func() {
		defer func() { attemptDone <- recover() }()
		stream.attempt(
			func() (proto.MemReport, error) { return proto.MemReport{MemTotalBytes: 1}, nil },
			func(proto.MemReport) error {
				close(started)
				<-release
				return nil
			},
			func(string, ...any) {},
		)
	}()
	<-started

	// A plain live attach reopens an already-live epoch. It must clear only
	// the pause gate; an admitted report still owns the low-bit count until
	// its exchange returns.
	stream.resumeEpoch()
	if got := stream.admission.Load() & memReportActiveMask; got != 1 {
		t.Errorf("active report count after live attach = %d, want 1", got)
	}
	close(release)
	if panicValue := <-attemptDone; panicValue != nil {
		t.Fatalf("report completion after live attach panicked: %v", panicValue)
	}
	if got := stream.admission.Load(); got != 0 {
		t.Fatalf("admission after report completion = %#x, want 0", got)
	}
}

func TestMemReportStreamRejectsEpochOverflow(t *testing.T) {
	stream := &memReportStream{epoch: ^uint64(0)}
	if _, err := stream.advanceEpochAndPause(); err == nil {
		t.Fatal("advanceEpochAndPause accepted uint64 overflow")
	}
}

func TestParseMemInfo(t *testing.T) {
	report, err := parseMemInfo([]byte("MemTotal: 1048576 kB\nMemFree: 1 kB\nMemAvailable: 0 kB\nCached: 2 kB\nAnonPages: 3 kB\nSReclaimable: 4 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := proto.MemReport{
		MemTotalBytes:     1 << 30,
		MemAvailableBytes: 0,
		MemFreeBytes:      1 << 10,
		CachedBytes:       2 << 10,
		AnonPagesBytes:    3 << 10,
		SReclaimableBytes: 4 << 10,
	}
	if !reflect.DeepEqual(report, want) {
		t.Fatalf("report = %+v, want %+v", report, want)
	}
}

func TestParseMemInfoRejectsMissingRequiredFields(t *testing.T) {
	for _, input := range []string{
		"MemAvailable: 1 kB\n",
		"MemTotal: 1 kB\n",
		"MemTotal: 0 kB\nMemAvailable: 0 kB\n",
	} {
		if _, err := parseMemInfo([]byte(input)); err == nil {
			t.Fatalf("parseMemInfo(%q) succeeded", input)
		}
	}
}

func TestParseMemInfoRejectsByteOverflow(t *testing.T) {
	input := fmt.Sprintf("MemTotal: %d kB\nMemAvailable: 1 kB\n", uint64(^uint64(0)>>10)+1)
	if _, err := parseMemInfo([]byte(input)); err == nil {
		t.Fatal("parseMemInfo accepted a kB value that overflows bytes")
	}
}
