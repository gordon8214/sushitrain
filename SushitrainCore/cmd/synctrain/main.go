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

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
)

// Set with -ldflags "-X main.version=…"
var version = ""

const appID = "com.gordonbyrnes.Synctrain"

func main() {
	if len(os.Args) == 3 && os.Args[1] == "versions" {
		os.Exit(runVersions(os.Args[2]))
	}
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "Usage: synctrain [versions <path>]")
		os.Exit(2)
	}
	app := adw.NewApplication(appID, gio.ApplicationFlagsNone)
	var w *mainWindow
	app.ConnectActivate(func() {
		if w == nil {
			w = newMainWindow(app)
		}
		w.win.Present()
	})
	os.Exit(app.Run(os.Args))
}
