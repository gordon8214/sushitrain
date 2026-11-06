# Copyright (C) 2024-2026 Tommy van der Vorst
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at https://mozilla.org/MPL/2.0/.
export PATH := $(CURDIR):/opt/homebrew/bin:$(HOME)/go/bin:$(PATH)
SHELL := /bin/bash
GO_VERSION := $(shell go version)

.PHONY: clean install deps test test-engine mcp-linux gateway-linux gateway-test-linux linux linux-window linux-test linux-nautilus linux-nautilus-test linux-kde linux-kde-test

build: build/SushitrainCore.xcframework

# gomobile and gobind are the versions in go.mod, so this leaves go.mod alone; the README says how to move them. gomobile init
# installs the latest gobind, and bind runs the gobind on the PATH, so the one from go.mod has to go back in its place.
deps:
ifndef GO_VERSION
	echo Go is not installed!
endif
	go tool gomobile init
	go install golang.org/x/mobile/cmd/gobind

# go-licenses is the version in go.mod, so this needs no network and leaves go.mod alone
notices.html: notices.template.html go.mod go.sum
	go tool go-licenses report ./... --template=./notices.template.html > notices.html

SYNCTHING_BUILD = github.com/syncthing/syncthing/lib/build

# Stamped when linking, because lib/build works out the long version in its init, before any of our code runs. The time is
# the commit's, as in Syncthing's own builds, and the builder is fixed, so that a build doesn't name the computer it was
# made on. The app and the gateway share these.
BUILDER_LDFLAGS = -X $(SYNCTHING_BUILD).User=synctrain -X $(SYNCTHING_BUILD).Host=gordon8214.github.io \
	-X $(SYNCTHING_BUILD).Stamp=$(shell git log -1 --format=%ct 2>/dev/null || date +%s)

# The engine's version in the app, which its GUI, other devices and logs all show. Scripts/update-fork.sh raises N when
# it moves go.mod to another commit of the fork. The engine won't start on a version lib/build can't parse.
ENGINE_VERSION = v2.1.7-synctrain.15
ENGINE_LDFLAGS = -X $(SYNCTHING_BUILD).Version=$(ENGINE_VERSION) -X $(SYNCTHING_BUILD).Tags=noupgrade,synctrain \
	$(BUILDER_LDFLAGS)

# Without noassets, the app's web GUI is the one built into the fork commit in go.mod, as in the gateway
ENGINE_TAGS = noupgrade

# The app's MACOSX_DEPLOYMENT_TARGET. Without it the objects take the SDK's version, and the linker warns about each one.
MACOS_VERSION = 27.0

