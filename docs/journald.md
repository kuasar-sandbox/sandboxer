[English](journald.md) | [简体中文](journald_zh.md)

# Explicit journal output targets

Each journal output is configured independently:

```text
journald=<tag>[,<FIELD>=<VALUE>...]
```

`run` accepts this target in `--stdout-to`, `--stderr-to`, `--console`, and
`--log-to`. `exec` accepts it in its existing `--stdout-to` and `--stderr-to`
options; the guest console belongs to the running sandbox, not an exec client.

```bash
sandbox-ctl run --config sandbox.yaml \
  --stdout-to 'journald=app,WORKLOAD_ID=worker-42,STREAM=stdout' \
  --stderr-to 'journald=app,WORKLOAD_ID=worker-42,STREAM=stderr' \
  --console 'journald=console,WORKLOAD_ID=worker-42' \
  --log-to 'journald=sandbox-ctl,WORKLOAD_ID=worker-42'
```

The tag becomes `SYSLOG_IDENTIFIER`. Extra fields belong only to that output:
there is no global field option, environment discovery, interpolation, or
inheritance between targets. Equal tags do not share fields or partial-line
buffers. A bare `journald=app` remains valid and has no extra fields. Callers
that previously relied on implicit identity environment variables must now
include their fields explicitly in every required target.

Sandboxer does not choose or interpret application, orchestration, or identity
fields. The caller selects the tag and non-secret values. They are host-side
output configuration: they do not enter guest environment variables, the guest
stdio protocol, sandbox YAML, portable artifacts, or snapshots. Every new run
or exec invocation supplies its own targets.

## Syntax and encoding

The tag is a nonempty `[A-Za-z0-9_-]` token. Field names are 1–64 ASCII bytes,
start with `A`–`Z`, and contain only uppercase letters, digits, and underscores.
Names beginning with `_` are reserved for journal trusted metadata. This
interface also reserves `MESSAGE`, `PRIORITY`, and `SYSLOG_IDENTIFIER` for the
writer and rejects duplicate field names. This single-value interface does
not imply that the underlying journal protocol forbids all repeated fields.

Fields are separated by literal commas. Each field is split at its first `=`:
`FIELD=a=b` has value `a=b`, and `FIELD=` explicitly supplies an empty value.
Values use percent encoding with Go `url.PathEscape`/`url.PathUnescape`
semantics. Splitting happens before decoding, and decoding happens once:

```text
journald=app,NOTE=a%2Cb,EXPR=x=y,PERCENT=100%25,PLUS=a+b,ONCE=%252C
```

This produces `NOTE=a,b`, `EXPR=x=y`, `PERCENT=100%`, `PLUS=a+b`, and
`ONCE=%2C`. A plus sign is not a space. `%20` is a space; encoded newlines and
other bytes remain field data, not new fields. There is no shell-variable or
backslash expansion inside the target parser. Quote the complete target when
writing shell commands; callers using `exec.Cmd` should pass it as one argument.

Missing `=`, empty names or segments, trailing commas, duplicate/reserved or
invalid names, malformed percent encodings, and invalid tags are command-line
errors. The encoded target is limited to 64 KiB and at most 64 extra fields.
These are local resource bounds, not claimed journal protocol limits. Ordinary
non-journal file paths are not decoded or split, including paths containing
commas, `=`, or `%`.

## Component logs versus guest outputs

`run --log-to default` (also the default when omitted) retains ordinary component
diagnostics on stderr. `run --log-to journald=...` explicitly sends Go logger
output and run/restore operational diagnostics to that target, including
startup preparation failures and terminal errors after target parsing. It does
not require journal-connected stderr or any particular identity field.
Invalid flag syntax and invalid `--log-to` values can still be reported on the
original stderr before a usable target exists.

`--stderr-to` remains the guest application's stderr destination. `--log-to`
does not take over process descriptors, change TTY/pipe selection, redirect
Cloud Hypervisor's own stderr, or capture runtime panic/system events. Existing
message text and log timestamp precision are retained. `exec`'s own diagnostic
stderr and error-capture behavior are unchanged. File, artifact, JSON, and other
non-journal stdout data remain unaffected.

## Framing, failure, and lifetime

Each output has its own line buffer. Complete lines are emitted at info priority;
ordinary CRLF endings are normalized and empty lines are omitted, as before.
Very long lines are split into messages of at most 60 KiB without first copying
the whole input into an unbounded buffer. A forced size boundary does not strip
carriage returns from the middle of a line. A final partial line is flushed on
Close. Producers are drained before their writer is closed; Close is idempotent,
and writes after Close return `io.ErrClosedPipe`.

A successful native send is not duplicated to stderr. A failed send is followed
by a best-effort write to the original fallback sink. Guest/console fallback
retains its `[tag]` prefix; component fallback does not add another prefix to
existing diagnostic text. Neither a native-send error nor a fallback-write
error terminates the sandbox. Fallback text does not retain structured fields.
Native journal I/O is synchronous: this is not an asynchronous queue or a
promise of nonblocking, lossless, exactly-once durable storage.

Use `journalctl WORKLOAD_ID=worker-42 -o json` to inspect the fields in the
journals available to that command. Cross-node history requires a separate
collector or access to the other nodes' journals; no exporter is added here.

Protocol references: [Journal Native Protocol](https://systemd.io/JOURNAL_NATIVE_PROTOCOL/)
and [Go net/url](https://pkg.go.dev/net/url).
