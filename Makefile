# sandboxer — microVM sandbox lifecycle engine.
#
#   sandbox-ctl     host control plane (run / snapshot / exec / config / info)
#   sandbox-init    guest PID 1 source/binary (packed by guest-runtime)
#
# `make build` produces the two sandboxer binaries. The guest-runtime repo
# consumes sandbox-init to produce sandbox-runtime.bundle.

SHELL := /bin/bash

.PHONY: all build sandbox-ctl sandbox-init cloud-hypervisor native-deps test vet bench release test-release clean help

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

test:
	CGO_ENABLED=0 $(GO) test ./...

vet:
	CGO_ENABLED=0 $(GO) vet ./...

clean:
	rm -rf bin build
	$(MAKE) -C native-deps clean

# Go micro-benchmarks. Sandbox-level e2e (cold/snapshot/restore/...) lives in
# platform/test/e2e — they need vmlinux + cloud-hypervisor + mkfs.erofs
# (from guest-runtime/native-deps), sandbox-runtime.bundle (from guest-runtime),
# and accelerator binaries, so they are cross-repo.
bench:
	CGO_ENABLED=0 $(GO) test -bench=. -benchmem -run=^$$ ./...

VERSION ?= v0.1.0

release: build
	@mkdir -p $(BUILD_DIR)
	rm -rf $(BUILD_DIR)/release-bundle
	SOURCE_DATE_EPOCH="$$(git show -s --format=%ct HEAD)" \
		bash scripts/release.sh package "$(VERSION)" "$(TARGET_ARCH)" \
		$(BUILD_DIR)/release-bundle

test-release:
	bash scripts/test-release.sh

help:
	@echo "sandboxer. Targets:"
	@echo "  build              sandbox-ctl + sandbox-init"
	@echo "  cloud-hypervisor   patched VMM consumed by sandbox-ctl"
	@echo "  sandbox-ctl        host control plane"
	@echo "  sandbox-init       guest PID 1 binary consumed by guest-runtime"
	@echo "  release            build a validated component release bundle"
	@echo "  test-release       test component release packaging"
	@echo "  test / vet / clean"
	@echo "  TARGET_ARCH        x86_64 (default) | aarch64"
