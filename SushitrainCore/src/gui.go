// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/rand"
)

// On macOS the engine runs Syncthing's web GUI and REST API, as Syncthing.app does, on the address in the
// configuration (for a new configuration 127.0.0.1:8384, with a random API key and password). On iOS they stay off for
// the configurations the app makes: there is no web GUI to serve, and the app runs only as long as iOS lets it.

// The file next to the configuration that records that the app turned the web GUI on for it once (so that turning it
// off later lasts) and, as JSON, where the app moved it when its address was in use (so that it moves back once free)
const guiEnabledMarkerName = "synctrain-gui-enabled"

// The GUI user of configurations the app makes, whose password nobody knows: tools use the API key
const guiUser = "synctrain"

// The GUI's own address in a new configuration, as in Syncthing
const defaultGUIAddress = "127.0.0.1:8384"

var ErrGUINotAvailable = errors.New("the web GUI and REST API are not available on iOS")

// What the marker records
type guiMarker struct {
	// The address the GUI is meant to have, while the app moved it to Moved
	Preferred string `json:"preferred,omitempty"`
	Moved     string `json:"moved,omitempty"`
}

// readGUIMarker reads the marker; ok is false when there is none (and only then)
func readGUIMarker(configDir string) (marker guiMarker, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(configDir, guiEnabledMarkerName))
	if errors.Is(err, os.ErrNotExist) {
		return guiMarker{}, false, nil
	} else if err != nil {
		return guiMarker{}, false, err
	}
	if len(data) > 0 {
		// An earlier or damaged marker still records that the GUI was turned on
		_ = json.Unmarshal(data, &marker)
	}
	return marker, true, nil
}

func writeGUIMarker(configDir string, marker guiMarker) {
	data := []byte{}
	if marker != (guiMarker{}) {
		data, _ = json.Marshal(marker)
	}
	if err := writeFileSynced(filepath.Join(configDir, guiEnabledMarkerName), data); err != nil {
		slog.Warn("could not record the web GUI's state", "cause", err)
	}
}

