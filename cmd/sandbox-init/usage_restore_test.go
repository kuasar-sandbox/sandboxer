package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

func usageReattachFixture(t *testing.T) (*usageService, *supervisorState, *consoleBridge) {
	t.Helper()
	s := newUsageService()
	s.paused, s.generation, s.epoch, s.lastRequest = true, 7, "old-host", 100
	s.sources[0].read = func() usageRawResult {
		return usageRawResult{memory: proto.UsageMemory{UsageReadState: proto.UsageReadState{Status: proto.UsageOK},
			Domain: "0/Normal", PresentPages: usageUint(100), BuddyFreePages: usageUint(10), PCPFreePages: usageUint(2), PageSize: usageUint(4096)}}
	}
	s.sources = append(s.sources, &usageSource{disk: "root", read: func() usageRawResult {
		return usageRawResult{filesystem: proto.UsageFilesystem{Disk: "root", Incarnation: "root/1",
			UsageReadState: proto.UsageReadState{Status: proto.UsageOK}, Blocks: usageUint(100), BFree: usageUint(40),
			BlockSize: usageUint(4096), FragmentSize: usageUint(4096), Type: usageUint(unix.EXT4_SUPER_MAGIC)}}
	}})
	sup := &supervisorState{execReg: newExecRegistry(), connReg: newConnRegistry(), acceptLn: newAcceptListeners(), pluginReg: newPluginRegistry()}
	sup.quiescing.Store(true)
	sup.execReg.beginQuiesce()
	sup.connReg.beginQuiesce()
	sup.acceptLn.closeAll()
	sup.pluginReg.beginQuiesce()
	reports := &memReportStream{epoch: 3}
	reports.pauseAndDrain()
	oldUsage, oldReports := guestUsage, guestMemReports
	guestUsage, guestMemReports = s, reports
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.pause(ctx); err != nil {
			t.Error(err)
		}
		s.close()
		guestUsage, guestMemReports = oldUsage, oldReports
	})
	b := newTestBridge()
	b.holder = newSessionHolder()
	t.Cleanup(func() {
		if session := b.holder.peek(); session != nil {
			_ = session.Close()
		}
	})
	return s, sup, b
}

func usageSocketPair(t *testing.T) (*vsockConn, *vsockConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	guest, host := &vsockConn{fd: fds[0]}, &vsockConn{fd: fds[1]}
	if err := host.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = host.shutdown()
		_ = guest.shutdown()
		_ = host.Close()
		_ = guest.Close()
	})
	return guest, host
}

func usageManagementConn(t *testing.T, sup *supervisorState, bridge *consoleBridge) (*vsockConn, <-chan struct{}) {
	t.Helper()
	guest, host := usageSocketPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !handleReverseConn(guest, sup, bridge) {
			_ = guest.Close() // same final owner as serveReverseChannel
		}
	}()
	t.Cleanup(func() {
		_ = host.shutdown()
		waitUsageDone(t, done)
	})
	return host, done
}

func waitUsageDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("usage operation did not finish")
	}
}

func writeUsageRequest(t *testing.T, c *vsockConn, epoch string, id uint64) {
	t.Helper()
	if err := proto.WriteMessage(c, &proto.Message{Type: proto.TypeUsageRequest, UsageRequest: &proto.UsageRequest{
		RunEpoch: epoch, RequestID: id, ReadBudgetNS: int64(100 * time.Millisecond),
	}}); err != nil {
		t.Fatal(err)
	}
}

func readUsageResponse(t *testing.T, c *vsockConn, epoch string, id uint64) *proto.UsageResponse {
	t.Helper()
	writeUsageRequest(t, c, epoch, id)
	msg, err := proto.ReadMessage(usageIO{c, time.Now().Add(2 * time.Second)})
	if err != nil || msg.Type != proto.TypeUsageResponse || msg.UsageResponse == nil {
		t.Fatalf("usage response: %+v, %v", msg, err)
	}
	r := msg.UsageResponse
	if r.RunEpoch != epoch || r.RequestID != id || r.Memory.Status != proto.UsageOK || r.Filesystems[0].Status != proto.UsageOK {
		t.Fatalf("fresh raw observation: %+v", r)
	}
	return r
}

func assertUsageLaunchGatesClosed(t *testing.T, sup *supervisorState) {
	t.Helper()
	conn := &vsockConn{fd: -1}
	if sup.execReg.beginSession(conn) {
		sup.execReg.endSession(conn)
		t.Error("exec admitted before successful thaw")
	}
	if !sup.quiescing.Load() || !guestMemReports.isPaused() {
		t.Error("app/mem_report resumed before successful thaw")
	}
	sup.pluginReg.mu.Lock()
	pluginPaused := sup.pluginReg.quiescing
	sup.pluginReg.mu.Unlock()
	if !pluginPaused {
		t.Error("plugin admitted before successful thaw")
	}
}

