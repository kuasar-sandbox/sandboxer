from pathlib import Path
import re
import subprocess

paths = {
    'pkg/sandbox/helpers_test.go': 1,
    'test/e2e/e2e_sandbox_cgroup_control.sh': 1,
    'test/e2e/e2e_sandbox_cold.sh': 1,
    'test/e2e/e2e_sandbox_cold_manifest.sh': 1,
    'test/e2e/e2e_sandbox_launchspec.sh': 1,
    'test/e2e/e2e_sandbox_path_id.sh': 2,
    'test/e2e/e2e_sandbox_placeholder.sh': 1,
    'test/e2e/e2e_sandbox_proto.sh': 1,
    'test/e2e/e2e_sandbox_restore.sh': 2,
    'test/e2e/e2e_sandbox_snapshot.sh': 1,
    'test/e2e/e2e_sandbox_stdio.sh': 1,
    'test/e2e/e2e_sandbox_upload_restore.sh': 4,
}
for name, count in paths.items():
    p = Path(name)
    old = p.read_text()
    new, actual = re.subn(r'(?m)^      size: (?:1GiB|512MiB)\n', '', old)
    assert actual == count, (name, actual, count)
    p.write_text(new)

p = Path('pkg/config/disk_size_test.go')
s = p.read_text()
old = '''\t\tpath string
\t\tyaml string
\t}{
\t\t{"boot.root", "boot: {root: {%s: %s}}"},
\t\t{"boot.root.overlay", "boot: {root: {overlay: {%s: %s}}}"},
\t\t{"boot.disks[0]", "boot: {disks: [{name: data, %s: %s}]}"},
\t\t{"boot.disks[0].overlay", "boot: {disks: [{name: data, overlay: {%s: %s}}]}"},
'''
new = '''\t\tpath, style, yaml string
\t}{
\t\t{"boot.root", "flow", "boot: {root: {%s: %s}}"},
\t\t{"boot.root.overlay", "flow", "boot: {root: {overlay: {%s: %s}}}"},
\t\t{"boot.disks[0]", "flow", "boot: {disks: [{name: data, %s: %s}]}"},
\t\t{"boot.disks[0].overlay", "flow", "boot: {disks: [{name: data, overlay: {%s: %s}}]}"},
\t\t{"boot.root", "block", "boot:\\n  root:\\n    %s: %s\\n"},
\t\t{"boot.root.overlay", "block", "boot:\\n  root:\\n    overlay:\\n      %s: %s\\n"},
\t\t{"boot.disks[0]", "block", "boot:\\n  disks:\\n    - name: data\\n      %s: %s\\n"},
\t\t{"boot.disks[0].overlay", "block", "boot:\\n  disks:\\n    - name: data\\n      overlay:\\n        %s: %s\\n"},
'''
assert s.count(old) == 1
s = s.replace(old, new)
old = 't.Run(shape.path+"/"+key+"/"+value+"/"+name'
assert s.count(old) == 1
s = s.replace(old, 't.Run(shape.path+"/"+shape.style+"/"+key+"/"+value+"/"+name')
p.write_text(s)

p = Path('test/e2e/e2e_sandbox_disk_config.sh')
s = p.read_text()
old = 'BIN_DIR="${BIN_DIR:-$ROOT/bin}"\nCTL="$BIN_DIR/sandbox-ctl"'
assert s.count(old) == 1
s = s.replace(old, 'BIN="${BIN:-$ROOT/bin}"\nCTL="$BIN/sandbox-ctl"')
old = 'for shape in root root-overlay disk disk-overlay; do'
assert s.count(old) == 1
s = s.replace(old, 'for shape in root root-overlay disk disk-overlay root-block root-overlay-block disk-block disk-overlay-block; do')
marker = '  esac\n  for key in diff_size size; do'
assert s.count(marker) == 1
extra = r'''    root-block) field='boot.root'; format='boot:\n  root:\n    %s: %s\n' ;;
    root-overlay-block) field='boot.root.overlay'; format='boot:\n  root:\n    overlay:\n      %s: %s\n' ;;
    disk-block) field='boot.disks[0]'; format='boot:\n  disks:\n    - name: data\n      %s: %s\n' ;;
    disk-overlay-block) field='boot.disks[0].overlay'; format='boot:\n  disks:\n    - name: data\n      overlay:\n        %s: %s\n' ;;
'''
s = s.replace(marker, extra + marker)
p.write_text(s)

subprocess.check_call(['gofmt', '-w', 'pkg/config/disk_size_test.go', 'pkg/sandbox/helpers_test.go'])
subprocess.check_call(['git', 'diff', '--check'])
for p in Path('test/e2e').rglob('*.sh'):
    subprocess.check_call(['bash', '-n', str(p)])
for p in [*Path('test').rglob('*.sh'), *Path('examples').rglob('*.yaml')]:
    if p.name == 'e2e_sandbox_disk_config.sh':
        continue
    assert not re.search(r'(^|[^\w])(diff_size|size)\s*:', p.read_text()), str(p)
allowed = set(paths) | {'pkg/config/disk_size_test.go', 'test/e2e/e2e_sandbox_disk_config.sh'}
actual = set(subprocess.check_output(['git', 'diff', '--name-only'], text=True).splitlines())
assert actual == allowed, (actual, allowed)
print('PASS: all shell syntax, whitespace and positive fixture size-key scan')
subprocess.check_call(['git', 'diff', '--stat'])
subprocess.check_call(['git', 'diff'])
