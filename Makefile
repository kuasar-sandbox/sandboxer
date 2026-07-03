# sandboxer — microVM sandbox lifecycle engine.
#
#   sandbox-ctl     host control plane (run / snapshot / exec / config / info)
#   sandbox-init    guest PID 1 (packed into sandbox-runtime.erofs)
#   sandbox-runtime sandbox-init packed into a virtio-pmem-mountable EROFS
#
# `make build` produces all three. The runtime image target needs mkfs.erofs
# from guest-runtime/native-deps; the finder below probes PATH, this repo's
# bin/, and the sibling guest-runtime/native-deps/bin/ (the org-root layout).

SHELL := /bin/bash

.PHONY: all build sandbox-ctl sandbox-init sandbox-runtime test vet bench clean help

# ---------------------------------------------------------------------------
# Architecture selection (identical block across all kuasar-sandbox repos)
# ---------------------------------------------------------------------------
HOST_ARCH   := $(shell uname -m)
TARGET_ARCH ?= $(HOST_ARCH)
ifeq ($(TARGET_ARCH),amd64)
  override TARGET_ARCH := x86_64
endif
ifeq ($(TARGET_ARCH),arm64)
  override TARGET_ARCH := aarch64
endif
ifeq ($(TARGET_ARCH),x86_64)
  GO_ARCH := amd64
else ifeq ($(TARGET_ARCH),aarch64)
  GO_ARCH := arm64
else
  $(error unsupported TARGET_ARCH=$(TARGET_ARCH); supported: x86_64, aarch64)
endif

# ---------------------------------------------------------------------------
# Build settings
# ---------------------------------------------------------------------------
GO             := go
GO_BUILD_FLAGS := -trimpath
BINDIR         := bin/$(TARGET_ARCH)
BUILD_DIR      := build/$(TARGET_ARCH)

# mkfs.erofs lookup chain (in priority order):
#   PATH → this repo's $(BINDIR)/ → this repo's bin/ symlink →
#   sibling guest-runtime/native-deps/bin/$(TARGET_ARCH)/ → sibling guest-runtime/native-deps/bin/ symlink
MKFS_EROFS ?= $(shell \
    command -v mkfs.erofs 2>/dev/null \
    || ( [ -x $(BINDIR)/mkfs.erofs ] && echo $(BINDIR)/mkfs.erofs ) \
    || ( [ -x bin/mkfs.erofs ] && echo bin/mkfs.erofs ) \
    || ( [ -x ../guest-runtime/native-deps/$(BINDIR)/mkfs.erofs ] && echo ../guest-runtime/native-deps/$(BINDIR)/mkfs.erofs ) \
    || ( [ -x ../guest-runtime/native-deps/bin/mkfs.erofs ] && echo ../guest-runtime/native-deps/bin/mkfs.erofs ))

define link_bin
@if [ "$(HOST_ARCH)" = "$(TARGET_ARCH)" ]; then \
   mkdir -p bin && ln -sfn $(TARGET_ARCH)/$(1) bin/$(1); \
 fi
endef

# ---------------------------------------------------------------------------
# Targets
# ---------------------------------------------------------------------------
all: build

build: sandbox-ctl sandbox-init sandbox-runtime

sandbox-ctl:
	@mkdir -p $(BINDIR)
	GOOS=linux GOARCH=$(GO_ARCH) CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -o $(BINDIR)/sandbox-ctl ./cmd/sandbox-ctl
	$(call link_bin,sandbox-ctl)

# guest PID 1: stripped (no libc inside the guest rootfs).
sandbox-init:
	@mkdir -p $(BINDIR)
	GOOS=linux GOARCH=$(GO_ARCH) CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -ldflags '-s -w' -o $(BINDIR)/sandbox-init ./cmd/sandbox-init
	$(call link_bin,sandbox-init)

