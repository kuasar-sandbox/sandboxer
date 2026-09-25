#!/usr/bin/env python3
"""Test-only CH exec wrapper: put Host-to-Guest vsock behind a wire relay."""
import json
import os
from pathlib import Path
import sys

args = sys.argv[1:]


def moved_socket(original):
    assert original.endswith("/vsock.sock"), "unexpected test vsock path"
    moved = original + ".real"
    # Guest->Host launch/mem_report remains the original listener.
    os.symlink(original + "_5000", moved + "_5000")
    return moved


if "--vsock" in args:
    index = args.index("--vsock") + 1
    parts = args[index].split(",")
    for ordinal, part in enumerate(parts):
        if part.startswith("socket="):
            original = part.removeprefix("socket=")
            parts[ordinal] = "socket=" + moved_socket(original)
    args[index] = ",".join(parts)
elif "--restore" in args:
    # Host-created private run state, not the immutable business snapshot.
    # CH restores its socket from config.json rather than a --vsock argument.
    argument = args[args.index("--restore") + 1]
    assert argument.startswith("source_url=file://")
    state = Path(argument.split(",", 1)[0].removeprefix("source_url=file://"))
    run = Path(args[args.index("--api-socket") + 1]).parent
    assert state == run / "snap-state", "unexpected disposable restore state"
    config = state / "config.json"
    value = json.loads(config.read_text())
    assert value["vsock"]["socket"] == str(run / "vsock.sock")
    value["vsock"]["socket"] = moved_socket(value["vsock"]["socket"])
    config.write_text(json.dumps(value))
binary = os.environ["USAGE_TEST_CH"]
os.execv(binary, [binary, *args])
