# sandboxer/native-deps

Native dependency builds owned by `sandboxer`.

| Artifact | Source | Consumer |
|---|---|---|
| `cloud-hypervisor` | Cloud Hypervisor v51.1 + `deps/ch-patches` | `sandbox-ctl` VMM child process |

```bash
make build
make cloud-hypervisor
make ch-fetch && (cd build/src/cloud-hypervisor && <edit+commit>) && make ch-patches-format
```

The output is `bin/<arch>/cloud-hypervisor`. The top-level `sandboxer` Makefile
copies it to `sandboxer/bin/<arch>/cloud-hypervisor` so release packages place it
beside `sandbox-ctl`, matching `sandbox-ctl --ch-binary` default lookup.
