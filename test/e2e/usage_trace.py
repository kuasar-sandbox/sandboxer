"""Optional, explicitly perturbed tracing of this test's own Host/CH PIDs.

No uretprobes: Go may move goroutine stacks. Ordinary uprobes on verified RET
instructions use the Go G register as the correlation key, not OS thread ID.
Only Linux amd64's verified current register ABI is supported by this harness.
"""
import json
import os
from pathlib import Path
import platform
import re
import signal
import subprocess
import time

from usage import BIN, run, write_json


class Trace:
    def __init__(self, directory, hosts, chs):
        assert platform.machine() == "x86_64", "diagnostic register probes require amd64"
        self.directory = directory
        binary = str((BIN / "sandbox-ctl").resolve())
        assert not any(c in binary for c in ':"\n')
        host_filter = " || ".join(f"pid == {p}" for p in hosts)
        init = []
        for pid in hosts+chs:
            for task in Path(f"/proc/{pid}/task").iterdir():
                init.append(f"@owner[{task.name}] = {pid};")
        prefix = "github.com/kuasar-sandbox/sandboxer/pkg/"
        api_layout = run("gdb", "-q", "-nx", "-batch", "-ex", "set auto-load off", "-ex", "set language c",
                         "-ex", f"p/x (unsigned long)&(('{prefix}resctl.BalloonController' *)0)->apiMu", binary)
        api_offset = int(re.search(r"\$1 = (0x[0-9a-f]+)", api_layout)[1], 16)
        assert 0 < api_offset < 4096

        def instructions(function):
            output = run("go", "tool", "objdump", "-s", "^"+re.escape(function)+"$", binary)
            rows = re.findall(r"\s(0x[0-9a-f]+)\s+[0-9a-f]+\s+([^\n]+)", output)
            assert rows, function
            return [(int(address, 16), op.strip()) for address, op in rows]

        def point(function, address=None):
            offset = "" if address is None else f"+{address}"
            return f'uprobe:{binary}:"{function}"{offset}'

        def returns(function):
            rows = instructions(function)
            points = [point(function, address-rows[0][0]) for address, op in rows if op == "RET"]
            assert points, function
            return ",\n".join(points)

        malloc = instructions("runtime.mallocgc")
        # Observe after the stack-growth guard, before size is overwritten.
        malloc_at = next(address for address, op in malloc if op == "TESTQ AX, AX")-malloc[0][0]
        types = directory / "trace-types.h"
        types.write_text("typedef int pid_t; typedef unsigned long size_t; typedef unsigned long long u64;\n")
        blocks = [f'#include "{types}"',
                  f'BEGIN {{ {" ".join(init)} printf("TRACE_READY\\n"); }}',
                  'tracepoint:sched:sched_process_fork /@owner[args->parent_pid]/ { @owner[args->child_pid] = @owner[args->parent_pid]; }',
                  'tracepoint:sched:sched_wakeup /@owner[args->pid]/ { @wakeups[@owner[args->pid]] = count(); }',
                  'tracepoint:sched:sched_process_exit /@owner[tid]/ { delete(@owner[tid]); }',
                  f'{point("runtime.mallocgc", malloc_at)} /({host_filter}) && reg("ax") > 0/ {{ @mallocgc_calls[pid] = count(); @mallocgc_requested_bytes[pid] = sum(reg("ax")); }}']
        for method in ("Observe", "TryObserveActual"):
            blocks.append(f'{point(prefix+"resctl.(*BalloonController)."+method)} /{host_filter}/ {{ @api_mutex[pid] = reg("ax")+{api_offset}; }}')
        mutex = "internal/sync.(*Mutex).lockSlow"
        blocks += [f'{point(mutex)} /({host_filter}) && reg("ax") == @api_mutex[pid]/ {{ @lock_start[pid, reg("r14")] = nsecs; }}',
                   f'{returns(mutex)} /@lock_start[pid, reg("r14")]/ {{ $d = nsecs-@lock_start[pid, reg("r14")]; @api_contended_locks[pid] = count(); @api_lock_wait_ns[pid] = sum($d); @api_lock_max_ns[pid] = max($d); delete(@lock_start[pid, reg("r14")]); }}']
        writer = prefix+"usage.(*Manager).write"
        blocks += [f'{point(writer)} /{host_filter}/ {{ @save_start[pid, reg("r14")] = nsecs; }}',
                   f'{returns(writer)} /@save_start[pid, reg("r14")]/ {{ $d=nsecs-@save_start[pid, reg("r14")]; @saves[pid]=count(); @save_total_ns[pid]=sum($d); @save_max_ns[pid]=max($d); @save_us[pid]=hist($d/1000); delete(@save_start[pid, reg("r14")]); }}']
        usage_prefix = '{"type":"usage_request"'
        info_prefix = 'GET /api/v1/vm.info '
        blocks += [f'''tracepoint:syscalls:sys_enter_write /{host_filter}/ {{
          if (args->count >= {len(usage_prefix)} && str(args->buf, {len(usage_prefix)+1}) == {json.dumps(usage_prefix)}) {{
            if (!@usage_fd[pid, args->fd]) {{ @usage_tx_bytes[pid]=sum(4); }}
            @usage_fd[pid, args->fd]=1; @usage_requests[pid]=count();
          }}
          if (args->count >= {len(info_prefix)} && str(args->buf, {len(info_prefix)+1}) == {json.dumps(info_prefix)}) {{ @ch_info_requests[pid]=count(); }}
          if (@usage_fd[pid, args->fd]) {{ @usage_write[tid]=pid; }}
        }}''',
          'tracepoint:syscalls:sys_exit_write /@usage_write[tid]/ { if (args->ret>0) { @usage_tx_bytes[@usage_write[tid]]=sum(args->ret); } delete(@usage_write[tid]); }',
          f'tracepoint:syscalls:sys_enter_read /({host_filter}) && @usage_fd[pid, args->fd]/ {{ @usage_read[tid]=pid; }}',
          'tracepoint:syscalls:sys_exit_read /@usage_read[tid]/ { if (args->ret>0) { @usage_rx_bytes[@usage_read[tid]]=sum(args->ret); } delete(@usage_read[tid]); }',
          f'tracepoint:syscalls:sys_enter_close /{host_filter}/ {{ delete(@usage_fd[pid, args->fd]); }}',
          'END { clear(@owner); clear(@api_mutex); clear(@usage_fd); clear(@usage_write); clear(@usage_read); }']
        script = directory / "trace.bt"
        script.write_text("\n".join(blocks)+"\n")
        self.log_path = directory / "trace.log"
        self.log = self.log_path.open("w")
        self.started = time.monotonic_ns()
        bpftrace = os.environ.get("BPFTRACE_BIN", "bpftrace")
        # Require a build with instruction-offset support. Never bypass its
        # instruction validation or replace Go return addresses.
        try:
            self.process = subprocess.Popen([bpftrace, "-B", "none", "-f", "json", str(script)],
                                            stdout=self.log, stderr=subprocess.STDOUT)
        except BaseException:
            self.log.close()
            raise
        deadline = time.monotonic()+20
        while time.monotonic() < deadline and self.process.poll() is None:
            if "TRACE_READY" in self.log_path.read_text():
                time.sleep(.5)
                break
            time.sleep(.1)
        else:
            self.close()
            raise AssertionError(f"tracer did not attach; see {self.log_path}")
        write_json(directory / "trace-method.json", {
            "binary": binary, "api_mutex_offset": api_offset, "malloc_offset": malloc_at,
            "hosts": hosts, "chs": chs, "bpftrace": run(bpftrace, "--version"),
            "definition": "perturbed load/query/stop diagnostic; mallocgc requested sizes, not rounded heap objects; wakeups include tracked process threads; management bytes are usage frames excluding CONNECT handshake; api wait is contended lockSlow duration; no uretprobe"})

    def close(self):
        if self.process.poll() is None:
            self.process.send_signal(signal.SIGINT)
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
                raise AssertionError("tracer did not stop cleanly")
        self.log.close()
        assert self.process.returncode == 0, f"tracer exit={self.process.returncode}; see {self.log_path}"
        write_json(self.directory / "trace-window.json", {"elapsed_ns": time.monotonic_ns()-self.started})
