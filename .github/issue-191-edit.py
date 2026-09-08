from pathlib import Path
import re
import subprocess

BASE = '76dba47d54c5d670ab29669d25e9e0f977a30030'
HEAD = '61f9746f082699daae1c86fc29e5458fab783ad4'
BRANCH = 'fix/191-remove-ineffective-disk-size'


def git(*args):
    return subprocess.check_output(['git', *args], text=True)


assert git('rev-parse', 'HEAD').strip() == HEAD, 'PR head changed; refusing to overwrite'
assert git('branch', '--show-current').strip() == BRANCH
assert not git('status', '--porcelain')


def original(path):
    return git('show', f'{BASE}:{path}')


def replace(text, old, new, count=1):
    assert text.count(old) == count, (old, text.count(old), count)
    return text.replace(old, new)


path = 'pkg/sandbox/lifecycle.go'
s = original(path)
s = replace(s, '\tdiffSize, err := opts.Cfg.DiffSizeBytes()\n\tif err != nil {\n\t\treturn -1, err\n\t}\n', '')
s = replace(s, '\tdiffSize, err := d.RootConfig.DiffSizeBytes(field)\n\tif err != nil {\n\t\treturn fail(err)\n\t}\n', '')
s = replace(s, 'PrepareDiff(diffPath, diffTemplate, baseSize, diffSize)', 'PrepareDiff(diffPath, diffTemplate, baseSize)', 2)
Path(path).write_text(s)

path = 'pkg/restore/restore.go'
s = original(path)
s = replace(s, '\trootDiffSize, err := snapCfg.DiffSizeBytes()\n\tif err != nil {\n\t\treturn -1, err\n\t}\n', '')
s = replace(s, '\t\tdsz, err := d.RootConfig.DiffSizeBytes(fmt.Sprintf("boot.disks[%d]", i))\n\t\tif err != nil {\n\t\t\treturn -1, err\n\t\t}\n', '')
s = replace(s, 'rootDiffURI, rootDiffTmpl, rootDiffSize, "overlay"', 'rootDiffURI, rootDiffTmpl, "overlay"')
s = replace(s, 'diffURI, diffTmpl, dsz, fmt.Sprintf("disk%d", i)', 'diffURI, diffTmpl, fmt.Sprintf("disk%d", i)')
s = replace(s, 'erofsBaseURI, diffURI, diffTemplate string, diffSize int64, diskKey string', 'erofsBaseURI, diffURI, diffTemplate, diskKey string')
s = replace(s, 'sandbox.PrepareDiff(diffPath, diffTemplate, baseReader.Size(), diffSize)', 'sandbox.PrepareDiff(diffPath, diffTemplate, baseReader.Size())')
Path(path).write_text(s)

path = 'pkg/sandbox/overlaydiff_test.go'
s = replace(original(path), ', 1<<30)', ')', 6)
Path(path).write_text(s)

for path in ['pkg/config/config_test.go', 'pkg/sandbox/ch_test.go', 'pkg/resctl/helpers_test.go']:
    s = original(path)
    if path == 'pkg/config/config_test.go':
        pattern = r'(?ms)^func (Test\w*DiffSize\w*)\(t \*testing\.T\) \{.*?^\}\n'
        found = re.findall(pattern, s)
        assert found, 'obsolete resolver test not found'
        print('Replaced obsolete resolver coverage:', found)
        s = re.sub(pattern, '', s)
    s, n = re.subn(r'\bDiffSize:\s*"[^"\n]*",?\s*', '', s)
    assert 'DiffSizeBytes' not in s
    assert not re.search(r'\bDiffSize\b', s), path
    Path(path).write_text(s)
    print('Removed obsolete fixture fields:', path, n)

for path in [
    'test/e2e/e2e_sandbox_path_id.sh',
    'test/e2e/e2e_sandbox_disks.sh',
    'test/e2e/e2e_sandbox_artifact.sh',
    'test/e2e/e2e_sandbox_bundle.sh',
    'docs/sandbox.md', 'docs/sandbox_zh.md',
]:
    s = original(path)
    s = re.sub(r'(?m)^\s*diff_size:\s*[^\n]*\n', '', s)
    s = re.sub(r',\s*diff_size:\s*[A-Za-z0-9.]+', '', s)
    s = '\n'.join(re.sub(r',\s*size:\s*[A-Za-z0-9.]+', '', line) if 'overlay:' in line else line for line in s.split('\n'))
    assert 'diff_size' not in s, path
    Path(path).write_text(s)

