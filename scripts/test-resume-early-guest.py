#!/usr/bin/env python3
"""Real Linux guest tests for early-resume snapshots; no existing services are modified."""

import argparse
import concurrent.futures
import copy
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import threading
import time


WORKLOAD = """import os, secrets, sys, threading, time
nonce = secrets.token_hex(16)
def writer():
    for line in sys.stdin:
        if line.strip() == 'MUTATE':
            with open('/snapshot-busy', 'r+b') as f:
                f.write(b'B' * (32 << 20))
                f.flush()
                os.fsync(f.fileno())
            print('MUTATION_DONE', flush=True)
threading.Thread(target=writer, daemon=True).start()
i = 0
while True:
    i += 1
    print('TICK', nonce, i, flush=True)
    time.sleep(.02)
"""


def require(condition, message):
    # Assertions must remain enabled even under python -O.
    if not condition:
        raise RuntimeError(message)


def write_marker(value):
    return ("import os; f=open('/snapshot-state','w'); f.write(" + repr(value)
            + "); f.flush(); os.fsync(f.fileno()); f.close()")


class Guest:
    def __init__(self, suite, config, reference=None):
        self.suite = suite
        self.sid = 'snap-test-' + secrets.token_hex(6)
        self.lines = []
        self.changed = threading.Condition()
        self.closed = False
        config_path = suite.output / (self.sid + '.json')
        config_path.write_text(json.dumps(config, indent=2))
        self.log = (suite.output / (self.sid + '.log')).open('w')
        command = [suite.binary / 'sandbox-ctl', 'run', '--config', config_path,
                   '--manifest-config', suite.manifest, '--sandbox-id', self.sid,
                   '--run-root', suite.run_root, '--base-root', suite.output / 'base',
                   '--ch-binary', suite.binary / 'cloud-hypervisor',
                   '--enable-checkpoint', '--stdin', '--stats-interval', '0']
        if reference:
            command += ['--restore', reference]
        try:
            self.process = subprocess.Popen(
                list(map(str, command)), stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT, text=True, bufsize=1, env=suite.env)
        except BaseException:
            self.log.close()
            raise
        suite.guests.append(self)
        self.reader = threading.Thread(target=self.read, daemon=True)
        self.reader.start()
        self.first_tick = self.wait_line(lambda line: line.startswith('TICK '))

    def read(self):
        try:
            for line in self.process.stdout:
                observed = time.monotonic_ns()
                self.log.write(line)
                self.log.flush()
                with self.changed:
                    self.lines.append((observed, line.rstrip()))
                    self.changed.notify_all()
        finally:
            with self.changed:
                self.changed.notify_all()

    def wait_line(self, predicate, since=0):
        deadline = time.monotonic() + self.suite.timeout
        with self.changed:
            while True:
                for observed, line in self.lines:
                    if observed >= since and predicate(line):
                        return observed, line
                if self.process.poll() is not None:
                    raise RuntimeError(f'{self.sid} exited; see {self.log.name}')
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError(f'{self.sid}: expected guest output; see {self.log.name}')
                self.changed.wait(min(remaining, .2))

    def py(self, code):
        return self.suite.call(self.suite.binary / 'sandbox-ctl', 'exec',
                               '--sandbox-id', self.sid, '--run-root', self.suite.run_root,
                               '--', self.suite.python, '-c', code).strip()

    def close(self):
        if self.closed:
            return
        if self.process.poll() is None:
            self.process.terminate()
        forced = False
        try:
            self.process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            forced = True
            self.process.kill()
            self.process.wait(timeout=10)
        self.reader.join(5)
        require(not self.reader.is_alive(), f'{self.sid}: log reader did not exit')
        self.process.stdin.close()
        self.process.stdout.close()
        self.log.close()
        self.closed = True
        require(not forced, f'{self.sid}: forced termination; inspect VMM cleanup before claiming success')


