#!/usr/bin/env python3
"""Real slow-source faults in disposable VMs, without product injection hooks.

Only the selected managed disk's fstatfs return is delayed. This is a syscall
delay, not a claim that the underlying ext4 block device failed. The first
completed raw value remains with its old request and must never be replayed.
"""
import argparse
import atexit
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time

from usage import BIN, Sandbox, digest, ext4, image_ref, metric, run, write_json
from usage_ch_relay import CHRelay
from usage_vsock_relay import UsageRelay


def tracer_dependencies(output):
    if "not found" in output:
        raise ValueError("strace has an unresolved shared library")
    paths = set(re.findall(r"(?:=>\s+)?(/\S+)", output))
    if not paths and "not a dynamic executable" not in output and "statically linked" not in output:
        raise ValueError("cannot identify the strace loader and libraries")
    return sorted(paths)


def install_tracer(root):
    tracer = Path(shutil.which("strace")).resolve()
    # Only inspect the already installed, trusted native test executable.
    result = subprocess.run(["ldd", str(tracer)], text=True, capture_output=True, timeout=10)
    output = result.stdout + result.stderr
    paths = tracer_dependencies(output)
    assert result.returncode == 0 or not paths, output
    shutil.copy2(tracer, root / "strace")
    copies = {"strace": digest(tracer)}
    for name in paths:
        source = Path(name)
        dest = root / source.relative_to("/")
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, dest)  # Dereference links at their guest-visible path.
        copies[name] = digest(source)
    return {"version": run(tracer, "-V"), "ldd": output, "sha256": copies}


def resources(sb):
    host = Path(f"/proc/{sb.process.pid}")
    return {"fds": len(list((host / "fd").iterdir())),
            "threads": len(list((host / "task").iterdir())), "status": (host / "status").read_text()}


def validate_blocked(samples, baseline):
    start = next((index for index, row in enumerate(samples) if metric(row["live"], "gauges", "filesystem.disk-0")["status"] != "ok"), len(samples))
    faults = samples[start:]
    assert faults, "selected disk never blocked"
    first = metric(faults[0]["live"], "gauges", "filesystem.disk-0")
    previous = metric(samples[start-1]["live"], "gauges", "filesystem.disk-0") if start else baseline
    for field in ("covered_total_ns", "integral_total_byte_ns", "last_value_bytes"):
        assert first[field] == previous[field], "first failure changed known usage"
    assert first["status"] == "timeout", first
    assert len(faults) >= 10, "too few distinct blocked requests"
    for index, row in enumerate(faults):
        disk = metric(row["live"], "gauges", "filesystem.disk-0")
        assert int(disk["last_request_id"]) == int(first["last_request_id"]) + index, "unobserved fault round"
        assert disk["status"] == ("timeout" if index == 0 else "busy"), disk
        assert not disk["continuous"]
        assert disk["covered_total_ns"] == first["covered_total_ns"], disk
        assert disk["integral_total_byte_ns"] == first["integral_total_byte_ns"], disk
        assert disk["last_value_bytes"] == first["last_value_bytes"], "missing disk was filled or replayed"
        for name in ("guest.memory", "filesystem.root", "filesystem.disk-1", "ch.rss_anon", "ch.rss_file", "sandbox_ctl.rss_anon", "sandbox_ctl.rss_file"):
            gauge = metric(row["live"], "gauges", name)
            assert gauge["status"] == "ok", (name, gauge)
    assert int(metric(faults[-1]["live"], "gauges", "filesystem.disk-0")["span_total_ns"]) > int(first["span_total_ns"])
    for name in ("guest.memory", "filesystem.root", "filesystem.disk-1", "ch.rss_anon", "ch.rss_file", "sandbox_ctl.rss_anon", "sandbox_ctl.rss_file"):
        start = metric(faults[0]["live"], "gauges", name)
        end = metric(faults[-1]["live"], "gauges", name)
        assert int(end["covered_total_ns"]) - int(start["covered_total_ns"]) >= 8_000_000_000, name
        assert int(end["last_request_id"]) > int(start["last_request_id"]), name
    return faults


def validate_recovery(samples, last, baseline, expected):
    valid = 0
    for row in samples:
        disk = metric(row["live"], "gauges", "filesystem.disk-0")
        assert int(disk["last_request_id"]) == last + 1, "unobserved recovery round"
        last += 1
        assert disk["status"] in ("busy", "ok"), disk
        if disk["status"] == "ok":
            assert int(disk["last_value_bytes"]) == expected, "late raw buffer was replayed"
            if valid == 0:
                for field in ("covered_total_ns", "integral_total_byte_ns"):
                    assert disk[field] == baseline[field], "missing interval was integrated"
            valid += 1
    assert valid >= 2, "fresh sampling did not resume"


def filesystem_config(work, name, ref):
    diff = work / f"{name}-root.ext4"
    ext4(diff)
    config = {"resources": {"capacity": {"cpu": 1, "memory": "512MiB"},
                            "allocatable": {"cpu": 1, "memory": "512MiB", "deflate_on_oom": False}},
              "boot": {"kernel": f"file://{BIN / 'vmlinux'}", "runtime": f"file://{BIN / 'sandbox-runtime.bundle'}",
                       "root": {"base": ref, "overlay": {"diff": f"file://{diff}"}}, "disks": []},
              "mounts": [], "launch": {"exec": "/probe", "args": ["wait"], "restart": "never", "pid_namespace": "shared"},
              "usage": {"enabled": True, "sample_interval": "1s", "flush_interval": "5s"}}
    for ordinal, target in enumerate(("/data", "/other")):
        disk = work / f"{name}-data-{ordinal}.ext4"
        ext4(disk)
        config["boot"]["disks"].append({"name": f"data{ordinal}", "diff": f"file://{disk}"})
        config["mounts"].append({"type": "disk", "source": f"data{ordinal}", "target": target})
    return config


