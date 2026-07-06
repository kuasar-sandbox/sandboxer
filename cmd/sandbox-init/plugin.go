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
	spec    proto.PluginSpec
	bo      backoff
	pending bool
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

// start launches the initial plugin set. Called once after the app is forked
// and placed in the app cgroup (app comes up first; plugins are companions).
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

	pid, err := startPluginProcess(p.spec)
	if err != nil {
		// A launch failure is treated like an exit: back off and retry.
		logf("plugin %s: launch failed: %v", p.spec.Exec, err)
		delay := p.bo.next(time.Now())
		go func() { time.Sleep(delay); r.launch(p) }()
		return
	}
	r.mu.Lock()
	p.bo.onStart(time.Now())
	p.pending = false
	r.byPID[pid] = p
	r.mu.Unlock()
	logf("plugin %s: started pid=%d (restart=%s)", p.spec.Exec, pid, p.policy())
}

// onExit handles a reaped pid: if it is a live plugin, apply its restart policy
// (backoff relaunch) and return true; otherwise return false so the caller
// routes the pid elsewhere (exec child / orphan).
func (r *pluginRegistry) onExit(pid int, status syscall.WaitStatus) bool {
	r.mu.Lock()
	p := r.byPID[pid]
	if p != nil {
		delete(r.byPID, pid)
	}
	r.mu.Unlock()
	if p == nil {
		return false
	}
	if !wantRestart(p.policy(), status) {
		logf("plugin %s: exited code=%d (restart=%s) — done", p.spec.Exec, status.ExitStatus(), p.policy())
		return true
	}
	delay := p.bo.next(time.Now())
	logf("plugin %s: exited code=%d — restarting in %s", p.spec.Exec, status.ExitStatus(), delay)
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

// startPluginProcess forks one plugin into the app cgroup with console stdio
// and returns its pid. Mirrors runInit's child setup (env over the default
// PATH, optional workdir + run-as user) but does NOT wait — the reaper does.
func startPluginProcess(s proto.PluginSpec) (int, error) {
	cmd := exec.Command(s.Exec, s.Args...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr // guest console
	cmd.Stderr = os.Stderr
	cmd.Env = envSliceFromMap(s.Env)
	if s.Workdir != "" && s.Workdir != "/" {
		cmd.Dir = s.Workdir
	}
	if s.User != "" {
		c, err := resolveCred(s.User)
		if err != nil {
			return 0, fmt.Errorf("user %q: %w", s.User, err)
		}
		if c != nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Credential: &syscall.Credential{Uid: c.uid, Gid: c.gid, Groups: c.sgids},
			}
		}
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	// Place in the app cgroup so a snapshot quiesce freezes the plugin with the
	// app (else it would run with stale clock/network across resume, §3.4).
	if err := cgroupPlaceApp(pid); err != nil {
		logf("plugin %s: cgroup place pid=%d: %v (continuing unfrozen)", s.Exec, pid, err)
	}
	return pid, nil
}
