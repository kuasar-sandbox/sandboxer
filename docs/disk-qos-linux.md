[English](disk-qos-linux.md) | [简体中文](disk-qos-linux_zh.md)

# Linux host I/O controls

`resources.diff_cow` controls sandboxer's rate and dirty-memory budgets. The
controls below operate at different boundaries: diffstore's thread I/O priority
and the host block I/O attributed to a runner or builder service. They do not
provide a minimum throughput or a latency guarantee.

## Diffstore DIO priority

The common diffstore positional read/write helper attempts best-effort class,
level zero (`BE0`) for the actual synchronous I/O, including its existing EINTR
retry loop. No additional configuration is required. It preserves an existing
RT or BE0 priority, including the effective RT class of an unset I/O priority
on a FIFO, RR, or DEADLINE CPU thread.

Temporary priority changes run on a goroutine whose entire lifetime belongs to
the helper. After I/O, the helper restores the execution thread's exact original
priority. If restoration fails, that goroutine retains its thread lock and
returns, causing Go to retire the thread from scheduling. Ordinary OS threads
terminate; Go's initial OS thread remains permanently parked. Neither is reused
for other goroutines. An arbitrary caller, including an export command or an
already pinned vhost worker, need not exit to prevent thread-pool contamination.

Priority query, setup, and restoration failures produce a bounded warning once
per failure stage. They do not replace the actual I/O byte count or error,
invalidate a successful write, cause a retry, or introduce a new cache fatal
condition. Real I/O failures retain their existing handling. A partial transfer
is never replayed just because priority management failed.

### Deployment conditions and cost

* I/O priority is scheduler-dependent. BFQ distinguishes BE levels. A typical
  unset priority on a normal `nice=0` thread corresponds to BE4.
* Linux mq-deadline implementations that schedule by class, including the
  examined Linux 6.18 implementation, place NONE and BE in the same class queue;
  setting BE0 instead of BE4 does not give a separate BE-level scheduling share.
* The helper does not change, discover, or qualify the host's scheduler.
  Successful `ioprio_set` is not evidence of a physical-device benefit. The
  helper's execution cost remains even on a device that does not distinguish
  BE levels. Cgroup priority policies and ancestors can also affect the result.
* O_DIRECT setup is not proof of physical cache bypass. When an existing
  diffstore path falls back to buffered writes, later filesystem writeback does
  not generally inherit the dirtying thread's temporary I/O priority.
* Moving an operation from a pinned vhost thread to the helper has a measurable
  handoff cost. Cache hits do not enter this syscall helper, but frequent small
  DIOs can be sensitive to that cost. Qualify the actual device and workload
  before assuming an end-to-end improvement; see the measurements below.

The priority scope is limited to diffstore's common DIO read/write path. It does
not permanently raise a complete vhost worker, pin one additional permanent
thread per disk, or add a global I/O queue.

## Find the host backing device

Run these read-only checks on the native host, using an existing diff file:

```sh
DIFF_PATH=/absolute/path/to/an/existing/diff-file
findmnt -T "$DIFF_PATH" -o TARGET,SOURCE,FSTYPE,MAJ:MIN
lsblk -o NAME,TYPE,PKNAME,MAJ:MIN,FSTYPE,MOUNTPOINTS
stat -fc %T /sys/fs/cgroup
cat /sys/fs/cgroup/cgroup.controllers
```

Identify the device that actually provides the backing storage. The following
example assumes that it has been verified as `/dev/nvme0n1`; replace it for the
host being configured:

```sh
QOS_DEVICE=/dev/nvme0n1
QOS_KNAME=$(lsblk -dn -o KNAME "$QOS_DEVICE")
lsblk -dn -o NAME,TYPE,MAJ:MIN "$QOS_DEVICE"
cat "/sys/class/block/$QOS_KNAME/queue/scheduler"
```

`[bfq]` identifies the active scheduler. This procedure does not switch a shared
device's scheduler. Verify the complete device mapping for dm, RAID, and other
stacked storage; a guest device name or an unchecked filesystem-device lookup
is not sufficient.

## Configure runner and builder parent services

