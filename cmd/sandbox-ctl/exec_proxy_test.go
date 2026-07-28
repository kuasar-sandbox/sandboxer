package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestProxyHeaderFlagPreservesRepeatedValuesAndRejectsOwnedFields(t *testing.T) {
	var headers proxyHeaderFlag
	for _, value := range []string{
		"X-Test: first",
		"x-test: second:with:colons",
		"Proxy-Authorization: Bearer secret",
	} {
		if err := headers.Set(value); err != nil {
			t.Fatalf("Set(%q): %v", value, err)
		}
	}
	if got := headers.Header().Values("X-Test"); len(got) != 2 || got[0] != "first" || got[1] != "second:with:colons" {
		t.Fatalf("X-Test values = %v", got)
	}
	if got := headers.String(); got != "<redacted>" || strings.Contains(got, "secret") {
		t.Fatalf("header flag String() = %q", got)
	}

	for _, value := range []string{
		"Host: other",
		"connection: close",
		"Proxy-Connection: keep-alive",
		"content-length: 1",
		"Transfer-Encoding: chunked",
		"Trailer: X-Later",
	} {
		var rejected proxyHeaderFlag
		if err := rejected.Set(value); err == nil {
			t.Fatalf("transport-owned header %q accepted", value)
		}
	}
	for _, value := range []string{"missing-colon", "Bad Header: value", "X-Test: bad\nvalue"} {
		var rejected proxyHeaderFlag
		if err := rejected.Set(value); err == nil || strings.Contains(err.Error(), value) {
			t.Fatalf("invalid header %q error = %v", value, err)
		}
	}
}

func TestParseExecProxyEndpoint(t *testing.T) {
	for _, test := range []struct {
		raw        string
		wantScheme string
		wantAddr   string
	}{
		{raw: "http://proxy.example", wantScheme: "http", wantAddr: "proxy.example:80"},
		{raw: "https://proxy.example/", wantScheme: "https", wantAddr: "proxy.example:443"},
		{raw: "http://127.0.0.1:8080", wantScheme: "http", wantAddr: "127.0.0.1:8080"},
		{raw: "https://[::1]:8443", wantScheme: "https", wantAddr: "[::1]:8443"},
	} {
		endpoint, err := parseExecProxyEndpoint(test.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", test.raw, err)
		}
		if endpoint.scheme != test.wantScheme || endpoint.address != test.wantAddr {
			t.Fatalf("parse %q = %+v, want scheme=%q address=%q", test.raw, endpoint, test.wantScheme, test.wantAddr)
		}
	}

	for _, raw := range []string{
		"proxy.example:8080",
		"ftp://proxy.example",
		"http://user:secret@proxy.example",
		"http://proxy.example/path",
		"http://proxy.example?query=1",
		"http://proxy.example/#fragment",
		"http://proxy.example:0",
		"http://proxy.example:65536",
	} {
		if _, err := parseExecProxyEndpoint(raw); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid URL %q error = %v", raw, err)
		}
	}
}