def reap_source_process(process):
    if process is None or process.poll() is not None:
        return
    try:
        process.terminate()
    finally:
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            try:
                process.kill()
            finally:
                process.wait(timeout=5)


def cleanup_sources(sb, trace=None, processes=(), relay=None, failure=None, owned_trace=False):
    # Fixed test-owned processes, not an unbounded cleanup queue. Preserve the
    # case's explicit failure; an unrelated enclosing except is not that failure.
    errors = []

    def attempt(action):
        try:
            action()
        except BaseException as error:
            errors.append(error)

    if owned_trace and sb.process.poll() is None:
        attempt(lambda: finish_owned_trace(sb))
    if trace is not None and trace.poll() is None:
        def detach():
            sb.cli("exec", "--", "/probe", "trace-stop")
            trace.wait(timeout=10)
        attempt(detach)
    for process in (trace, *processes):
        attempt(lambda: reap_source_process(process))
    attempt(sb.stop)
    if relay is not None:
        attempt(relay.stop)
        attempt(lambda: write_json(sb.dir / "relay.json", {"requests": relay.requests, "errors": relay.errors}))
    if errors:
        if failure is None:
            raise errors[0]
        for error in errors:
            print(f"usage source cleanup: {error}", file=sys.stderr)


def filesystem_case(work, ref, seconds):
    config = filesystem_config(work, "filesystem", ref)
    sb, trace, observer, release = Sandbox(work, "filesystem", config), None, None, None
    samples, observed_resources, failure = [], [], None
    try:
        sb.ready()
        guest = json.loads(sb.cli("exec", "--", "/probe", "inspect"))
        write_json(sb.dir / "guest.json", guest)
        assert guest["sandbox_init_sha256"] == digest(BIN / "sandbox-init"), "stale Guest runtime bundle"
        assert guest["balloon_proc_field"] == "false"
        assert "pagesets" in guest["zoneinfo"] and "count:" in guest["zoneinfo"]
        time.sleep(3)
        before = sb.view()
        for path in ("/data/payload", "/other/payload"):
            sb.cli("exec", "--", "/probe", "write", path, "8")
        time.sleep(3)
        grown = sb.view()
        write_json(sb.dir / "before.json", before)
        write_json(sb.dir / "grown.json", grown)
        for disk in ("filesystem.disk-0", "filesystem.disk-1"):
            assert int(metric(grown["live"], "gauges", disk)["last_value_bytes"]) - int(metric(before["live"], "gauges", disk)["last_value_bytes"]) >= 8 * 1024 * 1024
        with (sb.dir / "guest-trace.log").open("w") as output, (sb.dir / "guest-resources.log").open("w") as guest_output:
            trace = subprocess.Popen([str(BIN / "sandbox-ctl"), "exec", "--sandbox-id", sb.name,
                "--path-id", "instance", "--run-root", str(sb.runroot), "--", "/probe", "trace-fs", "/data", str((seconds+15)*1000)],
                stdout=output, stderr=subprocess.STDOUT)
            started, last, changed = time.monotonic(), 0, False
            while time.monotonic() - started < seconds:
                assert trace.poll() is None, f"Guest tracer exited {trace.returncode}"
                view = sb.view()
                disk = metric(view["live"], "gauges", "filesystem.disk-0")
                request = int(disk["last_request_id"])
                if request > last:
                    last = request
                    samples.append(view)
                    if disk["status"] == "busy" and not changed:
                        # The delayed syscall already contains the old value.
                        # New bytes distinguish a fresh post-fault read from
                        # replay of that old request's completed raw buffer.
                        sb.cli("exec", "--", "/probe", "write", "/data/after-read", "8")
                        sb.cli("exec", "--", "/probe", "write", "/other/during-fault", "8")
                        # A single exec observer avoids creating a new Go
                        # Process/pidfd for each measurement. Count all FDs.
                        observer = subprocess.Popen([str(BIN / "sandbox-ctl"), "exec", "--sandbox-id", sb.name,
                            "--path-id", "instance", "--run-root", str(sb.runroot), "--", "/probe", "init-resources-stream", str(seconds-5)],
                            stdout=guest_output, stderr=subprocess.STDOUT)
                        changed = True
                    if len(samples) % 5 == 0:
                        observed_resources.append(resources(sb))
                time.sleep(.1)
            assert changed
            write_json(sb.dir / "during.json", samples)
            write_json(sb.dir / "resources.json", observed_resources)
            faults = validate_blocked(samples, metric(grown["live"], "gauges", "filesystem.disk-0"))
            # The observer ends before the next ordinary exec opens transient
            # stdio/handshake FDs. Keep every recorded FD and the original bound.
            assert observer.poll() == 0, "resource observer outlived its fault-only window"
            raw = json.loads(sb.cli("exec", "--", "/probe", "statfs", "/data"))
            expected = (raw["Blocks"] - raw["Bfree"]) * (raw["Frsize"] or raw["Bsize"])
            write_json(sb.dir / "current-statfs.json", raw)
            assert expected > int(metric(faults[-1]["live"], "gauges", "filesystem.disk-0")["last_value_bytes"])
            # Keep sampling while the stop request and tracer detach run.
            # Waiting for either first would miss a transient replayed round.
            with (sb.dir / "release.log").open("w") as release_output:
                release = subprocess.Popen([str(BIN / "sandbox-ctl"), "exec", "--sandbox-id", sb.name,
                    "--path-id", "instance", "--run-root", str(sb.runroot), "--", "/probe", "trace-stop"],
                    stdout=release_output, stderr=subprocess.STDOUT)
                recovery_start = last
                recovered, end = [], time.monotonic() + 5
                while time.monotonic() < end:
                    view = sb.view()
                    disk = metric(view["live"], "gauges", "filesystem.disk-0")
                    if int(disk["last_request_id"]) > last:
                        last = int(disk["last_request_id"])
                        recovered.append(view)
                    time.sleep(.1)
                release.wait(timeout=5)
                assert release.returncode == 0
            trace.wait(timeout=5)
            assert trace.returncode == 0
        trace_text = (sb.dir / "guest-trace.log").read_text()
        assert len(re.findall(r"fstatfs\(\d+<", trace_text)) == 1, trace_text
        write_json(sb.dir / "recovered.json", recovered)
        validate_recovery(recovered, recovery_start, metric(faults[-1]["live"], "gauges", "filesystem.disk-0"), expected)
        observer.wait(timeout=10)
        assert observer.returncode == 0
        guest_resources = [json.loads(line) for line in (sb.dir / "guest-resources.log").read_text().splitlines()]
        for owner, rows in (("host", observed_resources), ("guest", guest_resources)):
            counts = [row["fds"] for row in rows]
            assert len(counts) >= 3 and max(counts) - min(counts) <= 4, (owner, counts)
        print(f"PASS usage-sources/filesystem ({len(faults)} timeout/busy rounds)", flush=True)
    except BaseException as error:
        failure = error
        raise
    finally:
        cleanup_sources(sb, trace, (release, observer), failure=failure)


