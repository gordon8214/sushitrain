//go:build linux && cgo

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"encoding/json"
	"fmt"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"strings"
	"t-shaped.nl/sushitrain/v2/desktopapi"
)

func (d *desktopUI) settings()                  { d.settingsCategory("general") }
func (d *desktopUI) edit(section, title string) { d.subpage(func() { d.editPage(section, title) }) }
func (d *desktopUI) editPage(section, title string) {
	if len(d.pages) > 0 {
		d.pages[len(d.pages)-1].editor = true
	}
	d.reset(title)
	d.load("settings.get", desktopapi.Request{Section: section}, func(b json.RawMessage) {
		var editor desktopapi.Editor
		if err := json.Unmarshal(b, &editor); err != nil {
			d.error(err)
			return
		}
		d.editor = editor.Token
		d.reset(title)
		if strings.HasPrefix(section, "advanced/") {
			d.text("These are expert settings. Incorrect values may damage your folder contents and stop synchronization.")
		}
		d.setDirty(false)
		if section == "gui" {
			d.load("engine.status", nil, func(b json.RawMessage) {
				var status struct{ GUIEnabled bool }
				json.Unmarshal(b, &status)
				group := d.group("Engine Access")
				r := adw.NewSwitchRow()
				r.SetTitle("Enable Web GUI and REST API")
				r.SetActive(status.GUIEnabled)
				r.NotifyProperty("active", func() {
					enabled := r.Active()
					if enabled == status.GUIEnabled {
						return
					}
					// Keep the saved state visible if the discard prompt is canceled.
					r.SetActive(status.GUIEnabled)
					d.guard(func() {
						d.run("engine.action", desktopapi.Request{Action: "gui", Enabled: enabled}, func(json.RawMessage) {
							d.setDirty(false)
							d.closeEditor()
							d.w.confirm("Restart Required", "Restart to apply the engine access change?", "Restart", false, func() { d.action("restart", "", "") })
							d.finishPage()
						})
					})
				})
				group.Add(r)
			})
		}
		if strings.HasPrefix(section, "reset/") {
			d.text(desktopapi.FolderResetDescription)
		}
		getters := map[string]func() string{}
		g := d.group(title)
		advanced := adw.NewExpanderRow()
		advanced.SetTitle("Advanced")
		hasAdvanced := false
		for _, field := range editor.Fields {
			f := field
			add := func(row gtk.Widgetter) { g.Add(row); d.revealSetting(f.Path, row) }
			if f.IsAdvanced {
				hasAdvanced = true
				add = func(row gtk.Widgetter) {
					advanced.AddRow(row)
					if d.matchesSettingsAnchor(f.Path) {
						advanced.SetExpanded(true)
					}
					d.revealSetting(f.Path, row)
				}
			}
			switch {
			case f.Kind == "bool":
				r := adw.NewSwitchRow()
				r.SetTitle(f.Label)
				r.SetActive(f.Value == "true")
				r.SetSensitive(!f.IsReadOnly)
				r.NotifyProperty("active", func() { d.setDirty(true) })
				getters[f.Path] = func() string {
					if r.Active() {
						return "true"
					}
					return "false"
				}
				add(r)
			case f.Kind == "enum":
				r := adw.NewComboRow()
				r.SetTitle(f.Label)
				r.SetModel(gtk.NewStringList(f.Options))
				selected := uint(0)
				for i, v := range f.Options {
					if v == f.Value {
						selected = uint(i)
					}
				}
				r.SetSelected(selected)
				r.SetSensitive(!f.IsReadOnly)
				r.NotifyProperty("selected", func() { d.setDirty(true) })
				getters[f.Path] = func() string {
					index := r.Selected()
					if int(index) < len(f.Options) {
						return f.Options[index]
					}
					return f.Value
				}
				add(r)
			case f.IsSecret:
				r := adw.NewPasswordEntryRow()
				r.SetTitle(f.Label + " (leave empty to keep)")
				r.SetSensitive(!f.IsReadOnly)
				r.ConnectChanged(func() { d.setDirty(true) })
				getters[f.Path] = func() string { return r.Text() }
				add(r)
			case f.Kind == "list" || f.Kind == "json":
				r := adw.NewExpanderRow()
				r.SetTitle(f.Label)
				buffer := gtk.NewTextBuffer(nil)
				buffer.SetText(f.Value)
				view := gtk.NewTextViewWithBuffer(buffer)
				view.SetMonospace(true)
				view.SetEditable(!f.IsReadOnly)
				view.SetSizeRequest(-1, 120)
				buffer.ConnectChanged(func() { d.setDirty(true) })
				r.AddRow(view)
				getters[f.Path] = func() string { return buffer.Text(buffer.StartIter(), buffer.EndIter(), true) }
				add(r)
			default:
				r := adw.NewEntryRow()
				r.SetTitle(f.Label)
				r.SetText(f.Value)
				r.SetSensitive(!f.IsReadOnly)
				r.ConnectChanged(func() { d.setDirty(true) })
				getters[f.Path] = func() string { return r.Text() }
				add(r)
			}
		}
		if hasAdvanced {
			g.Add(advanced)
		}
		d.button("Save", func() {
			values := map[string]string{}
			for key, get := range getters {
				values[key] = get()
			}
			d.runWithFailure("settings.save", desktopapi.Request{Token: editor.Token, Values: values}, func(b json.RawMessage) {
				d.editor = ""
				d.setDirty(false)
				var result struct{ RestartRequired bool }
				json.Unmarshal(b, &result)
				d.finishPage()
				if result.RestartRequired {
					d.w.confirm("Restart Required", "Restart the engine to apply the saved settings?", "Restart", false, func() { d.action("restart", "", "") })
				}
			}, func() {
				if hasAdvanced {
					advanced.SetExpanded(true)
				}
			})
		})
		g.ChildFocus(gtk.DirTabForward)
	})
}
func (d *desktopUI) policySettings() { d.subpage(func() { d.policySettingsPage() }) }
func (d *desktopUI) policySettingsPage() {
	d.reset("Automatic Pausing")
	d.load("policy.get", nil, func(b json.RawMessage) {
		var p struct {
			Metered, Cellular, PowerSaver, NetworkKnown, CellularKnown, PowerKnown bool
			Reason                                                                 string
		}
		json.Unmarshal(b, &p)
		d.reset("Automatic Pausing")
		g := d.group("Pause Synchronization")
		for _, setting := range []struct {
			title string
			get   func() bool
			set   func(bool)
		}{{"On metered networks", func() bool { return p.Metered }, func(b bool) { p.Metered = b }}, {"On cellular networks", func() bool { return p.Cellular }, func(b bool) { p.Cellular = b }}, {"In Power Saver", func() bool { return p.PowerSaver }, func(b bool) { p.PowerSaver = b }}} {
			s := setting
			r := adw.NewSwitchRow()
			r.SetTitle(s.title)
			r.SetActive(s.get())
			r.NotifyProperty("active", func() { s.set(r.Active()); d.setDirty(true) })
			g.Add(r)
		}
		if !p.NetworkKnown || p.Cellular && !p.CellularKnown {
			d.text("Network policy status is unavailable.")
		}
		if !p.PowerKnown {
			d.text("Power profile status is unavailable.")
		}
		if p.Reason != "" {
			d.text(p.Reason)
		}
		d.button("Save", func() { d.run("policy.save", p, func(json.RawMessage) { d.setDirty(false); d.finishPage() }) })
	})
}
func (d *desktopUI) logs() { d.subpage(func() { d.logsPage() }) }
func (d *desktopUI) logsPage() {
	d.reset("Activity Log")
	d.load("engine.logs", nil, func(b json.RawMessage) {
		d.reset("Activity Log")
		d.button("Refresh", func() { d.startView(); d.logsPage() })
		d.button("Logging Levels", func() {
			d.subpage(func() {
				d.load("engine.action", desktopapi.Request{Action: "logFacilities"}, func(b json.RawMessage) {
					var levels []struct {
						Name  string
						Level int
					}
					json.Unmarshal(b, &levels)
					d.reset("Logging Levels")
					for _, f := range levels {
						f := f
						r := row(f.Name, "")
						for _, level := range []struct {
							name  string
							value int
						}{{"Debug", -4}, {"Info", 0}, {"Warning", 4}, {"Error", 8}} {
							level := level
							r.AddSuffix(textButton(level.name, func() {
								d.run("engine.action", desktopapi.Request{Action: "logLevel", Name: f.Name, Offset: int64(level.value)}, nil)
							}))
						}
						d.body.Append(r)
					}
					d.button("Reset", func() { d.action("resetLogLevels", "", "") })
				})
			})
		})
		d.button("Database Maintenance", func() { d.action("maintenance", "", "") })
		var lines []struct {
			Message, Facility string
			Level             int
		}
		if json.Unmarshal(b, &lines) == nil {
			for _, line := range lines {
				d.text(strings.Join(nonEmpty(fmt.Sprint(line.Level), line.Facility, line.Message), " · "))
			}
		} else {
			d.text("The log is empty.")
		}
	})
}