build/SushitrainCore.xcframework: src/*.go desktopui/*.go desktopui/catalog.json hostbridge/*.go mcpapi/*.go mcpcontrol/*.go enginecontrol/*.go desktopapi/*.go gateway/*.go versionsnapshot/*.go sthome/*.go remoteapi/*.go remoteclient/*.go adminsock/*.go gatewayadmin/*.go files/*.go sharedfile/*.go sqlitefile/*.go previewcache/*.go Makefile go.mod go.sum notices.html
	# gomobile replaces CGO_CFLAGS, dropping Go's default -O2. CPPFLAGS reaches every C/C++ compile unchanged,
	# so restore optimization here, particularly for SQLite's amalgamation.
	CGO_CPPFLAGS="$(CGO_CPPFLAGS) -O2" go tool gomobile bind -target macos -macosversion $(MACOS_VERSION) -tags $(ENGINE_TAGS) -trimpath \
		-ldflags "$(ENGINE_LDFLAGS)" -o ./build/SushitrainCore.xcframework ./src ./hostbridge

install: build

clean:
	rm -rf build/* notices.html

test:
	ENGINE_VERSION=$(ENGINE_VERSION) go test ./... -tags noassets -ldflags "$(ENGINE_LDFLAGS)" -timeout 300s

# Starts and restarts the engine; about 70 seconds with the race detector
test-engine:
	go test -tags $(ENGINE_TAGS),engineintegration -ldflags "$(ENGINE_LDFLAGS)" -race -count=1 -timeout 600s -run Engine ./src

# The remote file service as a headless executable for Linux hosts, with Syncthing's web GUI
GATEWAY_VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
# The engine's version as Syncthing reports it (to its GUI and other devices)
SYNCTHING_VERSION ?= $(ENGINE_VERSION)
LINUX_ARCH ?= $(shell go env GOARCH)

gateway-linux: mcp-linux
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go build -trimpath -tags noupgrade -ldflags "-s -w -X main.version=$(GATEWAY_VERSION) \
		-X $(SYNCTHING_BUILD).Version=$(SYNCTHING_VERSION) -X $(SYNCTHING_BUILD).Tags=noupgrade $(BUILDER_LDFLAGS)" \
		-o build/linux-$(LINUX_ARCH)/synctrain-gateway ./cmd/synctrain-gateway

# Synctrain for Linux (Docs/linux.md, built on the Linux host): the daemons are static, like the gateway
LINUX_VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
mcp-linux:
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go build -mod=readonly -trimpath -ldflags "-s -w -X main.version=$(LINUX_VERSION)" -o build/linux-$(LINUX_ARCH)/synctrain-mcp ./cmd/synctrain-mcp

linux: gateway-linux
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go build -mod=readonly -trimpath -o build/linux-$(LINUX_ARCH)/synctrain-desktop ./cmd/synctrain-desktop
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(LINUX_VERSION)" \
		-o build/linux-$(LINUX_ARCH)/synctrain-files ./cmd/synctrain-files

# The window links GTK 4 and libadwaita (cgo): on a Linux host with their development packages, or in a container
# with ../Linux/build/build-window.sh
linux-window:
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=$(LINUX_VERSION)" \
		-o build/linux-$(LINUX_ARCH)/synctrain-gtk ./cmd/synctrain

# Native Nautilus module and its bounded GLib suite; build on the target Linux architecture.
linux-nautilus:
	$(MAKE) -C ../Linux/nautilus LINUX_ARCH="$(LINUX_ARCH)" BUILD_DIR="$(CURDIR)/build/linux-$(LINUX_ARCH)"

linux-nautilus-test:
	$(MAKE) -C ../Linux/nautilus LINUX_ARCH="$(LINUX_ARCH)" BUILD_DIR="$(CURDIR)/build/linux-$(LINUX_ARCH)" test

# Test executables of Synctrain for Linux, to run on a Linux host (go test -c). Fixtures are read from ../Docs, so run
# them from a checkout's SushitrainCore/<package> directory. The FUSE tests need /dev/fuse and fusermount3.
LINUX_TEST_PACKAGES = linuxdesktop desktopapi desktopsettings mcpapi mcpcontrol mcpfiles mcpserver remoteapi adminsock remoteclient files files/fusefs enginecontrol linuxpolicy desktoptray history safecopy previewcache
linux-test:
	for p in $(LINUX_TEST_PACKAGES); do \
		GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go test -c -tags noassets -o build/linux-$(LINUX_ARCH)/$$(echo $$p | tr / -).test \
			./$$p || exit 1; \
	done

# Test executables to run on a Linux host (go test -c), including the multi-device integration tests
gateway-test-linux:
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go test -c -tags noassets,gwintegration -o build/linux-$(LINUX_ARCH)/gateway.test ./gateway
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go test -c -o build/linux-$(LINUX_ARCH)/synctrain-gateway.test ./cmd/synctrain-gateway
	GOOS=linux GOARCH=$(LINUX_ARCH) CGO_ENABLED=0 go test -c -tags noassets -o build/linux-$(LINUX_ARCH)/sthome.test ./sthome

# Native Qt/KDE frontend and Dolphin integration, independent of GTK.
linux-kde:
	../Linux/build/build-kde.sh
linux-kde-test:
	../Linux/build/build-kde.sh test
