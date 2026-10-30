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

build/SushitrainCore.xcframework: src/*.go gateway/*.go sthome/*.go Makefile go.mod go.sum notices.html
	go tool gomobile bind -target ios,iossimulator,macos -iosversion=15 -tags noassets,noupgrade -o ./build/SushitrainCore.xcframework ./src

install: build

clean:
	rm -rf build/* notices.html

test:
	go test ./... -tags noassets -timeout 300s

# Starts and restarts the engine; about 30 seconds with the race detector
test-engine:
	go test -tags noassets,engineintegration -race -count=1 -timeout 600s -run Engine ./src

# The remote file service as a headless executable for Linux hosts, with Syncthing's web GUI
GATEWAY_VERSION ?= $(shell git describe --always --dirty 2>/dev/null)
# The engine's version as Syncthing reports it (to its GUI and other devices)
SYNCTHING_VERSION ?= v2.1.7-gateway.1
SYNCTHING_BUILD = github.com/syncthing/syncthing/lib/build

gateway-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -tags noupgrade -ldflags "-s -w -X main.version=$(GATEWAY_VERSION) \
		-X $(SYNCTHING_BUILD).Version=$(SYNCTHING_VERSION) -X $(SYNCTHING_BUILD).Tags=noupgrade" \
		-o build/linux-amd64/synctrain-gateway ./cmd/synctrain-gateway

# Test executables to run on a Linux host (go test -c), including the multi-device integration tests
gateway-test-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -tags noassets,gwintegration -o build/linux-amd64/gateway.test ./gateway
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o build/linux-amd64/synctrain-gateway.test ./cmd/synctrain-gateway
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -tags noassets -o build/linux-amd64/sthome.test ./sthome