class Suite:
    def __init__(self, args):
        self.args = args
        self.output = args.output.resolve()
        self.binary = args.bin.resolve()
        self.timeout = args.timeout
        self.python = args.python
        self.run_root = args.run_root / ('snap-test-' + secrets.token_hex(6))
        self.env = {k: v for k, v in os.environ.items()
                    if not k.startswith(('KUASAR_', 'SANDBOX_', 'E2B_'))}
        self.cold = json.loads(args.config.read_text())
        self.cold['launch'] = {'exec': self.python, 'args': ['-c', WORKLOAD], 'restart': 'never'}
        self.restore_config = {'resources': copy.deepcopy(self.cold['resources']),
                               'boot': {k: self.cold['boot'][k] for k in ['kernel', 'runtime']}}
        self.guests = []
        self.results = []
        self.store = None
        self.store_log = None
        self.manifest = args.manifest_config.resolve() if args.manifest_config else None

    def call(self, *command):
        result = subprocess.run(list(map(str, command)), capture_output=True, text=True,
                                timeout=self.timeout, env=self.env)
        require(result.returncode == 0, f'{command[0:2]} failed: {result.stderr[-4000:]}')
        return result.stdout

    def setup(self):
        self.output.mkdir(parents=True, exist_ok=False)
        self.run_root.mkdir(parents=True, exist_ok=False)
        if self.manifest:
            require(self.manifest.is_file(), 'manifest configuration does not exist')
            return
        # Dedicated loopback store, with no shared cache or service installation.
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        store_config = self.output / 'store.json'
        store_config.write_text(json.dumps({
            'listen': f'127.0.0.1:{port}', 'backend': 'fs',
            'fs': {'root': str(self.output / 'store-data'), 'verify_content_key': True}}))
        self.call(self.binary / 'store-ctl', 'init', '--config', store_config, '--generation', 'G1')
        self.manifest = self.output / 'manifest.json'
        config = {'manifest': {'key': secrets.token_hex(32), 'verify_content': True, 'write_generation': 'G1'},
                  'store': {'endpoint': f'127.0.0.1:{port}', 'pool': 4, 'timeout': '30s'},
                  'cache': {'endpoint': ''}, 'chunker': {'mode': 'cdc'},
                  'crypto': {'chunk': 'aes', 'manifest': 'aes'}}
        with open(self.manifest, 'x', opener=lambda path, flags: os.open(path, flags, 0o600)) as f:
            json.dump(config, f)
        self.store_log = (self.output / 'store.log').open('w')
        self.store = subprocess.Popen(
            [str(self.binary / 'store-ctl'), 'serve', '--config', str(store_config)],
            stdout=self.store_log, stderr=subprocess.STDOUT, env=self.env)
        deadline = time.monotonic() + self.timeout
        while True:
            require(self.store.poll() is None, 'test Store exited; see store.log')
            try:
                with socket.create_connection(('127.0.0.1', port), timeout=.2):
                    break
            except OSError:
                if time.monotonic() >= deadline:
                    raise TimeoutError('test Store readiness')
                time.sleep(.05)

    def save(self, parent, mode, concurrent_write=False):
        output = self.output / 'snapshots' / secrets.token_hex(6)
        command = [self.binary / 'sandbox-ctl', 'snapshot', '--resume-early', '--sandbox-id', parent.sid,
                   '--run-root', self.run_root, '--json']
        command += ['--output', output] if mode == 'local' else ['--upload']
        started = time.monotonic_ns()

        def capture():
            result = json.loads(self.call(*command))
            return result, time.monotonic_ns()

        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
            future = executor.submit(capture)
            if concurrent_write:
                # Do not bypass capture admission: use the process already running
                # in the guest, through its ordinary stdin, rather than a new exec.
                parent.wait_line(lambda line: 'resume-early: original guest resumed' in line, started)
                parent.process.stdin.write('MUTATE\n')
                parent.process.stdin.flush()
                mutated, _ = parent.wait_line(lambda line: line == 'MUTATION_DONE', started)
            result, finished = future.result(timeout=self.timeout)
        (self.output / (parent.sid + '-' + secrets.token_hex(3) + '-capture.json')).write_text(
            json.dumps(result, indent=2))
        require(result.get('type') == 'snapshot_done', 'capture did not complete')
        require(any(started < t < finished and line.startswith('TICK ') for t, line in parent.lines),
                'parent did not make progress during snapshot persistence')
        if concurrent_write:
            require(mutated < finished, 'write did not finish during persistence; concurrency NOT verified')
        reference = result['snapshot_ref']
        if mode == 'local':
            reference = 'file://' + str(output / reference.removeprefix('file://'))
        return reference, {'pause_ms': result['application_pause_ms'],
                           'save_complete_ms': (finished - started) / 1e6}

    def record(self, row):
        self.results.append(row)
        temporary = self.output / 'results.tmp'
        temporary.write_text(json.dumps(self.results, indent=2))
        temporary.replace(self.output / 'results.json')
        print(json.dumps(row), flush=True)

    def pair(self, mode, order, concurrent_write=False, source=None):
        parent = Guest(self, self.restore_config if source else self.cold, source)
        child = None
        try:
            marker = secrets.token_hex(16)
            parent.py(write_marker(marker))
            if concurrent_write:
                parent.py("import os; f=open('/snapshot-busy','wb'); f.write(b'A'*(32<<20)); "
                          "f.flush(); os.fsync(f.fileno()); f.close()")
            with parent.changed:
                previous = [line for _, line in parent.lines if line.startswith('TICK ')][-1].split()
            reference, timing = self.save(parent, mode, concurrent_write)
            parent.py(write_marker('PARENT-AFTER'))
            child = Guest(self, self.restore_config, reference)
            inherited = child.first_tick[1].split()
            require(inherited[1] == previous[1] and int(inherited[2]) >= int(previous[2]),
                    'memory identity/counter was not inherited')
            require(child.py("print(open('/snapshot-state').read())") == marker, 'snapshot file changed')
            if concurrent_write:
                require(child.py("print(open('/snapshot-busy','rb').read()==b'A'*(32<<20))") == 'True',
                        'concurrent parent write polluted snapshot')
                require(parent.py("print(open('/snapshot-busy','rb').read()==b'B'*(32<<20))") == 'True',
                        'parent lost concurrent write')
            child.py(write_marker('CHILD-AFTER'))
            require(parent.py("print(open('/snapshot-state').read())") == 'PARENT-AFTER', 'child changed parent')
            if order == 'parent-first':
                parent.close()
                require(child.py("print(open('/snapshot-state').read())") == 'CHILD-AFTER', 'child lost state')
            else:
                child.close()
                require(parent.py("print(open('/snapshot-state').read())") == 'PARENT-AFTER', 'parent lost state')
            self.record({'scenario': 'resnapshot' if source else ('concurrent-write' if concurrent_write else 'isolation'),
                         'mode': mode, 'deletion_order': order, 'passed': True,
                         'concurrent_write_during_save': concurrent_write,
                         'snapshot_ref': reference, **timing})
            return reference
        finally:
            try:
                if child:
                    child.close()
            finally:
                parent.close()

    def close(self):
        errors = []
        for guest in reversed(self.guests):
            try:
                guest.close()
            except Exception as error:
                errors.append(str(error))
        if self.store:
            if self.store.poll() is None:
                self.store.terminate()
            try:
                self.store.wait(timeout=20)
            except subprocess.TimeoutExpired:
                self.store.kill()
                self.store.wait()
                errors.append('test Store required forced termination')
        if self.store_log:
            self.store_log.close()
        return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bin', type=Path, required=True)
    parser.add_argument('--config', type=Path, required=True, help='cold-boot JSON config with Python image and writable disk')
    parser.add_argument('--manifest-config', type=Path, help='existing test Store configuration; omitted: start private fs Store')
    parser.add_argument('--output', type=Path, required=True, help='new directory; logs, artifacts and results are retained')
    parser.add_argument('--run-root', type=Path, default=Path('/run/kuasar-snapshot-tests'))
    parser.add_argument('--python', default='/usr/local/bin/python3', help='interpreter path inside image')
    parser.add_argument('--modes', nargs='+', choices=['local', 'remote'], default=['local', 'remote'])
    parser.add_argument('--scenario', choices=['all', 'isolation', 'concurrent', 'chain'], default='all')
    parser.add_argument('--iterations', type=int, default=1, help='repeat functional suite, without warmup or performance claims')
    parser.add_argument('--timeout', type=float, default=120)
    args = parser.parse_args()
    if args.iterations < 1 or args.timeout <= 0:
        parser.error('iterations and timeout must be positive')
    if args.output.exists():
        parser.error('output directory must not already exist')
    suite = Suite(args)
    failure = None

    def interrupted(_signal, _frame):
        raise KeyboardInterrupt('test interrupted')

    signal.signal(signal.SIGTERM, interrupted)
    try:
        suite.setup()
        for _ in range(args.iterations):
            for mode in dict.fromkeys(args.modes):
                for order in ['parent-first', 'child-first']:
                    if args.scenario in ['all', 'isolation']:
                        suite.pair(mode, order)
                    if args.scenario in ['all', 'concurrent']:
                        suite.pair(mode, order, concurrent_write=True)
                if args.scenario in ['all', 'chain']:
                    reference = suite.pair(mode, 'child-first')
                    # Each recapture starts from a restored live process, not a
                    # newly booted container. Also exercise local/remote crossing.
                    for index, target in enumerate(dict.fromkeys(args.modes)):
                        reference = suite.pair(target, 'parent-first' if index % 2 == 0 else 'child-first', source=reference)
    except BaseException as error:
        failure = f'{type(error).__name__}: {error}'
    finally:
        cleanup_errors = suite.close()
    summary = {'passed': failure is None and not cleanup_errors, 'cases': len(suite.results),
               'error': failure, 'cleanup_errors': cleanup_errors,
               'owned_runtime_pids_alive': [g.process.pid for g in suite.guests if g.process.poll() is None],
               'run_root': str(suite.run_root), 'artifacts_retained': True}
    if suite.output.exists():
        (suite.output / 'summary.json').write_text(json.dumps(summary, indent=2))
    print(json.dumps(summary), flush=True)
    return 0 if summary['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
