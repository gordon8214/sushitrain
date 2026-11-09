// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
//go:build linux && cgo

package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	"t-shaped.nl/sushitrain/v2/desktopui"
)

func canonicalRoute(route string) string {
	c := desktopui.Read()
	id := strings.TrimPrefix(route, "settings:")
	if alias, ok := c.Aliases[id]; ok {
		id = alias
	}
	for _, d := range c.Destinations {
		if d.ID == id || d.Title == route {
			return (desktopui.NavigationItem{ID: d.ID, Title: d.Title, Route: d.ID}).NativeRoute()
		}
	}
	for _, category := range c.Settings {
		if category.ID == id {
			return "settings:" + id
		}
	}
	return route
}

func (d *desktopUI) rebuildSidebar() {
	if d.shortcutCancel != nil {
		d.shortcutCancel()
	}
	d.shortcutContext, d.shortcutCancel = context.WithCancel(context.Background())
	if d.folderShortcuts != nil {
		d.shortcutsExpanded = d.folderShortcuts.Expanded()
	}
	d.shortcutRows = nil
	d.updatingSidebar = true
	defer func() { d.updatingSidebar = false }()
	for child := d.sidebar.FirstChild(); child != nil; child = d.sidebar.FirstChild() {
		d.sidebar.Remove(child)
	}
	d.sidebarRoutes = nil
	d.folderShortcuts = nil
	d.sidebarBack.SetVisible(false)
	d.sidebarSettings.SetVisible(false)
	if d.settingsSearchButton != nil {
		d.settingsSearchButton.SetVisible(true)
		d.settingsSearchBar.SetVisible(true)
	}
	d.showSettingsSearch()
	for _, group := range desktopui.Read().Navigation("gnome", d.mode) {
		header := gtk.NewListBoxRow()
		header.SetSelectable(false)
		header.SetActivatable(false)
		label := gtk.NewLabel(group.Title)
		label.SetXAlign(0)
		label.AddCSSClass("heading")
		label.SetMarginTop(16)
		label.SetMarginStart(12)
		header.SetChild(label)
		d.sidebar.Append(header)
		d.sidebarRoutes = append(d.sidebarRoutes, "")
		for _, item := range group.Items {
			if item.ID == "files" {
				r := adw.NewExpanderRow()
				r.SetExpanded(d.shortcutsExpanded)
				r.SetTitle(item.Title)
				r.AddSuffix(textButton("Browse", func() { d.guard(func() { d.navigate("Files") }) }))
				d.sidebar.Append(r)
				d.folderShortcuts = r
			} else {
				r := gtk.NewListBoxRow()
				l := gtk.NewLabel(item.Title)
				l.SetXAlign(0)
				l.SetMarginTop(12)
				l.SetMarginBottom(12)
				l.SetMarginStart(12)
				r.SetChild(l)
				d.sidebar.Append(r)
			}
			d.sidebarRoutes = append(d.sidebarRoutes, item.NativeRoute())
		}
	}
	for i, route := range d.sidebarRoutes {
		if route == d.route {
			d.sidebar.SelectRow(d.sidebar.RowAtIndex(i))
			break
		}
	}
}

func (d *desktopUI) settingsCategory(id string) {
	for _, c := range desktopui.Read().Settings {
		if c.ID != id {
			continue
		}
		d.reset(c.Title)
		d.loadSettingsSearch()
		for _, section := range c.Sections {
			if !section.Available("gnome", d.mode) {
				continue
			}
			g := d.group(section.Title)
			s := section
			title, subtitle := s.Title, ""
			if s.Action == "mode" {
				title, subtitle = "Switch to Files…", "Current mode: Synchronize"
				if d.mode == modeFiles {
					title, subtitle = "Switch to Synchronize…", "Current mode: Files"
				}
			}
			r := choiceRow(title, subtitle, func() { d.openCatalogSetting(s.Action, s.Title) })
			g.Add(r)
			d.revealSetting(s.Action, r)
		}
		return
	}
}

