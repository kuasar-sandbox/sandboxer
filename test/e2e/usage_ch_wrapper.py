#!/usr/bin/env python3
"""Test-only exec wrapper: move the real CH API behind a delaying Unix relay."""
import os
import sys

args = sys.argv[1:]
if "--api-socket" in args:
    index = args.index("--api-socket") + 1
    assert args[index].endswith("/ch.sock"), "unexpected test CH API path"
    args[index] += ".real"
binary = os.environ["USAGE_TEST_CH"]
os.execv(binary, [binary, *args])
