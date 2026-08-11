package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	childExtraFDBase       = 3
	childCgroupNamespaceFD = childExtraFDBase
	childMountNamespaceFD  = childExtraFDBase
	childBootstrapSyncFD   = childExtraFDBase
	childJoinedSyncFD      = childExtraFDBase + 1

	childMsgStart = "start"
	childMsgReady = "ready"
	childMsgGo    = "go"
	childMsgAbort = "abort"
	childMsgExec  = "exec"
	childMsgError = "error:"
)

// childSync is a private SOCK_STREAM protocol between sandbox-init and a
// short-lived re-exec helper. The helper blocks before namespace setup until
// its pid has been registered with the sole SIGCHLD reaper, then blocks again
// before final exec while the parent pins/verifies the initial namespace.
// CLOEXEC turns EOF after "go" into confirmation that final execve succeeded.
type childSync struct {
	file   *os.File
	reader *bufio.Reader
}

func newChildSyncPair() (parent, child *os.File, err error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "child-sync-parent"),
		os.NewFile(uintptr(fds[1]), "child-sync-child"), nil
}

func childSyncFromFD(fd int) (*childSync, error) {
	if fd < childExtraFDBase {
		return nil, fmt.Errorf("invalid child sync fd %d", fd)
	}
	// ExtraFiles deliberately clears CLOEXEC for the helper re-exec. Restore it
	// immediately so a successful final application exec closes the channel.
	unix.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), "child-sync")
	if f == nil {
		return nil, fmt.Errorf("open child sync fd %d", fd)
	}
	return &childSync{file: f, reader: bufio.NewReader(f)}, nil
}

func (s *childSync) close() { _ = s.file.Close() }

func (s *childSync) write(message string) error {
	_, err := io.WriteString(s.file, message+"\n")
	return err
}

func (s *childSync) read() (string, error) {
	line, err := s.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(line, "\n"), nil
}

func (s *childSync) waitFor(want string) error {
	got, err := s.read()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("got %q, want %q", got, want)
	}
	return nil
}

func (s *childSync) readyAndWait() error {
	if err := s.write(childMsgReady); err != nil {
		return fmt.Errorf("report ready: %w", err)
	}
	response, err := s.read()
	if err != nil {
		return fmt.Errorf("wait for release: %w", err)
	}
	switch response {
	case childMsgGo:
		return nil
	case childMsgAbort:
		return errors.New("parent aborted child setup")
	default:
		return fmt.Errorf("unexpected release %q", response)
	}
}

// fail reports a setup/exec error before exiting. It waits for an
// acknowledgement so the parent can mark the already-registered pid as a
// failed launch before SIGCHLD is delivered and the process is classified.
func (s *childSync) fail(err error) {
	message := strings.ReplaceAll(err.Error(), "\n", " ")
	_ = s.write(childMsgError + message)
	_, _ = s.read()
	s.close()
	os.Exit(127)
}

type childSyncResult int

const (
	// Successful final exec is observed as EOF because the sync fd is CLOEXEC.
	childSyncExecEOF childSyncResult = iota
	// A long-lived helper explicitly reports that its nested command was forked.
	childSyncStarted
)

