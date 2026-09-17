package chapi

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWaitReadyBoundsCompleteWireResponse(t *testing.T) {
	for name, wire := range map[string]string{
		"headers":       "HTTP/1.1 200 OK\r\nX-Large: " + strings.Repeat("x", maxReadyResponseBody+1),
		"body":          response(200, strings.Repeat(" ", maxReadyResponseBody+1)),
		"missing state": response(200, "null"),
		"multiple JSON": response(200, `{"state":"Paused"}{}`),
	} {
		t.Run(name, func(t *testing.T) {
			sock := unixSocket(t)
			stop := serveUnixHTTP(t, sock, func(conn net.Conn, _ *http.Request) {
				_, _ = io.WriteString(conn, wire)
			})
			defer stop()
			if err := WaitReady(context.Background(), sock, time.Second); err == nil {
				t.Fatal("invalid or oversized response accepted")
			}
		})
	}
}

func TestWaitReadyOversizeDoesNotWaitForPeerClose(t *testing.T) {
	sock := unixSocket(t)
	release := make(chan struct{})
	stop := serveUnixHTTP(t, sock, func(conn net.Conn, _ *http.Request) {
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n"+strings.Repeat(" ", maxReadyResponseBody+1))
		<-release
	})
	defer stop()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := WaitReady(ctx, sock, 0)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("oversize response did not fail before peer close: %v", err)
	}
}
