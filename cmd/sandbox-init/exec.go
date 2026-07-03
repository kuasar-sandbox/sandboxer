package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

// The guest side of `sandbox-ctl exec`: run an ad-hoc command inside
// the already-running sandbox as a sibling of the user app, each with
// its own stdio MUX over a dedicated reverse-channel connection.
// Sessions are concurrent (one goroutine per reverse conn).
//
// The command runs INSIDE the app's mount + pid namespaces (it sees the
// app's process tree and filesystem, like `docker exec`). Because
// setns(CLONE_NEWPID) only affects future children and setns(
// CLONE_NEWNS) is per-thread — both unsafe in the long-lived,
// multithreaded sandbox-init process — a short-lived re-exec helper
// ("exec-join", runExecJoin) does the namespace join then ForkExecs the
// command ("exec-child-joined"). The helper proxies the command's exit
// status as its own so the SIGCHLD reaper relays a faithful code.
//
// The helper is itself a Go process, so re-exec alone is not enough:
// the Go runtime spawns its threads with CLONE_FS, and the kernel's
// mntns_install() rejects setns(CLONE_NEWNS) unless the calling
// thread's fs_struct is unshared (fs->users == 1). runExecJoin
// therefore LockOSThreads and unshare(CLONE_FS) for that one thread
// before the join — LockOSThread alone leaves it sharing fs state and
// setns(mnt) fails EINVAL.

// execRegistry brokers child reaping between the single process-wide
// SIGCHLD reaper (phase3Supervise) and the per-session exec goroutines.
// The reaper owns Wait4(-1); it routes each non-app child's WaitStatus
// to the registered session waiter (or stashes it if the session has
// not registered the pid yet — the Start→register window is tiny but
// the child can exit inside it).
//
// In this sandbox model sandbox-init's only direct children are the
// user app and exec children (each CLONE_NEWPID, so a direct child of
// init), so any non-app reaped pid is an exec child.
type execRegistry struct {
	mu        sync.Mutex
	waiters   map[int]chan syscall.WaitStatus
	pending   map[int]syscall.WaitStatus
	live      map[int]struct{}
	quiescing bool
}

func newExecRegistry() *execRegistry {
	return &execRegistry{
		waiters: make(map[int]chan syscall.WaitStatus),
		pending: make(map[int]syscall.WaitStatus),
		live:    make(map[int]struct{}),
	}
}

// register records a freshly-started exec child and returns the channel
// the reaper will deliver its WaitStatus on. If the child was already
// reaped (lost the Start→register race) the status is delivered at once.
func (r *execRegistry) register(pid int) <-chan syscall.WaitStatus {
	ch := make(chan syscall.WaitStatus, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.pending[pid]; ok {
		delete(r.pending, pid)
		ch <- st
		return ch
	}
	r.waiters[pid] = ch
	r.live[pid] = struct{}{}
	return ch
}

// deliver routes a reaped non-app pid's status to its session. Returns
// true (always, in this model — see type doc) so the reaper treats it
// as handled and does not reboot.
func (r *execRegistry) deliver(pid int, st syscall.WaitStatus) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, pid)
	if ch, ok := r.waiters[pid]; ok {
		delete(r.waiters, pid)
		ch <- st
		return true
	}
	r.pending[pid] = st
	return true
}

// done drops bookkeeping for a finished session (idempotent).
func (r *execRegistry) done(pid int) {
	r.mu.Lock()
	delete(r.waiters, pid)
	delete(r.live, pid)
	delete(r.pending, pid)
	r.mu.Unlock()
}

func (r *execRegistry) isQuiescing() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.quiescing
}

// beginQuiesce marks the sandbox quiescing (new exec rejected) and
// returns the in-flight exec child pids to SIGKILL so the snapshot
// captures no running exec children.
func (r *execRegistry) beginQuiesce() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quiescing = true
	pids := make([]int, 0, len(r.live))
	for pid := range r.live {
		pids = append(pids, pid)
	}
	return pids
}

func (r *execRegistry) endQuiesce() {
	r.mu.Lock()
	r.quiescing = false
	r.mu.Unlock()
}