// coordinateChild releases one started re-exec helper through its two gates.
// onSpawn runs before the child can perform setup; callers use it to register
// the pid with the process-wide reaper. onReady runs while the helper is
// blocked immediately before final exec (first-primary namespace pinning).
// onFailure runs before acknowledging a child-reported error.
func coordinateChild(
	cmd *exec.Cmd,
	parent *os.File,
	childEnd *os.File,
	onSpawn func(int) error,
	onReady func(int) error,
	onFailure func(int),
	want childSyncResult,
) (int, error) {
	sync := &childSync{file: parent, reader: bufio.NewReader(parent)}
	if err := cmd.Start(); err != nil {
		_ = childEnd.Close()
		sync.close()
		return 0, err
	}
	_ = childEnd.Close()
	pid := cmd.Process.Pid
	if onSpawn != nil {
		if err := onSpawn(pid); err != nil {
			if onFailure != nil {
				onFailure(pid)
			}
			_ = sync.write(childMsgAbort)
			sync.close()
			return pid, err
		}
	}
	fail := func(err error) (int, error) {
		if onFailure != nil {
			onFailure(pid)
		}
		_ = sync.write(childMsgAbort)
		sync.close()
		return pid, err
	}
	if err := sync.write(childMsgStart); err != nil {
		return fail(fmt.Errorf("release child setup: %w", err))
	}
	message, err := sync.read()
	if err != nil {
		return fail(fmt.Errorf("wait child setup: %w", err))
	}
	if strings.HasPrefix(message, childMsgError) {
		return fail(errors.New(strings.TrimPrefix(message, childMsgError)))
	}
	if message != childMsgReady {
		return fail(fmt.Errorf("child setup response %q, want %q", message, childMsgReady))
	}
	if onReady != nil {
		if err := onReady(pid); err != nil {
			return fail(err)
		}
	}
	if err := sync.write(childMsgGo); err != nil {
		return fail(fmt.Errorf("release child exec: %w", err))
	}

	message, err = sync.read()
	if want == childSyncExecEOF && errors.Is(err, io.EOF) && message == "" {
		sync.close()
		return pid, nil
	}
	if err != nil {
		return fail(fmt.Errorf("wait child exec: %w", err))
	}
	if strings.HasPrefix(message, childMsgError) {
		return fail(errors.New(strings.TrimPrefix(message, childMsgError)))
	}
	if want == childSyncStarted && message == childMsgExec {
		sync.close()
		return pid, nil
	}
	return fail(fmt.Errorf("child exec response %q", message))
}

// runPrimaryExecChild is the first-primary/restart re-entry. bootstrap is
// already in a freshly unshared cgroup namespace rooted at real /app;
// restarts join the pinned namespace through fd 3. Both replace cgroupfs only
// after making their private mount namespace rslave, so the scoped mount can
// never propagate back into sandbox-init.
func runPrimaryExecChild(mode string, isolated, control, placeholder bool, cred, workdir, appPath string, args []string) {
	bootstrap := mode == "bootstrap"
	if !bootstrap && mode != "restart" {
		die("exec-child: invalid mode %q", mode)
	}
	syncFD := childJoinedSyncFD
	if bootstrap {
		syncFD = childBootstrapSyncFD
	}
	sync, err := childSyncFromFD(syncFD)
	if err != nil {
		die("exec-child: sync: %v", err)
	}
	if err := sync.waitFor(childMsgStart); err != nil {
		sync.fail(fmt.Errorf("wait for setup release: %w", err))
	}

	runtime.LockOSThread()
	if err := makeChildMountsRSlave(); err != nil {
		sync.fail(err)
	}
	if !bootstrap {
		unix.CloseOnExec(childCgroupNamespaceFD)
		if err := cgroupJoinNamespace(childCgroupNamespaceFD); err != nil {
			sync.fail(err)
		}
		if err := unix.Close(childCgroupNamespaceFD); err != nil {
			sync.fail(fmt.Errorf("close cgroup namespace fd: %w", err))
		}
	}
	if err := cgroupMountScoped(); err != nil {
		sync.fail(err)
	}
	if bootstrap && control {
		if err := cgroupMoveBootstrapToInit(); err != nil {
			sync.fail(err)
		}
	}
	if isolated {
		if err := remountChildProc(); err != nil {
			sync.fail(err)
		}
	}
	if err := sync.readyAndWait(); err != nil {
		sync.fail(err)
	}
	if err := execFinalApplication(sync, cred, workdir, appPath, args, placeholder, false); err != nil {
		sync.fail(err)
	}
}

// runPluginExecChild is the lightweight plugin re-exec. It is born directly
// in the final cgroup, joins the pinned cgroup namespace, installs a scoped
// cgroupfs in its private mount namespace, and only then execs the plugin.
func runPluginExecChild(cred, workdir, appPath string, args []string) {
	sync, err := childSyncFromFD(childJoinedSyncFD)
	if err != nil {
		die("plugin-child: sync: %v", err)
	}
	if err := sync.waitFor(childMsgStart); err != nil {
		sync.fail(fmt.Errorf("wait for setup release: %w", err))
	}
	runtime.LockOSThread()
	if err := makeChildMountsRSlave(); err != nil {
		sync.fail(err)
	}
	unix.CloseOnExec(childCgroupNamespaceFD)
	if err := cgroupJoinNamespace(childCgroupNamespaceFD); err != nil {
		sync.fail(err)
	}
	if err := unix.Close(childCgroupNamespaceFD); err != nil {
		sync.fail(fmt.Errorf("close cgroup namespace fd: %w", err))
	}
	if err := cgroupMountScoped(); err != nil {
		sync.fail(err)
	}
	if err := sync.readyAndWait(); err != nil {
		sync.fail(err)
	}
	if err := execFinalApplication(sync, cred, workdir, appPath, args, false, false); err != nil {
		sync.fail(err)
	}
}