func TestDialProxyExecPreservesAuthorityHeadersAndBufferedTunnelBytes(t *testing.T) {
	requestSeen := make(chan *http.Request, 1)
	clientBytes := make(chan string, 1)
	serverErr := make(chan error, 1)
	magic := "server-prefetched-bytes"
	server := newExecConnectServer(t, false, func(conn net.Conn, rw *bufio.ReadWriter, req *http.Request) {
		requestSeen <- req.Clone(context.Background())
		if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n" + magic); err != nil {
			serverErr <- err
			return
		}
		if err := rw.Flush(); err != nil {
			serverErr <- err
			return
		}
		body := make([]byte, len("client-tunnel-bytes"))
		if _, err := io.ReadFull(rw, body); err != nil {
			serverErr <- err
			return
		}
		clientBytes <- string(body)
	})
	defer server.Close()

	headers := http.Header{
		"X-Test":              {"first", "second"},
		"Proxy-Authorization": {"Bearer secret-value"},
		"User-Agent":          {"first-agent", "second-agent"},
	}
	conn, err := dialProxyExec(context.Background(), server.URL, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	gotMagic := make([]byte, len(magic))
	if _, err := io.ReadFull(conn, gotMagic); err != nil {
		t.Fatal(err)
	}
	if string(gotMagic) != magic {
		t.Fatalf("prefetched tunnel bytes = %q, want %q", gotMagic, magic)
	}
	if _, err := io.WriteString(conn, "client-tunnel-bytes"); err != nil {
		t.Fatal(err)
	}

	select {
	case req := <-requestSeen:
		if req.Method != http.MethodConnect || req.RequestURI != execProxyAuthority || req.Host != execProxyAuthority {
			t.Fatalf("CONNECT request method=%q URI=%q Host=%q", req.Method, req.RequestURI, req.Host)
		}
		if got := req.Header.Values("X-Test"); len(got) != 2 || got[0] != "first" || got[1] != "second" {
			t.Fatalf("X-Test = %v", got)
		}
		if got := req.Header.Get("Proxy-Authorization"); got != "Bearer secret-value" {
			t.Fatalf("Proxy-Authorization = %q", got)
		}
		if got := req.Header.Values("User-Agent"); len(got) != 2 || got[0] != "first-agent" || got[1] != "second-agent" {
			t.Fatalf("User-Agent = %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not observe CONNECT request")
	}
	select {
	case got := <-clientBytes:
		if got != "client-tunnel-bytes" {
			t.Fatalf("client tunnel bytes = %q", got)
		}
	case err := <-serverErr:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not receive tunnel bytes")
	}
}

func TestDialProxyExecHTTPSUsesTrustedCA(t *testing.T) {
	server := newExecConnectServer(t, true, func(conn net.Conn, rw *bufio.ReadWriter, req *http.Request) {
		_, _ = rw.WriteString("HTTP/1.1 204 No Content\r\n\r\n")
		_ = rw.Flush()
	})
	defer server.Close()

	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	conn, err := dialProxyExecWithTLS(context.Background(), server.URL, nil, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	if _, err := dialProxyExec(context.Background(), server.URL, nil); err == nil {
		t.Fatal("HTTPS proxy with an untrusted test certificate was accepted")
	}
}

func TestDialProxyExecForwardsExistingCtlProtocolWithoutTransportFrame(t *testing.T) {
	requestSeen := make(chan ctl.Request, 1)
	serverErr := make(chan error, 1)
	server := newExecConnectServer(t, false, func(conn net.Conn, rw *bufio.ReadWriter, req *http.Request) {
		if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			serverErr <- err
			return
		}
		if err := rw.Flush(); err != nil {
			serverErr <- err
			return
		}
		var request ctl.Request
		if err := ctl.ReadMessage(rw, &request); err != nil {
			serverErr <- err
			return
		}
		requestSeen <- request
	})
	defer server.Close()

	conn, err := dialProxyExec(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	want := &ctl.Request{Type: ctl.TypeExecRequest, Exec: &proto.ExecSpec{Argv: []string{"/bin/true"}}}
	if err := ctl.WriteMessage(conn, want); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-requestSeen:
		if got.Type != ctl.TypeExecRequest || got.Exec == nil || len(got.Exec.Argv) != 1 || got.Exec.Argv[0] != "/bin/true" {
			t.Fatalf("tunnel request = %+v", got)
		}
	case err := <-serverErr:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not receive ctl request")
	}
}

func TestDialProxyExecOmitsRejectedResponseBodyWhenHeadersArePresent(t *testing.T) {
	const secret = "kat1.secret-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "token="+secret+" "+strings.Repeat("x", execProxyErrorBodySize+1024))
	}))
	defer server.Close()

	_, err := dialProxyExec(context.Background(), server.URL, http.Header{"X-Access-Token": {secret}})
	if err == nil {
		t.Fatal("rejected CONNECT succeeded")
	}
	message := err.Error()
	if !strings.Contains(message, "403") || strings.Contains(message, "token=") ||
		strings.Contains(message, "<redacted>") || strings.Contains(message, "...") ||
		strings.Contains(message, secret) || len(message) > 128 {
		t.Fatalf("rejected CONNECT error = %q", message)
	}
}