// guiIsLocalOnly reports whether only this computer can reach a GUI: on a unix socket, or on a loopback address with
// Syncthing's host check (which keeps websites from reaching it through DNS rebinding)
func guiIsLocalOnly(gui config.GUIConfiguration) bool {
	if gui.Network() == "unix" {
		return true
	}
	host, _, err := net.SplitHostPort(gui.Address())
	if err != nil || gui.InsecureSkipHostCheck {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// protectGUI gives a GUI without a password a user and a random password nobody knows, so that only the API key opens
// it: other users of this computer could otherwise use it through its web pages' CSRF token
func protectGUI(gui *config.GUIConfiguration) {
	if gui.IsAuthEnabled() {
		return
	}
	gui.User = guiUser
	if err := gui.SetPassword(rand.String(32)); err != nil {
		slog.Warn("could not set the web GUI's password", "cause", err)
	}
}

// prepareGUI readies the web GUI of a configuration being loaded on macOS. It turns the GUI on once, where only this
// computer can reach it, with a password; keeps it on an address it can listen on, moving it to a free port when its
// address is in use (as by Syncthing.app still running) and back once that is free; and turns it off, rather than let
// the engine fail to start, when its address can't be used at all. It returns the marker to write once the
// configuration is saved (nil to leave it) and a notice for the user.
func prepareGUI(conf *config.Configuration, configDir string) (*guiMarker, string) {
	marker, ok, err := readGUIMarker(configDir)
	if err != nil {
		slog.Warn("could not read the web GUI's state; leaving the GUI as it is", "cause", err)
		return nil, ""
	}
	loaded := marker
	// The marker to write: none when it is as it was
	result := func() *guiMarker {
		if ok && marker == loaded {
			return nil
		}
		return &marker
	}
	notice := ""
	if !ok && !conf.GUI.Enabled {
		if guiIsLocalOnly(conf.GUI) || conf.GUI.IsAuthEnabled() {
			slog.Info("turning on the web GUI and REST API")
			conf.GUI.Enabled = true
			protectGUI(&conf.GUI)
		} else {
			notice = fmt.Sprintf("The web GUI and REST API were left off, as other computers could reach them on %s without a password.",
				conf.GUI.Address())
		}
	}
	if !conf.GUI.Enabled || conf.GUI.Network() != "tcp" || os.Getenv("STGUIADDRESS") != "" {
		return result(), notice
	}

	// Back where it is meant to be, once that is free and nobody moved it elsewhere meanwhile
	address := conf.GUI.Address()
	if marker.Moved != "" && address == marker.Moved && marker.Preferred != "" && listenable(marker.Preferred) == nil {
		slog.Info("moving the web GUI back", "address", marker.Preferred)
		conf.GUI.RawAddress = marker.Preferred
		address = marker.Preferred
		marker = guiMarker{}
	} else if address != marker.Moved {
		marker = guiMarker{}
	}

	err = listenable(address)
	switch {
	case err == nil:
		if marker.Moved != "" {
			notice = fmt.Sprintf("The web GUI and REST API are on %s while %s is in use.", marker.Moved, marker.Preferred)
		}
	case errors.Is(err, syscall.EADDRINUSE):
		host, _, _ := net.SplitHostPort(address)
		moved, freeErr := freeAddress(host)
		if freeErr != nil {
			conf.GUI.Enabled = false
			notice = fmt.Sprintf("The web GUI and REST API were turned off, as %s is in use and no other port is free.", address)
			break
		}
		slog.Warn("the web GUI's address is in use; moving it", "address", address, "to", moved)
		if marker.Preferred == "" {
			marker.Preferred = address
		}
		marker.Moved = moved
		conf.GUI.RawAddress = moved
		notice = fmt.Sprintf("The web GUI and REST API are on %s while %s is in use.", moved, marker.Preferred)
	default:
		slog.Warn("the web GUI can't listen on its address; turning it off", "address", address, "cause", err)
		conf.GUI.Enabled = false
		notice = fmt.Sprintf("The web GUI and REST API were turned off, as they can't listen on %s (%v).", address, err)
	}
	return result(), notice
}

// listenable tries listening on a TCP address
func listenable(address string) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	return listener.Close()
}

// freeAddress is an address on host with a port nothing listens on now
func freeAddress(host string) (string, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", err
	}
	defer listener.Close()
	return net.JoinHostPort(host, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)), nil
}

// IsGUIEnabled reports whether the configuration has the web GUI and REST API on
func (clt *Client) IsGUIEnabled() bool {
	cfg := clt.cfg()
	return cfg != nil && cfg.GUI().Enabled
}

// GUIAddress is the address the web GUI and REST API listen on (as STGUIADDRESS may set it)
func (clt *Client) GUIAddress() string {
	cfg := clt.cfg()
	if cfg == nil {
		return ""
	}
	return cfg.GUI().Address()
}

// GUIURL is the URL of the web GUI and REST API
func (clt *Client) GUIURL() string {
	cfg := clt.cfg()
	if cfg == nil {
		return ""
	}
	return cfg.GUI().URL()
}

// GUIAPIKey is the key that the REST API takes in the X-API-Key header
func (clt *Client) GUIAPIKey() string {
	cfg := clt.cfg()
	if cfg == nil {
		return ""
	}
	return cfg.GUI().APIKey
}

// GUIStatusMessage says what the last start did about the web GUI (such as moving it off an address in use), if
// anything
func (clt *Client) GUIStatusMessage() string {
	if message, ok := clt.guiNotice.Load().(string); ok {
		return message
	}
	return ""
}

// SetGUIEnabled turns the web GUI and REST API on or off. Either takes effect when the engine restarts
// (RestartEngine): they only start with it, and a running one keeps listening. On iOS they can only be turned off.
func (clt *Client) SetGUIEnabled(enabled bool) error {
	if build.IsIOS && enabled {
		return ErrGUINotAvailable
	}
	return clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.GUI.Enabled = enabled
		if enabled && !build.IsIOS {
			protectGUI(&cfg.GUI)
		}
	})
}