english = '''
## Writable disk capacity and migration

Writable disk capacity is inherited from the selected filesystem source, not
from a separate size setting. An existing non-empty active `diff` keeps its
logical capacity. A fresh diff initialized from `diff_template` inherits the
template's logical capacity; without a template, a fresh diff over a COW `base`
inherits that base's logical capacity. An empty existing diff or a fresh disk
without a filesystem source remains invalid. In two-device overlay mode, this
COW base belongs to the writable ext4 upper, not the read-only EROFS image.
These rules apply to both `boot.root` and `boot.disks[]` and remain unchanged
for cold starts, `run --from`, and memory restores.

The former `diff_size` setting never enforced capacity or a quota in these
supported paths. It has been removed, together with its misleading 1 GiB
default. Configurations that explicitly supply `diff_size` or the mistaken
`size` spelling directly under a root/data disk or its `overlay` now fail with
an actionable error, including empty and null values. Remove these keys from
existing configuration and provision a filesystem source with the required
capacity. New configuration output does not emit them. Unrelated fields and
opaque metadata are unaffected.

For a **new**, empty 512 MiB scratch filesystem, prepare a new template and
select it without a size override:

```bash
truncate -s 512M /tmp/scratch-512m.ext4
mkfs.ext4 -F /tmp/scratch-512m.ext4
```

```yaml
boot:
  # Keep the other required boot fields from the complete configuration.
  disks:
    - name: scratch
      diff_template: file:///tmp/scratch-512m.ext4
mounts:
  - target: /scratch
    type: disk
    source: scratch
```

Use a new active diff: an existing diff takes precedence, so changing a template
does not resize or replace existing data. Do not truncate an existing filesystem
to impose a smaller limit. Sandboxer does not automatically resize filesystems,
cap COW dirty bytes, or change disk capacity at startup or restore. Logical block
device capacity is distinct from an encrypted file's physical length, host disk
allocation, and the guest filesystem's available file-data space. This migration
does not introduce arbitrary per-instance capacity selection from one template.
'''
chinese = '''
## 可写磁盘容量与配置迁移

可写磁盘的容量继承自所选文件系统来源，而不是独立的大小配置。已有且非空的活动
`diff` 保留其逻辑容量；从 `diff_template` 初始化的新 diff 继承模板的逻辑容量；
没有模板时，基于 COW `base` 创建的新 diff 继承该 base 的逻辑容量。已有的空 diff，
以及没有文件系统来源的新磁盘，仍然属于无效输入。在双设备 overlay 模式下，这里的
COW base 属于可写 ext4 upper，不是只读 EROFS 镜像。这些规则同时适用于 `boot.root`
和 `boot.disks[]`，冷启动、`run --from` 以及内存快照恢复均保持原有行为。

原来的 `diff_size` 在这些受支持的路径上从未实施容量或配额限制。该配置及其具有误导性
的 1 GiB 默认值现已删除。在 root、数据盘或其 `overlay` 下显式配置 `diff_size`，
或使用错误的 `size` 写法，现在都会返回附带迁移指引的错误，包括空字符串和 null 值。
请从已有配置中删除这些键，并准备具有所需容量的文件系统来源。新生成的配置不再输出
这些键；无关配置字段和不透明的 metadata 不受影响。

对于一个**新建的**、空白的 512 MiB scratch 文件系统，可以准备新模板，使用时不再指定
大小覆盖项：

```bash
truncate -s 512M /tmp/scratch-512m.ext4
mkfs.ext4 -F /tmp/scratch-512m.ext4
```

```yaml
boot:
  # 保留完整配置中的其他必需 boot 字段。
  disks:
    - name: scratch
      diff_template: file:///tmp/scratch-512m.ext4
mounts:
  - target: /scratch
    type: disk
    source: scratch
```

必须使用新的活动 diff：已有 diff 优先，因此更换模板不会调整容量或替换已有数据。
不要通过截断已有文件系统来实施更小的限制。Sandboxer 不会自动调整文件系统大小，
不会限制 COW 脏数据字节数，也不会在启动或恢复时改变磁盘容量。逻辑块设备容量与加密
文件的物理长度、宿主磁盘实际分配空间、guest 文件系统可用于文件数据的空间并不是
同一个概念。本次迁移不提供从同一个模板任意选择各实例容量的新能力。
'''
for path, appendix in [('docs/sandbox.md', english), ('docs/sandbox_zh.md', chinese)]:
    p = Path(path)
    p.write_text(p.read_text().rstrip() + '\n' + appendix)

