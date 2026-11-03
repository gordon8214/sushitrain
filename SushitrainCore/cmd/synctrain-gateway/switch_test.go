// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"t-shaped.nl/sushitrain/v2/gateway"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	"t-shaped.nl/sushitrain/v2/gatewayadmin"
)

func TestServiceSwitch(t *testing.T) {
	e := gatewaytest.NewEngine(t, "default", nil)
	home := t.TempDir()
	// Unix socket paths are short on macOS
	dir, err := os.MkdirTemp("", "switch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "admin.sock")
	opts := serveOptions{service: gateway.Config{Home: home, ServiceName: "switch.test", SpoolQuota: 64 << 20,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	// A desktop: off until turned on
	defaults := serviceDefaults{settings: serviceSettings{Listen: "127.0.0.1:0"}}

	start := func(defaults serviceDefaults) *serviceSwitch {
		t.Helper()
		admin := gateway.NewAdminServer()
		w, err := newServiceSwitch(opts, defaults, e.Backend, admin)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.start(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		if err := admin.Listen(ctx, socket, false); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cancel()
			admin.Close()
			_ = w.stop()
		})
		return w
	}
	status := func() gatewayadmin.ServiceStatus {
		t.Helper()
		var st gatewayadmin.ServiceStatus
		if err := gateway.AdminCall(socket, "service.status", nil, &st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	w := start(defaults)
	if st := status(); st.Enabled || st.Running {
		t.Fatalf("on before being turned on: %+v", st)
	}
	err = gateway.AdminCall(socket, "exports.list", nil, nil)
	if err == nil || !strings.Contains(err.Error(), gatewayadmin.ErrServiceOff) {
		t.Fatalf("the service's methods answer while it is off: %v", err)
	}

	var st gatewayadmin.ServiceStatus
	if err := gateway.AdminCall(socket, "service.set", serviceSettings{Enabled: true, Listen: "127.0.0.1:0",
		PublicHost: "desktop.example.org"}, &st); err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.SPKI == "" || st.PublicHost != "desktop.example.org" {
		t.Fatalf("not running after being turned on: %+v", st)
	}
	var session gatewayadmin.PairingSession
	if err := gateway.AdminCall(socket, "pairing.start", nil, &session); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(session.URL, "desktop.example.org") {
		t.Fatalf("pairing code without the public host: %s", session.URL)
	}

	// Kept for the next start, whatever the default is
	if s, found, err := loadSettings(home); err != nil || !found || !s.Enabled || s.Listen != "127.0.0.1:0" {
		t.Fatalf("%+v %v %v", s, found, err)
	}
	if err := w.stop(); err != nil {
		t.Fatal(err)
	}
	effective, err := defaults.effective(home)
	if err != nil || !effective.Enabled {
		t.Fatalf("the setting wasn't kept: %+v %v", effective, err)
	}

	for _, bad := range []serviceSettings{{Enabled: true, Listen: "20920"}, {Enabled: true, PublicPort: 70000}} {
		if err := w.set(bad); err == nil {
			t.Fatalf("%+v was accepted", bad)
		}
	}
	if err := gateway.AdminCall(socket, "service.set", serviceSettings{}, &st); err != nil {
		t.Fatal(err)
	}
	if st.Running || st.Enabled {
		t.Fatalf("running after being turned off: %+v", st)
	}
	if err := gateway.AdminCall(socket, "status", nil, nil); err == nil {
		t.Fatal("status answered while the service is off")
	}
}

func TestServiceFlagsWin(t *testing.T) {
	home := t.TempDir()
	if err := saveSettings(home, serviceSettings{Enabled: false, Listen: ":1234", PublicHost: "file.example.org"}); err != nil {
		t.Fatal(err)
	}
	// A server: --service on --listen :20920
	d := serviceDefaults{settings: serviceSettings{Enabled: true, Listen: ":20920"}, enabled: true,
		fixed: []string{"enabled", "listen"}}
	s, err := d.effective(home)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Enabled || s.Listen != ":20920" || s.PublicHost != "file.example.org" {
		t.Fatalf("%+v", s)
	}
	w := &serviceSwitch{opts: serveOptions{service: gateway.Config{Home: home}}, defaults: d, settings: s}
	if err := w.set(serviceSettings{Enabled: false}); err == nil || !strings.Contains(err.Error(), "enabled") {
		t.Fatalf("turned off what a flag turned on: %v", err)
	}

	// No settings file: the default decides
	empty := t.TempDir()
	for _, enabled := range []bool{true, false} {
		s, err := serviceDefaults{settings: serviceSettings{Listen: ":20920"}, enabled: enabled}.effective(empty)
		if err != nil || s.Enabled != enabled || s.Listen != ":20920" {
			t.Fatalf("default %v: %+v %v", enabled, s, err)
		}
	}
}
