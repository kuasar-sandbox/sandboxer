package main

import (
	"errors"
	"testing"
	"time"
)

func TestExecRegistryQuiesceJoinsWholeSessions(t *testing.T) {
	reg := newExecRegistry()
	conn := &vsockConn{fd: -1}
	if !reg.beginSession(conn) {
		t.Fatal("session rejected before quiesce")
	}

	_, conns, drained := reg.beginQuiesce()
	if len(conns) != 1 || conns[0] != conn {
		t.Fatalf("quiesce connections = %v, want admitted session", conns)
	}
	select {
	case <-drained:
		t.Fatal("drain barrier closed before the admitted session exited")
	default:
	}
	if reg.beginSession(&vsockConn{fd: -1}) {
		t.Fatal("quiesce gate admitted a new exec session")
	}

	reg.endSession(conn)
	<-drained
	reg.endQuiesce()
	resumed := &vsockConn{fd: -1}
	if !reg.beginSession(resumed) {
		t.Fatal("session rejected after endQuiesce")
	}
	reg.endSession(resumed)
}

func TestExecRegistryQuiesceIncludesLiveChild(t *testing.T) {
	reg := newExecRegistry()
	conn := &vsockConn{fd: -1}
	if !reg.beginSession(conn) {
		t.Fatal("session rejected before quiesce")
	}
	wait, allowed := reg.register(42)
	if !allowed {
		t.Fatal("child rejected before quiesce")
	}
	_ = wait

	pids, _, _ := reg.beginQuiesce()
	if len(pids) != 1 || pids[0] != 42 {
		t.Fatalf("quiesce pids = %v, want [42]", pids)
	}
	reg.done(42)
	reg.endSession(conn)
}

func TestResumeAfterThawKeepsLaunchGatesClosed(t *testing.T) {
	sup := &supervisorState{
		execReg:   newExecRegistry(),
		connReg:   newConnRegistry(),
		acceptLn:  newAcceptListeners(),
		pluginReg: newPluginRegistry(),
	}
	sup.quiescing.Store(true)
	_, _, _ = sup.execReg.beginQuiesce()
	_ = sup.connReg.beginQuiesce()
	sup.acceptLn.closeAll()
	sup.pluginReg.beginQuiesce()
	reports := &memReportStream{epoch: 1}
	reports.pauseAndDrain()

	thawStarted := make(chan struct{})
	releaseThaw := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- resumeAfterThaw(sup, reports, func() error {
			close(thawStarted)
			<-releaseThaw
			return nil
		})
	}()
	<-thawStarted

	if sup.execReg.beginSession(&vsockConn{fd: -1}) {
		t.Fatal("exec gate reopened before cgroup thaw completed")
	}
	if sup.connReg.add(&connSession{}) {
		t.Fatal("connect gate reopened before cgroup thaw completed")
	}
	if !sup.quiescing.Load() {
		t.Fatal("application restart gate reopened before cgroup thaw completed")
	}
	sup.pluginReg.mu.Lock()
	pluginQuiescing := sup.pluginReg.quiescing
	sup.pluginReg.mu.Unlock()
	if !pluginQuiescing {
		t.Fatal("plugin gate reopened before cgroup thaw completed")
	}

	close(releaseThaw)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("resumeAfterThaw did not finish after thaw completed")
	}
	resumed := &vsockConn{fd: -1}
	if !sup.execReg.beginSession(resumed) {
		t.Fatal("exec gate remained closed after cgroup thaw completed")
	}
	sup.execReg.endSession(resumed)
	if sup.quiescing.Load() {
		t.Fatal("application restart gate remained closed after cgroup thaw completed")
	}
	if reports.isPaused() {
		t.Fatal("memory report stream remained paused after cgroup thaw completed")
	}
}

func TestResumeAfterThawFailureKeepsLaunchGatesClosed(t *testing.T) {
	sup := &supervisorState{
		execReg:   newExecRegistry(),
		connReg:   newConnRegistry(),
		acceptLn:  newAcceptListeners(),
		pluginReg: newPluginRegistry(),
	}
	sup.quiescing.Store(true)
	_, _, _ = sup.execReg.beginQuiesce()
	_ = sup.connReg.beginQuiesce()
	sup.acceptLn.closeAll()
	sup.pluginReg.beginQuiesce()
	reports := &memReportStream{epoch: 1}
	reports.pauseAndDrain()

	wantErr := errors.New("thaw failed")
	if err := resumeAfterThaw(sup, reports, func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("resumeAfterThaw() error = %v, want %v", err, wantErr)
	}
	if sup.execReg.beginSession(&vsockConn{fd: -1}) {
		t.Fatal("exec gate reopened after failed thaw")
	}
	if sup.connReg.add(&connSession{}) {
		t.Fatal("connect gate reopened after failed thaw")
	}
	if !sup.quiescing.Load() {
		t.Fatal("application restart gate reopened after failed thaw")
	}
	sup.pluginReg.mu.Lock()
	pluginQuiescing := sup.pluginReg.quiescing
	sup.pluginReg.mu.Unlock()
	if !pluginQuiescing {
		t.Fatal("plugin gate reopened after failed thaw")
	}
	if !reports.isPaused() {
		t.Fatal("memory report stream resumed after failed thaw")
	}
}
