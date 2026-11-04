// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux && cgo

// Command synctrain is Synctrain's window on Linux (GTK 4 and libadwaita). It sets a machine up in one of the two
// modes, and manages the daemon of that mode through its admin socket: it keeps no state of its own. "synctrain
// versions <path>" shows the previous versions of a file in the mount (the Nautilus extension opens it). See
// Docs/linux.md.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
)

// Set with -ldflags "-X main.version=…"
var version = ""

var queuedPairingLink string

const appID = "com.gordonbyrnes.Synctrain"

func main() {
	if len(os.Args) == 3 && os.Args[1] == "versions" {
		os.Exit(runVersions(os.Args[2]))
	}
	if len(os.Args) > 1 && os.Args[1] != "--background" && os.Args[1] != "--help" && os.Args[1] != "-h" && !strings.HasPrefix(os.Args[1], "synctrain+pair://") {
		fmt.Fprintln(os.Stderr, "Usage: synctrain [--background | versions <path> | pairing-link]")
		os.Exit(2)
	}
	app := adw.NewApplication(appID, gio.ApplicationHandlesCommandLine)
	app.AddMainOption("background", 0, glib.OptionFlagNone, glib.OptionArgNone, "Start the optional tray without opening a window", "")
	activeApplication = app
	installWindowActivation(app, nil)
	app.ConnectCommandLine(func(command *gio.ApplicationCommandLine) (status int) {
		// Complete forwarded launches explicitly rather than waiting for Go to
		// collect the command-line object's retained GObject reference.
		defer func() { command.SetExitStatus(status); command.Done() }()
		args := command.Arguments()
		backgroundOnly := command.OptionsDict().Contains("background")
		if backgroundOnly && statusTray == nil {
			if err := configureTray(true); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		if !backgroundOnly {
			app.Activate()
		}
		w := activeWindow
		if len(args) > 1 && strings.HasPrefix(args[1], "synctrain+pair://") && w != nil {
			if w.desk != nil && !w.changing {
				w.desk.guard(func() { w.desk.pairLink(args[1]) })
			} else {
				queuedPairingLink = args[1]
				if !w.changing {
					w.inform("Set Up Synctrain", "Choose a mode to continue with this pairing link.")
				}
			}
		}
		return 0
	})
	app.ConnectStartup(func() {
		if readPreferences().Tray || len(os.Args) > 1 && os.Args[1] == "--background" {
			if err := configureTray(true); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}
	})
	app.ConnectShutdown(func() {
		if statusTray != nil {
			statusTray.Close()
			statusTray = nil
		}
	})
	os.Exit(app.Run(os.Args))
}