func closeUsageTestMUX(t *testing.T, bridge *consoleBridge, host *vsockConn) {
	t.Helper()
	session := bridge.holder.peek()
	if session == nil {
		return
	}
	// Use the real terminal MUX handshake and join its reader before socket
	// cleanup allows FD reuse by the next subtest.
	done := make(chan struct{})
	go func() { _ = session.InitMuxClose(); close(done) }()
	wire := usageIO{host, time.Now().Add(2 * time.Second)}
	frame, err := mux.ReadFrame(wire)
	if err != nil || frame.Type != mux.FrameMuxClose {
		t.Errorf("MUX close: %+v, %v", frame, err)
	} else if err := mux.WriteFrame(wire, mux.Frame{Stream: mux.StreamControl, Type: mux.FrameMuxCloseAck}); err != nil {
		t.Error(err)
	}
	waitUsageDone(t, done)
	waitUsageDone(t, session.Done())
	_ = session.Close()
}

func TestUsageReattachAdmitsBeforeAckAndKeepsSourceSlots(t *testing.T) {
	for _, newHost := range []bool{false, true} {
		for _, failThaw := range []bool{false, true} {
			name := "attach"
			if newHost {
				name = "restore"
			}
			if failThaw {
				name += "-thaw-failure"
			}
			t.Run(name, func(t *testing.T) {
				s, sup, bridge := usageReattachFixture(t)
				blocked, releaseRead := make(chan struct{}), make(chan struct{})
				var reads atomic.Int32
				var releaseOnce sync.Once
				t.Cleanup(func() { releaseOnce.Do(func() { close(releaseRead) }) })
				s.sources = append(s.sources, &usageSource{disk: "blocked", read: func() usageRawResult {
					reads.Add(1)
					close(blocked)
					<-releaseRead
					return usageRawResult{filesystem: proto.UsageFilesystem{Disk: "blocked", UsageReadState: proto.UsageReadState{Status: proto.UsageOK}}}
				}})
				old := s.collect(proto.UsageRequest{RunEpoch: "old-host", RequestID: 100, ReadBudgetNS: int64(20 * time.Millisecond)}, nil)
				waitUsageDone(t, blocked)
				if old.Filesystems[1].Status != proto.UsageTimeout {
					t.Fatal("first blocked source was not timed out")
				}
				guest, host := usageSocketPair(t)
				thawStarted, releaseThaw, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var thawOnce sync.Once
				var handed bool
				var finishErr error
				wantErr := errors.New("thaw failed")
				go func() {
					defer close(finished)
					handed, finishErr = finishReattach(guest, sup, bridge, 19, newHost, func() error {
						close(thawStarted)
						<-releaseThaw
						if failThaw {
							return wantErr
						}
						return nil
					})
				}()
				t.Cleanup(func() {
					thawOnce.Do(func() { close(releaseThaw) })
					waitUsageDone(t, finished)
					closeUsageTestMUX(t, bridge, host)
				})
				ack, err := proto.ReadMessage(usageIO{host, time.Now().Add(2 * time.Second)})
				ackType, epoch, id := proto.TypeAttachAck, "old-host", uint64(101)
				if newHost {
					ackType, epoch, id = proto.TypeRestoreAck, "new-host", 1
				}
				if err != nil || ack.Type != ackType || ack.Epoch != 19 {
					t.Fatalf("ACK: %+v, %v", ack, err)
				}
				waitUsageDone(t, thawStarted)
				assertUsageLaunchGatesClosed(t, sup)
				client, connDone := usageManagementConn(t, sup, bridge)
				r := readUsageResponse(t, client, epoch, id)
				if r.Filesystems[1].Status != proto.UsageBusy || reads.Load() != 1 {
					t.Fatal("reattach replaced the still-running source or replayed its old result")
				}
				assertUsageLaunchGatesClosed(t, sup)
				thawOnce.Do(func() { close(releaseThaw) })
				waitUsageDone(t, finished)
				if !handed || bridge.holder.peek() == nil {
					t.Fatal("reattached MUX ownership lost")
				}
				if failThaw {
					if !errors.Is(finishErr, wantErr) {
						t.Fatalf("thaw error: %v", finishErr)
					}
					waitUsageDone(t, connDone)
					assertUsageLaunchGatesClosed(t, sup)
					s.mu.Lock()
					paused, conn := s.paused, s.conn
					s.mu.Unlock()
					if !paused || conn != nil {
						t.Fatal("failed thaw left newly admitted usage running")
					}
				} else {
					if finishErr != nil || sup.quiescing.Load() || guestMemReports.isPaused() {
						t.Fatalf("successful thaw failed to resume: %v", finishErr)
					}
					// Retry the accepted ID before any newer one could conceal an
					// erroneous post-thaw reset of the epoch/request endpoint.
					writeUsageRequest(t, client, epoch, id)
					if msg, err := proto.ReadMessage(usageIO{client, time.Now().Add(2 * time.Second)}); err == nil {
						t.Fatalf("thaw reset the accepted request IDs: %+v", msg)
					}
					waitUsageDone(t, connDone)
					client, _ = usageManagementConn(t, sup, bridge)
					readUsageResponse(t, client, epoch, id+1)
				}
			})
		}
	}
}

