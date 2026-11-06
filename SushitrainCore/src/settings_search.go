// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"encoding/json"
	"strings"

	"github.com/syncthing/syncthing/lib/config"
	"t-shaped.nl/sushitrain/v2/desktopui"
)

// SettingsSearchIndex returns a metadata-only snapshot for native settings search.
// Reading this snapshot never opens an editor or allocates a save token.
func (clt *Client) SettingsSearchIndex() ([]byte, error) {
	cfg := clt.cfg()
	if cfg == nil {
		return nil, ErrStillLoading
	}
	conf := cfg.RawCopy()
	return json.Marshal(settingsSearchEntries(&conf))
}

func settingsSearchEntries(conf *config.Configuration) []desktopui.SearchEntry {
	type section struct{ id, title string }
	sections := []section{{"options", "Options"}, {"gui", "Web GUI"}, {"ldap", "LDAP"},
		{"defaults/folder", "Default Folder"}, {"defaults/device", "Default Device"}, {"defaults/ignores", "Default Ignore Patterns"}}
	for _, folder := range conf.Folders {
		name := folder.Label
		if name == "" {
			name = folder.ID
		}
		sections = append(sections, section{"folder/" + folder.ID, "Folders › " + name})
	}
	for _, device := range conf.Devices {
		name := device.Name
		if name == "" {
			name = device.DeviceID.String()
		}
		sections = append(sections, section{"device/" + device.DeviceID.String(), "Devices › " + name})
	}
	entries := []desktopui.SearchEntry{}
	mappings := desktopui.Read().Search
	for _, s := range sections {
		advanced, err := newAdvancedSection(conf, s.id, false)
		if err != nil {
			continue
		}
		for _, field := range advanced.fields {
			e := desktopui.SearchEntry{ID: "engine." + s.id + "." + field.Path, Title: field.Label,
				Breadcrumb: "Advanced › " + s.title, Category: "advanced", Action: "advanced/" + s.id,
				Section: s.id, Field: field.Path, Anchor: field.Path, Capability: "synchronize", Keywords: []string{field.Group, field.Path}}
			for _, mapping := range mappings {
				template := mapping.Section == "folder/*" && strings.HasPrefix(s.id, "folder/") || mapping.Section == "device/*" && strings.HasPrefix(s.id, "device/")
				if (mapping.Section == s.id || template) && mapping.Field == field.Path {
					e.Title, e.Category, e.Action, e.Anchor = mapping.Title, mapping.Category, mapping.Action, mapping.Anchor
					e.Breadcrumb = mapping.Breadcrumb + " › " + mapping.Title
					if template {
						e.Breadcrumb = s.title + " › " + mapping.Title
						e.Action = strings.ReplaceAll(e.Action, "$id", strings.TrimPrefix(strings.TrimPrefix(s.id, "folder/"), "device/"))
					}
					e.Keywords = append(e.Keywords, mapping.Keywords...)
					break
				}
			}
			if strings.HasPrefix(s.id, "defaults/") {
				e.Category = "defaults"
			}
			entries = append(entries, e)
		}
	}
	return entries
}
