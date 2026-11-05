//go:build linux && cgo

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"encoding/json"
	"fmt"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"os"
	"path/filepath"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	"t-shaped.nl/sushitrain/v2/files"
)

// Nautilus opens the same independent local and paired-source history browser.
func runVersions(path string) int {
	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	app := adw.NewApplication(appID+".Versions", gio.ApplicationNonUnique)
	activeApplication = app
	app.ConnectShutdown(func() {
		if statusTray != nil {
			statusTray.Close()
			statusTray = nil
		}
	})
	installWindowActivation(app, func(w *mainWindow) {
		if w.desk == nil {
			w.inform("Set Up Synctrain", "Choose a mode before opening file history.")
			return
		}
		d := w.desk
		d.startView()
		d.reset("Previous Versions")
		if d.mode == modeFiles {
			d.load("files.resolvePath", map[string]string{"Path": abs}, func(b json.RawMessage) {
				var e files.Entry
				if err := json.Unmarshal(b, &e); err != nil {
					d.error(err)
					return
				}
				d.openFilesAt("", "", 0)
				d.filesVersions(browserRow{name: e.Name, item: e.Item, node: e.Node, size: e.Size, writable: e.Writable})
			})
		} else {
			d.load("engine.resolvePath", desktopapi.Request{Path: abs}, func(b json.RawMessage) {
				var target struct{ Folder, Path string }
				if err := json.Unmarshal(b, &target); err != nil {
					d.error(err)
					return
				}
				d.folder = target.Folder
				d.directory = filepath.ToSlash(filepath.Dir(target.Path))
				if d.directory == "." {
					d.directory = ""
				}
				d.openFilesAt(d.folder, d.directory, 0)
				d.localVersions(target.Path, filepath.Base(abs))
			})
		}
	})
	return app.Run(os.Args[:1])
}
