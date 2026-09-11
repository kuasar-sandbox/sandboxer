#!/usr/bin/env python3
"""Test-only CH exec wrapper: put Host-to-Guest vsock behind a wire relay."""
import os
import sys

args = sys.argv[1:]
if "--vsock" in args:
    index = args.index("--vsock") + 1
    parts = args[index].split(",")
    for ordinal, part in enumerate(parts):
        if part.startswith("socket="):
            original = part.removeprefix("socket=")
            assert original.endswith("/vsock.sock"), "unexpected test vsock path"
            moved = original + ".real"
            parts[ordinal] = "socket=" + moved
            # Guest->Host launch/mem_report remains the original listener.
            os.symlink(original + "_5000", moved + "_5000")
    args[index] = ",".join(parts)
binary = os.environ["USAGE_TEST_CH"]
os.execv(binary, [binary, *args])