func makeChildMountsRSlave() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("make-rslave /: %w", err)
	}
	return nil
}

func remountChildProc() error {
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		if remountErr := unix.Mount("none", "/proc", "", unix.MS_REMOUNT, ""); remountErr != nil {
			return fmt.Errorf("remount /proc: %v / %w", err, remountErr)
		}
	}
	return nil
}

func execFinalApplication(sync *childSync, cred, workdir, appPath string, args []string, placeholder, joined bool) error {
	if workdir != "" && workdir != "/" {
		if err := unix.Chdir(workdir); err != nil {
			return fmt.Errorf("chdir %s: %w", workdir, err)
		}
	}

	resolved := appPath
	if !placeholder && !strings.Contains(appPath, "/") {
		path, err := exec.LookPath(appPath)
		if err != nil {
			return fmt.Errorf("%s not found in PATH: %w", appPath, err)
		}
		resolved = path
	}
	if err := applyCred(cred); err != nil {
		return fmt.Errorf("drop privileges: %w", err)
	}
	if joined {
		// A credential transition can clear PDEATHSIG. Re-arm it after
		// applyCred so the final command cannot outlive exec-join.
		if err := armParentDeathSignal(); err != nil {
			return err
		}
	}

	if placeholder {
		// There is no final exec to close the CLOEXEC sync fd, so close it
		// explicitly before becoming the long-lived placeholder process.
		sync.close()
		logf("exec-child: placeholder app (no exec) — waiting for stop signal")
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		os.Exit(0)
	}
	if err := syscall.Exec(resolved, append([]string{appPath}, args...), os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", resolved, err)
	}
	return nil
}

// runJoinedExecChild is the final native-exec command helper. Its parent has
// already joined the pinned cgroup namespace and selected the primary PID
// namespace for this child. The child joins the primary mount namespace via
// fd 3, confirms setup over fd 4, and then execs the requested command. This
// ordering keeps /proc/self/exe usable for the helper fork even when the
// primary has a private PID namespace whose procfs cannot see the parent.
func runJoinedExecChild(userSpec, workdir, appPath string, args []string) {
	sync, err := childSyncFromFD(childJoinedSyncFD)
	if err != nil {
		die("exec-child-joined: sync: %v", err)
	}
	// Arm this before the first handshake wait. If exec-join is killed by
	// quiesce or a lost MUX session during setup, the nested helper cannot be
	// orphaned. execFinalApplication re-arms it after any credential change.
	if err := armParentDeathSignal(); err != nil {
		sync.fail(err)
	}
	if err := sync.waitFor(childMsgStart); err != nil {
		sync.fail(fmt.Errorf("wait for setup release: %w", err))
	}
	runtime.LockOSThread()
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		sync.fail(fmt.Errorf("unshare fs: %w", err))
	}
	unix.CloseOnExec(childMountNamespaceFD)
	if err := unix.Setns(childMountNamespaceFD, unix.CLONE_NEWNS); err != nil {
		sync.fail(fmt.Errorf("setns mnt: %w", err))
	}
	if err := unix.Close(childMountNamespaceFD); err != nil {
		sync.fail(fmt.Errorf("close mount namespace fd: %w", err))
	}
	credStr := "-"
	if userSpec != "" && userSpec != "-" {
		cred, err := resolveCred(userSpec)
		if err != nil {
			sync.fail(fmt.Errorf("resolve user %q: %w", userSpec, err))
		}
		credStr = cred.encode()
	}
	if err := sync.readyAndWait(); err != nil {
		sync.fail(err)
	}
	if err := execFinalApplication(sync, credStr, workdir, appPath, args, false, true); err != nil {
		sync.fail(err)
	}
}

func armParentDeathSignal() error {
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0); err != nil {
		return fmt.Errorf("set pdeathsig: %w", err)
	}
	return nil
}
