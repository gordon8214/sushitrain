//go:build linux && cgo

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"os"
	"path/filepath"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"t-shaped.nl/sushitrain/v2/desktoptray"
	"t-shaped.nl/sushitrain/v2/linuxdesktop"
)

var activeApplication *adw.Application
var activeWindow *mainWindow
var presentWindow func() *mainWindow
var statusTray *desktoptray.Tray

func configureTray(enabled bool) error {
	if enabled && statusTray != nil {
		return nil
	}
	if statusTray != nil {
		statusTray.Close()
		statusTray = nil
		activeApplication.Release()
	}
	if !enabled {
		return nil
	}
	ui := func(fn func()) { coreglib.IdleAdd(fn) }
	withWindow := func(route string, next func(*mainWindow)) {
		if presentWindow == nil {
			return
		}
		w := presentWindow()
		if w == nil || w.desk == nil || w.changing {
			return
		}
		w.desk.guard(func() {
			w.desk.navigate(route)
			if next != nil {
				next(w)
			}
		})
	}
	actions := []desktoptray.Action{
		{Label: "Show Synctrain", Run: func() { ui(func() { withWindow("Overview", nil) }) }},
		{Label: "Pause All Devices", Run: func() {
			ui(func() {
				withWindow("Overview", func(w *mainWindow) {
					if w.desk.mode == modeSynchronize {
						w.desk.action("pauseAll", "", "")
					}
				})
			})
		}},
		{Label: "Resume All Devices", Run: func() {
			ui(func() {
				withWindow("Overview", func(w *mainWindow) {
					if w.desk.mode == modeSynchronize {
						w.desk.action("resumeAll", "", "")
					}
				})
			})
		}},
		{Label: "Settings", Run: func() { ui(func() { withWindow("Settings", nil) }) }},
		{Label: "Quit and Stop Synctrain…", Run: func() {
			ui(func() {
				withWindow("Overview", func(w *mainWindow) {
					w.confirm("Quit and Stop Synctrain?", "Both synchronization and on-demand file access stop. Pending changes stay saved here.", "Quit and Stop", false, func() {
						d := w.desk
						if d == nil || d.saving || d.closed {
							return
						}
						d.saving = true
						d.detail.SetSensitive(false)
						background(func() (struct{}, error) {
							return struct{}{}, systemctl("stop", "synctrain-gateway.service", "synctrain-files.service")
						}, func(_ struct{}, err error) {
							d.saving = false
							d.detail.SetSensitive(true)
							if d.closed {
								return
							}
							if err != nil {
								w.failed(err)
								return
							}
							activeApplication.Quit()
						})
					})
				})
			})
		}},
	}

	tray, err := desktoptray.New(actions)
	if err != nil {
		return err
	}
	statusTray = tray
	activeApplication.Hold()
	return nil
}
func (d *desktopUI) desktopSettings() {
	g := d.group("Desktop Integration")
	tray := adw.NewSwitchRow()
	tray.SetTitle("Show a tray icon when supported")
	tray.SetSubtitle("GNOME needs an AppIndicator extension to display the icon.")
	tray.SetActive(d.preferences.Tray)
	tray.NotifyProperty("active", func() {
		next := d.preferences
		next.Tray = tray.Active()
		if next.Tray == d.preferences.Tray {
			return
		}
		if err := configureTray(next.Tray); err != nil {
			tray.SetActive(d.preferences.Tray)
			d.error(err)
			return
		}
		if err := writePreferences(&next); err != nil {
			_ = configureTray(d.preferences.Tray)
			tray.SetActive(d.preferences.Tray)
			d.error(err)
			return
		}
		d.preferences = next
	})
	g.Add(tray)
	startup := adw.NewSwitchRow()
	startup.SetTitle("Open the tray at login")
	file := filepath.Join(configDir(), "autostart", "synctrain.desktop")
	_, err := os.Stat(file)
	confirmedStartup := err == nil
	startup.SetActive(confirmedStartup)
	startup.NotifyProperty("active", func() {
		if startup.Active() == confirmedStartup {
			return
		}
		var err error
		if startup.Active() && !tray.Active() {
			tray.SetActive(true)
			if !d.preferences.Tray {
				startup.SetActive(false)
				return
			}
		}
		err = linuxdesktop.Autostart(startup.Active())
		if err != nil {
			startup.SetActive(confirmedStartup)
			d.error(err)
		} else {
			confirmedStartup = startup.Active()
		}
	})
	g.Add(startup)
	// Keep controls reachable even on a desktop without a tray host.
	open := gtk.NewLabel("Closing this window leaves the background service running. Use Background Service controls or the tray’s Quit and Stop command to stop it.")
	open.SetWrap(true)
	d.body.Append(open)
}
