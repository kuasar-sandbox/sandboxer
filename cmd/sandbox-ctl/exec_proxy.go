package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/http/httpguts"
)

const (
	execProxyAuthority          = "sandbox:443"
	execProxyDialTimeout        = 5 * time.Second
	execProxyHandshakeTimeout   = 10 * time.Second
	execProxyResponseHeaderSize = 64 << 10
	execProxyErrorBodySize      = 4 << 10
)

var errExecProxyResponseHeaderTooLarge = errors.New("CONNECT response header too large")

var forbiddenProxyHeaders = map[string]struct{}{
	"host":              {},
	"connection":        {},
	"proxy-connection":  {},
	"content-length":    {},
	"transfer-encoding": {},
	"trailer":           {},
}

// proxyHeaderFlag collects repeatable CONNECT headers without ever rendering
// their values back through flag diagnostics.
type proxyHeaderFlag struct {
	header http.Header
}

// redactedProxyHeaderValue prevents flag.FlagSet from quoting a rejected raw
// argument (which may contain a bearer credential) in its parse error. The
// underlying parser keeps its normal error contract for direct callers.
type redactedProxyHeaderValue struct {
	headers *proxyHeaderFlag
	err     error
}

func (v *redactedProxyHeaderValue) String() string {
	if v == nil || v.headers == nil {
		return ""
	}
	return v.headers.String()
}

func (v *redactedProxyHeaderValue) Set(raw string) error {
	if v.err == nil {
		v.err = v.headers.Set(raw)
	}
	return nil
}

func (v *redactedProxyHeaderValue) Err() error {
	if v == nil {
		return nil
	}
	return v.err
}

func (h *proxyHeaderFlag) String() string {
	if h == nil || len(h.header) == 0 {
		return ""
	}
	return "<redacted>"
}

func (h *proxyHeaderFlag) Set(raw string) error {
	name, value, ok := strings.Cut(raw, ":")
	if !ok {
		return errors.New("--proxy-header must be Name: value")
	}
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if !httpguts.ValidHeaderFieldName(name) {
		return errors.New("--proxy-header has an invalid name")
	}
	if _, forbidden := forbiddenProxyHeaders[strings.ToLower(name)]; forbidden {
		return fmt.Errorf("--proxy-header %s is transport-owned", name)
	}
	if !httpguts.ValidHeaderFieldValue(value) {
		return fmt.Errorf("--proxy-header %s has an invalid value", name)
	}
	if h.header == nil {
		h.header = make(http.Header)
	}
	h.header.Add(name, value)
	return nil
}

func (h *proxyHeaderFlag) Header() http.Header {
	if h == nil {
		return nil
	}
	return h.header.Clone()
}

type execProxyEndpoint struct {
	scheme     string
	address    string
	serverName string
}

func parseExecProxyEndpoint(raw string) (*execProxyEndpoint, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Opaque != "" || u.User != nil || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("invalid proxy URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, errors.New("proxy URL scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return nil, errors.New("proxy URL host is required")
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	} else {
		p, err := strconv.Atoi(port)
		if err != nil || p <= 0 || p > 65535 {
			return nil, errors.New("proxy URL port is invalid")
		}
	}
	return &execProxyEndpoint{
		scheme:     scheme,
		address:    net.JoinHostPort(host, port),
		serverName: host,
	}, nil
}

func validateExecProxyHeaders(headers http.Header) error {
	for name, values := range headers {
		if !httpguts.ValidHeaderFieldName(name) {
			return errors.New("proxy header has an invalid name")
		}
		if _, forbidden := forbiddenProxyHeaders[strings.ToLower(name)]; forbidden {
			return fmt.Errorf("proxy header %s is transport-owned", name)
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return fmt.Errorf("proxy header %s has an invalid value", name)
			}
		}
	}
	return nil
}

func dialProxyExec(ctx context.Context, rawURL string, headers http.Header) (net.Conn, error) {
	return dialProxyExecWithTLS(ctx, rawURL, headers, nil)
}

