// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"t-shaped.nl/sushitrain/v2/desktopui"
)

func TestSettingsSearchMetadata(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	before := wrapper.RawCopy()
	data, err := client.SettingsSearchIndex()
	if err != nil {
		t.Fatal(err)
	}
	var entries []desktopui.SearchEntry
	if err = json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[entry.ID] || entry.Title == "" || entry.Field == "" || entry.Section == "" || entry.Action == "" {
			t.Fatalf("invalid entry: %+v", entry)
		}
		seen[entry.ID] = true
	}
	for _, id := range []string{"engine.options.globalAnnounceEnabled", "engine.folder/" + advancedTestFolder + ".rescanIntervalS", "engine.gui.password"} {
		if !seen[id] {
			t.Errorf("missing %s", id)
		}
	}
	if bytes.Contains(data, []byte(`"value"`)) || bytes.Contains(data, []byte(`"token"`)) {
		t.Fatal("index includes editor data")
	}
	if !reflect.DeepEqual(before, wrapper.RawCopy()) {
		t.Fatal("index changed configuration")
	}
	if matches := desktopui.Search(entries, "Folder one rescan"); len(matches) != 1 {
		t.Fatalf("owner lookup: %+v", matches)
	}
}

func TestSettingsSearchMappingsTargetExistingFields(t *testing.T) {
	conf := advancedTestConfiguration(t)
	for _, mapping := range desktopui.Read().Search {
		if mapping.Section == "" {
			continue
		}
		section := strings.ReplaceAll(mapping.Section, "folder/*", "folder/"+advancedTestFolder)
		if mapping.Section == "device/*" {
			section = "defaults/device"
		}
		advanced, err := newAdvancedSection(&conf, section, false)
		if err != nil {
			t.Error(mapping.ID, err)
			continue
		}
		found := false
		for _, field := range advanced.fields {
			found = found || field.Path == mapping.Field
		}
		if !found {
			t.Errorf("%s targets missing field %s/%s", mapping.ID, section, mapping.Field)
		}
	}
}
