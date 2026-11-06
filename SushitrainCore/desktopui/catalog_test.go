// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
package desktopui

import (
	"reflect"
	"testing"
)

func TestNavigationContract(t *testing.T) {
	c := Read()
	if c.Version != 1 {
		t.Fatal("unsupported catalog version")
	}
	want := []string{"Overview", "Folders", "Devices", "Browse Files", "Remote Access", "History", "Activity"}
	if !reflect.DeepEqual(MainTitles(), want) {
		t.Fatal(MainTitles())
	}
	want = []string{"General", "Files & Storage", "Network", "Synchronization", "Integrations", "Defaults", "Advanced", "About"}
	var got []string
	ids := map[string]bool{}
	for _, item := range c.Settings {
		got = append(got, item.Title)
		if ids[item.ID] {
			t.Fatalf("duplicate category %s", item.ID)
		}
		ids[item.ID] = true
		sections := map[string]bool{}
		for _, s := range item.Sections {
			if s.ID == "" || s.Title == "" || s.LabelKey == "" || s.Action == "" || sections[s.ID] {
				t.Fatalf("invalid section %+v", s)
			}
			sections[s.ID] = true
			if s.Capability != "always" && s.Capability != "files" && s.Capability != "synchronize" {
				t.Fatal(s.Capability)
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	for alias, id := range c.Aliases {
		found := ids[id]
		for _, d := range c.Destinations {
			found = found || d.ID == id
		}
		if !found {
			t.Fatalf("alias %s targets missing destination %s", alias, id)
		}
	}
}

func TestAvailabilityAndIsolation(t *testing.T) {
	if !Available("always", "files") || Available("synchronize", "files") || !Available("synchronize", "synchronize") {
		t.Fatal("mode availability")
	}
	a := Read()
	a.Settings[0].Title = "changed"
	if Read().Settings[0].Title != "General" {
		t.Fatal("catalog shared mutable state")
	}
}

// Catch omissions and reorderings that can silently strand a moved setting.
func TestSectionAndDetailContract(t *testing.T) {
	c := Read()
	want := [][]string{
		{"identity", "mobileIdentity", "mode", "startup", "presence"},
		{"browsing", "previews", "storage", "locations", "mobileDisplay"},
		{"connectivity", "connections", "proxy", "bandwidth"},
		{"pausing", "resources", "engine", "reporting"},
		{"fileManager", "menuAccess", "mcp"},
		{"newFolders", "resetDefaults", "versioning", "newDevices", "ignores"},
		{"engineAccess", "configuration", "expert", "diagnostics"},
		{"versions", "support", "licenses"},
	}
	for i, category := range c.Settings {
		var ids []string
		for _, section := range category.Sections {
			ids = append(ids, section.ID)
		}
		if !reflect.DeepEqual(ids, want[i]) {
			t.Errorf("%s: %v", category.ID, ids)
		}
	}
	for kind, details := range c.DetailDescriptors {
		var titles []string
		seen := map[string]bool{}
		for _, detail := range details {
			if detail.ID == "" || detail.LabelKey == "" || seen[detail.ID] || detail.Capability == "" {
				t.Fatalf("invalid %s descriptor: %+v", kind, detail)
			}
			seen[detail.ID] = true
			titles = append(titles, detail.Title)
		}
		if !reflect.DeepEqual(titles, c.Details[kind]) {
			t.Errorf("%s labels differ", kind)
		}
	}
}

func TestUnifiedNavigationVisibility(t *testing.T) {
	c := Read()
	for _, platform := range []string{"macOS", "windows", "gnome", "kde", "ios"} {
		for _, mode := range []string{"files", "synchronize"} {
			if platform == "ios" && mode != "files" {
				continue
			}
			groups := c.Navigation(platform, mode)
			if len(groups) != 3 || groups[0].ID != "manage" || groups[1].ID != "preferences" || groups[2].ID != "tools" {
				t.Fatal(groups)
			}
			seen := map[string]bool{}
			for _, group := range groups {
				for _, item := range group.Items {
					if seen[item.ID] || item.ID == "settings" {
						t.Fatal(item)
					}
					seen[item.ID] = true
				}
			}
			if !seen["overview"] || !seen["folders"] || !seen["files"] || !seen["general"] || !seen["about"] {
				t.Fatal(seen)
			}
			if mode == "files" && (seen["devices"] || seen["network"] || seen["synchronization"] || seen["defaults"] || seen["advanced"]) {
				t.Fatal(seen)
			}
			if platform == "ios" && mode == "files" && (!seen["integrations"] || !seen["filesAndStorage"]) {
				t.Fatal(seen)
			}
		}
	}
	if c.Aliases["Settings"] != "general" {
		t.Fatal("legacy Settings route")
	}
}
