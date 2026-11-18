#!/usr/bin/env bash
# e2e-requires: ["kvm","userfaultfd"]
# Host source-status isolation, recovery, restored epochs and CH/vsock faults.
set -euo pipefail
source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}" "${WORK:?WORK is required}" "${OUT:?OUT is required}" "${USAGE_PROBE_BIN:?USAGE_PROBE_BIN is required}"
require_root
require_kvm
for command in python3 mount umount mkfs.ext4; do require_command "$command"; done
for binary in sandbox-ctl sandbox-init cloud-hypervisor flatten-ctl; do require_binary "$binary"; done
for file in sandbox-runtime.bundle vmlinux; do [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"; done
[ -x "$USAGE_PROBE_BIN" ] || e2e_fail "missing executable prepared usage probe"
for file in usage.py usage_ch_relay.py usage_vsock_relay.py usage_ch_wrapper.py usage_vsock_wrapper.py; do [ -f "$SANDBOXER_LIB/$file" ] || e2e_fail "missing prepared helper: $file"; done
export PYTHONDONTWRITEBYTECODE=1
python3 - "$SANDBOXER_LIB" <<'PY'
import sys
from pathlib import Path
LIB = Path(sys.argv[1])
sys.path.insert(0, str(LIB))
#!/usr/bin/env python3
"""Host source-status and transport faults on real Guest responses.

The relay changes one selected source status; it never blocks a Guest syscall.
Producer worker ownership and late completion are covered in guest-runtime.
"""
import atexit
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time

from usage import BIN, Sandbox, build_probe, digest, ext4, image_ref, metric, run, write_json, runtime_init_digest
from usage_ch_relay import CHRelay
from usage_vsock_relay import UsageRelay


def validate_source_injection(requests):
    """Prove the relay changes only one source in each actual Guest response."""
    faults = [row for row in requests if "source_fault" in row]
    assert faults, "no source-status fault was delivered"
    for row in faults:
        raw = row["response"]
        forwarded = row["source_forwarded"]
        selected = row["source_fault"]
        assert raw["type"] == forwarded["type"] == "usage_response"
        assert selected["status"] in ("timeout", "busy")
        response = raw["usage_response"]
        assert response["request_id"] == row["request"]["request_id"]
        assert response["run_epoch"] == row["request"]["run_epoch"]
        # Rebuild exactly the permitted change from the untouched Guest frame.
        expected = json.loads(json.dumps(raw))
        matches = [fs for fs in expected["usage_response"]["filesystems"] if fs["disk"] == selected["disk"]]
        assert len(matches) == 1
        source = matches[0]
        assert source["status"] == "ok", "injection concealed a real Guest source failure"
        assert "blocks" in source and "bfree" in source, "no actual Guest filesystem values"
        identity = {key: source[key] for key in ("disk", "incarnation") if key in source}
        source.clear()
        source.update(identity, status=selected["status"])
        assert forwarded == expected, "source injection changed another source or request identity"


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
            assert int(disk["last_value_bytes"]) == expected, "fresh filesystem value was not consumed"
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
                       "root": {"base": ref, "overlay": {"diff_template": f"file://{diff}"}}, "disks": []},
              "mounts": [], "launch": {"exec": "/probe", "args": ["wait"], "restart": "never", "pid_namespace": "shared"},
              "usage": {"enabled": True, "sample_interval": "1s", "flush_interval": "5s"}}
    for ordinal, target in enumerate(("/data", "/other")):
        disk = work / f"{name}-data-{ordinal}.ext4"
        ext4(disk)
        config["boot"]["disks"].append({"name": f"data{ordinal}", "diff_template": f"file://{disk}"})
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