def validate_wire_progress(baseline, observed):
    previous, in_gap = metric(baseline["live"], "gauges", "guest.memory"), False
    for view in observed:
        memory = metric(view["live"], "gauges", "guest.memory")
        if memory["status"] == "ok":
            if in_gap:
                for field in ("covered_total_ns", "integral_total_byte_ns"):
                    assert memory[field] == previous[field], "recovery integrated a disconnected interval"
            previous = memory
            in_gap = False
        else:
            in_gap = True
            for field in ("covered_total_ns", "integral_total_byte_ns", "last_value_bytes"):
                assert memory[field] == previous[field], "failed request filled or extended old memory"
    for name in ("ch.rss_anon", "ch.rss_file", "sandbox_ctl.rss_anon", "sandbox_ctl.rss_file"):
        before = metric(baseline["live"], "gauges", name)
        after = metric(observed[-1]["live"], "gauges", name)
        assert int(after["covered_total_ns"]) - int(before["covered_total_ns"]) >= 40_000_000_000, name
        assert int(after["last_request_id"]) - int(before["last_request_id"]) >= 40, name


def wait_observer_ready(observer, path):
    end = time.monotonic() + 5
    while time.monotonic() < end:
        assert observer.poll() is None, "resource observer exited before ready"
        text = path.read_text()
        if "\n" in text:
            assert json.loads(text.split("\n", 1)[0])["sequence"] == 0
            return
        time.sleep(.02)
    raise AssertionError("resource observer MUX did not become ready")


def wire_view_key(view):
    # Live queries can cut between the Manager's per-metric merges. Retain
    # changes to any metric, not only the first merged memory field.
    return tuple((g["name"], g["last_request_id"]) for g in view["live"]["gauges"])


def validate_wire_views(values, requests):
    by_id = {int(row["request"]["request_id"]): row for row in requests}
    for view in values:
        for name in ("guest.memory", "filesystem.root", "filesystem.disk-0", "filesystem.disk-1"):
            gauge = metric(view["live"], "gauges", name)
            request = int(gauge["last_request_id"])
            row = by_id.get(request)
            # An occupied Host slot can reject a tick without sending a new
            # request. Faulted frames also produce missing, never a new value.
            expected = "missing"
            if row is not None and not row["mode"]:
                assert row.get("sent_ns"), "unforwarded response became visible"
                response = row["response"]
                if response["type"] == "usage_response":
                    raw = response["usage_response"]
                    if name == "guest.memory":
                        expected = raw["memory"]["status"]
                    else:
                        expected = next(fs["status"] for fs in raw["filesystems"] if fs["disk"] == name.split(".")[1])
                else:
                    assert response == {"type": "error", "msg": "usage busy"}, response
            assert gauge["status"] == expected, (name, request, gauge["status"], expected)


