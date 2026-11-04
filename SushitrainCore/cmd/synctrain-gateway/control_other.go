// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build !linux

package main

import (
	"context"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/syncthing"
	"t-shaped.nl/sushitrain/v2/gateway"
)

func prepareNativeControl(config.Wrapper) error { return nil }
func registerNativeControl(context.Context, *gateway.AdminServer, *syncthing.App, config.Wrapper, events.Logger) error {
	return nil
}

func nativeConfiguration(_ context.Context, cfg config.Wrapper) (config.Wrapper, error) {
	return cfg, nil
}

func stopNativeConfiguration(config.Wrapper) {}
