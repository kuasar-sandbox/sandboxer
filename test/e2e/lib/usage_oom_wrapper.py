#!/usr/bin/env python3
"""Test-only cold CH wrapper for observing unchanged Guest report delivery."""
import os
import sys

args = sys.argv[1:]
index = args.index("--vsock") + 1
parts = args[index].split(",")
for ordinal, part in enumerate(parts):
    if part.startswith("socket="):
        original = part.removeprefix("socket=")
        assert original.endswith("/vsock.sock")
        moved = original + ".oom"
        # Host->Guest remains a direct socket. Only Guest->Host LaunchPort
        # passes through the bounded observer; it changes no payload or ACK.
        os.symlink(moved, original)
        os.symlink(original + "_5000", moved + "_5000.real")
        parts[ordinal] = "socket=" + moved
args[index] = ",".join(parts)
binary = os.environ["USAGE_TEST_CH"]
os.execv(binary, [binary, *args])
