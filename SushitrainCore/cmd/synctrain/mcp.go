// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build linux && cgo

package main

import (
	"encoding/json"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"t-shaped.nl/sushitrain/v2/mcpcontrol"
)

func (d *desktopUI) mcpSettings() { d.subpage(d.mcpSettingsPage) }
func (d *desktopUI) mcpSettingsPage() {
	d.reset("MCP")
	d.text("Connected assistants can inspect settings, manage synchronization, and change or delete accessible files. Changes can propagate to other devices.")
	group := d.group("Assistant Access")
	row := adw.NewSwitchRow()
	row.SetTitle("Enable MCP")
	row.SetSubtitle("Local stdio · Full management access")
	row.SetSensitive(false)
	group.Add(row)
	d.load("mcp.settings", nil, func(data json.RawMessage) {
		var settings mcpcontrol.Settings
		if err := json.Unmarshal(data, &settings); err != nil {
			d.error(err)
			return
		}
		row.SetActive(settings.Enabled)
		row.SetSensitive(true)
		row.NotifyProperty("active", func() {
			desired := row.Active()
			if desired == settings.Enabled {
				return
			}
			// Show the confirmed state until the host persists the change.
			// In particular, a failed disable must never look disabled.
			row.SetActive(settings.Enabled)
			generation := d.generation
			d.run("mcp.settings", mcpcontrol.Settings{Enabled: desired}, func(data json.RawMessage) {
				if generation != d.generation {
					return
				}
				if err := json.Unmarshal(data, &settings); err != nil {
					d.error(err)
					return
				}
				row.SetActive(settings.Enabled)
			})
		})
	})
	d.text("Start Synctrain's configured background service before connecting. Closing an MCP client leaves the service running.")
	configuration := gtk.NewLabel("{\"mcpServers\":{\"synctrain\":{\"command\":\"/usr/bin/synctrain-mcp\"}}}")
	configuration.SetSelectable(true)
	configuration.SetWrap(true)
	configuration.SetXAlign(0)
	d.body.Append(configuration)
}
