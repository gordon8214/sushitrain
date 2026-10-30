// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/syncthing"
)

// The facility this package logs as
const synctrainFacility = "t-shaped.nl/sushitrain/v2/src"

// keepLogLevels restores the levels of the facilities, and those chosen in the app, after a test
func keepLogLevels(t *testing.T) {
	before := syncthing.LogLevels()
	chosenLogLevelsMut.Lock()
	chosenBefore := maps.Clone(chosenLogLevels)
	chosenLogLevelsMut.Unlock()
	t.Cleanup(func() {
		for name, level := range before {
			syncthing.SetLogLevel(name, level)
		}
		chosenLogLevelsMut.Lock()
		chosenLogLevels = chosenBefore
		chosenLogLevelsMut.Unlock()
	})
}

// uniqueToken returns text for log messages that no other test, or run of the test, logs
func uniqueToken(t *testing.T) string {
	return fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
}

func logLinesWith(t *testing.T, c *Client, after int64, message string) []logLine {
	js, err := c.LogLinesJSON(after)
	if err != nil {
		t.Fatal(err)
	}
	var lines []logLine
	if err := json.Unmarshal(js, &lines); err != nil {
		t.Fatal(err)
	}
	var found []logLine
	for _, line := range lines {
		if strings.Contains(line.Message, message) {
			found = append(found, line)
		}
	}
	return found
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
	if f := found["t-shaped.nl/sushitrain/v2/gateway"]; f.Description == "" {
		t.Errorf("the remote file service isn't a facility: %+v", f)
	}
	if f := found["model"]; f.Level != int(slog.LevelDebug) {
		t.Errorf("model facility %+v", f)
	}
	if f := found["protocol"]; f.Level != int(slog.LevelWarn) {
		t.Errorf("protocol facility %+v", f)
	}

	// After a restart with another default (INFO when the app saves its log), the levels ChangedLogLevels returned
	// come back, including the one that now equals the default, and are kept again
	restarted := &Client{defaultLogLevel: slog.LevelInfo}
	restarted.ResetLogLevels()
	if restarted.ChangedLogLevels() != "" {
		t.Fatal("the reset left levels changed")
	}
	restarted.ApplyLogLevels(changed)
	if kept := restarted.ChangedLogLevels(); kept != changed {
		t.Fatalf("kept %q, expected %q", kept, changed)
	}
	if levels := syncthing.LogLevels(); levels["model"] != slog.LevelDebug || levels[synctrainFacility] != slog.LevelInfo {
		t.Errorf("levels after applying: model %v, Synctrain %v", levels["model"], levels[synctrainFacility])
	}
}

func TestApplyLogLevelsKeepsSTTRACE(t *testing.T) {
	keepLogLevels(t)
	t.Setenv("STTRACE", "model:ERROR")
	c := &Client{defaultLogLevel: slog.LevelWarn}
	c.ResetLogLevels()

	// Set as STTRACE would at startup; not a level chosen in the app
	syncthing.SetLogLevel("model", slog.LevelError)
	if changed := c.ChangedLogLevels(); changed != "" {
		t.Fatalf("a level from STTRACE is kept: %q", changed)
	}

	// STTRACE wins over a kept level, which stays kept for a start without it
	c.ApplyLogLevels("model:DEBUG,protocol:INFO")
	levels := syncthing.LogLevels()
	if levels["model"] != slog.LevelError || levels["protocol"] != slog.LevelInfo {
		t.Errorf("model %v, protocol %v", levels["model"], levels["protocol"])
	}
	if kept := c.ChangedLogLevels(); kept != "model:DEBUG,protocol:INFO" {
		t.Errorf("kept %q", kept)
	}
}

func TestParseLogLevels(t *testing.T) {
	got := parseLogLevels(" model:ERROR, protocol,,scanner:nonsense, versioner:INFO+2")
	want := map[string]slog.Level{"model": slog.LevelError, "protocol": slog.LevelDebug, "versioner": slog.LevelInfo + 2}
	if !maps.Equal(got, want) {
		t.Errorf("parsed %v, expected %v", got, want)
	}
}

func TestLogLinesJSON(t *testing.T) {
	keepLogLevels(t)
	c := &Client{defaultLogLevel: slog.LevelWarn}
	token := uniqueToken(t)
	hidden := token + " hidden"
	shown := token + " about device ABCDEFG-HIJKLMN-OPQRSTU-VWXYZ23-4567ABC-DEFGHIJ-KLMNOPQ-RSTUVWX"
	c.SetLogLevel(synctrainFacility, int(slog.LevelInfo))
	slog.Debug(hidden)
	c.SetLogLevel(synctrainFacility, int(slog.LevelDebug))
	slog.Debug(shown)

	if found := logLinesWith(t, c, 0, hidden); len(found) != 0 {
		t.Errorf("logged below the level: %+v", found)
	}
	found := logLinesWith(t, c, 0, token)
	if len(found) != 1 || found[0].Facility != synctrainFacility || found[0].Level != int(slog.LevelDebug) {
		t.Fatalf("lines %+v", found)
	}
	if !strings.Contains(found[0].Message, "about device ABCDEFG•••") {
		t.Errorf("not redacted: %q", found[0].Message)
	}

	// Lines after a given one
	later := token + " later"
	slog.Debug(later)
	if found := logLinesWith(t, c, found[0].When, token); len(found) != 1 || !strings.Contains(found[0].Message, later) {
		t.Errorf("lines after the first %+v", found)
	}
}

func TestWriteLogLines(t *testing.T) {
	token := uniqueToken(t)
	slog.Warn(token + " from 192.168.1.20")
	slog.Info(token + " not a warning")
	format := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3} [+-]\d{4} WARN .* from •••\.•••\.•••\.20 `)

	// The support bundle's tail of all lines, and its warnings, which debug output can't push out
	for name, lines := range map[string][]syncthing.LogLine{
		"lines":    syncthing.LogLines(time.Time{}),
		"warnings": syncthing.LogWarnings(time.Time{}),
	} {
		var buf bytes.Buffer
		if err := writeLogLines(&buf, lines); err != nil {
			t.Fatal(err)
		}
		var ours []string
		for l := range strings.SplitSeq(buf.String(), "\n") {
			if strings.Contains(l, token) {
				ours = append(ours, l)
			}
		}
		if len(ours) == 0 || !format.MatchString(ours[0]) {
			t.Errorf("%s: %q", name, ours)
		}
		if name == "warnings" && len(ours) != 1 {
			t.Errorf("warnings include other levels: %q", ours)
		}
	}
}
