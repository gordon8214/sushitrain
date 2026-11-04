# Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
CC ?= cc
PKG_CONFIG ?= pkg-config
LINUX_ARCH ?= $(shell go env GOARCH)
BUILD_DIR ?= ../../SushitrainCore/build/linux-$(LINUX_ARCH)
PACKAGES = libnautilus-extension-4 gio-unix-2.0 json-glib-1.0 gmodule-2.0
PACKAGE_CFLAGS = $(shell $(PKG_CONFIG) --cflags $(PACKAGES))
PACKAGE_LIBS = $(shell $(PKG_CONFIG) --libs $(PACKAGES))
CFLAGS ?= -O2 -g
# These are required even when callers override optimization or sanitizer flags.
MODULE_CFLAGS = -std=c11 -Wall -Wextra -Werror -Wformat=2 -Wstrict-prototypes -Wmissing-prototypes -fPIC -fvisibility=hidden

.PHONY: all test check-native FORCE
all: $(BUILD_DIR)/libsynctrain-nautilus.so

check-native:
	@case "$$(uname -s):$(LINUX_ARCH):$$(uname -m)" in \
		Linux:amd64:x86_64|Linux:arm64:aarch64) ;; \
		*) echo "Build the Nautilus module on a Linux host of the requested architecture ($(LINUX_ARCH))" >&2; exit 2 ;; \
	esac
	@case "$(LINUX_ARCH):$$($(CC) -dumpmachine)" in \
		amd64:x86_64-*|arm64:aarch64-*) ;; \
		*) echo "The C compiler must target $(LINUX_ARCH)" >&2; exit 2 ;; \
	esac

$(BUILD_DIR): | check-native
	mkdir -p "$@"

# Small C targets rebuild on every invocation: native/container library baselines,
# compiler options, and installed development packages can change without source edits.
FORCE:

$(BUILD_DIR)/libsynctrain-nautilus.so: synctrain.c support.c support.h Makefile FORCE | check-native $(BUILD_DIR)
	$(CC) $(CPPFLAGS) $(PACKAGE_CFLAGS) $(CFLAGS) $(MODULE_CFLAGS) -shared -Wl,-z,defs -Wl,-z,relro,-z,now -o "$@" synctrain.c support.c $(LDFLAGS) $(LDLIBS) $(PACKAGE_LIBS)

$(BUILD_DIR)/nautilus.test: ../tests/test_nautilus.c synctrain.c support.c support.h Makefile FORCE | check-native $(BUILD_DIR)
	$(CC) $(CPPFLAGS) $(PACKAGE_CFLAGS) $(CFLAGS) $(MODULE_CFLAGS) -I. -o "$@" ../tests/test_nautilus.c support.c $(LDFLAGS) $(LDLIBS) $(PACKAGE_LIBS)

# Small native suite, normally under 5 seconds. Bound it even outside CI.
test: all $(BUILD_DIR)/nautilus.test
	timeout 20 $(BUILD_DIR)/nautilus.test $(BUILD_DIR)/libsynctrain-nautilus.so
