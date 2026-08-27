package main

import "testing"

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
