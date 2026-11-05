// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
	"fmt"
	"strings"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	core "t-shaped.nl/sushitrain/v2/src"
	"testing"
	"time"
)

func TestSettingsMergePreservesUneditedSecrets(t *testing.T) {
	baseline := core.ProxySettings{URL: "http://old.example:3128", User: "old", Password: "old secret"}
	edited := baseline
	edited.Bypass = "localhost"
	current := core.ProxySettings{URL: "https://new.example:3128", User: "new", Password: "new secret"}
	merged := mergeSettings(&current, &baseline, &edited)
	if merged.URL != current.URL || merged.User != current.User || merged.Password != current.Password || merged.Bypass != "localhost" {
		t.Fatal("draft replaced unedited settings", merged)
	}
	if current.Bypass != "" {
		t.Fatal("merging mutated the current settings")
	}
}

func TestRejectedConversionDoesNotChangeDraft(t *testing.T) {
	value := &struct {
		Enabled bool
		Count   int
	}{true, 7}
	c := New(nil, nil)
	c.editors["token"] = &draft{value: value, fields: []Field{{Path: "Enabled", Kind: "bool"}, {Path: "Count", Kind: "int"}}, expires: time.Now().Add(time.Minute)}
	if _, err := c.saveSettings(Request{Token: "token", Values: map[string]string{"Enabled": "false", "Count": "not a number"}}); err == nil {
		t.Fatal("invalid number accepted")
	}
	if !value.Enabled || value.Count != 7 {
		t.Fatalf("rejected draft changed: %#v", value)
	}
}
func TestExpiredDraftCannotBeSaved(t *testing.T) {
	c := New(nil, nil)
	c.editors["old"] = &draft{expires: time.Now().Add(-time.Second)}
	if _, err := c.saveSettings(Request{Token: "old"}); err == nil {
		t.Fatal("expired draft accepted")
	}
}

func TestFolderFieldVisibility(t *testing.T) {
	for _, section := range []string{"folder/test", "defaults/folder", "basic/folder/test"} {
		for _, field := range []string{"ID", "Group", "minDiskFree.value", "minDiskFree.unit", "Label", "Path", "Type", "RescanIntervalS", "FSWatcherEnabled", "MinDiskFree", "MinDiskFreeUnit", "SyncXattrs", "SendXattrs", "SyncOwnership", "SendOwnership", "CopyOwnershipFromParent", "IgnorePerms"} {
			if desktopapi.FolderFieldAdvanced(section, field) || desktopapi.FolderFieldAdvanced(section, strings.ToLower(field[:1])+field[1:]) {
				t.Errorf("%s/%s should stay visible", section, field)
			}
		}
		for _, field := range []string{"Copiers", "Hashers", "Order", "FSWatcherDelayS", "MaxConflicts", "BlockIndexing", "XattrMode", "XattrFilterEntries", "FutureTuningOption"} {
			if !desktopapi.FolderFieldAdvanced(section, field) {
				t.Errorf("%s/%s should be advanced", section, field)
			}
		}
	}
	for _, section := range []string{"engine", "connections", "device/test", "defaults/device", "versioning/test", "advanced/folder/test", "advanced/defaults/folder"} {
		if desktopapi.FolderFieldAdvanced(section, "Order") {
			t.Errorf("unrelated editor %s changed", section)
		}
	}
}

func TestSettingsSavePreservesHiddenValues(t *testing.T) {
	for _, includeAdvanced := range []bool{false, true} {
		t.Run(fmt.Sprint(includeAdvanced), func(t *testing.T) {
			value := &core.FolderSettings{MinDiskFree: "1", Copiers: "7", XattrFilterEntries: "+com.apple.*"}
			c := New(&core.Client{}, nil)
			defer c.Close()
			saved := false
			c.editors["token"] = &draft{value: value, fields: []Field{
				{Path: "MinDiskFree", Kind: "string"},
				{Path: "Copiers", Kind: "string", IsAdvanced: true},
				{Path: "XattrFilterEntries", Kind: "list", IsAdvanced: true},
			}, expires: time.Now().Add(time.Minute), save: func() error { saved = true; return nil }}
			values := map[string]string{"MinDiskFree": "5"}
			if includeAdvanced {
				values["Copiers"] = "9"
			}
			if _, err := c.saveSettings(Request{Token: "token", Values: values}); err != nil {
				t.Fatal(err)
			}
			wantCopiers := "7"
			if includeAdvanced {
				wantCopiers = "9"
			}
			if !saved || value.MinDiskFree != "5" || value.Copiers != wantCopiers || value.XattrFilterEntries != "+com.apple.*" {
				t.Fatalf("hidden values lost: %+v", value)
			}
		})
	}
}
