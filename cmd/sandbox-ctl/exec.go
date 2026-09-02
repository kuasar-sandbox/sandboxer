package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
)

// envFlag collects repeatable --env KEY=VALUE pairs.
type envFlag []string

func (e *envFlag) String() string { return strings.Join(*e, ",") }
func (e *envFlag) Set(v string) error {
	if !strings.Contains(v, "=") {
		return fmt.Errorf("--env must be KEY=VALUE (got %q)", v)
	}
	*e = append(*e, v)
	return nil
}

// execCmd implements `sandbox-ctl exec`: run an ad-hoc command inside
// an already-running sandbox as a sibling of the user app. It shares
// the run command's stdio model (--tty / --stdin / --stdout / --stderr
// and their -from/-to variants); the command + args follow `--`.
//
// The exec request travels over the target sandbox's ctl.sock to its
// `sandbox-ctl run` process, which opens a fresh guest exec session and
// pipes its stdio MUX back here. Exit status mirrors the guest process
// (128+signo if it was signalled).
//
// Stdio model: docs/sandbox.md §2.2.
func execCmd(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	sandboxID := fs.String("sandbox-id", "", "target sandbox id (local path fallback when --path-id is omitted)")
	pathID := fs.String("path-id", "", "local run-root directory leaf (takes precedence over --sandbox-id)")
	runRoot := fs.String("run-root", "", "tmpfs run root (overrides SANDBOX_RUN_ROOT env; default /run/sandbox)")
	proxyURL := fs.String("proxy", "", "HTTP/HTTPS CONNECT endpoint (remote mode)")
	var proxyHeaders proxyHeaderFlag
	proxyHeaderArgs := redactedProxyHeaderValue{headers: &proxyHeaders}
	fs.Var(&proxyHeaderArgs, "proxy-header", "CONNECT header 'Name: value' (repeatable; remote mode)")
	cwd := fs.String("cwd", "", "working directory inside the sandbox (default: guest root)")
	user := fs.String("user", "", "run-as identity: \"uid[:gid]\" or a user name from the sandbox's /etc/passwd (default: root)")
	var env envFlag
	fs.Var(&env, "env", "environment variable KEY=VALUE (repeatable)")

	// Same stdio model as `run` (docs/sandbox.md §2.2). No --console:
	// the guest kernel dmesg belongs to the run process, not exec.
	stdinFlag := fs.Bool("stdin", false, "pipe mode: connect command stdin to exec's stdin (default off → /dev/null)")
	stdoutFlag := fs.Bool("stdout", true, "pipe mode: command stdout → exec stdout (default on; --stdout=false discards)")
	stderrFlag := fs.Bool("stderr", true, "pipe mode: command stderr → exec stderr (default on; --stderr=false discards)")
	stdinFrom := fs.String("stdin-from", "", "pipe mode: command stdin reads from FILE (implies --stdin)")
	stdoutTo := fs.String("stdout-to", "", "pipe mode: command stdout → FILE or journald=TAG (implies --stdout)")
	stderrTo := fs.String("stderr-to", "", "pipe mode: command stderr → FILE or journald=TAG (implies --stderr)")
	ttyFlag := fs.Bool("tty", false, "give the command a pty + put our terminal in raw mode (default: auto = on iff stdin&stdout are terminals; mutually exclusive with --stdin/--stdout/--stderr/--*-from/--*-to)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := proxyHeaderArgs.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "exec: %v\n", err)
		return 2
	}
	connectHeaders := proxyHeaders.Header()
	targetPathID := ""
	if *proxyURL == "" {
		if *sandboxID == "" && *pathID == "" {
			fmt.Fprintln(os.Stderr, "exec: --sandbox-id or --path-id is required")
			return 2
		}
		var err error
		targetPathID, err = resolveTargetPathID(*sandboxID, *pathID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "exec: target path: %v\n", err)
			return 2
		}
	}
	if *proxyURL == "" && len(connectHeaders) != 0 {
		fmt.Fprintln(os.Stderr, "exec: --proxy-header requires --proxy")
		return 2
	}
	command := fs.Args()
	if len(command) == 0 {
		if *proxyURL == "" {
			fmt.Fprintln(os.Stderr, "exec: missing command (use: sandbox-ctl exec [--sandbox-id <sid>] [--path-id <leaf>] [flags] -- CMD [ARGS...])")
		} else {
			fmt.Fprintln(os.Stderr, "exec: missing command (use: sandbox-ctl exec --proxy <url> [flags] -- CMD [ARGS...])")
		}
		return 2
	}

	// Tri-state bool flags: "not set" must differ from "set false".
	var stdinSet, stdoutSet, stderrSet, ttySet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "stdin":
			stdinSet = true
		case "stdout":
			stdoutSet = true
		case "stderr":
			stderrSet = true
		case "tty":
			ttySet = true
		}
	})
	tri := func(set bool, v *bool) *bool {
		if !set {
			return nil
		}
		x := *v
		return &x
	}
	stdioMode, err := stdio.FromFlags(
		tri(stdinSet, stdinFlag), tri(stdoutSet, stdoutFlag), tri(stderrSet, stderrFlag),
		*stdinFrom, *stdoutTo, *stderrTo, tri(ttySet, ttyFlag), "off")
	if err != nil {
		fmt.Fprintf(os.Stderr, "exec: %v\n", err)
		return 2
	}

	rd := *runRoot
	if rd == "" {
		rd = os.Getenv("SANDBOX_RUN_ROOT")
	}
	if rd == "" {
		rd = "/run/sandbox"
	}

	envMap := make(map[string]string, len(env))
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		envMap[kv[:i]] = kv[i+1:]
	}
	spec := proto.ExecSpec{
		Argv:  command,
		Env:   envMap,
		Cwd:   *cwd,
		User:  *user,
		Stdio: stdioMode.ProtoSpec(),
	}
	if cols, rows, ok := stdioMode.InitialWinsize(); ok {
		spec.Stdio.Winsize = &proto.Winsize{Cols: cols, Rows: rows}
	}

	ctlSock := ""
	var dial func(context.Context) (net.Conn, error)
	if *proxyURL != "" {
		dial = func(ctx context.Context) (net.Conn, error) {
			return dialProxyExec(ctx, *proxyURL, connectHeaders)
		}
	} else {
		ctlSock = filepath.Join(rd, targetPathID, "ctl.sock")
		dial = func(ctx context.Context) (net.Conn, error) {
			return dialLocalExec(ctx, ctlSock)
		}
	}
	dialSignals := make(chan os.Signal, 1)
	signal.Notify(dialSignals, syscall.SIGINT, syscall.SIGTERM)
	c, err := dialExecInterruptible(dial, dialSignals, func() { signal.Stop(dialSignals) })
	if err != nil {
		if ctlSock != "" {
			fmt.Fprintf(os.Stderr, "exec: dial %s: %v\n", ctlSock, err)
		} else {
			fmt.Fprintf(os.Stderr, "exec: proxy: %v\n", err)
		}
		return 1
	}
	defer c.Close()
	return runExecOverConnWithRemoteRedaction(
		c, spec, stdioMode, *proxyURL != "" && hasExecProxyHeaderValue(connectHeaders),
	)
}

