#!/usr/bin/env bash
# Live/saved usage, disk accounting, balloon/OOM and restored epoch contracts.
set -euo pipefail
source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}" "${WORK:?WORK is required}" "${OUT:?OUT is required}" "${USAGE_PROBE_BIN:?USAGE_PROBE_BIN is required}"
require_root
require_kvm
for command in python3 mkfs.ext4; do require_command "$command"; done
for binary in sandbox-ctl sandbox-init cloud-hypervisor flatten-ctl mkfs.erofs; do require_binary "$binary"; done
for file in sandbox-runtime.bundle vmlinux; do [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"; done
[ -x "$USAGE_PROBE_BIN" ] || e2e_fail "missing executable prepared usage probe"
for file in usage.py usage_report_relay.py usage_vsock_relay.py usage_oom_wrapper.py; do [ -f "$SANDBOXER_LIB/$file" ] || e2e_fail "missing prepared helper: $file"; done
export PYTHONDONTWRITEBYTECODE=1
python3 - "$SANDBOXER_LIB" <<'PY'
import sys
from pathlib import Path
LIB = Path(sys.argv[1])
sys.path.insert(0, str(LIB))
#!/usr/bin/env python3
"""Real usage integration; keep complete per-case evidence, never synthesize usage."""
import atexit
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import select
import shutil
import signal
import socket
import subprocess
import sys
import tarfile
import time

from usage_report_relay import ReportRelay

from usage import (BIN, Sandbox, build_probe, digest, ext4, image_ref, metric, run, write_json, runtime_init_digest, oom_pressure)

def main():
    seconds = 12
    variants = ("off", "overlay", "single", "balloon", "balloon-no-oom", "oom", "multidisk", "restore")
    work = Path(os.environ["WORK"]) / "usage"
    work.mkdir(parents=True, exist_ok=True)
    print(f"usage evidence: {work}", flush=True)
    # Only small evidence files enter the CI artifact, not disk/runtime images.
    # Product E2E copies only the already validated prepared probe and records
    # the identities of every runtime artifact it exercises.
    if os.environ["OUT"]:
        evidence = Path(os.environ["OUT"]) / "usage"
        def collect_evidence():
            evidence.mkdir(parents=True, exist_ok=True)
            for path in work.rglob("*"):
                if path.is_file() and path.suffix in (".json", ".log", ".usage"):
                    dest = evidence / path.relative_to(work)
                    dest.parent.mkdir(parents=True, exist_ok=True)
                    # The suite runs as root; do not preserve private source modes in CI evidence.
                    shutil.copyfile(path, dest)
        atexit.register(collect_evidence)
    metadata = {"host_kernel": run("uname", "-a").strip(),
                "ch_version": run(BIN / "cloud-hypervisor", "--version").strip(),
                "artifacts": {n: digest(BIN / n) for n in ("sandbox-ctl", "sandbox-init", "sandbox-runtime.bundle", "vmlinux", "cloud-hypervisor")},
                "seconds": seconds, "cases": variants}
    write_json(work / "source-set.json", metadata)
    root = work / "rootfs"
    root.mkdir()
    for name in ("tmp", "proc", "sys", "dev", "data", "cache", "other"):
        (root / name).mkdir()
    build_probe(root / "probe")
    image = work / "root.img"
    run(BIN / "flatten-ctl", "export", "--output", image, "--no-progress", root)
    ref = image_ref(image)
    results = []
    for name in metadata["cases"]:
        assert name in ("off", "overlay", "single", "balloon", "balloon-no-oom", "oom", "multidisk", "restore"), name
        balloon = name in ("balloon", "balloon-no-oom", "oom")
        diff = work / f"{name}.ext4"
        ext4(diff, root if name == "single" else None)
        config = {"resources": {"capacity": {"cpu": 1, "memory": "512MiB"},
                                "allocatable": {"cpu": 1, "memory": "256MiB" if balloon else "512MiB",
                                                "deflate_on_oom": name in ("balloon", "oom")}},
                  "boot": {"kernel": f"file://{BIN / 'vmlinux'}", "runtime": f"file://{BIN / 'sandbox-runtime.bundle'}",
                           "cmdline": "console=hvc0 printk.time=1",
                           "root": {"diff": f"file://{diff}"} if name == "single" else
                           {"base": ref, "overlay": {"diff": f"file://{diff}"}}},
                  # Shared PID namespace makes /proc/1/exe the real init for
                  # artifact verification; the default private namespace's PID 1 is the app.
                  "launch": {"exec": "/probe", "args": ["wait"], "restart": "never", "pid_namespace": "shared"},
                  "usage": {"enabled": name != "off", "sample_interval": "1s", "flush_interval": "5s"}}
        if name == "multidisk":
            config["boot"]["disks"] = []
            config["mounts"] = []
            for ordinal, target in enumerate(("/data", "/other")):
                disk = work / f"data-{ordinal}.ext4"
                ext4(disk)
                config["boot"]["disks"].append({"name": f"data{ordinal}", "diff": f"file://{disk}"})
                config["mounts"].append({"type": "disk", "source": f"data{ordinal}", "target": target})
            config["mounts"].append({"type": "empty", "target": "/data/cache"})
        sb = Sandbox(work, name, config, ch_binary=LIB.joinpath("usage_oom_wrapper.py") if name == "oom" else None)
        relay, failure = None, None
        try:
            if name == "oom":
                relay = ReportRelay(sb.runroot / "instance/vsock.sock.oom_5000")
            sb.ready()
            inspect = json.loads(sb.cli("exec", "--", "/probe", "inspect"))
            write_json(sb.dir / "guest.json", inspect)
            assert inspect["sandbox_init_sha256"] == runtime_init_digest(metadata["artifacts"]["sandbox-init"]), "runtime bundle does not contain the selected sandbox-init"
            assert inspect["balloon_proc_field"] == "false", "this case requires the unpatched Balloon proc ABI"
            assert "pagesets" in inspect["zoneinfo"] and "count:" in inspect["zoneinfo"], "PCP source missing"
            time.sleep(2.2)
            before = sb.view()
            write_json(sb.dir / "before.json", before)
            started = time.monotonic()
            sb.cli("exec", "--", "/probe", "cpu", "2")
            sb.cli("exec", "--", "/probe", "write", "/tmp/usage-data", "16")
            if name == "multidisk":
                sb.cli("exec", "--", "/probe", "write", "/data/cache/usage-data", "16")
            time.sleep(max(0, seconds - (time.monotonic() - started)))
            after = sb.view()
            write_json(sb.dir / "after.json", after)
            if name == "off":
                assert not after["enabled"] and "live" not in after
                assert not list(sb.baseroot.rglob("*.usage"))
            else:
                b, a = before["live"], after["live"]
                cpu = int(metric(a, "counters", "guest.cpu")["known_total_ns"]) - int(metric(b, "counters", "guest.cpu")["known_total_ns"])
                assert cpu > 1_000_000_000, ("guest CPU did not observe busy workload", cpu)
                for field in ("guest.memory", "filesystem.root", "ch.rss_anon", "ch.rss_file", "sandbox_ctl.rss_anon", "sandbox_ctl.rss_file"):
                    gauge = metric(a, "gauges", field)
                    assert gauge["status"] == "ok" and int(gauge["covered_total_ns"]) > 0, gauge
                fs_before = int(metric(b, "gauges", "filesystem.root")["last_value_bytes"])
                fs_after = int(metric(a, "gauges", "filesystem.root")["last_value_bytes"])
                assert fs_after - fs_before >= 16 * 1024 * 1024, (fs_before, fs_after)
                count = sum(g["name"].startswith("filesystem.") for g in a["gauges"])
                assert count == (3 if name == "multidisk" else 1), count
                assert after.get("saved") and int(after["saved_end"]) > 0, after
                assert a["sandbox_id"] == name and (sb.baseroot / "instance" / f"{name}.usage").exists()
                write_json(sb.dir / "history.json", json.loads(sb.cli("usage", "--history", "--limit", "10")))
                if balloon:
                    initial = sb.ch_info()
                    write_json(sb.dir / "ch-before-pressure.json", initial)
                    assert initial["config"]["balloon"]["size"] > 0 and initial["memory_actual_size"] < 512*1024*1024, "no actual balloon inflation observed"
                    assert initial["config"]["balloon"]["deflate_on_oom"] == (name != "balloon-no-oom")
                if name == "oom":
                    oom_pressure(sb, relay)
                    time.sleep(2)
                    pressure_view = sb.view()
                    write_json(sb.dir / "after-pressure.json", pressure_view)
                    ram = metric(pressure_view["live"], "gauges", "guest.memory")
                    assert ram["status"] == "ok" and int(ram["covered_total_ns"]) > int(metric(a, "gauges", "guest.memory")["covered_total_ns"]), ram
                if name == "restore":
                    output = sb.dir / "snapshot"
                    output.mkdir()
                    capture = sb.cli("snapshot", "--output", output, "--resume", "--drop-caches=false", "--merge-ref=false", timeout=60)
                    (sb.dir / "snapshot.log").write_text(capture)
                    assert (output / f"{name}.snapshot").is_file()
                    sb.cli("exec", "--", "/probe", "cpu", "2")
                    time.sleep(2)
                    write_json(sb.dir / "resumed.json", sb.view())
            results.append({"case": name, "passed": True})
        except BaseException as error:
            failure = error
            raise
        finally:
            try:
                try:
                    sb.stop()
                except BaseException as error:
                    if failure is None:
                        failure = error
                        raise
                    print(f"usage sandbox cleanup: {error}", file=sys.stderr)
            finally:
                if relay is not None:
                    try:
                        try:
                            relay.stop()
                        finally:
                            write_json(sb.dir / "reports.json", {"requests": relay.requests, "errors": relay.errors})
                    except BaseException as error:
                        if failure is None:
                            raise
                        print(f"usage report relay cleanup: {error}", file=sys.stderr)
        if name != "off":
            saved = sb.view()
            write_json(sb.dir / "stopped.json", saved)
            assert saved["saved"]["snapshot"]["closed"], saved
            if name == "restore":
                parent = saved["saved"]["snapshot"]
                parent_cpu = int(metric(parent, "counters", "guest.cpu")["known_total_ns"])
                parent_fs = int(metric(parent, "gauges", "filesystem.root")["last_value_bytes"])
                host = {"resources": config["resources"],
                        "boot": {"kernel": config["boot"]["kernel"], "runtime": config["boot"]["runtime"]},
                        "restore": {"prefetch": "off"},
                        # A periodic retry must not conceal a missed immediate
                        # post-ACK observation in a short restored run.
                        "usage": {"enabled": True, "sample_interval": "1m", "flush_interval": "5m"},
                        "timeouts": {"restore": "30s"}}
                for same_id in (False, True):
                    role = "same-restore" if same_id else "clone-restore"
                    restore_started = time.monotonic()
                    child = Sandbox(work, role, host, restore=output / f"{name}.snapshot",
                                    sandbox_id=name if same_id else role,
                                    base_root=sb.baseroot if same_id else None)
                    try:
                        child.ready()
                        time.sleep(3)
                        v = child.view()
                        elapsed = time.monotonic() - restore_started
                        assert elapsed < 60, ("restore test crossed its first periodic tick", elapsed)
                        write_json(child.dir / "first-round.json", {
                            "elapsed_seconds": elapsed, "sample_interval_seconds": 60,
                            "before_first_periodic_tick": True})
                        write_json(child.dir / "restored.json", v)
                        live = v["live"]
                        cpu = int(metric(live, "counters", "guest.cpu")["known_total_ns"])
                        assert (cpu >= parent_cpu) if same_id else (cpu < parent_cpu), (same_id, cpu, parent_cpu)
                        fs = metric(live, "gauges", "filesystem.root")
                        assert fs["status"] == "ok" and int(fs["last_value_bytes"]) >= parent_fs, fs
                        memory = metric(live, "gauges", "guest.memory")
                        assert memory["status"] == "ok", memory
                        assert int(fs["window"]["samples"]) > 0 and int(memory["window"]["samples"]) > 0, v
                        assert live["run_epoch"] != parent["run_epoch"]
                    finally:
                        child.stop()
        print(f"PASS usage/{name}", flush=True)
    write_json(work / "results.json", results)


if __name__ == "__main__":
    main()
PY