def validate_wire_deadline(fault, requests):
    fault_id = int(fault["request"]["request_id"])
    later = [row for row in requests if int(row["request"]["request_id"]) > fault_id]
    assert later and later[0]["connection"] != fault["connection"]
    # Observe actual peer closure during the held reply. The next ticker can
    # see a Host read or Guest admission slot still exiting and correctly
    # report busy/missing;
    # a replacement request's start time is not the preceding read deadline.
    assert 0 < fault["client_closed_ns"] - fault["start_ns"] < 1_300_000_000
    assert later[0]["start_ns"] - fault["start_ns"] < 2_300_000_000
    if fault["mode"] == "delay":
        assert fault["delay_released_ns"] - fault["received_ns"] >= 1_300_000_000
        assert fault["client_closed_ns"] < fault["delay_released_ns"]
    else:
        assert len(fault["fragments_sent_ns"]) >= 2


def vsock_case(work, ref):
    sb = Sandbox(work, "vsock", filesystem_config(work, "vsock", ref),
                 ch_binary=Path(__file__).with_name("usage_vsock_wrapper.py"))
    relay, trace, observer, failure = None, None, None, None
    try:
        relay = UsageRelay(sb.runroot / "instance/vsock.sock")
        sb.ready()
        guest = json.loads(sb.cli("exec", "--", "/probe", "inspect"))
        write_json(sb.dir / "guest.json", guest)
        assert guest["sandbox_init_sha256"] == digest(BIN / "sandbox-init")
        time.sleep(3)
        with (sb.dir / "guest-trace.log").open("w") as output, (sb.dir / "guest-resources.log").open("w") as guest_output:
            trace = subprocess.Popen([str(BIN / "sandbox-ctl"), "exec", "--sandbox-id", sb.name,
                "--path-id", "instance", "--run-root", str(sb.runroot), "--", "/probe", "trace-fs", "/data", "85000"],
                stdout=output, stderr=subprocess.STDOUT)
            time.sleep(3)
            baseline = sb.view()
            write_json(sb.dir / "baseline.json", baseline)
            assert metric(baseline["live"], "gauges", "filesystem.disk-0")["status"] == "busy"
            # Test ordinary exec while the filesystem is busy, but outside the
            # all-FD observer window: its temporary stdio/handshake FDs are not
            # a reconnect leak. The observer MUX spans all eleven wire faults.
            sb.cli("exec", "--", "/probe", "true")
            observer = subprocess.Popen([str(BIN / "sandbox-ctl"), "exec", "--sandbox-id", sb.name,
                "--path-id", "instance", "--run-root", str(sb.runroot), "--", "/probe", "init-resources-stream", "61"],
                stdout=guest_output, stderr=subprocess.STDOUT)
            wait_observer_ready(observer, sb.dir / "guest-resources.log")
            observed, counts, faults = [], [], []
            for ordinal, mode in enumerate(["drop"]*8 + ["delay", "repeat", "fragment"]):
                relay.arm(mode)
                values, started, last = [], time.monotonic(), None
                while time.monotonic() - started < 5:
                    assert trace.poll() is None and observer.poll() is None
                    view = sb.view()
                    key = wire_view_key(view)
                    if key != last:
                        last = key
                        values.append(view)
                    time.sleep(.1)
                entries = [row for row in relay.requests if row["mode"]]
                assert len(entries) == ordinal + 1 and entries[-1]["mode"] == mode, "fault did not reach the dedicated usage connection"
                fault = entries[-1]
                faults.append(fault)
                write_json(sb.dir / f"fault-{ordinal}.json", {"fault": fault, "values": values})
                fault_id = int(fault["request"]["request_id"])
                assert fault.get("response", {}).get("type") == "usage_response", "fault did not intercept a real raw response"
                raw = fault["response"]["usage_response"]
                assert raw["request_id"] == str(fault_id) and raw["run_epoch"] == fault["request"]["run_epoch"]
                assert raw["memory"]["status"] == "ok"
                if mode == "repeat":
                    forwarded = fault["forwarded"]["usage_response"]
                    assert fault.get("sent_ns") and int(forwarded["request_id"]) < fault_id
                    assert forwarded["run_epoch"] == raw["run_epoch"]
                missing = [v for v in values if metric(v["live"], "gauges", "guest.memory")["status"] == "missing" and
                           (int(metric(v["live"], "gauges", "guest.memory")["last_request_id"]) == fault_id if mode in ("drop", "repeat") else
                            fault_id <= int(metric(v["live"], "gauges", "guest.memory")["last_request_id"]) <= fault_id+1)]
                assert missing, (mode, values)
                if mode != "drop":
                    assert fault.get("client_closed_ns"), "Host did not reject the fault connection"
                if mode in ("delay", "fragment"):
                    validate_wire_deadline(fault, relay.requests)
                validate_wire_views(values, relay.requests)
                assert metric(values[-1]["live"], "gauges", "guest.memory")["status"] == "ok", "connection did not recover with new data"
                for view in values:
                    disk = metric(view["live"], "gauges", "filesystem.disk-0")
                    assert disk["status"] in ("busy", "missing"), "reconnect replaced the blocked source"
                    for field in ("covered_total_ns", "integral_total_byte_ns", "last_value_bytes"):
                        assert disk[field] == metric(baseline["live"], "gauges", "filesystem.disk-0")[field]
                    for name in ("ch.rss_anon", "ch.rss_file", "sandbox_ctl.rss_anon", "sandbox_ctl.rss_file"):
                        assert metric(view["live"], "gauges", name)["status"] == "ok", name
                observed.extend(values)
                counts.append(resources(sb))
            write_json(sb.dir / "during.json", observed)
            write_json(sb.dir / "resources.json", counts)
            observer.wait(timeout=10)
            assert observer.returncode == 0
            assert metric(sb.view()["live"], "gauges", "filesystem.disk-0")["status"] == "busy"
            sb.cli("exec", "--", "/probe", "true")
            release_started = time.monotonic_ns()
            sb.cli("exec", "--", "/probe", "trace-stop")
            trace.wait(timeout=10)
            assert trace.returncode == 0
        text = (sb.dir / "guest-trace.log").read_text()
        assert len(re.findall(r"fstatfs\(\d+<", text)) == 1, text
        with relay.lock:
            requests = list(relay.requests)
        ids = [int(row["request"]["request_id"]) for row in requests]
        assert all(b > a for a, b in zip(ids, ids[1:])), ids
        assert len({row["request"]["run_epoch"] for row in requests}) == 1
        assert len({row["connection"] for row in requests}) >= len(faults)+1
        # Verify raw Guest slot state across all connections, not merely Host
        # missing statuses when a deliberately dropped response hides it.
        raw_disks = [next(fs for fs in row["response"]["usage_response"]["filesystems"] if fs["disk"] == "disk-0")
                     for row in requests if row.get("response", {}).get("type") == "usage_response" and
                     row["received_ns"] < release_started]
        first_timeout = next(i for i, fs in enumerate(raw_disks) if fs["status"] == "timeout")
        assert all(fs["status"] == "busy" for fs in raw_disks[first_timeout+1:]), "Guest replaced a busy worker on reconnect"
        host_counts = [row["fds"] for row in counts]
        assert max(host_counts) - min(host_counts) <= 4, host_counts
        guest_resources = [json.loads(line) for line in (sb.dir / "guest-resources.log").read_text().splitlines()]
        guest_counts = [row["fds"] for row in guest_resources]
        assert len(guest_counts) == 61 and max(guest_counts) - min(guest_counts) <= 4, guest_counts
        validate_wire_progress(baseline, observed)
        assert not relay.errors, relay.errors
        print("PASS usage-sources/vsock (11 real frame faults across one blocked filesystem slot)", flush=True)
    except BaseException as error:
        failure = error
        raise
    finally:
        cleanup_sources(sb, trace, (observer,), relay, failure)