func dialProxyExecWithTLS(ctx context.Context, rawURL string, headers http.Header, tlsConfig *tls.Config) (net.Conn, error) {
	endpoint, err := parseExecProxyEndpoint(rawURL)
	if err != nil {
		return nil, err
	}
	if err := validateExecProxyHeaders(headers); err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: execProxyDialTimeout, KeepAlive: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", endpoint.address)
	if err != nil {
		return nil, fmt.Errorf("connect proxy endpoint: %w", err)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = conn.Close()
		}
	}()
	transportConn := conn
	stopCancelClose := context.AfterFunc(ctx, func() { _ = transportConn.Close() })
	defer stopCancelClose()

	deadline := time.Now().Add(execProxyHandshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set proxy handshake deadline: %w", err)
	}

	if endpoint.scheme == "https" {
		config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: endpoint.serverName}
		if tlsConfig != nil {
			config = tlsConfig.Clone()
			if config.ServerName == "" {
				config.ServerName = endpoint.serverName
			}
			if config.MinVersion < tls.VersionTLS12 {
				config.MinVersion = tls.VersionTLS12
			}
		}
		tlsConn := tls.Client(conn, config)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("proxy TLS handshake: %w", err)
		}
		conn = tlsConn
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: execProxyAuthority},
		Host:   execProxyAuthority,
		Header: headers.Clone(),
	}
	if err := writeExecProxyConnect(conn, req.Header); err != nil {
		return nil, fmt.Errorf("write CONNECT request: %w", err)
	}

	handshakeReader := &execProxyHandshakeReader{r: conn, remaining: execProxyResponseHeaderSize, limited: true}
	buffered := bufio.NewReader(handshakeReader)
	var (
		resp       *http.Response
		statusCode int
	)
	for {
		var head *execProxyResponseHead
		head, err = readExecProxyResponseHead(buffered)
		if err != nil {
			return nil, classifyExecProxyResponseError(ctx, handshakeReader, err)
		}
		statusCode = head.statusCode
		if statusCode >= 100 && statusCode < 200 && statusCode != http.StatusSwitchingProtocols {
			continue
		}
		if statusCode < 200 || statusCode >= 300 {
			resp, err = head.readResponse(buffered, req)
			if err != nil {
				return nil, classifyExecProxyResponseError(ctx, handshakeReader, err)
			}
			statusCode = resp.StatusCode
		}
		if statusCode < 100 || statusCode >= 200 || statusCode == http.StatusSwitchingProtocols {
			break
		}
	}
	if statusCode < 200 || statusCode >= 300 {
		if hasExecProxyHeaderValue(headers) || statusCode < 200 {
			return nil, fmt.Errorf("proxy CONNECT rejected: HTTP %d", statusCode)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, execProxyErrorBodySize+1))
		if readErr != nil {
			if errors.Is(readErr, errExecProxyResponseHeaderTooLarge) || handshakeReader.remaining <= 0 {
				return nil, fmt.Errorf("read CONNECT response: %w", errExecProxyResponseHeaderTooLarge)
			}
			return nil, fmt.Errorf("proxy CONNECT rejected: HTTP %d", statusCode)
		}
		message := sanitizeExecProxyErrorBody(body, headers)
		if message == "" {
			return nil, fmt.Errorf("proxy CONNECT rejected: HTTP %d", statusCode)
		}
		return nil, fmt.Errorf("proxy CONNECT rejected: HTTP %d: %s", statusCode, message)
	}
	handshakeReader.limited = false
	stopCancelClose()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear proxy handshake deadline: %w", err)
	}

	succeeded = true
	return &execProxyConn{Conn: conn, reader: buffered}, nil
}

type execProxyResponseHead struct {
	proto      string
	status     string
	statusCode int
	header     http.Header
}

// readExecProxyResponseHead stops at the empty line so that a successful
// CONNECT response cannot interpret tunnel bytes through HTTP body framing.
func readExecProxyResponseHead(r *bufio.Reader) (*execProxyResponseHead, error) {
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil {
		return nil, err
	}
	proto, status, ok := strings.Cut(line, " ")
	if !ok {
		return nil, errors.New("malformed HTTP response")
	}
	status = strings.TrimLeft(status, " ")
	statusText, _, _ := strings.Cut(status, " ")
	if len(statusText) != 3 {
		return nil, errors.New("malformed HTTP status code")
	}
	statusCode, err := strconv.Atoi(statusText)
	if err != nil || statusCode < 0 {
		return nil, errors.New("malformed HTTP status code")
	}
	if _, _, ok := http.ParseHTTPVersion(proto); !ok {
		return nil, errors.New("malformed HTTP version")
	}
	header, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	return &execProxyResponseHead{
		proto:      proto,
		status:     status,
		statusCode: statusCode,
		header:     http.Header(header),
	}, nil
}

// readResponse reparses a rejected response with net/http's strict transfer
// framing so its bounded diagnostic body can be consumed safely.
func (h *execProxyResponseHead) readResponse(r *bufio.Reader, req *http.Request) (*http.Response, error) {
	var raw strings.Builder
	fmt.Fprintf(&raw, "%s %s\r\n", h.proto, h.status)
	if err := h.header.Write(&raw); err != nil {
		return nil, err
	}
	raw.WriteString("\r\n")
	return http.ReadResponse(bufio.NewReader(io.MultiReader(strings.NewReader(raw.String()), r)), req)
}

func classifyExecProxyResponseError(ctx context.Context, r *execProxyHandshakeReader, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, errExecProxyResponseHeaderTooLarge) || r.remaining <= 0 {
		return fmt.Errorf("read CONNECT response: %w", errExecProxyResponseHeaderTooLarge)
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return errors.New("read CONNECT response: timeout")
	}
	return errors.New("read CONNECT response: malformed or incomplete response")
}

// writeExecProxyConnect serializes only the validated, caller-provided Header
// values. http.Request.Write special-cases User-Agent and would otherwise
// collapse repeated opaque values.
func writeExecProxyConnect(w io.Writer, headers http.Header) error {
	var request strings.Builder
	fmt.Fprintf(&request, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", execProxyAuthority, execProxyAuthority)
	if err := headers.Write(&request); err != nil {
		return err
	}
	request.WriteString("\r\n")
	_, err := io.WriteString(w, request.String())
	return err
}

type execProxyHandshakeReader struct {
	r         io.Reader
	remaining int64
	limited   bool
}

func (r *execProxyHandshakeReader) Read(p []byte) (int, error) {
	if !r.limited {
		return r.r.Read(p)
	}
	if r.remaining <= 0 {
		return 0, errExecProxyResponseHeaderTooLarge
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.r.Read(p)
	r.remaining -= int64(n)
	return n, err
}

type execProxyConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *execProxyConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *execProxyConn) CloseRead() error {
	if closer, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return closer.CloseRead()
	}
	return nil
}

func (c *execProxyConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

func sanitizeExecProxyErrorBody(body []byte, headers http.Header) string {
	if hasExecProxyHeaderValue(headers) {
		return ""
	}
	truncated := len(body) > execProxyErrorBodySize
	if truncated {
		body = body[:execProxyErrorBodySize]
	}
	message := strings.ToValidUTF8(string(body), "?")
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r' {
			return '?'
		}
		return r
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	if truncated {
		message += " ..."
	}
	return message
}

func hasExecProxyHeaderValue(headers http.Header) bool {
	for _, values := range headers {
		for _, value := range values {
			if value != "" {
				return true
			}
		}
	}
	return false
}