// dialExecInterruptible cancels an in-flight dial on SIGINT/SIGTERM and also
// observes a signal delivered in the narrow interval after dial returns but
// before signal delivery is stopped. stopSignals must synchronously prevent
// further sends to signals before it returns, as signal.Stop does.
func dialExecInterruptible(
	dial func(context.Context) (net.Conn, error),
	signals <-chan os.Signal,
	stopSignals func(),
) (net.Conn, error) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	dialFinished := make(chan struct{})
	signalHandled := make(chan struct{})
	go func() {
		defer close(signalHandled)
		select {
		case sig := <-signals:
			if sig != nil {
				cancel(fmt.Errorf("%s signal received", sig))
			}
		case <-dialFinished:
		}
	}()

	conn, err := dial(ctx)
	close(dialFinished)
	if stopSignals != nil {
		stopSignals()
	}
	<-signalHandled
	cause := context.Cause(ctx)
	if cause == nil {
		select {
		case sig := <-signals:
			if sig != nil {
				cause = fmt.Errorf("%s signal received", sig)
			}
		default:
		}
	}
	if cause != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, cause
	}
	return conn, err
}

func dialLocalExec(ctx context.Context, ctlSock string) (net.Conn, error) {
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", ctlSock)
}

func runExecOverConn(c net.Conn, spec proto.ExecSpec, stdioMode stdio.Mode) int {
	return runExecOverConnWithRemoteRedaction(c, spec, stdioMode, false)
}