def validate_ch_missing(samples, baseline):
    # The first two seconds can still legitimately reuse a fresh pre-fault
    # actual. Once stale, no target write or old actual can refresh it.
    steady = samples[3:]
    assert len(steady) >= 8
    for row in samples:
        memory = metric(row["live"], "gauges", "guest.memory")
        if memory["status"] == "ok":
            baseline = memory
        else:
            for field in ("covered_total_ns", "integral_total_byte_ns", "last_value_bytes"):
                assert memory[field] == baseline[field], "first CH failure changed known usage"
    baseline = metric(steady[0]["live"], "gauges", "guest.memory")
    for row in steady:
        memory = metric(row["live"], "gauges", "guest.memory")
        assert memory["status"] == "missing", memory
        for field in ("covered_total_ns", "integral_total_byte_ns", "last_value_bytes"):
            assert memory[field] == baseline[field], (field, memory)
        for name in ("filesystem.root", "ch.rss_anon", "ch.rss_file", "sandbox_ctl.rss_anon", "sandbox_ctl.rss_file"):
            assert metric(row["live"], "gauges", name)["status"] == "ok", (name, row)
    for name in ("filesystem.root", "ch.rss_anon", "ch.rss_file", "sandbox_ctl.rss_anon", "sandbox_ctl.rss_file"):
        first = metric(steady[0]["live"], "gauges", name)
        last = metric(steady[-1]["live"], "gauges", name)
        assert int(last["covered_total_ns"]) - int(first["covered_total_ns"]) >= 6_000_000_000, name


def finish_owned_trace(sb):
    sb.cli("exec", "--", "/probe", "trace-stop", timeout=3)
    end = time.monotonic() + 5
    while time.monotonic() < end:
        state = json.loads(sb.cli("exec", "--", "/probe", "trace-state", timeout=2))
        if state.get("usage-owned-done") == "detached":
            return state
        time.sleep(.1)
    raise AssertionError("snapshot-owned Guest tracer did not detach")


def validate_preserved_slot(baseline, view):
    disk = metric(view["live"], "gauges", "filesystem.disk-0")
    assert disk["status"] == "busy" and not disk["continuous"], "lifecycle replaced the occupied slot"
    for field in ("covered_total_ns", "integral_total_byte_ns", "last_value_bytes"):
        assert disk[field] == baseline[field], "lifecycle filled or extended the blocked source"
    for name in ("guest.memory", "filesystem.root", "filesystem.disk-1", "ch.rss_anon", "ch.rss_file", "sandbox_ctl.rss_anon", "sandbox_ctl.rss_file"):
        assert metric(view["live"], "gauges", name)["status"] == "ok", name


