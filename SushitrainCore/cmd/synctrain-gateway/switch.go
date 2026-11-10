// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"t-shaped.nl/sushitrain/v2/gateway"
	"t-shaped.nl/sushitrain/v2/gatewayadmin"
)

// On a server the remote file service is why the gateway runs; on a desktop in Synchronize mode (Synctrain for Linux)
// it is something to turn on, from Synctrain's window, while Syncthing goes on running. Its settings are kept in
// service.json in the service's home; flags given to serve take precedence over them.

type serviceSettings = gatewayadmin.ServiceSettings

var errServiceOff = errors.New(gatewayadmin.ErrServiceOff)

func settingsPath(home string) string {
	return filepath.Join(home, "service.json")
}

func loadSettings(home string) (serviceSettings, bool, error) {
	data, err := os.ReadFile(settingsPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return serviceSettings{}, false, nil
	}
	if err != nil {
		return serviceSettings{}, false, err
	}
	var s serviceSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return serviceSettings{}, false, fmt.Errorf("%s: %w", settingsPath(home), err)
	}
	return s, true, nil
}

func saveSettings(home string, s serviceSettings) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := settingsPath(home) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, settingsPath(home))
}

// serviceDefaults are the service's settings as flags give them, and which of them flags fix
type serviceDefaults struct {
	settings serviceSettings
	// Without a settings file: whether the service runs (on for a server)
	enabled bool
	fixed   []string
}

// effective are the settings for a run: the settings file's, except what flags fix
func (d serviceDefaults) effective(home string) (serviceSettings, error) {
	s, found, err := loadSettings(home)
	if err != nil {
		return serviceSettings{}, err
	}
	if !found {
		s.Enabled = d.enabled
	}
	for _, f := range d.fixed {
		switch f {
		case "enabled":
			s.Enabled = d.settings.Enabled
		case "listen":
			s.Listen = d.settings.Listen
		case "publicHost":
			s.PublicHost = d.settings.PublicHost
		case "publicPort":
			s.PublicPort = d.settings.PublicPort
		}
	}
	if s.Listen == "" {
		s.Listen = d.settings.Listen
	}
	return s, nil
}

// serviceSwitch runs the remote file service for a running Syncthing, or doesn't, as its settings say
type serviceSwitch struct {
	opts     serveOptions
	defaults serviceDefaults
	backend  *gateway.EngineBackend
	admin    *gateway.AdminServer

	mutex    sync.Mutex
	settings serviceSettings
	svc      *gateway.Service
	err      error
	methods  []string
}

func newServiceSwitch(opts serveOptions, defaults serviceDefaults, backend *gateway.EngineBackend,
	admin *gateway.AdminServer) (*serviceSwitch, error) {
	settings, err := defaults.effective(opts.service.Home)
	if err != nil {
		return nil, err
	}
	// The names of the service's methods, which answer that it is off while it is. Registering doesn't call them, so
	// a service that doesn't exist will do.
	names := gateway.NewAdminServer()
	(*gateway.Service)(nil).RegisterAdmin(names, "", 0)
	w := &serviceSwitch{opts: opts, defaults: defaults, backend: backend, admin: admin, settings: settings,
		methods: names.Methods()}
	admin.Register("service.status", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return w.status(), nil
	})
	admin.Register("service.set", func(ctx context.Context, params json.RawMessage) (any, error) {
		var s serviceSettings
		if err := json.Unmarshal(params, &s); err != nil {
			return nil, err
		}
		if err := w.set(s); err != nil {
			return nil, err
		}
		return w.status(), nil
	})
	return w, nil
}

// start starts the service if its settings say it runs. A failure fixed by flags is returned; otherwise it is
// reported in service.status, and Syncthing runs on.
func (w *serviceSwitch) start() error {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	if !w.settings.Enabled {
		w.registerOff()
		return nil
	}
	if err := w.startLocked(); err != nil {
		w.err = err
		w.registerOff()
		if slices.Contains(w.defaults.fixed, "enabled") {
			return err
		}
		slog.Warn("The remote file service couldn't start", "error", err)
	}
	return nil
}

