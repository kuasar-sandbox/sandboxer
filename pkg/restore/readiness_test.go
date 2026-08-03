package restore

import (
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestOpenAndEstablishRestoreMUXNotifiesAfterBothSteps(t *testing.T) {
	host, peer := net.Pipe()
	defer host.Close()
	defer peer.Close()
	spec := proto.StdioSpec{TTY: true}
	var order []string
	got, err := openAndEstablishRestoreMUX(
		func() (net.Conn, proto.StdioSpec, error) {
			order = append(order, "restore_ack")
			return host, spec, nil
		},
		func(conn net.Conn, got proto.StdioSpec) error {
			order = append(order, "establish_mux")
			if conn != host || !reflect.DeepEqual(got, spec) {
				t.Fatalf("establish args = (%v, %+v), want host and %+v", conn, got, spec)
			}
			return nil
		},
		func() { order = append(order, "ready") },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, spec) {
		t.Fatalf("spec = %+v, want %+v", got, spec)
	}
	want := []string{"restore_ack", "establish_mux", "ready"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestOpenAndEstablishRestoreMUXOpenFailureSkipsReady(t *testing.T) {
	established, notified := false, false
	_, err := openAndEstablishRestoreMUX(
		func() (net.Conn, proto.StdioSpec, error) {
			return nil, proto.StdioSpec{}, errors.New("restore ack failed")
		},
		func(net.Conn, proto.StdioSpec) error { established = true; return nil },
		func() { notified = true },
	)
	if err == nil || !strings.Contains(err.Error(), "notify restore") {
		t.Fatalf("error = %v, want notify restore failure", err)
	}
	if established || notified {
		t.Fatalf("established=%v notified=%v, want both false", established, notified)
	}
}

func TestOpenAndEstablishRestoreMUXEstablishFailureSkipsReady(t *testing.T) {
	host, peer := net.Pipe()
	defer peer.Close()
	notified := false
	_, err := openAndEstablishRestoreMUX(
		func() (net.Conn, proto.StdioSpec, error) {
			return host, proto.StdioSpec{}, nil
		},
		func(net.Conn, proto.StdioSpec) error { return errors.New("bridge failed") },
		func() { notified = true },
	)
	if err == nil || !strings.Contains(err.Error(), "stdio MUX bridge") {
		t.Fatalf("error = %v, want MUX establishment failure", err)
	}
	if notified {
		t.Fatal("ready emitted after EstablishMUX failure")
	}
	buf := make([]byte, 1)
	if _, readErr := peer.Read(buf); readErr == nil {
		t.Fatal("restore MUX connection was not closed after EstablishMUX failure")
	}
}

func TestOpenAndEstablishRestoreMUXNilNotifier(t *testing.T) {
	host, peer := net.Pipe()
	defer host.Close()
	defer peer.Close()
	if _, err := openAndEstablishRestoreMUX(
		func() (net.Conn, proto.StdioSpec, error) { return host, proto.StdioSpec{}, nil },
		func(net.Conn, proto.StdioSpec) error { return nil },
		nil,
	); err != nil {
		t.Fatal(err)
	}
}
