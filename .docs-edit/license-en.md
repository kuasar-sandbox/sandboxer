# License scope

Except for the upstream material described below, original project content in
this repository without a separate license declaration is covered by the Apache
License 2.0 in the repository root.

## Cloud Hypervisor patches

The patches in `native-deps/deps/ch-patches/` are based on Cloud Hypervisor v51.1.
The project's new modifications are submitted under Apache-2.0. Upstream context
retained in the mbox patches remains subject to the existing license declarations
of the corresponding Cloud Hypervisor source files.

Cloud Hypervisor v51.1 is predominantly Apache-2.0, with BSD-3-Clause code in some
files. This repository therefore does not assign a misleading uniform
`Apache-2.0 OR BSD-3-Clause` SPDX expression to the complete patch set. The root
`LICENSE` contains the full Apache-2.0 text.
[`LICENSES/BSD-3-Clause.txt`](LICENSES/BSD-3-Clause.txt) preserves the BSD-3-Clause
text and attribution used by the upstream source represented in the patches.

Cloud Hypervisor sources downloaded during the build and not tracked in this
repository remain subject to their upstream per-file license declarations.
