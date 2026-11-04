// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package linuxpolicy

import (
	"context"
	"errors"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
	"os"
	"path/filepath"
	"testing"
)

func TestPolicyReasons(t *testing.T) {
	cases := []struct {
		s    Settings
		e    Environment
		want string
	}{{Settings{}, Environment{}, ""}, {Settings{Metered: true}, Environment{}, "Waiting for network policy status"}, {Settings{PowerSaver: true}, Environment{}, "Waiting for power policy status"}, {Settings{Metered: true}, Environment{NetworkKnown: true, Metered: true}, "Paused on a metered network"}, {Settings{Cellular: true}, Environment{NetworkKnown: true}, "Waiting for network policy status"}, {Settings{Cellular: true}, Environment{CellularKnown: true, Cellular: true}, "Paused on a cellular network"}, {Settings{PowerSaver: true}, Environment{PowerKnown: true, PowerSaver: true}, "Paused in Power Saver"}}
	for _, c := range cases {
		if got := Reason(c.s, c.e); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}
func TestPolicyPreservesManualChoicesAcrossRestart(t *testing.T) {
	self := protocol.LocalDeviceID
	remote := protocol.NewDeviceID([]byte("remote policy peer"))
	conf := config.New(self)
	conf.SetDevice(config.DeviceConfiguration{DeviceID: remote, Name: "Peer", Addresses: []string{"dynamic"}})
	cfg := config.Wrap(filepath.Join(t.TempDir(), "config.xml"), conf, self, events.NoopLogger)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); cfg.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	file := filepath.Join(t.TempDir(), "connectivity.json")
	p, err := Open(cfg, file)
	if err != nil {
		t.Fatal(err)
	}
	p.settings.Metered = true
	p.environment = Environment{NetworkKnown: true, Metered: true}
	if err = p.apply(); err != nil {
		t.Fatal(err)
	}
	device, _ := cfg.Device(remote)
	if !device.Paused {
		t.Fatal("metered policy did not pause")
	}
	if err = p.SetPaused([]string{remote.String()}, true); err != nil {
		t.Fatal(err)
	}
	p.environment.Metered = false
	if err = p.apply(); err != nil {
		t.Fatal(err)
	}
	device, _ = cfg.Device(remote)
	if !device.Paused {
		t.Fatal("policy resumed a manually paused device")
	}
	restored, err := Open(cfg, file)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.settings.Manual[remote.String()] {
		t.Fatal("manual pause was not persisted")
	}
	restored.environment = p.environment
	if err = restored.SetPaused([]string{remote.String()}, false); err != nil {
		t.Fatal(err)
	}
	device, _ = cfg.Device(remote)
	if device.Paused {
		t.Fatal("explicit resume did not clear manual pause")
	}
}
func TestExternalResumeStaysFenced(t *testing.T) {
	self := protocol.LocalDeviceID
	remote := protocol.NewDeviceID([]byte("external peer"))
	conf := config.New(self)
	conf.SetDevice(config.DeviceConfiguration{DeviceID: remote, Name: "Peer", Addresses: []string{"dynamic"}})
	cfg := config.Wrap(filepath.Join(t.TempDir(), "config.xml"), conf, self, events.NoopLogger)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); cfg.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	p, err := Open(cfg, filepath.Join(t.TempDir(), "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	p.settings.Metered = true
	p.environment = Environment{NetworkKnown: true, Metered: true}
	p.apply()
	waiter, err := p.Modify(func(c *config.Configuration) { d, _, _ := c.Device(remote); d.Paused = false; c.SetDevice(d) })
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
	d, _ := cfg.Device(remote)
	if !d.Paused {
		t.Fatal("external resume escaped automatic pause")
	}
	p.environment.Metered = false
	p.apply()
	d, _ = cfg.Device(remote)
	if d.Paused {
		t.Fatal("external resume was not honored when policy cleared")
	}
}

func TestExplicitPauseIntentAndConfigurationRoundTrip(t *testing.T) {
	self := protocol.LocalDeviceID
	peer := protocol.NewDeviceID([]byte("explicit pause peer"))
	conf := config.New(self)
	conf.SetDevice(config.DeviceConfiguration{DeviceID: peer, Addresses: []string{"dynamic"}})
	cfg := config.Wrap(filepath.Join(t.TempDir(), "config.xml"), conf, self, events.NoopLogger)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); cfg.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	file := filepath.Join(t.TempDir(), "policy.json")
	p, err := Open(cfg, file)
	if err != nil {
		t.Fatal(err)
	}
	p.settings.Metered = true
	p.environment = Environment{NetworkKnown: true, Metered: true}
	if err = p.apply(); err != nil {
		t.Fatal(err)
	}
	// A GET followed by PUT repeats the effective automatic pause, not new intent.
	echo := p.RawCopy()
	waiter, err := p.Modify(func(current *config.Configuration) { *current = echo })
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
	if p.settings.Manual[peer.String()] {
		t.Fatal("configuration round trip converted an automatic pause to manual")
	}
	if err = p.SetDevicePause([]protocol.DeviceID{peer}, true); err != nil {
		t.Fatal(err)
	}
	p.environment.Metered = false
	if err = p.apply(); err != nil {
		t.Fatal(err)
	}
	device, _ := cfg.Device(peer)
	if !device.Paused {
		t.Fatal("explicit pause was lost when the automatic fence cleared")
	}
	reopened, err := Open(cfg, file)
	if err != nil || !reopened.settings.Manual[peer.String()] {
		t.Fatal("explicit pause was not persisted", err)
	}
	p.environment.Metered = true
	if err = p.apply(); err != nil {
		t.Fatal(err)
	}
	if err = p.SetDevicePause([]protocol.DeviceID{peer}, false); err != nil {
		t.Fatal(err)
	}
	device, _ = cfg.Device(peer)
	if !device.Paused || p.settings.Manual[peer.String()] {
		t.Fatal("resume must clear manual intent while respecting the automatic fence")
	}
	p.environment.Metered = false
	if err = p.apply(); err != nil {
		t.Fatal(err)
	}
	device, _ = cfg.Device(peer)
	if device.Paused {
		t.Fatal("resumed device stayed paused after the automatic fence cleared")
	}
}