// killExecChildren SIGKILLs every in-flight exec child. Called by the
// quiesce handler; sessions tear down once the reaper delivers.
func killExecChildren(reg *execRegistry) {
	for _, pid := range reg.beginQuiesce() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// execBridge is the per-session stdio bridge. Unlike consoleBridge it
// has no session-swap machinery: an exec session has exactly one MUX
// for its whole life. It reuses setupAppStdio's fd setup (via the
// returned consoleBridge's stable ends).
type execBridge struct {
	tty       bool
	ptyMaster *os.File
	stdinW    *os.File
	stdoutR   *os.File
	stderrR   *os.File
	wg        sync.WaitGroup // guest→host pumps (the ones that EOF the stream)
}

func (b *execBridge) onSetWinsize(cols, rows uint16) {
	if b.tty && b.ptyMaster != nil {
		_ = setWinsize(int(b.ptyMaster.Fd()), cols, rows)
	}
}

// pump wires the child fds to sess's streams. guest→host copies EOF
// their stream when the child closes its fd (child exit), so the host
// sees a clean end-of-stream before the exit status + MUX close.
func (b *execBridge) pump(sess *mux.Session) {
	guestToHost := func(id uint8, src *os.File) {
		defer b.wg.Done()
		st := sess.Stream(id)
		_, _ = io.Copy(st, src)
		_ = st.CloseWrite()
	}
	if b.tty {
		b.wg.Add(1)
		go guestToHost(mux.StreamPTY, b.ptyMaster)
		go func() { _, _ = io.Copy(b.ptyMaster, sess.Stream(mux.StreamPTY)) }()
		return
	}
	if b.stdinW != nil {
		go func() {
			_, _ = io.Copy(b.stdinW, sess.Stream(mux.StreamStdin))
			_ = b.stdinW.Close()
		}()
	}
	if b.stdoutR != nil {
		b.wg.Add(1)
		go guestToHost(mux.StreamStdout, b.stdoutR)
	}
	if b.stderrR != nil {
		b.wg.Add(1)
		go guestToHost(mux.StreamStderr, b.stderrR)
	}
}

// drain waits (bounded) for the guest→host pumps to flush the child's
// final output, then closes the sandbox-init-held fds.
func (b *execBridge) drain() {
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	if b.ptyMaster != nil {
		_ = b.ptyMaster.Close()
	}
	if b.stdinW != nil {
		_ = b.stdinW.Close()
	}
	if b.stdoutR != nil {
		_ = b.stdoutR.Close()
	}
	if b.stderrR != nil {
		_ = b.stderrR.Close()
	}
}

// runExecSession services one proto.TypeExec reverse-channel conn end
// to end: set up stdio, fork the command, exec_ack, run the MUX, wait
// for the child, report its exit code, then close the MUX. It owns c
// (closes it). Blocks until the session ends.
func runExecSession(c *vsockConn, req *proto.Message, sup *supervisorState) {
	defer c.Close()
	reg := sup.execReg

	fail := func(msg string) {
		_ = proto.WriteMessage(c, &proto.Message{Type: proto.TypeError, Msg: msg})
	}

	spec := req.Exec
	if spec == nil || len(spec.Argv) == 0 {
		fail("exec: empty argv")
		return
	}
	if reg.isQuiescing() {
		fail("exec: sandbox quiescing (snapshot in progress)")
		return
	}

	cs, cb, err := setupAppStdio(spec.Stdio)
	if err != nil {
		fail("exec: setup stdio: " + err.Error())
		return
	}
	eb := &execBridge{
		tty:       cb.tty,
		ptyMaster: cb.ptyMaster,
		stdinW:    cb.stdinW,
		stdoutR:   cb.stdoutR,
		stderrR:   cb.stderrR,
	}

	pid, err := forkExecChild(spec, cs, int(sup.appPid.Load()))
	if err != nil {
		eb.drain()
		fail("exec: fork: " + err.Error())
		return
	}
	waitCh := reg.register(pid)
	defer reg.done(pid)

	established := spec.Stdio
	if err := proto.WriteMessage(c, &proto.Message{Type: proto.TypeExecAck, Stdio: &established}); err != nil {
		logf("exec: write exec_ack pid=%d: %v", pid, err)
		_ = syscall.Kill(pid, syscall.SIGKILL)
		<-waitCh
		eb.drain()
		return
	}

	_ = c.SetDeadline(time.Time{})
	sess := mux.NewSession(c, streamSetFor(established), mux.Options{OnSetWinsize: eb.onSetWinsize})
	eb.pump(sess)

	// If the host disconnects before the child exits (CLI Ctrl-C / lost
	// connection), kill the child so it can't outlive its session.
	var st syscall.WaitStatus
	select {
	case st = <-waitCh:
	case <-sess.Done():
		logf("exec: session conn lost (pid=%d) — killing child", pid)
		_ = syscall.Kill(pid, syscall.SIGKILL)
		st = <-waitCh
	}

	code := exitCodeFromStatus(st)
	eb.drain() // flush + close child ends; guest→host streams EOF
	if err := sess.SendExitStatus(code); err != nil {
		logf("exec: send exit status pid=%d: %v", pid, err)
	}
	done := make(chan struct{})
	go func() { _ = sess.InitMuxClose(); close(done) }()
	select {
	case <-done:
	case <-time.After(muxCloseTimeout):
		logf("exec: MUX_CLOSE_ACK timed out (pid=%d) — forcing conn close", pid)
	}
	_ = sess.Close()
	logf("exec: session done pid=%d code=%d", pid, code)
}

// forkExecChild spawns the nsenter helper (re-exec "exec-join"), which
// joins the app (appPid)'s mount + pid namespaces and then ForkExecs
// the command. The returned pid is the HELPER's — it is what the
// SIGCHLD reaper tracks and what quiesce/lost-session kills; the
// command dies with the helper via PR_SET_PDEATHSIG armed by the
// joined child itself (see runExecJoin / runExecChild).
func forkExecChild(spec *proto.ExecSpec, cs childStdio, appPid int) (int, error) {
	self := "/proc/self/exe"
	ttyArg := "0"
	if cs.tty {
		ttyArg = "1"
	}
	userArg := spec.User
	if userArg == "" {
		userArg = "-"
	}
	args := append([]string{self, "exec-join", strconv.Itoa(appPid), ttyArg, userArg, spec.Cwd, spec.Argv[0]}, spec.Argv[1:]...)

	cmd := exec.Cmd{
		Path:   self,
		Args:   args,
		Env:    execEnv(spec.Env),
		Dir:    "/",
		Stdin:  cs.stdin,
		Stdout: cs.stdout,
		Stderr: cs.stderr,
		// No Cloneflags / Setsid here: the helper joins the app's
		// namespaces and the COMMAND (forked by the helper) gets the
		// new session + controlling tty (see runExecJoin).
		SysProcAttr: &syscall.SysProcAttr{},
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// Drop our copies of the child ends so EOF propagates once the
	// command closes its fds (the execBridge keeps the opposite ends).
	_ = cs.stdin.Close()
	if cs.stdout != cs.stdin {
		_ = cs.stdout.Close()
	}
	if cs.stderr != cs.stdin && cs.stderr != cs.stdout {
		_ = cs.stderr.Close()
	}
	return cmd.Process.Pid, nil
}

// runExecJoin is the re-exec entrypoint "exec-join": the nsenter
// helper. It enters the app's mount + pid namespaces, then ForkExecs
// the command ("exec-child-joined") so the command runs as a sibling
// of the user app inside its namespaces, and proxies the command's
// exit status as its own (so the reaper relays a faithful code).
//
// The command's lifetime is tied to this helper via PR_SET_PDEATHSIG=
// SIGKILL — but armed by the joined child itself (runExecChild), NOT
// via syscall.SysProcAttr.Pdeathsig here. Go's ForkExec runs a child-
// side "is my parent already dead?" self-check that compares getppid()
// against the parent pid captured before clone; because the child is
// placed in the app's PID namespace (setns CLONE_NEWPID below), its
// getppid() resolves in that ns where the helper is invisible (→ 0),
// never equals the helper's init-ns pid, so the check would SIGKILL
// the command at startup every time. Arming pdeathsig from the child
// after it is in the new ns avoids that bogus check; the kernel tracks
// the real parent task regardless of pid-ns visibility, and
// PR_SET_PDEATHSIG survives a normal (non-setuid) execve.
func runExecJoin(appPidStr, ttyStr, userSpec, cwd, argv0 string, args []string) {
	// setns(CLONE_NEWNS) is per-thread; pin this goroutine so the join
	// and the subsequent fork happen on the same (joined) thread.
	runtime.LockOSThread()
	// The kernel's mntns_install() rejects setns(CLONE_NEWNS) unless the
	// calling thread's fs_struct is unshared (fs->users == 1). Go's
	// runtime threads are clone()d with CLONE_FS, so LockOSThread alone
	// still leaves this thread sharing root/cwd/umask with its siblings
	// → setns(mnt) fails EINVAL. Unshare CLONE_FS for just this locked
	// thread first; the fs values are preserved (private copy), only the
	// sharing is broken. Must precede the setns below.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		die("exec-join: unshare fs: %v", err)
	}

	appPid, err := strconv.Atoi(appPidStr)
	if err != nil {
		die("exec-join: bad app pid %q: %v", appPidStr, err)
	}
	// Open the ns handles while still in the initial mount ns (where
	// /proc/<appPid> resolves) — before joining the app's mount ns.
	mntFD, err := unix.Open(fmt.Sprintf("/proc/%d/ns/mnt", appPid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		die("exec-join: open app mnt ns: %v", err)
	}
	pidFD, err := unix.Open(fmt.Sprintf("/proc/%d/ns/pid", appPid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		die("exec-join: open app pid ns: %v", err)
	}
	if err := unix.Setns(mntFD, unix.CLONE_NEWNS); err != nil {
		die("exec-join: setns mnt: %v", err)
	}
	// setns(CLONE_NEWPID) does not move us; it places our future
	// children (the ForkExec below) into the app's pid namespace.
	if err := unix.Setns(pidFD, unix.CLONE_NEWPID); err != nil {
		die("exec-join: setns pid: %v", err)
	}
	_ = unix.Close(mntFD)
	_ = unix.Close(pidFD)

	// No Pdeathsig here: Go's ForkExec child self-check (getppid vs
	// pre-clone parent pid) misfires across setns(CLONE_NEWPID) and
	// SIGKILLs the command at startup. The joined child arms
	// PR_SET_PDEATHSIG itself instead (runExecChild).
	sys := &syscall.SysProcAttr{}
	if ttyStr == "1" {
		sys.Setsid = true
		sys.Setctty = true // Ctty defaults to 0 = the pty slave
	}
	// Resolve the run-as identity AFTER the namespace joins, against the
	// APP rootfs's /etc/passwd (names like "user" mean the image's user).
	credStr := "-"
	if userSpec != "" && userSpec != "-" {
		c, err := resolveCred(userSpec)
		if err != nil {
			die("exec-join: resolve user %q: %v", userSpec, err)
		}
		credStr = c.encode()
	}
	self := "/proc/self/exe"
	gargv := append([]string{self, "exec-child-joined", credStr, cwd, argv0}, args...)
	pid, err := syscall.ForkExec(self, gargv, &syscall.ProcAttr{
		Dir:   "/",
		Env:   os.Environ(), // = execEnv(spec.Env), inherited via our exec
		Files: []uintptr{0, 1, 2},
		Sys:   sys,
	})
	if err != nil {
		die("exec-join: fork command: %v", err)
	}

	var ws syscall.WaitStatus
	for {
		_, werr := syscall.Wait4(pid, &ws, 0, nil)
		if werr == syscall.EINTR {
			continue
		}
		if werr != nil {
			die("exec-join: wait command: %v", werr)
		}
		break
	}
	if ws.Signaled() {
		os.Exit(128 + int(ws.Signal()))
	}
	os.Exit(ws.ExitStatus())
}

// execEnv builds the child environment: a default PATH baseline that
// the caller's --env can override, plus any extra keys. Unlike the
// user app (whose env is the merged image config), an exec command
// only carries the operator's explicit --env, so a sane PATH must
// always be present or bare argv[0] lookups (`ls`, `sh`) would fail.
func execEnv(extra map[string]string) []string {
	const defaultPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	m := map[string]string{"PATH": defaultPath[len("PATH="):]}
	for k, v := range extra {
		m[k] = v
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// exitCodeFromStatus maps a WaitStatus to the shell 128+signo
// convention (matches handleAppExit).
func exitCodeFromStatus(st syscall.WaitStatus) int {
	if st.Signaled() {
		return 128 + int(st.Signal())
	}
	return st.ExitStatus()
}
