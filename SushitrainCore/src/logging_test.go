// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/syncthing/syncthing/lib/syncthing"
)

// The facility this package logs as
const synctrainFacility = "t-shaped.nl/sushitrain/v2/src"

func keepLogLevels(t *testing.T) {
	before := syncthing.LogLevels()
	t.Cleanup(func() {
		for name, level := range before {
			syncthing.SetLogLevel(name, level)
		}
	})
}

func TestLogLevels(t *testing.T) {
	keepLogLevels(t)
	c := &Client{defaultLogLevel: slog.LevelWarn}
	c.ResetLogLevels()
	if changed := c.ChangedLogLevels(); changed != "" {
		t.Fatalf("after a reset, changed %q", changed)
	}

	c.SetLogLevel(synctrainFacility, int(slog.LevelInfo))
	c.SetLogLevel("model", int(slog.LevelDebug))
	changed := c.ChangedLogLevels()
	if changed != "model:DEBUG,"+synctrainFacility+":INFO" {
		t.Fatalf("changed %q", changed)
	}

	js, err := c.LogFacilitiesJSON()
	if err != nil {
		t.Fatal(err)
	}
	var facilities []logFacility
	if err := json.Unmarshal(js, &facilities); err != nil {
		t.Fatal(err)
	}
	found := map[string]logFacility{}
	for _, f := range facilities {
		found[f.Name] = f
	}
	if f := found[synctrainFacility]; f.Description != "Synctrain" || f.Level != int(slog.LevelInfo) {
		t.Errorf("Synctrain facility %+v", f)
	}
	if f := found["model"]; f.Level != int(slog.LevelDebug) {
		t.Errorf("model facility %+v", f)
	}
	if f := found["protocol"]; f.Level != int(slog.LevelWarn) {
		t.Errorf("protocol facility %+v", f)
	}

	// The levels ChangedLogLevels returned restore what a reset undid
	c.ResetLogLevels()
	if c.ChangedLogLevels() != "" {
		t.Fatal("the reset left levels changed")
	}
	c.ApplyLogLevels(changed)
	if restored := c.ChangedLogLevels(); restored != changed {
		t.Fatalf("restored %q, expected %q", restored, changed)
	}
}

func TestLogLinesJSON(t *testing.T) {
	keepLogLevels(t)
	c := &Client{defaultLogLevel: slog.LevelWarn}
	c.SetLogLevel(synctrainFacility, int(slog.LevelInfo))
	slog.Debug("A hidden line")
	c.SetLogLevel(synctrainFacility, int(slog.LevelDebug))
	slog.Debug("A line about device ABCDEFG-HIJKLMN-OPQRSTU-VWXYZ23-4567ABC-DEFGHIJ-KLMNOPQ-RSTUVWX")

	js, err := c.LogLinesJSON()
	if err != nil {
		t.Fatal(err)
	}
	var lines []logLine
	if err := json.Unmarshal(js, &lines); err != nil {
		t.Fatal(err)
	}
	var ours []logLine
	for _, line := range lines {
		if line.Facility == synctrainFacility && strings.HasPrefix(line.Message, "A ") {
			ours = append(ours, line)
		}
	}
	if len(ours) != 1 || ours[0].Level != int(slog.LevelDebug) || ours[0].When == 0 {
		t.Fatalf("lines %+v", ours)
	}
	if !strings.HasPrefix(ours[0].Message, "A line about device ABCDEFG•••") {
		t.Errorf("not redacted: %q", ours[0].Message)
	}
}
