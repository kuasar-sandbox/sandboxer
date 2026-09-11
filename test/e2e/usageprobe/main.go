// usageprobe is a static guest workload for e2e_usage.sh. It is not shipped.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func number(s string) int {
	n, err := strconv.Atoi(s)
	fail(err)
	if n < 0 {
		panic("negative amount")
	}
	return n
}
func main() {
	if len(os.Args) < 2 {
		panic("usageprobe MODE [ARGS]")
	}
	switch os.Args[1] {
	case "wait":
		fmt.Println("USAGE-PROBE-READY")
		for {
			if _, err := os.Stat("/tmp/usage-exit"); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	case "exit":
		fail(os.WriteFile("/tmp/usage-exit", nil, 0600))
	case "true":
	case "cpu", "cpu-ms":
		unit := time.Second
		if os.Args[1] == "cpu-ms" {
			unit = time.Millisecond
		}
		end := time.Now().Add(time.Duration(number(os.Args[2])) * unit)
		var n uint64
		for time.Now().Before(end) {
			for i := 0; i < 100000; i++ {
				n = n*1664525 + 1013904223
			}
		}
		fmt.Println(n)
	case "memory":
		b := make([]byte, number(os.Args[2])*1024*1024)
		for i := range b {
			b[i] = byte(i*17 + 1)
		}
		fmt.Printf("MEMORY-READY %d\n", len(b))
		time.Sleep(time.Duration(number(os.Args[3])) * time.Second)
		runtime.KeepAlive(b)
	case "write":
		f, err := os.Create(os.Args[2])
		fail(err)
		b := make([]byte, 1024*1024)
		for i := range b {
			b[i] = byte(i*17 + 1)
		}
		for i := 0; i < number(os.Args[3]); i++ {
			_, err = f.Write(b)
			fail(err)
		}
		fail(f.Sync())
		fail(f.Close())
	case "statfs":
		var s syscall.Statfs_t
		fail(syscall.Statfs(os.Args[2], &s))
		fail(json.NewEncoder(os.Stdout).Encode(s))
	case "trace-fs":
		traceFilesystem(os.Args[2], time.Duration(number(os.Args[3]))*time.Millisecond)
	case "trace-launch":
		launchOwnedTrace(os.Args[2], os.Args[3])
	case "trace-owned":
		ownedTrace(os.Args[2], os.Args[3])
	case "trace-state":
		ownedTraceState()
	case "trace-stop":
		fail(os.WriteFile("/tmp/usage-fs-trace-stop", nil, 0600))
	case "init-resources":
		initResources(0)
	case "init-resources-stream":
		for i := 0; i < number(os.Args[2]); i++ {
			initResources(i)
			time.Sleep(time.Second)
		}
	case "inspect":
		out := map[string]string{}
		init, err := os.ReadFile("/proc/1/exe")
		fail(err)
		out["sandbox_init_sha256"] = fmt.Sprintf("%x", sha256.Sum256(init))
		for _, name := range []string{"version", "zoneinfo", "buddyinfo", "meminfo", "self/status", "self/stat"} {
			b, err := os.ReadFile("/proc/" + name)
			fail(err)
			out[name] = string(b)
		}
		out["balloon_proc_field"] = strconv.FormatBool(strings.Contains(out["meminfo"], "Balloon"))
		out["page_size"] = strconv.Itoa(os.Getpagesize())
		fail(json.NewEncoder(os.Stdout).Encode(out))
	default:
		panic("unknown workload")
	}
}

// A normal exec is killed by quiesce. This test-only owner forks with separate
// stdio and a new session; the launching exec's PDEATHSIG is not inherited.
// Guest PID 1 reaps the adopted owner, which alone waits for its strace child.
func launchOwnedTrace(path, delay string) {
	if ms := number(delay); ms == 0 || ms > 80000 {
		panic("invalid bounded tracer delay")
	}
	log, err := os.OpenFile("/tmp/usage-owned-trace.log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	fail(err)
	defer log.Close()
	input, err := os.Open(os.DevNull)
	fail(err)
	defer input.Close()
	cmd := exec.Command("/probe", "trace-owned", path, delay)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	fail(cmd.Start())
	fail(json.NewEncoder(os.Stdout).Encode(map[string]int{"owner_pid": cmd.Process.Pid}))
	fail(cmd.Process.Release())
}

func ownedTrace(path, delay string) {
	ms := number(delay)
	if ms == 0 || ms > 80000 || os.Geteuid() != 0 {
		panic("bounded root/shared-PID disposable Guest required")
	}
	// Linux UAPI: x86_64 asm/unistd_64.h and arm64 asm-generic/unistd.h.
	nr, ok := map[string]uintptr{"amd64": 308, "arm64": 268}[runtime.GOARCH]
	if !ok {
		panic("unsupported setns architecture")
	}
	runtime.LockOSThread() // strace must fork from this joined thread.
	ns, err := os.Open("/proc/1/ns/cgroup")
	fail(err)
	_, _, errno := syscall.RawSyscall(nr, ns.Fd(), 0x02000000, 0) // CLONE_NEWCGROUP
	if errno != 0 {
		fail(errno)
	}
	fail(ns.Close())
	// Move only this dedicated test owner, before forking strace. Never move
	// PID 1, the application, or an exec-join process out of their cgroups.
	fail(os.WriteFile("/proc/1/root/sys/fs/cgroup/cgroup.procs", []byte(strconv.Itoa(os.Getpid())), 0600))
	own, err := os.ReadFile("/proc/self/cgroup")
	fail(err)
	root, err := os.ReadFile("/proc/1/cgroup")
	fail(err)
	if strings.TrimSpace(string(own)) != strings.TrimSpace(string(root)) {
		panic("test tracer owner did not enter Guest root cgroup")
	}
	info, err := json.Marshal(map[string]any{"owner_pid": os.Getpid(), "cgroup": string(own)})
	fail(err)
	fail(os.WriteFile("/tmp/usage-owned-ready.json", info, 0600))
	traceFilesystem(path, time.Duration(ms)*time.Millisecond)
	fail(os.WriteFile("/tmp/usage-owned-done", []byte("detached"), 0600))
}

func ownedTraceState() {
	result := map[string]string{}
	for _, name := range []string{"usage-owned-ready.json", "usage-owned-trace.log", "usage-owned-done"} {
		file, err := os.Open("/tmp/" + name)
		if os.IsNotExist(err) {
			continue
		}
		fail(err)
		value, err := io.ReadAll(io.LimitReader(file, 2*1024*1024+1))
		fail(err)
		fail(file.Close())
		if len(value) > 2*1024*1024 {
			panic("test trace output too large")
		}
		result[name] = string(value)
	}
	fail(json.NewEncoder(os.Stdout).Encode(result))
}

func initResources(sequence int) {
	fds, err := os.ReadDir("/proc/1/fd")
	fail(err)
	threads, err := os.ReadDir("/proc/1/task")
	fail(err)
	status, err := os.ReadFile("/proc/1/status")
	fail(err)
	identities, infos := make(map[string]string), make(map[string]string)
	for _, fd := range fds {
		link, err := os.Readlink("/proc/1/fd/" + fd.Name())
		if err == nil {
			identities[fd.Name()] = link
			info, err := os.ReadFile("/proc/1/fdinfo/" + fd.Name())
			if err == nil {
				infos[fd.Name()] = string(info)
			}
		}
	}
	fail(json.NewEncoder(os.Stdout).Encode(map[string]any{"sequence": sequence, "fds": len(fds), "fd_identities": identities,
		"fdinfo": infos, "threads": len(threads), "status": string(status)}))
}

// traceFilesystem is a test-only tracer owner. Select the retained directory
// handle by the mounted filesystem's device, not by a guessed fd or a user
// mount enumeration. The native strace and its loader live only in this test's
// application image, never in the product runtime bundle.
func traceFilesystem(path string, delay time.Duration) {
	var target syscall.Stat_t
	fail(syscall.Stat(path, &target))
	entries, err := os.ReadDir("/proc/1/fd")
	fail(err)
	selected, selectedFD := "", ""
	for _, entry := range entries {
		fdPath := filepath.Join("/proc/1/fd", entry.Name())
		var st syscall.Stat_t
		if syscall.Stat(fdPath, &st) != nil || st.Dev != target.Dev || st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			continue
		}
		if selected != "" {
			panic("multiple retained directory handles for selected filesystem")
		}
		selected, err = os.Readlink(fdPath)
		fail(err)
		selectedFD = entry.Name()
	}
	if selected == "" {
		panic("managed filesystem handle not found")
	}
	info, err := os.ReadFile("/proc/1/fdinfo/" + selectedFD)
	fail(err)
	flags := uint64(0)
	for _, line := range strings.Split(string(info), "\n") {
		if value, ok := strings.CutPrefix(line, "flags:\t"); ok {
			flags, err = strconv.ParseUint(value, 8, 64)
			fail(err)
		}
	}
	if flags&syscall.O_CLOEXEC == 0 {
		panic("managed filesystem handle is not CLOEXEC")
	}
	fail(json.NewEncoder(os.Stdout).Encode(map[string]string{"fd": selectedFD, "path": selected, "fdinfo": string(info)}))
	cmd := exec.Command("/strace", "-f", "-qq", "-yy", "-ttt", "-T", "-p", "1",
		"-e", "trace=fstatfs", "-e", "signal=none", "-e", fmt.Sprintf("inject=fstatfs:delay_exit=%d", delay.Microseconds()), "-P", selected)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	fail(cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	limit := time.NewTimer(90 * time.Second)
	defer limit.Stop()
	for {
		select {
		case err := <-done:
			fail(err)
			panic("tracer ended before requested stop")
		case <-ticker.C:
			if _, err := os.Stat("/tmp/usage-fs-trace-stop"); err != nil {
				continue
			}
		case <-limit.C:
		}
		fail(cmd.Process.Signal(syscall.SIGINT))
		select {
		case err := <-done:
			if exit, ok := err.(*exec.ExitError); ok && exit.ProcessState.Sys().(syscall.WaitStatus).Signal() == syscall.SIGINT {
				return
			}
			fail(err)
			return
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			panic("tracer failed to detach")
		}
	}
}