def validate_old_epoch(requests, old_epoch, new_epoch):
    faults = [row for row in requests if row["mode"] == "old-epoch"]
    assert len(faults) == 1
    fault = faults[0]
    raw = fault["response"]["usage_response"]
    forwarded = fault["forwarded"]["usage_response"]
    assert old_epoch != new_epoch and raw["run_epoch"] == new_epoch
    assert forwarded == {**raw, "run_epoch": old_epoch}, "fault changed more than the old run identity"
    assert raw["request_id"] == fault["request"]["request_id"]
    assert fault.get("sent_ns") and fault.get("client_closed_ns"), "Host did not reject the old-epoch connection"
    later = [row for row in requests if int(row["request"]["request_id"]) > int(raw["request_id"])]
    assert later and later[0]["connection"] != fault["connection"], "Host accepted the old epoch"
    ids = [int(row["request"]["request_id"]) for row in requests]
    assert all(b > a for a, b in zip(ids, ids[1:])), "restore reconnect reset request identity"
    for row in requests:
        assert row["request"]["run_epoch"] == new_epoch
        if row.get("response", {}).get("type") == "usage_response":
            response = row["response"]["usage_response"]
            assert response["memory"]["status"] == "ok"
            disk = next(fs for fs in response["filesystems"] if fs["disk"] == "disk-0")
            assert disk["status"] == "busy", "restore or reconnect created a replacement worker"


def restore_case(work, ref):
    config = filesystem_config(work, "restore-source", ref)
    sb = Sandbox(work, "restore-original", config, sandbox_id="restore-source")
    child, relay, release, failure, launched = None, None, None, None, False
    try:
        sb.ready()
        guest = json.loads(sb.cli("exec", "--", "/probe", "inspect"))
        write_json(sb.dir / "guest.json", guest)
        assert guest["sandbox_init_sha256"] == digest(BIN / "sandbox-init")
        assert guest["balloon_proc_field"] == "false" and "pagesets" in guest["zoneinfo"] and "count:" in guest["zoneinfo"]
        sb.cli("exec", "--", "/probe", "write", "/data/payload", "8")
        time.sleep(3)
        # This bounded test owner alone joins the disposable Guest's root
        # cgroup. Ordinary execs are correctly killed by quiesce; neither
        # PID 1 nor application/exec-join processes are moved by this test.
        launched = True
        launch = json.loads(sb.cli("exec", "--", "/probe", "trace-launch", "/data", "80000"))
        write_json(sb.dir / "trace-launch.json", launch)
        time.sleep(3)
        state = json.loads(sb.cli("exec", "--", "/probe", "trace-state"))
        write_json(sb.dir / "trace-start.json", state)
        owner_identity = state["usage-owned-ready.json"]
        assert json.loads(state["usage-owned-ready.json"])["owner_pid"] == launch["owner_pid"]
        assert "usage-owned-done" not in state
        before = sb.view()
        write_json(sb.dir / "before.json", before)
        baseline = metric(before["live"], "gauges", "filesystem.disk-0")
        validate_preserved_slot(baseline, before)
        sb.cli("exec", "--", "/probe", "write", "/data/after-read", "8")
        output = sb.dir / "snapshot"
        output.mkdir()
        capture = sb.cli("snapshot", "--output", output, "--resume", "--drop-caches=false", "--merge-ref=false", timeout=60)
        (sb.dir / "snapshot.log").write_text(capture)
        snapshot = output / "restore-source.snapshot"
        snapshot_hash = digest(snapshot)
        sb.cli("exec", "--", "/probe", "true")
        time.sleep(3)
        resumed = sb.view()
        write_json(sb.dir / "resumed.json", resumed)
        assert resumed["live"]["run_epoch"] == before["live"]["run_epoch"]
        validate_preserved_slot(baseline, resumed)
        write_json(sb.dir / "trace-done.json", finish_owned_trace(sb))
        sb.stop()
        parent = sb.view()
        write_json(sb.dir / "stopped.json", parent)
        old_epoch = parent["saved"]["snapshot"]["run_epoch"]
        host = {"resources": config["resources"],
                "boot": {"kernel": config["boot"]["kernel"], "runtime": config["boot"]["runtime"]},
                "restore": {"prefetch": "off"}, "usage": config["usage"], "timeouts": {"restore": "30s"}}
        child = Sandbox(work, "restore-child", host, restore=snapshot, sandbox_id=sb.name,
                        base_root=sb.baseroot, ch_binary=Path(__file__).with_name("usage_vsock_wrapper.py"))
        # CH restore reads the socket from private run-state config, not from
        # --vsock. The wrapper relays that socket without editing the snapshot.
        relay = UsageRelay(child.runroot / "instance/vsock.sock", old_epoch=old_epoch)
        child.ready()
        assert digest(snapshot) == snapshot_hash
        restored_guest = json.loads(child.cli("exec", "--", "/probe", "inspect"))
        write_json(child.dir / "guest.json", restored_guest)
        assert restored_guest["sandbox_init_sha256"] == guest["sandbox_init_sha256"]
        time.sleep(3)
        state = json.loads(child.cli("exec", "--", "/probe", "trace-state"))
        write_json(child.dir / "trace-restored.json", state)
        assert state["usage-owned-ready.json"] == owner_identity
        assert "usage-owned-done" not in state, "snapshot lost its blocked tracer"
        restored = child.view()
        write_json(child.dir / "restored.json", restored)
        new_epoch = restored["live"]["run_epoch"]
        assert new_epoch != old_epoch
        validate_preserved_slot(metric(parent["saved"]["snapshot"], "gauges", "filesystem.disk-0"), restored)
        with relay.lock:
            requests = list(relay.requests)
        validate_old_epoch(requests, old_epoch, new_epoch)
        child.cli("exec", "--", "/probe", "write", "/data/after-restore", "8")
        raw = json.loads(child.cli("exec", "--", "/probe", "statfs", "/data"))
        write_json(child.dir / "current-statfs.json", raw)
        expected = (raw["Blocks"] - raw["Bfree"]) * (raw["Frsize"] or raw["Bsize"])
        baseline = metric(child.view()["live"], "gauges", "filesystem.disk-0")
        assert baseline["status"] == "busy" and expected > int(baseline["last_value_bytes"])
        start_id = last = int(baseline["last_request_id"])
        with (child.dir / "release.log").open("w") as log:
            release = subprocess.Popen([str(BIN / "sandbox-ctl"), "exec", "--sandbox-id", child.name,
                "--path-id", "instance", "--run-root", str(child.runroot), "--", "/probe", "trace-stop"],
                stdout=log, stderr=subprocess.STDOUT)
            recovered, end = [], time.monotonic() + 5
            while time.monotonic() < end:
                view = child.view()
                request = int(metric(view["live"], "gauges", "filesystem.disk-0")["last_request_id"])
                if request > last:
                    recovered.append(view)
                    last = request
                time.sleep(.1)
            release.wait(timeout=5)
            assert release.returncode == 0
        write_json(child.dir / "recovered.json", recovered)
        validate_recovery(recovered, start_id, baseline, expected)
        state = json.loads(child.cli("exec", "--", "/probe", "trace-state"))
        write_json(child.dir / "trace-done.json", state)
        assert state.get("usage-owned-done") == "detached"
        assert len(re.findall(r"fstatfs\(\d+<", state["usage-owned-trace.log"])) == 1
        assert not relay.errors, relay.errors
        print("PASS usage-sources/restore (occupied slot across quiesce/lazy restore; old epoch rejected; fresh recovery)", flush=True)
    except BaseException as error:
        failure = error
        raise
    finally:
        try:
            if child is not None:
                cleanup_sources(child, processes=(release,), relay=relay, failure=failure, owned_trace=True)
        except BaseException as error:
            if failure is None:
                failure = error
            raise
        finally:
            cleanup_sources(sb, failure=failure, owned_trace=launched)