func (d *desktopUI) advancedConfiguration() { d.subpage(func() { d.advancedConfigurationPage() }) }
func (d *desktopUI) advancedConfigurationPage() {
	d.reset("Advanced Configuration")
	d.text("Expert settings. Changes can affect connectivity, synchronization and security.")
	g := d.group("Configuration Sections")
	for _, s := range []string{"options", "gui", "ldap", "defaults/folder", "defaults/device", "defaults/ignores"} {
		s := s
		g.Add(choiceRow(s, "", func() { d.edit("advanced/"+s, "Advanced Configuration") }))
	}
	d.load("engine.folders", nil, func(b json.RawMessage) {
		var list []desktopapi.Folder
		json.Unmarshal(b, &list)
		for _, f := range list {
			f := f
			g.Add(choiceRow("Folder: "+f.Label, "", func() { d.edit("advanced/folder/"+f.ID, "Advanced Folder Configuration") }))
		}
	})
	d.load("engine.devices", nil, func(b json.RawMessage) {
		var list []desktopapi.Device
		json.Unmarshal(b, &list)
		for _, v := range list {
			v := v
			g.Add(choiceRow("Device: "+v.Name, "", func() { d.edit("advanced/device/"+v.ID, "Advanced Device Configuration") }))
		}
	})
}

func (d *desktopUI) resourceSettings() { d.subpage(func() { d.resourceSettingsPage() }) }
func (d *desktopUI) resourceSettingsPage() {
	d.reset("Background Resource Use")
	d.text("These controls affect the entire synchronization service, including transfers.")
	for _, p := range []struct{ title, value string }{{"Normal", "normal"}, {"Utility", "utility"}, {"Background", "background"}} {
		p := p
		d.body.Append(textButton(p.title, func() {
			d.run("resources.set", map[string]string{"Priority": p.value}, func(json.RawMessage) { d.w.toast("Updated service resource priority.") })
		}))
	}
}
