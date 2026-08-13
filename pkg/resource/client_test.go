package resource

import (
	"net"
	"path/filepath"
	"testing"
)

func TestConnectFailureClearsClosedConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{SocketPath: path}
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(); err == nil {
		t.Fatal("Connect unexpectedly succeeded after listener closed")
	}
	if c.Connected() {
		t.Fatal("failed Connect retained the closed previous connection")
	}
}