def validate_ch_recovery(samples, baseline):
    last, first_valid, valid = int(baseline["last_request_id"]), None, 0
    for row in samples:
        memory = metric(row["live"], "gauges", "guest.memory")
        assert int(memory["last_request_id"]) == last + 1, "unobserved CH recovery round"
        last += 1
        if memory["status"] == "ok":
            if first_valid is None:
                for field in ("covered_total_ns", "integral_total_byte_ns"):
                    assert memory[field] == baseline[field], "CH gap was integrated"
                first_valid = memory
            valid += 1
    assert valid >= 2
    final = metric(samples[-1]["live"], "gauges", "guest.memory")
    assert final["status"] == "ok"
    assert int(final["covered_total_ns"]) > int(first_valid["covered_total_ns"])


def ch_case(work, ref, mode):
    diff = work / f"ch-{mode}.ext4"
    ext4(diff)
    config = {"resources": {"capacity": {"cpu": 1, "memory": "512MiB"},
                            "allocatable": {"cpu": 1, "memory": "256MiB", "deflate_on_oom": False}},
              "boot": {"kernel": f"file://{BIN / 'vmlinux'}", "runtime": f"file://{BIN / 'sandbox-runtime.bundle'}",
                       "root": {"base": ref, "overlay": {"diff": f"file://{diff}"}}},
              "launch": {"exec": "/probe", "args": ["wait"], "restart": "never", "pid_namespace": "shared"},
              "usage": {"enabled": True, "sample_interval": "1s", "flush_interval": "5s"}}
    sb = Sandbox(work, f"ch-{mode}", config, ch_binary=Path(__file__).with_name("usage_ch_wrapper.py"))
    relay, pressure, failure = None, None, None
    try:
        relay = CHRelay(sb.runroot / "instance/ch.sock")
        sb.ready()
        guest = json.loads(sb.cli("exec", "--", "/probe", "inspect"))
        write_json(sb.dir / "guest.json", guest)
        assert guest["sandbox_init_sha256"] == digest(BIN / "sandbox-init")
        assert guest["balloon_proc_field"] == "false"
        # Wait for a real nonzero target with enough current memory for the
        # workload; do not race an assumed five-second controller phase.
        end = time.monotonic() + 15
        prior_info = None
        while time.monotonic() < end:
            info_reads = [row for row in relay.requests if row["path"] == "/api/v1/vm.info" and row.get("status") == 200]
            if info_reads:
                prior_info = json.loads(info_reads[-1]["response_body"])
                if prior_info["config"]["balloon"]["size"] >= 128 * 1024 * 1024 and prior_info["memory_actual_size"] >= 384 * 1024 * 1024:
                    break
            time.sleep(.05)
        else:
            raise AssertionError("controller did not reach the pressure-test baseline")
        before = sb.view()
        write_json(sb.dir / "before.json", before)
        write_json(sb.dir / "prior-ch-info.json", prior_info)
        prior_target = prior_info["config"]["balloon"]["size"]
        relay.arm("/api/v1/vm." + mode, after_resize=(mode == "info"), resize_below=prior_target)
        # Only normal Guest pressure drives this resize. Hold either its real
        # reply or the immediate confirming GET within the same existing
        # mutationGate + apiMu transaction, never an arbitrary racing GET.
        # The test itself never calls resize or changes actual/target.
        with (sb.dir / "pressure.log").open("w") as output:
            pressure_started = time.monotonic_ns()
            pressure = subprocess.Popen([str(BIN / "sandbox-ctl"), "exec", "--sandbox-id", sb.name,
                "--path-id", "instance", "--run-root", str(sb.runroot), "--", "/probe", "memory", "320", "18"],
                stdout=output, stderr=subprocess.STDOUT)
        assert relay.held.wait(timeout=15), "selected real CH reply was not reached"
        samples, counts = [], []
        for _ in range(12):
            samples.append(sb.view())
            counts.append(resources(sb))
            time.sleep(1)
        write_json(sb.dir / "during.json", samples)
        write_json(sb.dir / "resources.json", counts)
        validate_ch_missing(samples, metric(before["live"], "gauges", "guest.memory"))
        held = [row for row in relay.requests if row.get("held")]
        assert len(held) == 1
        assert held[0]["method"] == ("PUT" if mode == "resize" else "GET")
        assert 200 <= held[0]["status"] < 300
        resize = held[0]
        if mode == "info":
            index = relay.requests.index(held[0])
            previous = relay.requests[index-1]
            assert index > 0 and previous["method"] == "PUT" and previous["path"] == "/api/v1/vm.resize"
            assert 200 <= previous["status"] < 300
            resize = previous
        assert resize["start_ns"] >= pressure_started
        assert 0 <= json.loads(resize["request_body"])["desired_balloon"] < prior_target, "held resize was not a pressure-driven deflate"
        held_end = time.monotonic_ns()
        assert not [row for row in relay.requests if held[0]["received_ns"] < row["start_ns"] < held_end], "CH reads piled up behind the held transaction"
        fd_counts = [row["fds"] for row in counts]
        assert max(fd_counts) - min(fd_counts) <= 4, fd_counts
        sb.cli("exec", "--", "/probe", "write", "/tmp/during-ch-hold", "1")
        relay.release.set()
        recovered, end = [], time.monotonic() + 5
        last = int(metric(samples[-1]["live"], "gauges", "guest.memory")["last_request_id"])
        while time.monotonic() < end:
            view = sb.view()
            request = int(metric(view["live"], "gauges", "guest.memory")["last_request_id"])
            if request > last:
                recovered.append(view)
                last = request
            time.sleep(.1)
        write_json(sb.dir / "recovered.json", recovered)
        validate_ch_recovery(recovered, metric(samples[-1]["live"], "gauges", "guest.memory"))
        assert any(row["path"] == "/api/v1/vm.info" and row["start_ns"] > held_end and row.get("status") == 200 for row in relay.requests), "no new successful CH actual read after release"
        if pressure is not None:
            pressure.wait(timeout=10)
            assert pressure.returncode == 0, "pressure workload failed"
            assert "MEMORY-READY 335544320" in (sb.dir / "pressure.log").read_text()
        assert not relay.errors, relay.errors
        print(f"PASS usage-sources/ch-{mode} (real reply held for 12s)", flush=True)
    except BaseException as error:
        failure = error
        raise
    finally:
        if relay is not None:
            relay.release.set()
        cleanup_sources(sb, processes=(pressure,), relay=relay, failure=failure)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--seconds", type=int, default=30)
    parser.add_argument("--cases", default="filesystem,ch-info,ch-resize,vsock,restore")
    args = parser.parse_args()
    assert 20 <= args.seconds <= 60
    assert os.geteuid() == 0
    cases = args.cases.split(",")
    assert set(cases) <= {"filesystem", "ch-info", "ch-resize", "vsock", "restore"}
    work = Path(tempfile.mkdtemp(prefix="e2e-usage-sources-"))
    print(f"usage source evidence: {work}", flush=True)
    if os.environ.get("KUASAR_CI_DIR"):
        def collect():
            evidence = Path(os.environ["KUASAR_CI_DIR"]) / "usage-sources"
            for path in work.rglob("*"):
                if path.is_file() and path.suffix in (".json", ".log", ".usage"):
                    dest = evidence / path.relative_to(work)
                    dest.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copy2(path, dest)
        atexit.register(collect)
    root = work / "rootfs"
    root.mkdir()
    for name in ("tmp", "proc", "sys", "dev", "data", "other"):
        (root / name).mkdir()
    metadata = {"host_kernel": run("uname", "-a"), "seconds": args.seconds, "cases": cases,
                "artifacts": {name: digest(BIN / name) for name in ("sandbox-ctl", "sandbox-init", "sandbox-runtime.bundle", "vmlinux", "cloud-hypervisor")},
                "tracer": install_tracer(root)}
    write_json(work / "source-set.json", metadata)
    run("go", "build", "-trimpath", "-o", root / "probe", Path(__file__).parent / "usageprobe/main.go",
        env={**os.environ, "CGO_ENABLED": "0", "GOWORK": "off"})
    image = work / "root.img"
    run(BIN / "flatten-ctl", "export", "--output", image, "--no-progress", root)
    ref = image_ref(image)
    for case in cases:
        if case == "filesystem":
            filesystem_case(work, ref, args.seconds)
        elif case == "vsock":
            vsock_case(work, ref)
        elif case == "restore":
            restore_case(work, ref)
        else:
            ch_case(work, ref, case.removeprefix("ch-"))


if __name__ == "__main__":
    main()
