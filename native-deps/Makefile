# sandboxer/native-deps — native runtime dependencies for sandbox-ctl.
#
# Produces cloud-hypervisor with the platform patch set used by sandbox-ctl.

SHELL := /bin/bash

HOST_ARCH   := $(shell uname -m)
TARGET_ARCH ?= $(HOST_ARCH)
ifeq ($(TARGET_ARCH),amd64)
  override TARGET_ARCH := x86_64
endif
ifeq ($(TARGET_ARCH),arm64)
  override TARGET_ARCH := aarch64
endif
ifeq ($(TARGET_ARCH),x86_64)
  RUST_TARGET := x86_64-unknown-linux-gnu
else ifeq ($(TARGET_ARCH),aarch64)
  RUST_TARGET := aarch64-unknown-linux-gnu
else
  $(error unsupported TARGET_ARCH=$(TARGET_ARCH); supported: x86_64, aarch64)
endif

ifeq ($(HOST_ARCH),$(TARGET_ARCH))
  CROSS_PREFIX :=
else ifeq ($(TARGET_ARCH),x86_64)
  CROSS_PREFIX := x86_64-linux-gnu-
else ifeq ($(TARGET_ARCH),aarch64)
  CROSS_PREFIX := aarch64-linux-gnu-
endif

BINDIR      := bin/$(TARGET_ARCH)
BUILD_DIR   := build/$(TARGET_ARCH)
TARBALL_DIR := build/tarball

CLOUD_HYPERVISOR_TARBALL        ?= https://github.com/cloud-hypervisor/cloud-hypervisor/archive/refs/tags/v51.1.tar.gz\#cloud-hypervisor-51.1.tar.gz
CLOUD_HYPERVISOR_TARBALL_SHA256 ?=
CLOUD_HYPERVISOR_SRC            ?= $(abspath build/src/cloud-hypervisor)
CLOUD_HYPERVISOR_BUILD_OUT      ?= $(abspath $(BUILD_DIR)/cloud-hypervisor)
CH_PATCHES_DIR                  := $(abspath deps/ch-patches)

DEPS_ENV = \
    TARGET_ARCH="$(TARGET_ARCH)" \
    RUST_TARGET="$(RUST_TARGET)" \
    CROSS_PREFIX="$(CROSS_PREFIX)" \
    BUILD_DIR="$(abspath $(BUILD_DIR))" \
    BINDIR="$(abspath $(BINDIR))" \
    TARBALL_CACHE="$(abspath $(TARBALL_DIR))"

define link_bin
@if [ "$(HOST_ARCH)" = "$(TARGET_ARCH)" ]; then \
   mkdir -p bin && ln -sfn $(TARGET_ARCH)/$(1) bin/$(1); \
 fi
endef

.PHONY: all build cloud-hypervisor ch-fetch ch-patches-apply ch-patches ch-patches-format ch-build test clean help

all: build
build: cloud-hypervisor

CH_INVOKE = $(DEPS_ENV) \
    CLOUD_HYPERVISOR_TARBALL="$(CLOUD_HYPERVISOR_TARBALL)" \
    CLOUD_HYPERVISOR_TARBALL_SHA256="$(CLOUD_HYPERVISOR_TARBALL_SHA256)" \
    CH_SRC="$(CLOUD_HYPERVISOR_SRC)" \
    CH_BUILD_OUT="$(CLOUD_HYPERVISOR_BUILD_OUT)" \
    PATCHES_DIR="$(CH_PATCHES_DIR)"

ch-fetch:
	STAGE=fetch $(CH_INVOKE) bash deps/build-cloud-hypervisor.sh
ch-patches-apply: ch-fetch
	STAGE=patches-apply $(CH_INVOKE) bash deps/build-cloud-hypervisor.sh
ch-patches: ch-patches-apply
ch-patches-format:
	STAGE=patches-format $(CH_INVOKE) bash deps/build-cloud-hypervisor.sh
ch-build:
	STAGE=build $(CH_INVOKE) bash deps/build-cloud-hypervisor.sh
cloud-hypervisor: ch-patches-apply ch-build
	$(call link_bin,cloud-hypervisor)

test:
	bash -n deps/*.sh

clean:
	rm -rf $(CLOUD_HYPERVISOR_BUILD_OUT) bin

help:
	@echo "sandboxer/native-deps targets:"
	@echo "  build / cloud-hypervisor  build patched cloud-hypervisor"
	@echo "  ch-fetch                  fetch/import upstream source"
	@echo "  ch-patches-apply          apply deps/ch-patches"
	@echo "  ch-patches-format         extract source WIP back to deps/ch-patches"
	@echo "  test                      syntax-check build scripts"
	@echo "  clean                     remove build artifacts"
	@echo "  TARGET_ARCH               x86_64 (default) | aarch64"
