#!/usr/bin/env python3
"""Test-only CH exec wrapper: put Host-to-Guest vsock behind a wire relay."""
import json
import os
from pathlib import Path
import sys

from usage_vsock_relay import vsock_backend_path


def moved_socket(original):
    moved = str(vsock_backend_path(original))
    if os.path.lexists(moved):
        raise FileExistsError(f"test vsock backend already exists: {moved}")
    # Guest->Host launch/mem_report remains the original listener.
    alias = Path(moved + "_5000")
    os.symlink(original + "_5000", alias)  # Exclusive: never replace another owner.
    identity = alias.lstat()
    return moved, (alias, identity.st_dev, identity.st_ino)


def main():
    args, owned, restore_edit = sys.argv[1:], None, None
    try:
        if "--vsock" in args:
            index = args.index("--vsock") + 1
            parts = args[index].split(",")
            selected = [ordinal for ordinal, part in enumerate(parts) if part.startswith("socket=")]
            assert len(selected) == 1, "expected one test vsock endpoint"
            ordinal = selected[0]
            moved, owned = moved_socket(parts[ordinal].removeprefix("socket="))
            parts[ordinal] = "socket=" + moved
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
            original_config = config.read_text()
            value = json.loads(original_config)
            assert value["vsock"]["socket"] == str(run / "vsock.sock")
            value["vsock"]["socket"], owned = moved_socket(value["vsock"]["socket"])
            updated = json.dumps(value)
            config.write_text(updated)
            identity = config.stat()
            restore_edit = (config, identity.st_dev, identity.st_ino, original_config, updated)
        binary = os.environ["USAGE_TEST_CH"]
        os.execv(binary, [binary, *args])
    finally:
        # A successful exec replaces this process and never reaches this block.
        # On setup/exec failure remove only our alias, not a colliding/replaced
        # file. Successful-run endpoints remain in the owned case directory.
        if owned is not None:
            alias, device, inode = owned
            try:
                current = alias.lstat()
                if (current.st_dev, current.st_ino) == (device, inode):
                    alias.unlink()
            except FileNotFoundError:
                pass
        if restore_edit is not None:
            config, device, inode, original, updated = restore_edit
            try:
                current = config.stat()
                if (current.st_dev, current.st_ino) == (device, inode) and config.read_text() == updated:
                    config.write_text(original)
            except FileNotFoundError:
                pass


main()