def cleanup_sources(sb, processes=(), relay=None, failure=None):
    # Every auxiliary process and relay belongs to this disposable case.
    errors = []
    def attempt(action):
        try:
            action()
        except BaseException as error:
            errors.append(error)
    for process in processes:
        attempt(lambda process=process: reap_source_process(process))
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
    sb = Sandbox(work, "filesystem", config, ch_binary=LIB.joinpath("usage_vsock_wrapper.py"))
    relay, observer, failure = None, None, None
    samples, observed_resources = [], []
    try:
        relay = UsageRelay(sb.runroot / "instance/vsock.sock")
        sb.ready()
        guest = json.loads(sb.cli("exec", "--", "/probe", "inspect"))
        write_json(sb.dir / "guest.json", guest)
        assert guest["sandbox_init_sha256"] == runtime_init_digest(digest(BIN / "sandbox-init")), "stale Guest runtime bundle"
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
        relay.block_source()
        with (sb.dir / "guest-resources.log").open("w") as guest_output:
            started, last, changed = time.monotonic(), 0, False
            while time.monotonic() - started < seconds:
                assert not relay.errors, relay.errors
                view = sb.view()
                disk = metric(view["live"], "gauges", "filesystem.disk-0")
                request = int(disk["last_request_id"])
                if request > last:
                    last = request
                    samples.append(view)
                    if disk["status"] == "busy" and not changed:
                        sb.cli("exec", "--", "/probe", "write", "/data/after-read", "8")
                        sb.cli("exec", "--", "/probe", "write", "/other/during-fault", "8")
                        # This observes the user executor, not outer producer FDs.
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
            assert observer.poll() == 0, "resource observer outlived its fault-only window"
            raw = json.loads(sb.cli("exec", "--", "/probe", "statfs", "/data"))
            expected = (raw["Blocks"] - raw["Bfree"]) * (raw["Frsize"] or raw["Bsize"])
            write_json(sb.dir / "current-statfs.json", raw)
            assert expected > int(metric(faults[-1]["live"], "gauges", "filesystem.disk-0")["last_value_bytes"])
            assert not relay.errors, relay.errors
            recovery_view = sb.view()
            recovery_baseline = metric(recovery_view["live"], "gauges", "filesystem.disk-0")
            previous = metric(faults[-1]["live"], "gauges", "filesystem.disk-0")
            assert recovery_view["live"]["run_epoch"] == faults[-1]["live"]["run_epoch"], "filesystem recovery epoch changed"
            assert recovery_baseline["status"] == "busy" and not recovery_baseline["continuous"], "filesystem recovery baseline is not blocked"
            for field in ("covered_total_ns", "integral_total_byte_ns", "last_value_bytes"):
                assert recovery_baseline[field] == previous[field], "blocked filesystem recovery baseline changed"
            recovery_start = last = int(recovery_baseline["last_request_id"])
            assert recovery_start >= int(previous["last_request_id"]), "filesystem recovery request regressed"
            relay.release_source()
            recovered, end = [], time.monotonic() + 5
            while time.monotonic() < end:
                view = sb.view()
                disk = metric(view["live"], "gauges", "filesystem.disk-0")
                if int(disk["last_request_id"]) > last:
                    last = int(disk["last_request_id"])
                    recovered.append(view)
                time.sleep(.1)
        write_json(sb.dir / "recovery-baseline.json", recovery_view)
        write_json(sb.dir / "recovered.json", recovered)
        validate_recovery(recovered, recovery_start, recovery_baseline, expected)
        observer.wait(timeout=10)
        assert observer.returncode == 0
        guest_resources = [json.loads(line) for line in (sb.dir / "guest-resources.log").read_text().splitlines()]
        for owner, rows in (("host", observed_resources), ("user-executor", guest_resources)):
            counts = [row["fds"] for row in rows]
            assert len(counts) >= 3 and max(counts) - min(counts) <= 4, (owner, counts)
        validate_source_injection(relay.requests)
        assert not relay.errors, relay.errors
        print(f"PASS usage-sources/filesystem ({len(faults)} injected timeout/busy consumer rounds)", flush=True)
    except BaseException as error:
        failure = error
        raise
    finally:
        cleanup_sources(sb, processes=(observer,), relay=relay, failure=failure)


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
    for name in ("filesystem.root", "filesystem.disk-1"):
        before = metric(baseline["live"], "gauges", name)
        after = metric(observed[-1]["live"], "gauges", name)
        # Deliberate whole-response faults remove both the failed interval
        # and the first recovery interval. Healthy FS must still accumulate
        # substantial real coverage and reach recent requests, not freeze at
        # an old OK value that happened to match an earlier raw frame.
        assert after["status"] == "ok", name
        assert int(after["covered_total_ns"]) - int(before["covered_total_ns"]) >= 20_000_000_000, name
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
    for row in requests:
        response = row.get("response", {})
        if response.get("type") == "usage_response":
            for disk in ("root", "disk-1"):
                fs = next(fs for fs in response["usage_response"]["filesystems"] if fs["disk"] == disk)
                assert fs["status"] == "ok", (disk, row["request"], fs)
    for view in values:
        memory_id = int(metric(view["live"], "gauges", "guest.memory")["last_request_id"])
        for name in ("guest.memory", "filesystem.root", "filesystem.disk-0", "filesystem.disk-1"):
            gauge = metric(view["live"], "gauges", name)
            request = int(gauge["last_request_id"])
            assert 0 <= memory_id - request <= 1, (name, memory_id, request, "stale publication")
            row = by_id.get(request)
            # An occupied Host slot can reject a tick without sending a new
            # request. Faulted frames also produce missing, never a new value.
            expected = "missing"
            if row is not None and not row["mode"]:
                assert row.get("sent_ns"), "unforwarded response became visible"
                response = row.get("forwarded", row["response"])
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
                 ch_binary=LIB.joinpath("usage_vsock_wrapper.py"))
    relay, observer, failure = None, None, None
    try:
        relay = UsageRelay(sb.runroot / "instance/vsock.sock")
        sb.ready()
        guest = json.loads(sb.cli("exec", "--", "/probe", "inspect"))
        write_json(sb.dir / "guest.json", guest)
        assert guest["sandbox_init_sha256"] == runtime_init_digest(digest(BIN / "sandbox-init"))
        time.sleep(3)
        with (sb.dir / "guest-resources.log").open("w") as guest_output:
            relay.block_source()
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
                    assert observer.poll() is None and not relay.errors
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
                    assert disk["status"] in ("busy", "missing"), "Host lost the injected source fault across reconnect"
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
            relay.release_source()
        with relay.lock:
            requests = list(relay.requests)
        ids = [int(row["request"]["request_id"]) for row in requests]
        assert all(b > a for a, b in zip(ids, ids[1:])), ids
        assert len({row["request"]["run_epoch"] for row in requests}) == 1
        assert len({row["connection"] for row in requests}) >= len(faults)+1
        # Verify source-status injection persisted across all connections,
        # including responses subsequently dropped by a transport fault.
        injected_disks = [next(fs for fs in row["source_forwarded"]["usage_response"]["filesystems"] if fs["disk"] == "disk-0")
                          for row in requests if "source_fault" in row]
        assert injected_disks[0]["status"] == "timeout"
        assert all(fs["status"] == "busy" for fs in injected_disks[1:]), "source-status injection changed across reconnect"
        host_counts = [row["fds"] for row in counts]
        assert max(host_counts) - min(host_counts) <= 4, host_counts
        guest_resources = [json.loads(line) for line in (sb.dir / "guest-resources.log").read_text().splitlines()]
        guest_counts = [row["fds"] for row in guest_resources]
        assert len(guest_counts) == 61 and max(guest_counts) - min(guest_counts) <= 4, guest_counts
        validate_wire_progress(baseline, observed)
        assert not relay.errors, relay.errors
        validate_source_injection(requests)
        print("PASS usage-sources/vsock (11 real frame faults with explicit source-status injection)", flush=True)
    except BaseException as error:
        failure = error
        raise
    finally:
        cleanup_sources(sb, processes=(observer,), relay=relay, failure=failure)