func (w *serviceSwitch) startLocked() error {
	host, port, err := pairingAddress(w.settings.PublicHost, w.settings.PublicPort, w.settings.Listen)
	if err != nil {
		return err
	}
	cfg := w.opts.service
	cfg.ListenAddress = w.settings.Listen
	svc, err := gateway.New(cfg, w.backend)
	if err != nil {
		return err
	}
	if err := svc.Start(); err != nil {
		_ = svc.Close()
		return err
	}
	svc.RegisterAdmin(w.admin, host, port)
	w.svc, w.err = svc, nil
	slog.Info("The remote file service runs", "address", svc.Addr().String())
	return nil
}

// registerOff makes the service's admin methods answer that it is off. Callers hold the mutex.
func (w *serviceSwitch) registerOff() {
	for _, m := range w.methods {
		w.admin.Register(m, func(context.Context, json.RawMessage) (any, error) { return nil, errServiceOff })
	}
}

func (w *serviceSwitch) stopLocked() error {
	if w.svc == nil {
		return nil
	}
	svc := w.svc
	w.svc = nil
	w.registerOff()
	return svc.Close()
}

// stop stops the service, when Syncthing stops
func (w *serviceSwitch) stop() error {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.stopLocked()
}

func (w *serviceSwitch) status() gatewayadmin.ServiceStatus {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	st := gatewayadmin.ServiceStatus{ServiceSettings: w.settings, Running: w.svc != nil, Fixed: w.defaults.fixed}
	if w.err != nil {
		st.Error = w.err.Error()
	}
	if w.svc != nil {
		st.SPKI = w.svc.SPKI()
	}
	return st
}

// set changes the settings, keeps them, and starts, stops or restarts the service to match (a new address for clients
// alone needs no restart)
func (w *serviceSwitch) set(s serviceSettings) error {
	if s.Listen != "" {
		if _, _, err := net.SplitHostPort(s.Listen); err != nil {
			return fmt.Errorf("the listening address %q isn't host:port", s.Listen)
		}
	}
	if s.PublicPort < 0 || s.PublicPort > 65535 {
		return errors.New("the public port must be between 1 and 65535 (or 0 for the listening port)")
	}
	if _, _, err := net.SplitHostPort(strings.TrimSpace(s.PublicHost)); err == nil {
		return fmt.Errorf("the host name for clients %q has a port: give the host only", s.PublicHost)
	}
	w.mutex.Lock()
	defer w.mutex.Unlock()
	for _, f := range w.defaults.fixed {
		switch {
		case f == "enabled" && s.Enabled != w.settings.Enabled,
			f == "listen" && s.Listen != w.settings.Listen && s.Listen != "",
			f == "publicHost" && s.PublicHost != w.settings.PublicHost,
			f == "publicPort" && s.PublicPort != w.settings.PublicPort:
			return fmt.Errorf("%s is set by a flag of synctrain-gateway serve", f)
		}
	}
	if s.Listen == "" {
		s.Listen = w.settings.Listen
	}
	// Only where pairing codes send clients changes: the running service takes it, and its clients stay connected
	if w.svc != nil && s.Enabled && s.Listen == w.settings.Listen {
		host, port, err := pairingAddress(s.PublicHost, s.PublicPort, s.Listen)
		if err != nil {
			return err
		}
		if err := saveSettings(w.opts.service.Home, s); err != nil {
			return err
		}
		w.settings = s
		w.svc.RegisterAdmin(w.admin, host, port)
		return nil
	}
	previous := w.settings
	if err := w.apply(s); err != nil {
		// What ran before runs again, so that a typo doesn't leave paired clients without the service
		if previous.Enabled {
			slog.Warn("The remote file service couldn't start with new settings; back to the previous ones",
				"error", err)
		}
		if restoreErr := w.apply(previous); restoreErr != nil {
			slog.Warn("The remote file service couldn't start with its previous settings either", "error", restoreErr)
		}
		return err
	}
	return nil
}

// apply keeps settings and starts, stops or restarts the service to match. Callers hold the mutex.
func (w *serviceSwitch) apply(s serviceSettings) error {
	if err := saveSettings(w.opts.service.Home, s); err != nil {
		return err
	}
	if err := w.stopLocked(); err != nil {
		slog.Warn("Stopping the remote file service", "error", err)
	}
	w.settings, w.err = s, nil
	if !s.Enabled {
		slog.Info("The remote file service is off")
		return nil
	}
	if err := w.startLocked(); err != nil {
		w.err = err
		w.registerOff()
		return err
	}
	return nil
}