# Pack sandbox-init into the guest "/" image (virtio-pmem, DAX, read-only,
# shared across sandboxes via host page cache). Needs mkfs.erofs; EROFS is
# endian-neutral / cross-mountable. mkfs flags mirror accelerator's
# flatten.go buildEROFS for deterministic, dedup-friendly output. stderr
# discarded because mkfs.erofs 1.9 emits a false-positive
# "<E> Compression is not enabled" on -Ededupe even when --chunksize already
# triggers chunk-based dedup (matches flatten.go Stderr=io.Discard).
sandbox-runtime: sandbox-init
	@[ -n "$(MKFS_EROFS)" ] || { echo "mkfs.erofs not found — build it in guest-runtime/native-deps (\`make -C ../guest-runtime/native-deps erofs\`) or set MKFS_EROFS=<path>" >&2; exit 1; }
	rm -rf $(BUILD_DIR)/sandbox-runtime
	mkdir -p $(BUILD_DIR)/sandbox-runtime/sbin $(BUILD_DIR)/sandbox-runtime/proc \
	         $(BUILD_DIR)/sandbox-runtime/sys $(BUILD_DIR)/sandbox-runtime/dev \
	         $(BUILD_DIR)/sandbox-runtime/overlay/lower $(BUILD_DIR)/sandbox-runtime/overlay/upper \
	         $(BUILD_DIR)/sandbox-runtime/sysroot $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime
	@# Pre-baked mountpoints for boot.disks[] data disks (max 8, ordinals 0-7):
	@# disk-N (assembled fs / single ext4), disk-N-lower (overlay erofs base),
	@# disk-N-upper (overlay ext4 upper). The guest mounts onto these read-only
	@# dirs (mounting shadows the dir, no write to the erofs). Keep the count (8)
	@# in sync with config.MaxDataDisks; the guest rejects a disk whose dir is absent.
	mkdir -p $(BUILD_DIR)/sandbox-runtime/sysdisks/disk-{0..7}{,-lower,-upper}
	cp $(BINDIR)/sandbox-init $(BUILD_DIR)/sandbox-runtime/sbin/init
	chmod +x $(BUILD_DIR)/sandbox-runtime/sbin/init
	rm -f $(BINDIR)/sandbox-runtime.erofs
	"$(MKFS_EROFS)" \
	    -Ededupe \
	    --chunksize=4096 \
	    --all-root \
	    -T0 \
	    -b4096 \
	    -x-1 \
	    -U 00000000-0000-0000-0000-000000000000 \
	    $(BINDIR)/sandbox-runtime.erofs $(BUILD_DIR)/sandbox-runtime 2>/dev/null
	@# virtio-pmem requires 2 MiB-aligned backing; EROFS self-describes its
	@# extent in the superblock so sparse padding is invisible to mount.
	@actual=$$(stat -c %s $(BINDIR)/sandbox-runtime.erofs); \
	 aligned=$$(( ($$actual + 2097151) / 2097152 * 2097152 )); \
	 [ "$$aligned" = "$$actual" ] || truncate -s $$aligned $(BINDIR)/sandbox-runtime.erofs
	@echo "==> built $(BINDIR)/sandbox-runtime.erofs"

test:
	CGO_ENABLED=0 $(GO) test ./...

vet:
	CGO_ENABLED=0 $(GO) vet ./...

clean:
	rm -rf bin build

# Go micro-benchmarks. Sandbox-level e2e (cold/snapshot/restore/...) lives in
# kuasar-sandbox/test/e2e — they need vmlinux + cloud-hypervisor + mkfs.erofs
# (from guest-runtime/native-deps) plus accelerator binaries, so they're cross-repo and
# their natural home is the umbrella.
bench:
	CGO_ENABLED=0 $(GO) test -bench=. -benchmem -run=^$$ ./...

help:
	@echo "sandboxer. Targets:"
	@echo "  build              sandbox-ctl + sandbox-init + sandbox-runtime.erofs"
	@echo "  sandbox-ctl        host control plane"
	@echo "  sandbox-init       guest PID 1 (stripped)"
	@echo "  sandbox-runtime    pack sandbox-init into the guest erofs (needs mkfs.erofs)"
	@echo "  test / vet / clean"
	@echo "  TARGET_ARCH        x86_64 (default) | aarch64"