def observe_ch_hold(sb, relay, before):
    # Follow every usage round while pressure reaches the selected real reply,
    # then throughout the hold. A wait here could hide the last healthy update.
    observed, samples, counts = [], [], []
    deadline, held_at = time.monotonic() + 15, None
    while True:
        view = sb.view()
        assert view["live"]["run_epoch"] == before["live"]["run_epoch"], "CH hold observation epoch changed"
        observed.append(view)
        now = time.monotonic()
        if relay.held.is_set():
            if held_at is None:
                held_at = now
            if len(samples) < 12 and now - held_at >= len(samples):
                samples.append(view)
                counts.append(resources(sb))
            if now - held_at >= 12:
                assert len(samples) == 12
                return observed, samples, counts
        else:
            assert now < deadline, "selected real CH reply was not reached"
        time.sleep(.1)


def validate_ch_missing(samples, baseline, observed=None):
    # The first two seconds can still legitimately reuse a fresh pre-fault
    # actual. Once stale, no target write or old actual can refresh it.
    steady = samples[3:]
    assert len(steady) >= 8
    previous_id = int(baseline["last_request_id"])
    for row in samples if observed is None else observed:
        memory = metric(row["live"], "gauges", "guest.memory")
        request = int(memory["last_request_id"])
        assert 0 <= request - previous_id <= 1, ("unobserved CH fault round", previous_id, request)
        if memory["status"] == "ok":
            baseline = memory
        else:
            for field in ("covered_total_ns", "integral_total_byte_ns", "last_value_bytes"):
                assert memory[field] == baseline[field], ("first CH failure changed known usage", field,
                    previous_id, request, baseline[field], memory[field])
        previous_id = request
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




