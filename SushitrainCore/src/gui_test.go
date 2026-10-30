// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
)

// freeGUIAddress is an address on 127.0.0.1 that nothing listens on right now
func freeGUIAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

// writeGUIConfig writes a configuration as an earlier version of the app made it, with the GUI as given
func writeGUIConfig(t *testing.T, dir string, enabled bool, address string) string {
	t.Helper()
	cfg := config.New(protocolID(testIdentity(t, dir)))
	cfg.GUI.Enabled = enabled
	cfg.GUI.RawAddress = address
	path := filepath.Join(dir, "config.xml")
	writeConfiguration(t, path, cfg)
	return path
}

func loadGUI(t *testing.T, path string, ios bool) (config.GUIConfiguration, string) {
	t.Helper()
	cert := testIdentity(t, filepath.Dir(path))
	cfg, notice, err := loadEngineConfig(path, cert, events.NoopLogger, t.TempDir(), &remoteManagedSet{}, ios)
	if err != nil {
		t.Fatal(err)
	}
	saved := savedConfig(t, path, cert)
	if saved.GUI != cfg.GUI() {
		t.Fatal("the GUI's configuration was not saved")
	}
	return saved.GUI, notice
}

func TestTheGUIIsTurnedOnOnceOnMacOS(t *testing.T) {
	dir := t.TempDir()
	path := writeGUIConfig(t, dir, false, freeGUIAddress(t))
	gui, _ := loadGUI(t, path, false)
	if !gui.Enabled || !gui.IsAuthEnabled() || gui.User != guiUser {
		t.Fatalf("GUI on %v, with a password %v", gui.Enabled, gui.IsAuthEnabled())
	}

	// Turned off since, it stays off
	cert := testIdentity(t, dir)
	conf := savedConfig(t, path, cert)
	conf.GUI.Enabled = false
	writeConfiguration(t, path, conf)
	if gui, _ := loadGUI(t, path, false); gui.Enabled {
		t.Fatal("the GUI was turned on again")
	}
}

func TestTheGUIIsLeftOffWhereOthersCouldReachIt(t *testing.T) {
	_, port, _ := net.SplitHostPort(freeGUIAddress(t))
	for _, address := range []string{"0.0.0.0:" + port, ":" + port, "192.0.2.1:" + port} {
		dir := t.TempDir()
		path := writeGUIConfig(t, dir, false, address)
		if gui, notice := loadGUI(t, path, false); gui.Enabled || notice == "" {
			t.Errorf("%s: GUI on %v (%q)", address, gui.Enabled, notice)
		}
	}
	// Nor where websites could reach it through DNS rebinding
	dir := t.TempDir()
	cert := testIdentity(t, dir)
	conf := config.New(protocolID(cert))
	conf.GUI.Enabled = false
	conf.GUI.RawAddress = freeGUIAddress(t)
	conf.GUI.InsecureSkipHostCheck = true
	path := filepath.Join(dir, "config.xml")
	writeConfiguration(t, path, conf)
	if gui, _ := loadGUI(t, path, false); gui.Enabled {
		t.Error("turned on without the host check")
	}
	// With a password, anywhere
	dir = t.TempDir()
	cert = testIdentity(t, dir)
	conf = config.New(protocolID(cert))
	conf.GUI.Enabled = false
	conf.GUI.RawAddress = "0.0.0.0:" + port
	conf.GUI.User = "me"
	if err := conf.GUI.SetPassword("password"); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "config.xml")
	writeConfiguration(t, path, conf)
	if gui, _ := loadGUI(t, path, false); !gui.Enabled || gui.User != "me" {
		t.Errorf("GUI on %v for %s", gui.Enabled, gui.User)
	}
}

func TestTheGUIStaysOffOnIOS(t *testing.T) {
	dir := t.TempDir()
	path := writeGUIConfig(t, dir, false, freeGUIAddress(t))
	if gui, _ := loadGUI(t, path, true); gui.Enabled {
		t.Fatal("the GUI was turned on")
	}
	if _, err := os.Stat(filepath.Join(dir, guiEnabledMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("marker written: %v", err)
	}
}

func TestAnUnreadableMarkerLeavesTheGUIAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	dir := t.TempDir()
	path := writeGUIConfig(t, dir, false, freeGUIAddress(t))
	marker := filepath.Join(dir, guiEnabledMarkerName)
	if err := os.WriteFile(marker, nil, 0); err != nil {
		t.Fatal(err)
	}
	if gui, _ := loadGUI(t, path, false); gui.Enabled {
		t.Fatal("turned on, although it may have been turned off")
	}
}

func TestATakenGUIAddressMovesAndMovesBack(t *testing.T) {
	// As when Syncthing.app still runs
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := taken.Addr().String()

	dir := t.TempDir()
	path := writeGUIConfig(t, dir, true, address)
	gui, notice := loadGUI(t, path, false)
	host, port, err := net.SplitHostPort(gui.Address())
	if err != nil || host != "127.0.0.1" || gui.Address() == address || port == "0" || notice == "" {
		t.Fatalf("GUI at %s (%q)", gui.Address(), notice)
	}
	// Still taken: it stays, and says so
	if again, notice := loadGUI(t, path, false); again.Address() != gui.Address() || notice == "" {
		t.Fatalf("at %s (%q)", again.Address(), notice)
	}
	// Free again: back
	taken.Close()
	if back, notice := loadGUI(t, path, false); back.Address() != address || notice != "" {
		t.Fatalf("at %s (%q)", back.Address(), notice)
	}
}

func TestAnAddressChosenSinceIsNotMovedBack(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := taken.Addr().String()
	dir := t.TempDir()
	path := writeGUIConfig(t, dir, true, address)
	loadGUI(t, path, false)
	taken.Close()

	// Chosen in the web GUI or the REST API meanwhile
	cert := testIdentity(t, dir)
	conf := savedConfig(t, path, cert)
	chosen := freeGUIAddress(t)
	conf.GUI.RawAddress = chosen
	writeConfiguration(t, path, conf)
	if gui, notice := loadGUI(t, path, false); gui.Address() != chosen || notice != "" {
		t.Fatalf("at %s (%q)", gui.Address(), notice)
	}
}

func TestAnAddressThatCannotBeUsedTurnsTheGUIOff(t *testing.T) {
	// Not on this computer, so nothing can listen there and the engine could not start
	dir := t.TempDir()
	path := writeGUIConfig(t, dir, true, "192.0.2.1:8384")
	if gui, notice := loadGUI(t, path, false); gui.Enabled || notice == "" {
		t.Fatalf("GUI on %v (%q)", gui.Enabled, notice)
	}
}

func TestAnAddressFromTheEnvironmentIsLeftAlone(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	t.Setenv("STGUIADDRESS", taken.Addr().String())
	dir := t.TempDir()
	address := freeGUIAddress(t)
	path := writeGUIConfig(t, dir, true, address)
	if gui, notice := loadGUI(t, path, false); gui.RawAddress != address || notice != "" {
		t.Fatalf("GUI at %s (%q)", gui.RawAddress, notice)
	}
}