Path('pkg/sandbox/overlaydiff_capacity_test.go').write_text(r'''package sandbox

import (
    "bytes"
    "os"
    "path/filepath"
    "testing"

    "github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

type capacityBase struct{ *bytes.Reader }
func (capacityBase) Close() error { return nil }

func TestPrepareDiffPreservesTemplateAndExistingLogicalCapacity(t *testing.T) {
    const size int64 = 8 * 4096
    for _, tc := range []struct {
        name string
        encryptedTemplate, encryptedTarget bool
    }{
        {"plaintext", false, false},
        {"plaintext-template-encrypted-target", false, true},
        {"encrypted-template-encrypted-target", true, true},
    } {
        t.Run(tc.name, func(t *testing.T) {
            key := [32]byte{1}
            options := func(encrypted bool) []vhost.BlockCOWOption {
                if encrypted { return []vhost.BlockCOWOption{vhost.WithDiffEncryption(key, true)} }
                return nil
            }
            dir := t.TempDir()
            template, target := filepath.Join(dir, "template.diff"), filepath.Join(dir, "active.diff")
            seed, err := vhost.OpenBlockCOW(template, nil, vhost.DiffInit{CreateSize: size}, options(tc.encryptedTemplate)...)
            if err != nil { t.Fatal(err) }
            t.Cleanup(func() { _ = seed.Close() })
            payload := bytes.Repeat([]byte{0x5a}, 4096)
            if _, err := seed.WriteAt(payload, size-4096); err != nil { t.Fatal(err) }
            if err := seed.Close(); err != nil { t.Fatal(err) }
            before, err := os.ReadFile(template)
            if err != nil { t.Fatal(err) }

            plan, err := PrepareDiff(target, "file://"+template, size/2)
            if err != nil { t.Fatal(err) }
            cow, err := vhost.OpenBlockCOW(target, nil, plan, options(tc.encryptedTarget)...)
            if err != nil { t.Fatal(err) }
            t.Cleanup(func() { _ = cow.Close() })
            if cow.Size() != size { t.Fatalf("capacity=%d, want template logical size=%d", cow.Size(), size) }
            if _, err := cow.WriteAt([]byte{1}, size); err == nil { t.Fatal("out-of-capacity write accepted") }
            got := make([]byte, len(payload))
            if _, err := cow.ReadAt(got, size-4096); err != nil || !bytes.Equal(got, payload) { t.Fatalf("template payload: %v", err) }
            if err := cow.Close(); err != nil { t.Fatal(err) }
            info, err := os.Stat(target)
            if err != nil { t.Fatal(err) }
            if tc.encryptedTarget && info.Size() <= size { t.Fatal("encrypted physical size did not include its header") }

            // Existing data wins even with an unavailable template and a different base size.
            plan, err = PrepareDiff(target, "file:///unused-template", 2*size)
            if err != nil || !plan.Existing { t.Fatalf("existing plan=%+v err=%v", plan, err) }
            reopened, err := vhost.OpenBlockCOW(target, nil, plan, options(tc.encryptedTarget)...)
            if err != nil { t.Fatal(err) }
            t.Cleanup(func() { _ = reopened.Close() })
            if reopened.Size() != size { t.Fatalf("existing capacity changed to %d", reopened.Size()) }
            if _, err := reopened.ReadAt(got, size-4096); err != nil || !bytes.Equal(got, payload) { t.Fatalf("existing payload: %v", err) }
            after, err := os.ReadFile(template)
            if err != nil || !bytes.Equal(before, after) { t.Fatalf("template changed: %v", err) }
        })
    }
}

func TestPrepareDiffInheritsBaseLogicalCapacity(t *testing.T) {
    for _, encrypted := range []bool{false, true} {
        name := "plaintext"
        if encrypted { name = "encrypted" }
        t.Run(name, func(t *testing.T) {
            payload := bytes.Repeat([]byte{0x35}, 4*4096)
            base := capacityBase{bytes.NewReader(payload)}
            path := filepath.Join(t.TempDir(), "active.diff")
            plan, err := PrepareDiff(path, "", base.Size())
            if err != nil { t.Fatal(err) }
            var options []vhost.BlockCOWOption
            if encrypted { options = append(options, vhost.WithDiffEncryption([32]byte{1}, true)) }
            cow, err := vhost.OpenBlockCOW(path, base, plan, options...)
            if err != nil { t.Fatal(err) }
            defer cow.Close()
            if cow.Size() != base.Size() || plan.CreateSize != base.Size() { t.Fatalf("capacity=%d plan=%+v base=%d", cow.Size(), plan, base.Size()) }
            got := make([]byte, len(payload))
            if _, err := cow.ReadAt(got, 0); err != nil || !bytes.Equal(got, payload) { t.Fatalf("base payload: %v", err) }
        })
    }
}
''')

changed = git('diff', '--name-only').splitlines()
new = git('ls-files', '--others', '--exclude-standard').splitlines()
go_files = [p for p in changed+new if p.endswith('.go')]
subprocess.check_call(['gofmt', '-w', *go_files])
for p in [p for p in changed if p.endswith('.sh')]:
    subprocess.check_call(['bash', '-n', p])
subprocess.check_call(['git', 'diff', '--check'])
assert not any(p.startswith('.github/') for p in changed+new)
result = subprocess.run(['git', 'grep', '-n', '-E', r'\bDiffSize\b|DiffSizeBytes', '--', '*.go'], text=True, capture_output=True)
assert result.returncode == 1, result.stdout + result.stderr
print(git('diff', '--stat', BASE))
print(git('diff', BASE, '--', 'pkg/sandbox/lifecycle.go', 'pkg/restore/restore.go', 'pkg/config/config_test.go'))
result = subprocess.run(['git', 'grep', '-n', '-E', 'diff_size|overlay:.*size:', '--', '*.go', '*.sh', '*.md', '*.yaml', '*.yml'], text=True, capture_output=True)
print('Remaining retired-key references (review migration/negative tests):\n' + result.stdout)
subprocess.check_call(['git', 'add', '--', *changed, *new])
subprocess.check_call(['git', 'commit', '-m', 'fix(config): complete obsolete disk-size cleanup and capacity regression coverage'])
subprocess.check_call(['git', 'push', 'origin', f'HEAD:refs/heads/{BRANCH}'])
print('PUSHED_HEAD=' + git('rev-parse', 'HEAD').strip())
