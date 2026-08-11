package main

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// Plugin supervision: companion processes (launch.plugin[]) run alongside the
// user app in the same guest rootfs + app cgroup, each restarted per its own
// policy with the shared backoff (supervise.go). A plugin's exit never affects
// the sandbox lifecycle — only the user app's exit does. Plugins are
// sandbox-init's direct children, so the phase3Supervise Wait4(-1) reaper
// collects them and routes their exits here (docs/sandbox-init.md §3.2).

// pluginProc is one supervised plugin: its spec + backoff state. pending marks
// a relaunch deferred by a snapshot quiesce, issued at endQuiesce.
type pluginProc struct {
	spec         proto.PluginSpec
	bo           backoff
	pending      bool
	launchFailed bool
}

// policy returns the effective restart policy (plugins default to always).
func (p *pluginProc) policy() string {
	if p.spec.Restart == "" {
		return "always"
	}
	return p.spec.Restart
}

// pluginRegistry supervises every plugin: a pid→proc map of the live ones plus
// the full set (for deferred relaunch after a quiesce window). quiescing gates
// new launches (a snapshot must not fork an unfrozen process); closed stops all
// restarts at shutdown.
type pluginRegistry struct {
	mu        sync.Mutex
	byPID     map[int]*pluginProc
	all       []*pluginProc
	quiescing bool
	closed    bool
}

func newPluginRegistry() *pluginRegistry {
	return &pluginRegistry{byPID: make(map[int]*pluginProc)}
}

// start launches the initial plugin set. Called once after the app completes
// its cgroup bootstrap (app comes up first; plugins are companions).
func (r *pluginRegistry) start(specs []proto.PluginSpec) {
	for _, s := range specs {
		p := &pluginProc{spec: s}
		r.mu.Lock()
		r.all = append(r.all, p)
		r.mu.Unlock()
		r.launch(p)
	}
}

// launch forks one plugin and registers it. During a quiesce it defers
// (pending) instead — a snapshot must not capture a freshly-forked, unfrozen
// plugin; endQuiesce issues the deferred launch on resume.
func (r *pluginRegistry) launch(p *pluginProc) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if r.quiescing {
		p.pending = true
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	pid, err := startPluginProcess(p.spec, func(pid int) error {
		// Register before the helper leaves its start gate. The global reaper
		// can therefore classify even a namespace/mount/final-exec failure.
		r.mu.Lock()
		p.bo.onStart(time.Now())
		p.pending = false
		p.launchFailed = false
		r.byPID[pid] = p
		r.mu.Unlock()
		return nil
	}, func(pid int) {
		// coordinateChild invokes this before acknowledging the helper's
		// error, so onExit observes launchFailed deterministically.
		r.mu.Lock()
		if r.byPID[pid] == p {
			p.launchFailed = true
		}
		r.mu.Unlock()
	})
	if err != nil {
		logf("plugin %s: launch failed: %v", p.spec.Exec, err)
		if pid > 0 {
			// The registered helper will exit after the error acknowledgement;
			// onExit owns its backoff/relaunch so there is exactly one retry.
			return
		}
		// clone3/exec of the helper itself failed, so no child exists for the
		// reaper to drive. Back off and retry here.
		delay := p.bo.next(time.Now())
		go func() { time.Sleep(delay); r.launch(p) }()
		return
	}
	logf("plugin %s: started pid=%d (restart=%s)", p.spec.Exec, pid, p.policy())
}

// onExit handles a reaped pid: if it is a live plugin, apply its restart policy
// (backoff relaunch) and return true; otherwise return false so the caller
// routes the pid elsewhere (exec child / orphan).
func (r *pluginRegistry) onExit(pid int, status syscall.WaitStatus) bool {
	r.mu.Lock()
	p := r.byPID[pid]
	launchFailed := false
	if p != nil {
		delete(r.byPID, pid)
		launchFailed = p.launchFailed
		p.launchFailed = false
	}
	r.mu.Unlock()
	if p == nil {
		return false
	}
	if !launchFailed && !wantRestart(p.policy(), status) {
		logf("plugin %s: exited code=%d (restart=%s) — done", p.spec.Exec, status.ExitStatus(), p.policy())
		return true
	}
	delay := p.bo.next(time.Now())
	if launchFailed {
		logf("plugin %s: launch helper exited after setup failure — retrying in %s", p.spec.Exec, delay)
	} else {
		logf("plugin %s: exited code=%d — restarting in %s", p.spec.Exec, status.ExitStatus(), delay)
	}
	go func() { time.Sleep(delay); r.launch(p) }()
	return true
}

// beginQuiesce gates new plugin launches before a snapshot. Running plugins are
// frozen with the app cgroup; this only stops the supervisor from forking new
// ones into the frozen window.
func (r *pluginRegistry) beginQuiesce() {
	r.mu.Lock()
	r.quiescing = true
	r.mu.Unlock()
}

// endQuiesce re-enables launches after resume and issues any relaunch that came
// due (pending) during the frozen window.
func (r *pluginRegistry) endQuiesce() {
	r.mu.Lock()
	r.quiescing = false
	var pending []*pluginProc
	for _, p := range r.all {
		if p.pending {
			pending = append(pending, p)
		}
	}
	r.mu.Unlock()
	for _, p := range pending {
		r.launch(p)
	}
}

// startPluginProcess starts a lightweight sandbox-init re-exec atomically in
// the final application cgroup. The helper joins the pinned cgroup namespace,
// installs a scoped cgroupfs in a private mount namespace, drops credentials,
// and execs the plugin. Namespace, mount, placement, and final-exec failures
// are returned as launch failures; there is no post-Start cgroup migration or
// fail-open "continuing unfrozen" path.
func startPluginProcess(s proto.PluginSpec, onSpawn func(int) error, onFailure func(int)) (int, error) {
	credStr := "-"
	if s.User != "" {
		c, err := resolveCred(s.User)
		if err != nil {
			return 0, fmt.Errorf("user %q: %w", s.User, err)
		}
		if c != nil {
			credStr = c.encode()
		}
	}
	ns, err := cgroupNamespaceFile()
	if err != nil {
		return 0, err
	}
	parentSync, childSync, err := newChildSyncPair()
	if err != nil {
		return 0, fmt.Errorf("child handshake socketpair: %w", err)
	}
	self := "/proc/self/exe"
	args := append([]string{self, "plugin-child", credStr, s.Workdir, s.Exec}, s.Args...)
	sysAttr := &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	if err := cgroupConfigureClone(sysAttr, false); err != nil {
		_ = parentSync.Close()
		_ = childSync.Close()
		return 0, err
	}
	// Preserve the documented/default PATH for bare plugin executables;
	// explicit plugin PATH still wins.
	pluginEnv := execEnv(s.Env)
	cmd := exec.Cmd{
		Path:        self,
		Args:        args,
		Env:         pluginEnv,
		Dir:         "/",
		Stdin:       nil,
		Stdout:      os.Stderr,
		Stderr:      os.Stderr,
		ExtraFiles:  []*os.File{ns, childSync}, // fd 3 cgroup ns, fd 4 sync
		SysProcAttr: sysAttr,
	}
	return coordinateChild(&cmd, parentSync, childSync, onSpawn, nil, onFailure, childSyncExecEOF)
}
