// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"context"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
	"path/filepath"
	"t-shaped.nl/sushitrain/v2/enginecontrol"
	"t-shaped.nl/sushitrain/v2/gateway"
	"t-shaped.nl/sushitrain/v2/history"
	"t-shaped.nl/sushitrain/v2/linuxpolicy"
	core "t-shaped.nl/sushitrain/v2/src"
)

func prepareNativeControl(cfg config.Wrapper) error { return core.PrepareDesktopEngine(cfg) }
func nativeConfiguration(ctx context.Context, cfg config.Wrapper) (config.Wrapper, error) {
	p, err := linuxpolicy.Open(cfg, filepath.Join(locations.GetBaseDir(locations.ConfigBaseDir), "connectivity.json"))
	if err != nil {
		return nil, err
	}
	if err = p.Start(ctx); err != nil {
		return nil, err
	}
	return p, nil
}
func registerNativeControl(ctx context.Context, a *gateway.AdminServer, app *syncthing.App, cfg config.Wrapper, logger events.Logger) error {
	cert, err := loadIdentity()
	if err != nil {
		return err
	}
	c, detach, err := core.AttachDesktopEngine(a.Context(), app, cfg, logger, cert)
	if err != nil {
		return err
	}
	control := enginecontrol.New(c, func() error { go app.Stop(svcutil.ExitRestart); return nil })
	if p, ok := cfg.(*linuxpolicy.Policy); ok {
		control.Pause = p.SetPaused
		p.Register(a)
	}
	control.Register(a)
	if err := history.RegisterDesktop(a); err != nil {
		control.Close()
		detach()
		return err
	}
	a.OnClose(func() { control.Close(); detach() })
	return nil
}

func stopNativeConfiguration(cfg config.Wrapper) {
	if p, ok := cfg.(*linuxpolicy.Policy); ok {
		p.Stop()
	}
}