The orchestrator's `resource_listen.pools[].unit_dropins` accepts absolute paths
to systemd fragments. The same resource-pool fragment is copied to the runner
and builder templates that select that resource pool.

Example `/etc/kuasar/pools/io.conf`, assuming the device above:

```ini
[Service]
IOAccounting=yes
IOReadBandwidthMax=/dev/nvme0n1 50M
IOWriteBandwidthMax=/dev/nvme0n1 20M
IOReadIOPSMax=/dev/nvme0n1 1000
IOWriteIOPSMax=/dev/nvme0n1 500
```

These are illustrative limits for each service instance, not defaults. systemd
uses decimal multipliers for `M` here. The limits account for host block I/O,
whose requests and byte counts differ from sandboxer's logical disk I/O.

An optional `IOWeight=200` configures a relative weight among siblings at that
cgroup level. It is not a bandwidth reservation. BFQ group scheduling requires
kernel support and an active BFQ scheduler. systemd versions with BFQ support
also apply `IOWeight` through `io.bfq.weight`; read back the effective values.
Weights on services in different slices do not by themselves define the
relative allocation of those slices.

Minimal related orchestrator configuration:

```yaml
resource_listen:
  enabled: true
  pools:
    - name: default
      unit_dropins:
        - /etc/kuasar/pools/io.conf
units:
  install: true
  dir: /etc/systemd/system
  runner_pools:
    - name: runner-io
      resource_pool: default
      unit: runner-io@.service
      size: 0
  builder_pools:
    - name: builder-io
      resource_pool: default
      unit: builder-io@.service
      size: 0
```

For an existing deployment, add the fragment to its existing target pool and
retain its capacity, allocatable budgets, and other pool entries. Do not replace
the full lists with this minimal example. `size: 0` disables idle prewarming; it
does not set execution capacity to zero.

At conductor startup, `InstallUnits` installs the templates and fragments and
reloads systemd when files change. For this example, the generated fragments are:

```text
/etc/systemd/system/runner-io@.service.d/50-kuasar-resource-pool-0000.conf
/etc/systemd/system/builder-io@.service.d/50-kuasar-resource-pool-0000.conf
```

Use the deployment's existing conductor configuration rollout procedure.
`units.install: false` leaves installation to the operator and does not read or
install these source fragments.

**Each service instance receives its own limit.** A pool with several instances
does not automatically share a single 50 MB/s allowance. An aggregate slice
limit is a separate cgroup-level deployment decision.

The generated parent service contains sibling `ctl` and `vmm` cgroups.
`node-ctl` and its `sandbox-ctl` child reside in `ctl`; the VMM is started in
`vmm`. The parent service is the common boundary for both. Limiting only `vmm`
omits diffstore I/O performed by `sandbox-ctl`.

## Verify a running instance

```sh
systemctl cat runner-io@.service builder-io@.service
systemctl list-units --all --no-pager \
  'runner-io@*.service' 'builder-io@*.service'
```

Select an actual existing instance, then inspect its properties and cgroup:

```sh
QOS_UNIT='runner-io@<actual-run-id>.service'
systemctl show "$QOS_UNIT" \
  -p ControlGroup -p Slice -p Delegate -p IOAccounting -p IOWeight \
  -p IOReadBandwidthMax -p IOWriteBandwidthMax \
  -p IOReadIOPSMax -p IOWriteIOPSMax
QOS_CGROUP=$(systemctl show "$QOS_UNIT" -p ControlGroup --value)
cat "/sys/fs/cgroup$QOS_CGROUP/io.max"
cat "/sys/fs/cgroup$QOS_CGROUP/io.stat"
cat "/sys/fs/cgroup$QOS_CGROUP/ctl/cgroup.procs"
cat "/sys/fs/cgroup$QOS_CGROUP/vmm/cgroup.procs"
```

When using BFQ weights, also inspect
`/sys/fs/cgroup$QOS_CGROUP/io.bfq.weight`. Confirm the major:minor device in
`io.max`, process membership, and accounting deltas under an isolated test.
Do not infer successful enforcement from the fragment alone. `io.max` permits
temporary bursts; it is not an exact per-request completion schedule.