func TestDialProxyExecBoundsRejectedResponseBodyWithoutHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "diagnostic "+strings.Repeat("x", execProxyErrorBodySize+1024))
	}))
	defer server.Close()

	_, err := dialProxyExec(context.Background(), server.URL, nil)
	if err == nil {
		t.Fatal("rejected CONNECT succeeded")
	}
	message := err.Error()
	if !strings.Contains(message, "403: diagnostic") || !strings.Contains(message, "...") ||
		len(message) > execProxyErrorBodySize+256 {
		t.Fatalf("bounded rejected CONNECT error = %q", message)
	}
}

func TestDialProxyExecReadsFinalResponseAfterInformationalResponses(t *testing.T) {
	const magic = "tunnel-after-103"
	server := newExecConnectServer(t, false, func(conn net.Conn, rw *bufio.ReadWriter, req *http.Request) {
		_, _ = rw.WriteString("HTTP/1.1 103 Early Hints\r\nLink: </ready>\r\n\r\n")
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n" + magic)
		_ = rw.Flush()
	})
	defer server.Close()

	conn, err := dialProxyExec(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got := make([]byte, len(magic))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != magic {
		t.Fatalf("tunnel bytes = %q, err=%v", got, err)
	}
}

func TestDialProxyExecBoundsRejectedResponseTrailers(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = http.ReadRequest(bufio.NewReader(conn))
		_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nTransfer-Encoding: chunked\r\n\r\n")
		_, _ = io.WriteString(conn, "0\r\nX-Fill: "+strings.Repeat("x", execProxyResponseHeaderSize)+"\r\n\r\n")
	}()

	_, err = dialProxyExec(context.Background(), "http://"+listener.Addr().String(), nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("oversized rejected response trailer error = %v", err)
	}
	<-done
}

func TestDialProxyExecDoesNotDrainRejectedResponseBody(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_, _ = http.ReadRequest(bufio.NewReader(conn))
		_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 1048576\r\n\r\n")
		_, _ = io.WriteString(conn, strings.Repeat("x", execProxyErrorBodySize+1))
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		one := make([]byte, 1)
		_, err = conn.Read(one)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		done <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	_, err = dialProxyExec(ctx, "http://"+listener.Addr().String(), nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("rejected CONNECT error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("rejected CONNECT drained unread body for %s", elapsed)
	}
	if err := <-done; err != nil {
		t.Fatalf("server did not observe prompt connection close: %v", err)
	}
}

func TestDialProxyExecBoundsResponseHeaders(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = http.ReadRequest(bufio.NewReader(conn))
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nX-Fill: "+strings.Repeat("x", execProxyResponseHeaderSize)+"\r\n\r\n")
	}()

	_, err = dialProxyExec(context.Background(), "http://"+listener.Addr().String(), nil)
	if !errors.Is(err, errExecProxyResponseHeaderTooLarge) {
		t.Fatalf("oversized response header error = %v", err)
	}
	<-done
}

func TestDialProxyExecRejectsMalformedResponse(t *testing.T) {
	const secret = "malformed-reflected-secret"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = http.ReadRequest(bufio.NewReader(conn))
		_, _ = io.WriteString(conn, "not-an-http-response "+secret+"\r\n\r\n")
	}()

	_, err = dialProxyExec(context.Background(), "http://"+listener.Addr().String(), http.Header{"X-Access-Token": {secret}})
	if err == nil || !strings.Contains(err.Error(), "read CONNECT response") || strings.Contains(err.Error(), secret) {
		t.Fatalf("malformed response error = %v", err)
	}
	<-done
}