func TestUsageReattachAckFailurePreservesPriorAdmission(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "paused", true: "already-live"}[live], func(t *testing.T) {
			s, sup, bridge := usageReattachFixture(t)
			var client *vsockConn
			if live {
				_, generation := s.resume(false)
				s.commitResume(generation)
				client, _ = usageManagementConn(t, sup, bridge)
				readUsageResponse(t, client, "old-host", 101)
			}
			guest, host := usageSocketPair(t)
			_ = host.shutdown()
			_ = host.Close()
			handed, err := finishReattach(guest, sup, bridge, 19, !live, func() error {
				t.Error("thaw called after failed ACK")
				return nil
			})
			if handed || err == nil || bridge.holder.peek() != nil {
				t.Fatalf("failed ACK changed connection ownership: %v, %v", handed, err)
			}
			s.mu.Lock()
			paused := s.paused
			s.mu.Unlock()
			if paused == live {
				t.Fatal("failed ACK did not preserve prior admission")
			}
			if live {
				readUsageResponse(t, client, "old-host", 102)
			}
		})
	}
}

func TestUsageResumeRollbackIgnoresLaterGeneration(t *testing.T) {
	s, sup, bridge := usageReattachFixture(t)
	_, generation := s.resume(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.pause(ctx); err != nil {
		t.Fatal(err)
	}
	s.resume(true)
	client, _ := usageManagementConn(t, sup, bridge)
	readUsageResponse(t, client, "later-host", 1)
	if err := s.rollbackResume(ctx, generation); err != nil {
		t.Fatal(err)
	}
	readUsageResponse(t, client, "later-host", 2)
}

func TestUsageOlderAttachFailureAfterRetry(t *testing.T) {
	for _, failRetry := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry-succeeds", true: "both-fail"}[failRetry], func(t *testing.T) {
			testUsageOlderAttachFailureAfterRetry(t, failRetry)
		})
	}
}

func testUsageOlderAttachFailureAfterRetry(t *testing.T, failRetry bool) {
	t.Helper()
	s, sup, bridge := usageReattachFixture(t)
	firstGuest, firstHost := usageSocketPair(t)
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	var firstErr error
	wantErr := errors.New("older thaw failed late")
	go func() {
		defer close(finished)
		_, firstErr = finishReattach(firstGuest, sup, bridge, 1, false, func() error {
			close(started)
			<-release
			return wantErr
		})
	}()
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		waitUsageDone(t, finished)
	})
	ack, err := proto.ReadMessage(usageIO{firstHost, time.Now().Add(2 * time.Second)})
	if err != nil || ack.Type != proto.TypeAttachAck {
		t.Fatalf("first ACK: %+v, %v", ack, err)
	}
	waitUsageDone(t, started)
	client, connDone := usageManagementConn(t, sup, bridge)
	readUsageResponse(t, client, "old-host", 101)
	// The Host retries an ambiguous ACK. Its next TypeAttach handler drains
	// the first MUX while that handler's thaw/query has not yet returned.
	closeUsageTestMUX(t, bridge, firstHost)
	bridge.closeLiveMUX()
	secondGuest, secondHost := usageSocketPair(t)
	handed, err := finishReattach(secondGuest, sup, bridge, 2, false, func() error {
		if failRetry {
			return wantErr
		}
		return nil
	})
	t.Cleanup(func() { closeUsageTestMUX(t, bridge, secondHost) })
	if !handed || (failRetry && !errors.Is(err, wantErr)) || (!failRetry && err != nil) {
		t.Fatalf("second attach: handed=%v, %v", handed, err)
	}
	ack, err = proto.ReadMessage(usageIO{secondHost, time.Now().Add(2 * time.Second)})
	if err != nil || ack.Type != proto.TypeAttachAck {
		t.Fatalf("second ACK: %+v, %v", ack, err)
	}
	if failRetry {
		waitUsageDone(t, connDone)
	} else {
		readUsageResponse(t, client, "old-host", 102)
	}
	once.Do(func() { close(release) })
	waitUsageDone(t, finished)
	if !errors.Is(firstErr, wantErr) {
		t.Fatalf("older error: %v", firstErr)
	}
	s.mu.Lock()
	paused := s.paused
	s.mu.Unlock()
	if paused != failRetry {
		t.Fatalf("admission after overlapping failures: paused=%v, retry failed=%v", paused, failRetry)
	}
	if failRetry {
		assertUsageLaunchGatesClosed(t, sup)
	} else {
		readUsageResponse(t, client, "old-host", 103)
	}
}