Buffered writeback requires filesystem cgroup-writeback support and follows
inode ownership. The kernel documents support for ext2, ext4, btrfs, f2fs, and
xfs; simultaneous writes to one inode from multiple cgroups are not accurately
attributed in all cases. Parent process membership alone does not establish
ownership of every associated filesystem writeback.

## Validation scope and measurements

The 2026-11-19 native validation used Linux 6.6, Go 1.26.4, and an x86-64 host.
Normal and race tests passed, including a real BE0 thread whose injected restore
failure caused that thread to disappear from `/proc/self/task`. Setup failures
preserved the actual I/O result, and RT-preservation branches were checked with
injected states without changing a host thread to RT.

Separate Go 1.26.4 retirement regressions force the helper onto both an ordinary
thread and the initial thread. They check disappearance of the ordinary thread,
retention without reuse of the parked initial thread, unchanged priority on 32
simultaneously pinned probe threads, and a subsequent successful positional read.

A private 64 MiB loop device, an ext4 filesystem, and a 16 MiB test file were
created for short read-only device tests. BFQ was selected only on that loop;
the shared backing device retained its original `none` scheduler. The loop's
backing file was cached, so these figures describe this fast virtual-device
test, not physical-disk performance or a production capacity recommendation.

| 4 KiB read case | Bare pinned call | Priority helper with pinned caller |
| --- | ---: | ---: |
| `/dev/zero` syscall overhead | 0.416–0.418 µs | 13.10–13.16 µs |
| Private BFQ loop, no competing reader | 10.47–11.53 µs | 23.47–23.61 µs; first sample 79.73 µs |
| Private BFQ loop, BE4 background reader | 11.63–18.77 µs | 24.75–38.29 µs |

The full DIO wrapper allocated 224 bytes in three allocations per promoted call
in this benchmark. An already-BE0 caller avoided the handoff and measured
10.81–11.18 µs on the idle loop. These results demonstrate meaningful helper cost
and **no end-to-end speedup for this fast-loop workload**.

A separate fixed-thread fio probe measured roughly equal 78.5k/78.5k IOPS for
BE4/BE4 and 73.5k/16.1k for BE0/BE4 on the private BFQ loop. That confirms
BE-level differentiation in that setup; it is not a guaranteed ratio and does
not remove the helper cost measured above.

A separate transient parent service imposed `8:0 rbps=2097152` on the unchanged
backing device. Direct readers in sibling `ctl` and `vmm` groups achieved about
1.047 and 1.060 MB/s over 5.007 seconds. Parent `io.stat` accounted for their
combined 10,551,296 read bytes. The parent's subtree enabled only `cpu memory`,
matching the relevant delegation boundary: its own IO limit still covered both
children. The transient service, mount, and loop mapping were removed afterward.

To repeat the opt-in, read-only DIO benchmark on an isolated prepared file:

```sh
SANDBOXER_DIO_BENCH_FILE=/absolute/path/to/an/isolated/data-file \
  go test ./pkg/diffstore/internal/directio -run '^$' \
  -bench '^BenchmarkIOPriorityDirectRead$' -benchtime=1000x -count=3
```

The benchmark does not create files, change schedulers, or drop host caches.
Real-device contention and latency must be qualified for the target deployment.

## References

* [Go runtime thread retirement (`mexit`)](https://go.dev/src/runtime/proc.go)
* [Linux I/O priorities](https://docs.kernel.org/block/ioprio.html)
* [BFQ scheduling and group support](https://docs.kernel.org/block/bfq-iosched.html)
* [Linux mq-deadline implementation, v6.18](https://github.com/torvalds/linux/blob/v6.18/block/mq-deadline.c)
* [Cgroup v2 IO and writeback](https://docs.kernel.org/admin-guide/cgroup-v2.html)
* [systemd resource controls](https://www.freedesktop.org/software/systemd/man/latest/systemd.resource-control.html)
* [Orchestrator resource-pool fields](https://github.com/kuasar-sandbox/orchestrator/blob/1d82400e202d894390e409b385233a8f1b7937e7/config/resource_pools.go)
* [Orchestrator unit installation](https://github.com/kuasar-sandbox/orchestrator/blob/1d82400e202d894390e409b385233a8f1b7937e7/internal/orch/units.go)
