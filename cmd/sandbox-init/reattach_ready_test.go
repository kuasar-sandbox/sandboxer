package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

func TestReattachAckWaitsForThawAndLaunchGates(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(map[bool]string{false: "attach", true: "restore"}[restore], func(t *testing.T) {
			_, sup, bridge := usageReattachFixture(t)
			guest, host := usageSocketPair(t)
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			var handed bool
			var finishErr error
			go func() {
				defer close(done)
				handed, finishErr = finishReattach(guest, sup, bridge, 23, restore, func() error {
					close(started)
					<-release
					return nil
				})
			}()
			t.Cleanup(func() {
				once.Do(func() { close(release) })
				waitUsageDone(t, done)
				closeUsageTestMUX(t, bridge, host)
			})
			waitUsageDone(t, started)
			assertUsageLaunchGatesClosed(t, sup)
			var peek [1]byte
			if n, _, err := unix.Recvfrom(host.fd, peek[:], unix.MSG_PEEK|unix.MSG_DONTWAIT); n != -1 || !errors.Is(err, unix.EAGAIN) {
				t.Errorf("ACK exposed while thaw blocked: bytes=%d err=%v", n, err)
			}
			once.Do(func() { close(release) })
			ack, err := proto.ReadMessage(usageIO{host, time.Now().Add(2 * time.Second)})
			want := proto.TypeAttachAck
			if restore {
				want = proto.TypeRestoreAck
			}
			if err != nil || ack.Type != want || ack.Epoch != 23 {
				t.Fatalf("ACK = %+v, %v", ack, err)
			}
			// The host is allowed to publish ready as soon as ACK/MUX setup
			// completes. Check admission immediately, without joining the guest
			// handler and accidentally hiding a post-ACK race.
			exec := &vsockConn{fd: -1}
			if !sup.execReg.beginSession(exec) {
				t.Error("ACK exposed a quiescing exec gate")
			} else {
				sup.execReg.endSession(exec)
			}
			if sup.quiescing.Load() || sup.connReg.isQuiescing() {
				t.Error("ACK exposed closed app/forward gates")
			}
			sup.pluginReg.mu.Lock()
			pluginPaused := sup.pluginReg.quiescing
			sup.pluginReg.mu.Unlock()
			sup.acceptLn.mu.Lock()
			acceptPaused := sup.acceptLn.quiescing
			sup.acceptLn.mu.Unlock()
			if pluginPaused || acceptPaused {
				t.Error("ACK exposed closed plugin/accept gates")
			}
			waitUsageDone(t, done)
			if !handed || finishErr != nil || guestMemReports.isPaused() {
				t.Fatalf("reattach completion: handed=%v err=%v reports paused=%v", handed, finishErr, guestMemReports.isPaused())
			}
		})
	}
}

func TestReattachAckDoesNotWaitForConsoleDrain(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(map[bool]string{false: "attach", true: "restore"}[restore], func(t *testing.T) {
			_, sup, bridge := usageReattachFixture(t)
			guest, host := usageSocketPair(t)
			pr, pw, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
			bridge.hasStdout, bridge.stdoutR = true, pr
			bridge.appDrain = newAppGenerationDrain(drainStdout)
			bridge.start()
			t.Cleanup(func() {
				shutdownBridge(bridge)
				bridge.holder.shutdown()
			})
			payload := bytes.Repeat([]byte("backpressured console\n"), 1<<17)
			written := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				_, err := finishReattach(guest, sup, bridge, 1, restore, func() error {
					// Simulate the thawed primary filling its output while the
					// host still waits for ACK. No host MUX reader exists yet.
					go func() {
						_, err := pw.Write(payload)
						_ = pw.Close()
						written <- err
					}()
					return nil
				})
				if err != nil {
					t.Error(err)
				}
			}()
			t.Cleanup(func() { waitUsageDone(t, finished) })
			ack, err := proto.ReadMessage(usageIO{host, time.Now().Add(2 * time.Second)})
			if err != nil || ack.Stdio == nil || !ack.Stdio.Stdout {
				t.Fatalf("ACK blocked or interleaved with MUX output: %+v, %v", ack, err)
			}
			hostMUX := mux.NewSession(host, streamSetFor(*ack.Stdio), mux.Options{})
			t.Cleanup(func() {
				// Join the real terminal handshake before the raw socket FDs
				// can be reused by another test. Close alone does not join a
				// syscall reader blocked on an AF_UNIX/AF_VSOCK descriptor.
				shutdownBridge(bridge)
				if session := bridge.holder.peek(); session != nil {
					done := make(chan struct{})
					go func() { _ = session.InitMuxClose(); close(done) }()
					waitUsageDone(t, done)
					waitUsageDone(t, session.Done())
					waitUsageDone(t, hostMUX.Done())
					_ = session.Close()
				}
				_ = hostMUX.Close()
			})
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(hostMUX.Stream(mux.StreamStdout), got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("console output changed across ACK/MUX handoff")
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
			waitUsageDone(t, finished)
		})
	}
}

func TestLiveAttachPreservesReportAndUsageOwnership(t *testing.T) {
	s, sup, bridge := usageReattachFixture(t)
	if err := resumeAfterThaw(sup, nil); err != nil {
		t.Fatal(err)
	}
	reports := guestMemReports
	reports.resumeEpoch()
	reports.seq = 17
	reports.pending = &proto.MemReport{Epoch: reports.epoch, Seq: reports.seq}
	pending := reports.pending
	if !reports.beginAttempt() {
		t.Fatal("live report not admitted")
	}
	t.Cleanup(reports.endAttempt)
	_, generation := s.resume(false)
	s.commitResume(generation)
	client, _ := usageManagementConn(t, sup, bridge)
	readUsageResponse(t, client, "old-host", 101)
	guest, host := usageSocketPair(t)
	handed, err := finishReattach(guest, sup, bridge, 24, false, func() error { return nil })
	t.Cleanup(func() { closeUsageTestMUX(t, bridge, host) })
	if !handed || err != nil {
		t.Fatalf("live attach: handed=%v err=%v", handed, err)
	}
	ack, err := proto.ReadMessage(usageIO{host, time.Now().Add(2 * time.Second)})
	if err != nil || ack.Type != proto.TypeAttachAck {
		t.Fatalf("ACK: %+v, %v", ack, err)
	}
	if reports.admission.Load() != 1 || reports.pending != pending || reports.epoch != 3 || reports.seq != 17 {
		t.Fatal("live attach replaced the current report or its admitted owner")
	}
	readUsageResponse(t, client, "old-host", 102)
}
