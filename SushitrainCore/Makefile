# Copyright (C) 2024-2026 Tommy van der Vorst
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this file,
# You can obtain one at https://mozilla.org/MPL/2.0/.
PATH := $(shell pwd):/opt/homebrew/bin:$(HOME)/go/bin:$(PATH)
SHELL := env PATH="$(PATH)" /bin/bash
GO_VERSION := $(shell go version)

.PHONY: clean install deps test test-engine gateway-linux gateway-test-linux

build: build/SushitrainCore.xcframework

deps:
ifndef GO_VERSION
	echo Go is not installed!
endif
	go get -tool golang.org/x/mobile/cmd/gomobile
	go get -tool golang.org/x/mobile/cmd/gobind
	go tool gomobile init

notices.html: notices.template.html go.mod go.sum
	go get -tool github.com/google/go-licenses@latest
	go tool go-licenses report ./... --template=./notices.template.html > notices.html

SYNCTHING_BUILD = github.com/syncthing/syncthing/lib/build

# Stamped when linking, because lib/build works out the long version in its init, before any of our code runs. The time is
# the commit's, as in Syncthing's own builds, and the builder is fixed, so that a build doesn't name the computer it was
# made on. The app and the gateway share these.
BUILDER_LDFLAGS = -X $(SYNCTHING_BUILD).User=synctrain -X $(SYNCTHING_BUILD).Host=gordon8214.github.io \
	-X $(SYNCTHING_BUILD).Stamp=$(shell git log -1 --format=%ct 2>/dev/null || date +%s)

# The engine's version in the app, which its GUI, other devices and logs all show. Raise N when go.mod moves to another
# commit of the fork. The engine won't start on a version lib/build can't parse.
ENGINE_VERSION = v2.1.7-synctrain.6
ENGINE_LDFLAGS = -X $(SYNCTHING_BUILD).Version=$(ENGINE_VERSION) -X $(SYNCTHING_BUILD).Tags=noupgrade,synctrain \
	$(BUILDER_LDFLAGS)

# Without noassets, the app's web GUI is the one built into the fork commit in go.mod, as in the gateway
ENGINE_TAGS = noupgrade

build/SushitrainCore.xcframework: src/*.go gateway/*.go sthome/*.go Makefile go.mod go.sum notices.html
	go tool gomobile bind -target macos -tags $(ENGINE_TAGS) -trimpath -ldflags "$(ENGINE_LDFLAGS)" \
		-o ./build/SushitrainCore.xcframework ./src

install: build

clean:
	rm -rf build/* notices.html

test:
	ENGINE_VERSION=$(ENGINE_VERSION) go test ./... -tags noassets -ldflags "$(ENGINE_LDFLAGS)" -timeout 300s

# Starts and restarts the engine; about 70 seconds with the race detector
test-engine:
	go test -tags $(ENGINE_TAGS),engineintegration -ldflags "$(ENGINE_LDFLAGS)" -race -count=1 -timeout 600s -run Engine ./src

# The remote file service as a headless executable for Linux hosts, with Syncthing's web GUI
GATEWAY_VERSION ?= $(shell git describe --always --dirty 2>/dev/null)
# The engine's version as Syncthing reports it (to its GUI and other devices)
SYNCTHING_VERSION ?= v2.1.7-gateway.1

gateway-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -tags noupgrade -ldflags "-s -w -X main.version=$(GATEWAY_VERSION) \
		-X $(SYNCTHING_BUILD).Version=$(SYNCTHING_VERSION) -X $(SYNCTHING_BUILD).Tags=noupgrade $(BUILDER_LDFLAGS)" \
		-o build/linux-amd64/synctrain-gateway ./cmd/synctrain-gateway

# Test executables to run on a Linux host (go test -c), including the multi-device integration tests
gateway-test-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -tags noassets,gwintegration -o build/linux-amd64/gateway.test ./gateway
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o build/linux-amd64/synctrain-gateway.test ./cmd/synctrain-gateway
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -tags noassets -o build/linux-amd64/sthome.test ./sthome