func (d *desktopUI) openCatalogSetting(action, title string) {
	switch action {
	case "name":
		d.input("Device Name", "Name shown to other devices", "", func(name string) { d.run("engine.action", desktopapi.Request{Action: "name", Name: name}, nil) })
	case "mode":
		d.w.switchMode.Activate(nil)
	case "startup", "presence":
		d.subpage(func() {
			d.reset(title)
			d.desktopSettings()
			if action == "presence" {
				d.serviceButtons()
			}
		})
	case "previews":
		d.previewSettings()
	case "storage":
		d.subpage(func() { d.reset(title); d.legacyFiles() })
	case "locations":
		d.navigate("Folders")
	case "fileManager":
		d.subpage(func() {
			d.reset(title)
			d.text("Nautilus and Dolphin provide sync status, offline availability, and Previous Versions for Synctrain folders.")
			d.button("Open Synctrain Folders", func() { d.navigate("Folders") })
		})
	case "menuAccess":
		d.subpage(func() { d.reset(title); d.desktopSettings() })
	case "mcp":
		d.mcpSettings()
	case "policy":
		d.policySettings()
	case "resources":
		d.resourceSettings()
	case "configuration":
		d.exportConfiguration()
	case "advanced":
		d.advancedConfiguration()
	case "logs":
		d.logs()
	case "versions", "support", "licenses":
		d.w.about()
	default:
		d.edit(action, title)
	}
}

func (d *desktopUI) historyPage() {
	g := d.group("Previous Versions")
	g.Add(choiceRow("Choose a File", "Browse a folder, then choose Previous Versions for a file.", func() { d.navigate("Files") }))
	if d.mode == modeSynchronize {
		g.Add(choiceRow("Deleted Files", "Choose a synchronized folder to view its archive.", func() { d.navigate("Folders") }))
	}
	d.historySources()
}

func (d *desktopUI) activityPage() {
	if d.mode == modeFiles {
		d.legacyFiles()
		return
	}
	g := d.group("Transfers")
	d.transferStatus(g)
	g.Add(choiceRow("Activity Log", "Recent events, errors, and diagnostics", d.logs))
	d.text("Pending file changes and recovery are available in Files mode.")
}

// Shortcut enumeration reads only the index; opening the sidebar never opens file contents.
func (d *desktopUI) populateFolderShortcuts() {
	if d.folderShortcuts == nil {
		return
	}
	target := d.folderShortcuts
	d.shortcutGeneration++
	generation := d.shortcutGeneration
	method := "engine.folders"
	if d.mode == modeFiles {
		method = "files.children"
	}
	ctx := d.shortcutContext
	if ctx == nil {
		ctx = context.Background()
	}
	background(func() (json.RawMessage, error) {
		var data json.RawMessage
		err := d.request(ctx, method, desktopapi.Request{}, &data)
		return data, err
	}, func(b json.RawMessage, err error) {
		if d.closed || ctx.Err() != nil || target != d.folderShortcuts || generation != d.shortcutGeneration {
			return
		}
		if err != nil {
			d.error(err)
			return
		}
		type shortcut struct {
			ID, Label, Name string
			Item            int64
		}
		var rows []shortcut
		if err := json.Unmarshal(b, &rows); err != nil {
			d.error(err)
			return
		}
		for _, old := range d.shortcutRows {
			target.Remove(old)
		}
		d.shortcutRows = nil
		sort.SliceStable(rows, func(i, j int) bool {
			first, second := rows[i].Label+rows[i].Name, rows[j].Label+rows[j].Name
			if first == "" {
				first = rows[i].ID
			}
			if second == "" {
				second = rows[j].ID
			}
			x, y := strings.ToLower(first), strings.ToLower(second)
			if x == y {
				if rows[i].ID == rows[j].ID {
					return rows[i].Item < rows[j].Item
				}
				return rows[i].ID < rows[j].ID
			}
			return x < y
		})
		for _, value := range rows {
			value := value
			name := value.Label
			if name == "" {
				name = value.Name
			}
			if name == "" {
				name = value.ID
			}
			shortcut := choiceRow(name, "", func() {
				d.guard(func() {
					d.openFilesAt(value.ID, "", value.Item)
				})
			})
			target.AddRow(shortcut)
			d.shortcutRows = append(d.shortcutRows, shortcut)
		}
	})
}