func TestDialProxyExecContextCancellationInterruptsHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = http.ReadRequest(bufio.NewReader(conn))
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := dialProxyExec(ctx, "http://"+listener.Addr().String(), nil)
		result <- err
	}()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy handshake did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled handshake error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled handshake did not return")
	}
	<-done
}

func TestExecCmdProxyDoesNotRequireSandboxIDOrEchoHeaders(t *testing.T) {
	const secret = "secret-proxy-header"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	code, stderr := captureStderr(t, func() int {
		return execCmd([]string{"--proxy", server.URL, "--proxy-header", "X-Access-Token: " + secret, "--", "/bin/true"})
	})
	if code != 1 || strings.Contains(stderr, "--sandbox-id required") || strings.Contains(stderr, secret) {
		t.Fatalf("exec proxy code=%d stderr=%q", code, stderr)
	}

	code, stderr = captureStderr(t, func() int { return execCmd([]string{"--", "/bin/true"}) })
	if code != 2 || !strings.Contains(stderr, "exec: --sandbox-id required\n") {
		t.Fatalf("local exec validation code=%d stderr=%q", code, stderr)
	}

	code, stderr = captureStderr(t, func() int {
		return execCmd([]string{"--sandbox-id", "local", "--proxy-header", "X-Access-Token: " + secret, "--", "/bin/true"})
	})
	if code != 2 || !strings.Contains(stderr, "--proxy-header requires --proxy") || strings.Contains(stderr, secret) {
		t.Fatalf("local proxy-header validation code=%d stderr=%q", code, stderr)
	}

	code, stderr = captureStderr(t, func() int {
		return execCmd([]string{"--proxy", server.URL, "--proxy-header", "Bad Header: " + secret, "--", "/bin/true"})
	})
	if code != 2 || !strings.Contains(stderr, "invalid name") || strings.Contains(stderr, secret) {
		t.Fatalf("invalid proxy-header validation code=%d stderr=%q", code, stderr)
	}
}

func TestExecCmdProxyRunsExistingCtlAndMuxProtocol(t *testing.T) {
	serverErr := make(chan error, 1)
	server := newExecConnectServer(t, false, func(conn net.Conn, rw *bufio.ReadWriter, req *http.Request) {
		if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			serverErr <- err
			return
		}
		if err := rw.Flush(); err != nil {
			serverErr <- err
			return
		}
		var request ctl.Request
		if err := ctl.ReadMessage(rw, &request); err != nil {
			serverErr <- err
			return
		}
		if request.Type != ctl.TypeExecRequest || request.Exec == nil || len(request.Exec.Argv) != 1 || request.Exec.Argv[0] != "/bin/result" {
			serverErr <- errors.New("unexpected exec request")
			return
		}
		if err := ctl.WriteMessage(rw, &ctl.Response{Type: ctl.TypeExecAck, Stdio: &request.Exec.Stdio}); err != nil {
			serverErr <- err
			return
		}
		if err := rw.Flush(); err != nil {
			serverErr <- err
			return
		}
		session := mux.NewSession(conn, mux.StreamSet{}, mux.Options{})
		defer session.Close()
		if err := session.SendExitStatus(37); err != nil {
			serverErr <- err
		}
	})
	defer server.Close()

	code, stderr := captureStderr(t, func() int {
		return execCmd([]string{"--proxy", server.URL, "--stdout=false", "--stderr=false", "--", "/bin/result"})
	})
	if code != 37 || stderr != "" {
		t.Fatalf("remote exec code=%d stderr=%q", code, stderr)
	}
	select {
	case err := <-serverErr:
		t.Fatal(err)
	default:
	}
}

func newExecConnectServer(t *testing.T, useTLS bool, tunnel func(net.Conn, *bufio.ReadWriter, *http.Request)) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		tunnel(conn, rw, req)
	})
	if !useTLS {
		return httptest.NewServer(handler)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	return server
}