func runExecOverConnWithRemoteRedaction(c net.Conn, spec proto.ExecSpec, stdioMode stdio.Mode, redactHandshake bool) int {
	if err := ctl.WriteMessage(c, &ctl.Request{Type: ctl.TypeExecRequest, Exec: &spec}); err != nil {
		fmt.Fprintf(os.Stderr, "exec: send request: %v\n", err)
		return 1
	}
	var resp ctl.Response
	if err := ctl.ReadMessage(c, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "exec: recv response: %v\n", err)
		return 1
	}
	if resp.Type == ctl.TypeError {
		if redactHandshake {
			fmt.Fprintln(os.Stderr, "exec: remote exec rejected")
		} else {
			fmt.Fprintf(os.Stderr, "exec: %s\n", resp.Msg)
		}
		return 1
	}
	if resp.Type != ctl.TypeExecAck || resp.Stdio == nil {
		if redactHandshake {
			fmt.Fprintln(os.Stderr, "exec: unexpected remote response")
		} else {
			fmt.Fprintf(os.Stderr, "exec: unexpected response %q\n", resp.Type)
		}
		return 1
	}

	// The conn now speaks the stdio MUX end-to-end with the guest.
	established := *resp.Stdio
	ss := stdio.StreamSetFor(established)
	sess := mux.NewSession(c, ss, mux.Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := stdioMode.Bridge(ctx, sess, ss)
	if err != nil {
		_ = sess.Close()
		fmt.Fprintf(os.Stderr, "exec: bridge stdio: %v\n", err)
		return 1
	}

	// SIGINT/SIGTERM: in tty mode the host terminal is raw (ISIG off) so
	// ^C is delivered as a pty byte to the guest line discipline; this
	// handler only fires in pipe mode, where it tears the session down
	// (the guest then SIGKILLs the command).
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			_ = sess.Close()
		case <-sess.Done():
		}
	}()

	// The guest sends EXIT_STATUS after every stdout/stderr/pty EOF, then
	// immediately performs the guest-initiated MUX_CLOSE handshake. Do not
	// return in between those two control frames: closing here would prevent
	// this host-side Session from ACKing MUX_CLOSE and would let a following
	// short exec overlap the previous vsock teardown. The MUX_CLOSE handler
	// writes the ACK and closes our conn itself, so Done is a local protocol
	// barrier; it does not wait for CH to propagate a remote orderly close.
	select {
	case <-sess.ExitReceived():
		<-sess.Done()
	case <-sess.Done():
	}
	signal.Stop(sigCh)
	cancel()
	cleanup()        // drains stdout/stderr/pty pumps + restores the terminal
	_ = sess.Close() // tear our side down; don't wait for CH to surface the guest close

	code, ok := sess.ExitStatus()
	if !ok {
		fmt.Fprintln(os.Stderr, "exec: connection lost before the command reported an exit status")
		return 1
	}
	return code
}