type rejectingConfig struct{}

func (rejectingConfig) VerifyConfiguration(_, _ config.Configuration) error {
	return errors.New("rejected")
}
func (rejectingConfig) CommitConfiguration(_, _ config.Configuration) bool { return true }
func (rejectingConfig) String() string                                     { return "reject policy change" }
func TestRejectedConfigurationDoesNotPersistIntent(t *testing.T) {
	self := protocol.LocalDeviceID
	remote := protocol.NewDeviceID([]byte("rejected peer"))
	conf := config.New(self)
	conf.SetDevice(config.DeviceConfiguration{DeviceID: remote, Addresses: []string{"dynamic"}})
	cfg := config.Wrap(filepath.Join(t.TempDir(), "config.xml"), conf, self, events.NoopLogger)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); cfg.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	file := filepath.Join(t.TempDir(), "policy.json")
	p, err := Open(cfg, file)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Subscribe(rejectingConfig{})
	if _, err = p.Modify(func(c *config.Configuration) { d, _, _ := c.Device(remote); d.Paused = true; c.SetDevice(d) }); err == nil {
		t.Fatal("change was not rejected")
	}
	if p.settings.Manual[remote.String()] {
		t.Fatal("rejected configuration changed saved pause intention")
	}
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("rejected edit persisted policy", err)
	}
	if err = p.SetPaused([]string{remote.String(), "invalid"}, true); err == nil {
		t.Fatal("invalid device accepted")
	}
	if p.settings.Manual[remote.String()] {
		t.Fatal("part of an invalid bulk pause was applied")
	}
}
