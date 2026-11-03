// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux && cgo

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"t-shaped.nl/sushitrain/v2/files"
)

// runVersions shows the previous versions of a file in the mount, which the gateway keeps, to preview, restore or
// copy
func runVersions(path string) int {
	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// A window of its own for each file, beside the main window
	app := adw.NewApplication(appID+".Versions", gio.ApplicationNonUnique)
	app.ConnectActivate(func() {
		w := &mainWindow{app: app, win: adw.NewApplicationWindow(&app.Application), toasts: adw.NewToastOverlay()}
		w.win.SetTitle("Previous Versions of " + filepath.Base(abs))
		w.win.SetDefaultSize(560, 520)
		toolbar := adw.NewToolbarView()
		toolbar.AddTopBar(adw.NewHeaderBar())
		group := newListGroup(filepath.Base(abs), "The versions the folder's versioning keeps on the gateway, "+
			"newest first. Restoring replaces the file there; its current content is kept as a version.")
		prefs := adw.NewPreferencesPage()
		prefs.Add(group.group)
		toolbar.SetContent(prefs)
		w.toasts.SetChild(toolbar)
		w.win.SetContent(w.toasts)
		w.win.Present()
		var load func()
		load = func() {
			background(func() (files.Versions, error) {
				var list files.Versions
				err := callFiles("versions.list", map[string]string{"path": abs}, &list)
				return list, err
			}, func(list files.Versions, err error) {
				group.set(key(list)+fmt.Sprint(err), func() []gtk.Widgetter {
					if err != nil {
						return []gtk.Widgetter{row("The versions can't be listed", err.Error())}
					}
					if len(list.Versions) == 0 {
						reason := "The gateway keeps no previous versions of this file."
						switch list.Versioning {
						case "none", "":
							reason = "The folder keeps no previous versions: turn on versioning for it on the gateway."
						case "external", "customPath":
							reason = "The folder's versioning keeps versions where the gateway can't list them."
						}
						return []gtk.Widgetter{row("No previous versions", reason)}
					}
					// To the minute, unless two versions would look the same
					layout := "Monday 2 January 2006, 15:04"
					seen := map[string]bool{}
					for _, v := range list.Versions {
						label := v.ModTime.Local().Format(layout)
						if seen[label] {
							layout = "Monday 2 January 2006, 15:04:05"
						}
						seen[label] = true
					}
					var rows []gtk.Widgetter
					for _, v := range list.Versions {
						version := v
						r := row(v.ModTime.Local().Format(layout),
							humanBytes(v.Size)+" · archived "+v.Archived.Local().Format("2 Jan 2006 15:04"))
						open := iconButton("document-open-symbolic", "Open", func() { openVersion(w, abs, version) })
						r.AddSuffix(open)
						save := textButton("Save a Copy", func() { copyVersion(w, abs, version, load) })
						restore := textButton("Restore", func() { restoreVersion(w, abs, version, load) })
						restore.AddCSSClass("destructive-action")
						for _, b := range []*gtk.Button{save, restore} {
							if !list.Writable {
								b.SetSensitive(false)
								b.SetTooltipText("The folder can't be changed from this machine")
							}
							r.AddSuffix(b)
						}
						rows = append(rows, r)
					}
					return rows
				})
			})
		}
		load()
	})
	return app.Run(os.Args[:1])
}

func openVersion(w *mainWindow, path string, v files.Version) {
	background(func() (string, error) {
		var result struct{ File string }
		err := callFiles("versions.preview", map[string]string{"path": path, "version": v.ID}, &result)
		return result.File, err
	}, func(file string, err error) {
		if err != nil {
			w.toast("The version can't be opened: " + err.Error())
			return
		}
		w.openPath(file)
	})
}

func restoreVersion(w *mainWindow, path string, v files.Version, reload func()) {
	w.confirm("Restore This Version?", filepath.Base(path)+" is replaced with the version of "+
		v.ModTime.Local().Format("2 January 2006, 15:04")+" on the gateway. Its current content is kept as a version.",
		"Restore", true, func() {
			background(func() (struct{}, error) {
				return struct{}{}, callFiles("versions.restore", map[string]string{"path": path, "version": v.ID}, nil)
			}, func(_ struct{}, err error) {
				if err != nil {
					w.toast("Not restored: " + err.Error())
					return
				}
				w.toast("Restored")
				reload()
			})
		})
}

func copyVersion(w *mainWindow, path string, v files.Version, reload func()) {
	background(func() (string, error) {
		var result struct{ Name string }
		err := callFiles("versions.copy", map[string]string{"path": path, "version": v.ID}, &result)
		return result.Name, err
	}, func(name string, err error) {
		if err != nil {
			w.toast("Not copied: " + err.Error())
			return
		}
		w.toast("Saved as “" + name + "”")
		reload()
	})
}
