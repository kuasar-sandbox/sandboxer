# sandboxer — microVM sandbox lifecycle engine.
#
#   sandbox-ctl     host control plane (run / snapshot / exec / config / info)
#   sandbox-init    guest PID 1 source/binary (packed by guest-runtime)
#
# `make build` produces the two sandboxer binaries. The guest-runtime repo
# consumes sandbox-init to produce sandbox-runtime.bundle.

SHELL := /bin/bash

.PHONY: all build sandbox-ctl sandbox-init cloud-hypervisor native-deps test vet bench e2e-usage-probe e2e-cgroup-fork-probe test-e2e-scripts release test-release clean help

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
E2E_FIXTURE_DIR ?= $(abspath build/e2e-tools/$(TARGET_ARCH))

# Honor an explicitly provided compiler; otherwise choose the target toolchain.
E2E_PROBE_CC := $(CC)
ifeq ($(origin CC),default)
  ifneq ($(HOST_ARCH),$(TARGET_ARCH))
    ifeq ($(TARGET_ARCH),aarch64)
      E2E_PROBE_CC := aarch64-linux-gnu-gcc
    else
      E2E_PROBE_CC := x86_64-linux-gnu-gcc
    endif
  endif
endif

define link_bin
@if [ "$(HOST_ARCH)" = "$(TARGET_ARCH)" ]; then \
   mkdir -p bin && ln -sfn $(TARGET_ARCH)/$(1) bin/$(1); \
 fi
endef

# ---------------------------------------------------------------------------
# Targets
# ---------------------------------------------------------------------------
all: build

build: sandbox-ctl sandbox-init cloud-hypervisor

sandbox-ctl:
	@mkdir -p $(BINDIR)
	GOOS=linux GOARCH=$(GO_ARCH) CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -o $(BINDIR)/sandbox-ctl ./cmd/sandbox-ctl
	$(call link_bin,sandbox-ctl)

# guest PID 1: stripped (no libc inside the guest rootfs).
sandbox-init:
	@mkdir -p $(BINDIR)
	GOOS=linux GOARCH=$(GO_ARCH) CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -ldflags '-s -w' -o $(BINDIR)/sandbox-init ./cmd/sandbox-init
	$(call link_bin,sandbox-init)

native-deps:
	$(MAKE) -C native-deps build TARGET_ARCH=$(TARGET_ARCH)

cloud-hypervisor:
	$(MAKE) -C native-deps cloud-hypervisor TARGET_ARCH=$(TARGET_ARCH)
	@mkdir -p $(BINDIR)
	cp -f native-deps/bin/$(TARGET_ARCH)/cloud-hypervisor $(BINDIR)/cloud-hypervisor
	$(call link_bin,cloud-hypervisor)

test: test-e2e-scripts
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test-environment-rust.py
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test-cloud-hypervisor-retry.py
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test-environment-tools.py
	CGO_ENABLED=0 $(GO) test ./...

vet:
	CGO_ENABLED=0 $(GO) vet ./...

clean:
	rm -rf bin build
	$(MAKE) -C native-deps clean

# Sandbox lifecycle E2E is maintained here, next to the implementation. It uses
# an assembled platform BIN because boot/restore cases need artifacts built by
# accelerator and guest-runtime as well as sandboxer.
bench:
	CGO_ENABLED=0 $(GO) test -bench=. -benchmem -run=^$$ ./...

e2e-usage-probe:
	@mkdir -p "$(E2E_FIXTURE_DIR)"
	GOWORK=off GOOS=linux GOARCH=$(GO_ARCH) CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) \
		-o "$(E2E_FIXTURE_DIR)/usage-probe" test/fixtures/usageprobe/main.go

# The cgroup-control E2E must fork before libc or a language runtime starts in
# order to detect Start->cgroup.procs races. Build that tiny libc-free probe in
# the source-build stage; product E2E only consumes the prepared binary.
e2e-cgroup-fork-probe:
	@mkdir -p "$(E2E_FIXTURE_DIR)"
	$(E2E_PROBE_CC) -nostdlib -static -fno-stack-protector -fno-builtin -ffreestanding \
		-fno-pie -no-pie -Wl,--build-id=none \
		-o "$(E2E_FIXTURE_DIR)/cgroup-fork-probe" test/fixtures/cgroup_fork_probe.c
	python3 -c 'import struct,sys; h=open(sys.argv[1],"rb").read(64); assert h[:7]==b"\x7fELF\x02\x01\x01" and struct.unpack_from("<H",h,18)[0]=={"x86_64":62,"aarch64":183}[sys.argv[2]], "cgroup probe compiler produced the wrong target architecture"' "$(E2E_FIXTURE_DIR)/cgroup-fork-probe" "$(TARGET_ARCH)"

test-e2e-scripts:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test-ci-source-checks.py
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_e2e_path_id.py
	PYTHONDONTWRITEBYTECODE=1 python3 test/source/cgroup_fork_probe_test.py
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test-environment-go-privilege.py
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_e2e_upload_restore_tick.py
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_e2e_disks_restore.py
	BIN="$(abspath $(BINDIR))" PYTHONDONTWRITEBYTECODE=1 python3 test/source/usage_harness_test.py

VERSION ?= v0.1.0
ACCELERATOR_VERSION ?= v0.1.3
CONNECTOR_VERSION ?= v0.1.2

release: build
	@mkdir -p $(BUILD_DIR)
	rm -rf $(BUILD_DIR)/release-bundle
	SOURCE_DATE_EPOCH="$$(git show -s --format=%ct HEAD)" \
	RELEASE_ACCELERATOR_VERSION="$(ACCELERATOR_VERSION)" \
	RELEASE_CONNECTOR_VERSION="$(CONNECTOR_VERSION)" \
		bash scripts/release.sh package "$(VERSION)" "$(TARGET_ARCH)" \
		$(BUILD_DIR)/release-bundle

test-release:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test-environment-tools.py
	bash scripts/test-release.sh

help:
	@echo "sandboxer. Targets:"
	@echo "  build              sandbox-ctl + sandbox-init"
	@echo "  cloud-hypervisor   patched VMM consumed by sandbox-ctl"
	@echo "  sandbox-ctl        host control plane"
	@echo "  sandbox-init       guest PID 1 binary consumed by guest-runtime"
	@echo "  test-e2e-scripts   source/helper regressions; product E2E uses the prepared platform runner"
	@echo "  release            build a validated component release bundle"
	@echo "  test-release       test component release packaging"
	@echo "  test / vet / clean"
	@echo "  TARGET_ARCH        x86_64 (default) | aarch64"