def validate_preserved_slot(baseline, view):
    disk = metric(view["live"], "gauges", "filesystem.disk-0")
    assert disk["status"] == "busy" and not disk["continuous"], "lifecycle lost the injected busy source"
    for field in ("covered_total_ns", "integral_total_byte_ns", "last_value_bytes"):
        assert disk[field] == baseline[field], "lifecycle filled or extended the unavailable source"
    for name in ("guest.memory", "filesystem.root", "filesystem.disk-1", "ch.rss_anon", "ch.rss_file", "sandbox_ctl.rss_anon", "sandbox_ctl.rss_file"):
        assert metric(view["live"], "gauges", name)["status"] == "ok", name


def validate_capture_failure(capture, log):
    # Predictable dependency failures occur before quiesce and cannot establish
    # rollback. This specific error is emitted only inside capture after Pause.
    assert capture["returncode"] != 0, "snapshot unexpectedly succeeded"
    message = capture["stdout"] + capture["stderr"]
    assert "export data disk 0: pack overlay:" in message, message
    assert "no space left on device" in message, message
    assert "snapshot dependencies:" not in message, message
    assert "quiesce: guest acked" in log and "proceeding to /vm.pause" in log
    assert "stdio MUX re-attached after failed snapshot" in log


def capture_enospc(sb, work):
    output = sb.dir / "limited-snapshot"
    output.mkdir()
    # The immutable root image is emitted before quiesce. Allow its actual
    # carrier plus bounded slack, but not the 16 MiB written to disk-0. This
    # derives the allowance for the installed architecture's probe.
    image_bytes = (work / "root.img").stat().st_size
    assert image_bytes <= 64 * 1024 * 1024, "unexpectedly large test root image"
    limit = ((image_bytes + 4095) // 4096) * 4096 + 4 * 1024 * 1024
    run("mount", "-t", "tmpfs", "-o", f"size={limit},mode=700,nosuid,nodev,noexec", "tmpfs", output)
    failure = None
    try:
        log_start = (sb.dir / "run.log").stat().st_size
        started = time.monotonic_ns()
        try:
            stdout = sb.cli("snapshot", "--output", output, "--resume", "--drop-caches=false", "--merge-ref=false", timeout=60)
            capture = {"returncode": 0, "stdout": stdout, "stderr": ""}
        except subprocess.CalledProcessError as error:
            capture = {"returncode": error.returncode, "stdout": error.stdout, "stderr": error.stderr}
        capture.update(elapsed_ns=time.monotonic_ns() - started, output_limit_bytes=limit,
                       output_statvfs=list(os.statvfs(output)),
                       remaining_files=[path.name for path in output.iterdir()])
        with (sb.dir / "run.log").open("rb") as log:
            log.seek(log_start)
            capture_log = log.read().decode()
        write_json(sb.dir / "capture-failure.json", capture)
        (sb.dir / "capture-failure.log").write_text(capture_log)
        validate_capture_failure(capture, capture_log)
    except BaseException as error:
        failure = error
        raise
    finally:
        try:
            run("umount", output)
        except BaseException as error:
            if failure is None:
                raise
            print(f"usage source output unmount: {error}", file=sys.stderr)


def validate_old_epoch(requests, old_epoch, new_epoch):
    faults = [row for row in requests if row["mode"] == "old-epoch"]
    assert len(faults) == 1
    fault = faults[0]
    raw = fault["response"]["usage_response"]
    forwarded = fault["forwarded"]["usage_response"]
    assert old_epoch != new_epoch and raw["run_epoch"] == new_epoch
    source = fault["source_forwarded"]["usage_response"]
    assert forwarded == {**source, "run_epoch": old_epoch}, "epoch fault changed more than the old run identity"
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
            assert disk["status"] == "ok", "real Guest source failed during restore"
            injected = next(fs for fs in row["source_forwarded"]["usage_response"]["filesystems"] if fs["disk"] == "disk-0")
            assert injected["status"] == "busy", "Host source-status injection was lost across reconnect"


def dump_restore_failure(phase, *sandboxes):
    """Keep bounded owned logs in Actions even when the ephemeral case is removed."""
    print(f"usage restore failure: phase={phase}", file=sys.stderr)
    for sb in sandboxes:
        if sb is None:
            continue
        for name in ("run.log", "capture-failure.log", "snapshot.log",
                     "relay.json", "before.json", "restored.json"):
            path = sb.dir / name
            try:
                with path.open("rb") as source:
                    source.seek(0, 2)
                    size = source.tell()
                    offset = max(0, size - 4096)
                    source.seek(offset)
                    tail = source.read(4096).decode(errors="replace")
                if offset:
                    tail = tail.partition("\n")[2]
                tail = re.sub(r"(?i)[0-9a-f]{64}", "<redacted>", tail)
                print(f"usage restore diagnostic {sb.dir.name}/{name} bytes={size}", file=sys.stderr)
                print("\n".join(tail.splitlines()[-20:]), file=sys.stderr)
            except OSError:
                print(f"usage restore diagnostic {sb.dir.name}/{name}: unavailable", file=sys.stderr)


def cleanup_restore_sources(sb, child, source_relay, relay, failure):
    """Retain evidence when cleanup, rather than the case body, first fails."""
    try:
        if child is not None:
            cleanup_sources(child, relay=relay, failure=failure)
    except BaseException as error:
        if failure is None:
            failure = error
            dump_restore_failure("child-cleanup", sb, child)
        raise
    finally:
        try:
            cleanup_sources(sb, relay=source_relay, failure=failure)
        except BaseException:
            if failure is None:
                dump_restore_failure("source-cleanup", sb, child)
            raise


def restore_case(work, ref):
    config = filesystem_config(work, "restore-source", ref)
    sb = Sandbox(work, "restore-original", config, sandbox_id="restore-source",
                 ch_binary=LIB.joinpath("usage_vsock_wrapper.py"))
    child, source_relay, relay, failure = None, None, None, None
    phase = "source-ready"
    try:
        source_relay = UsageRelay(sb.runroot / "instance/vsock.sock")
        sb.ready()
        guest = json.loads(sb.cli("exec", "--", "/probe", "inspect"))
        write_json(sb.dir / "guest.json", guest)
        assert guest["sandbox_init_sha256"] == runtime_init_digest(digest(BIN / "sandbox-init"))
        assert guest["balloon_proc_field"] == "false" and "pagesets" in guest["zoneinfo"] and "count:" in guest["zoneinfo"]
        sb.cli("exec", "--", "/probe", "write", "/data/payload", "8")
        time.sleep(3)
        source_relay.block_source()
        time.sleep(3)
        before = sb.view()
        write_json(sb.dir / "before.json", before)
        baseline = metric(before["live"], "gauges", "filesystem.disk-0")
        validate_preserved_slot(baseline, before)
        sb.cli("exec", "--", "/probe", "write", "/data/after-read", "8")
        phase = "capture-rollback"
        capture_enospc(sb, work)
        sb.cli("exec", "--", "/probe", "true")
        info = sb.ch_info()
        write_json(sb.dir / "post-failure-ch.json", info)
        assert info["state"] == "Running", info
        time.sleep(3)
        rolled_back = sb.view()
        write_json(sb.dir / "rolled-back.json", rolled_back)
        assert rolled_back["live"]["run_epoch"] == before["live"]["run_epoch"]
        validate_preserved_slot(baseline, rolled_back)
        assert int(metric(rolled_back["live"], "gauges", "filesystem.disk-0")["last_request_id"]) > int(baseline["last_request_id"])
        for name in ("guest.memory", "filesystem.root", "filesystem.disk-1"):
            assert int(metric(rolled_back["live"], "gauges", name)["covered_total_ns"]) > int(metric(before["live"], "gauges", name)["covered_total_ns"]), name
        output = sb.dir / "snapshot"
        output.mkdir()
        phase = "source-snapshot"
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
        sb.stop()
        parent = sb.view()
        write_json(sb.dir / "stopped.json", parent)
        old_epoch = parent["saved"]["snapshot"]["run_epoch"]
        host = {"resources": config["resources"],
                "boot": {"kernel": config["boot"]["kernel"], "runtime": config["boot"]["runtime"]},
                "restore": {"prefetch": "off"}, "usage": config["usage"], "timeouts": {"restore": "30s"}}
        child = Sandbox(work, "restore-child", host, restore=snapshot, sandbox_id=sb.name,
                        base_root=sb.baseroot, ch_binary=LIB.joinpath("usage_vsock_wrapper.py"))
        relay = UsageRelay(child.runroot / "instance/vsock.sock", old_epoch=old_epoch)
        # This fault state belongs to the Host test relay. It is deliberately
        # re-established, not claimed to be a Guest worker saved in memory.
        relay.block_source(first_status="busy")
        phase = "child-restore"
        child.ready()
        assert digest(snapshot) == snapshot_hash
        restored_guest = json.loads(child.cli("exec", "--", "/probe", "inspect"))
        write_json(child.dir / "guest.json", restored_guest)
        assert restored_guest["sandbox_init_sha256"] == guest["sandbox_init_sha256"]
        time.sleep(3)
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
        phase = "child-source-recovery"
        relay.release_source()
        recovered, end = [], time.monotonic() + 5
        while time.monotonic() < end:
            view = child.view()
            request = int(metric(view["live"], "gauges", "filesystem.disk-0")["last_request_id"])
            if request > last:
                recovered.append(view)
                last = request
            time.sleep(.1)
        write_json(child.dir / "recovered.json", recovered)
        validate_recovery(recovered, start_id, baseline, expected)
        validate_source_injection(source_relay.requests)
        validate_source_injection(relay.requests)
        assert not relay.errors and not source_relay.errors, (relay.errors, source_relay.errors)
        print("PASS usage-sources/restore (consumer gap across capture rollback/lazy restore; old epoch rejected; fresh raw recovery)", flush=True)
    except BaseException as error:
        failure = error
        dump_restore_failure(phase, sb, child)
        raise
    finally:
        cleanup_restore_sources(sb, child, source_relay, relay, failure)


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
                       "root": {"base": ref, "overlay": {"diff_template": f"file://{diff}"}}},
              "launch": {"exec": "/probe", "args": ["wait"], "restart": "never", "pid_namespace": "shared"},
              "usage": {"enabled": True, "sample_interval": "1s", "flush_interval": "5s"}}
    sb = Sandbox(work, f"ch-{mode}", config, ch_binary=LIB.joinpath("usage_ch_wrapper.py"))
    relay, pressure, failure = None, None, None
    try:
        relay = CHRelay(sb.runroot / "instance/ch.sock")
        sb.ready()
        guest = json.loads(sb.cli("exec", "--", "/probe", "inspect"))
        write_json(sb.dir / "guest.json", guest)
        assert guest["sandbox_init_sha256"] == runtime_init_digest(digest(BIN / "sandbox-init"))
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
        observed, samples, counts = observe_ch_hold(sb, relay, before)
        write_json(sb.dir / "ch-transition.json", observed)
        write_json(sb.dir / "during.json", samples)
        write_json(sb.dir / "resources.json", counts)
        validate_ch_missing(samples, metric(before["live"], "gauges", "guest.memory"), observed)
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
        fd_counts = [row["fds"] for row in counts]
        assert max(fd_counts) - min(fd_counts) <= 4, fd_counts
        sb.cli("exec", "--", "/probe", "write", "/tmp/during-ch-hold", "1")
        # Synchronous checks can span more held rounds after the final sample.
        # Re-anchor while still held, then observe recovery without that gap.
        recovery_view = sb.view()
        recovery_baseline = metric(recovery_view["live"], "gauges", "guest.memory")
        previous = metric(samples[-1]["live"], "gauges", "guest.memory")
        assert recovery_view["live"]["run_epoch"] == samples[-1]["live"]["run_epoch"], "CH recovery epoch changed"
        assert recovery_baseline["status"] == "missing" and not recovery_baseline["continuous"], "CH recovery baseline is not held"
        assert int(recovery_baseline["last_request_id"]) >= int(previous["last_request_id"]), "CH recovery request regressed"
        for field in ("covered_total_ns", "integral_total_byte_ns", "last_value_bytes"):
            assert recovery_baseline[field] == previous[field], "held CH recovery baseline changed"
        held_end = time.monotonic_ns()
        assert not [row for row in relay.requests if held[0]["received_ns"] < row["start_ns"] < held_end], "CH reads piled up behind the held transaction"
        relay.release.set()
        recovered, end = [], time.monotonic() + 5
        last = int(recovery_baseline["last_request_id"])
        while time.monotonic() < end:
            view = sb.view()
            request = int(metric(view["live"], "gauges", "guest.memory")["last_request_id"])
            if request > last:
                recovered.append(view)
                last = request
            time.sleep(.1)
        write_json(sb.dir / "recovery-baseline.json", recovery_view)
        write_json(sb.dir / "recovered.json", recovered)
        validate_ch_recovery(recovered, recovery_baseline)
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
    seconds = 30
    cases = ("filesystem", "ch-info", "ch-resize", "vsock", "restore")
    work = Path(os.environ["WORK"]) / "usage-sources"
    work.mkdir(parents=True, exist_ok=True)
    print(f"usage source evidence: {work}", flush=True)
    if os.environ["OUT"]:
        def collect():
            evidence = Path(os.environ["OUT"]) / "usage-sources"
            for path in work.rglob("*"):
                if path.is_file() and path.suffix in (".json", ".log", ".usage"):
                    dest = evidence / path.relative_to(work)
                    dest.parent.mkdir(parents=True, exist_ok=True)
                    # The suite runs as root; do not preserve private source modes in CI evidence.
                    shutil.copyfile(path, dest)
        atexit.register(collect)
    root = work / "rootfs"
    root.mkdir()
    for name in ("tmp", "proc", "sys", "dev", "data", "other"):
        (root / name).mkdir()
    metadata = {"host_kernel": run("uname", "-a"), "seconds": seconds, "cases": cases,
                "artifacts": {name: digest(BIN / name) for name in ("sandbox-ctl", "sandbox-init", "sandbox-runtime.bundle", "vmlinux", "cloud-hypervisor")},
                "fault_boundary": "Host relay source-status injection; Guest producer tested in guest-runtime"}
    write_json(work / "source-set.json", metadata)
    build_probe(root / "probe")
    image = work / "root.img"
    run(BIN / "flatten-ctl", "export", "--output", image, "--no-progress", root)
    ref = image_ref(image)
    for case in cases:
        if case == "filesystem":
            filesystem_case(work, ref, seconds)
        elif case == "vsock":
            vsock_case(work, ref)
        elif case == "restore":
            restore_case(work, ref)
        else:
            ch_case(work, ref, case.removeprefix("ch-"))


if __name__ == "__main__":
    main()
PY
