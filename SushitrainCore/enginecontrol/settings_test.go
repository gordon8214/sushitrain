// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
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
