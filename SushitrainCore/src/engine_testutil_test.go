// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"context"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/syncthing"
)

// serveConfiguration serves a configuration for the rest of a test. The last save stays out of the way of the removal
// of the test's temporary directories, which t.Context ending would not.
func serveConfiguration(t *testing.T, cfg config.Wrapper) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cfg.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// withTestRun gives a client built for a test the configuration (served by the test) and, when not nil, the app and
// event logger of a run, as if Load and Start had run. The test owns these: the run is for reading and changing the
// configuration and calling the model, not for stopping or restarting.
func (clt *Client) withTestRun(cfg config.Wrapper, app *syncthing.App, evLogger events.Logger) *Client {
	r := &engineRun{config: cfg, app: app, evLogger: evLogger, firstRun: true, released: true, guiStarted: cfg.GUI().Enabled}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	state := EngineStateLoaded
	if app != nil && app.Internals != nil {
		r.internals.Store(app.Internals)
		state = EngineStateRunning
	}
	clt.run.Store(r)
	clt.status.Store(&engineStatus{state: state})
	return clt
}
