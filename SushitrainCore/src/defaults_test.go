// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
)

func TestDefaultFolderSettingsChangeNothingUnlessEdited(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	before := wrapper.DefaultFolder()
	if err := client.SetDefaultFolderSettings(client.DefaultFolderSettings()); err != nil {
		t.Fatal(err)
	}
	if after := wrapper.DefaultFolder(); !reflect.DeepEqual(before, after) {
		t.Fatalf("changed:\n%+v\n%+v", before, after)
	}
}

func TestDefaultFolderSettingsAreSaved(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	settings := client.DefaultFolderSettings()
	settings.Type = "sendonly"
	settings.Path = "~/Elsewhere"
	settings.RescanIntervalS = "60"
	settings.FSWatcherEnabled = false
	settings.FSWatcherDelayS = "2.5"
	settings.MaxConflicts = "-1"
	settings.BlockIndexing = false
	settings.Hashers = "2"
	settings.SyncXattrs = false
	settings.SendXattrs = true
	settings.XattrFilterEntries = "+user.*"
	if err := client.SetDefaultFolderSettings(settings); err != nil {
		t.Fatal(err)
	}

	after := wrapper.DefaultFolder()
	if after.Type != config.FolderTypeSendOnly || after.Path != "~/Elsewhere" || after.RescanIntervalS != 60 ||
		after.FSWatcherEnabled || after.FSWatcherDelayS != 2.5 || after.MaxConflicts != -1 || after.BlockIndexing ||
		after.Hashers != 2 || !after.SendXattrs || len(after.XattrFilter.Entries) != 1 {
		t.Fatalf("got %+v", after)
	}
}

func TestDefaultFolderSettingsAreChecked(t *testing.T) {
	tests := []struct {
		name string
		edit func(*FolderSettings)
		want error
	}{
		{"a type that can't be changed afterwards", func(s *FolderSettings) { s.Type = "receiveencrypted" }, nil},
		{"a relative path", func(s *FolderSettings) { s.Path = "folders" }, errDefaultPathRelative},
		{"a path that only looks like it is at home", func(s *FolderSettings) { s.Path = "~folders" }, errDefaultPathRelative},
		{"no delay for the watcher", func(s *FolderSettings) { s.FSWatcherDelayS = "0" }, nil},
		{"a rescan interval the engine would limit", func(s *FolderSettings) { s.RescanIntervalS = "31536001" }, nil},
		{"negative conflicts", func(s *FolderSettings) { s.MaxConflicts = "-2" }, nil},
		{"what applies to folders as well", func(s *FolderSettings) { s.Copiers = "-1" }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, wrapper := advancedTestClient(t)
			before := wrapper.DefaultFolder()
			settings := client.DefaultFolderSettings()
			tt.edit(settings)
			err := client.SetDefaultFolderSettings(settings)
			if err == nil {
				t.Fatal("saved")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if after := wrapper.DefaultFolder(); !reflect.DeepEqual(before, after) {
				t.Fatal("something was changed")
			}
		})
	}
}

// The engine only creates a marker of the default name, so another one would stop every new folder
func TestDefaultFolderSettingsLeaveTheMarkerAlone(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	settings := client.DefaultFolderSettings()
	settings.MarkerName = "marker"
	if err := client.SetDefaultFolderSettings(settings); err != nil {
		t.Fatal(err)
	}
	if got := wrapper.DefaultFolder().MarkerName; got != config.DefaultMarkerName {
		t.Fatalf("the marker became %q", got)
	}
}

func TestDefaultVersioningSettings(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	settings := client.DefaultVersioningSettings()
	settings.Type = VersioningTypeStaggered
	settings.KeepPlatformHistory = true
	if err := client.SetDefaultVersioningSettings(settings); err != nil {
		t.Fatal(err)
	}
	got := wrapper.DefaultFolder().Versioning
	if got.Type != VersioningTypeStaggered || got.Params["maxAge"] != "31536000" ||
		got.Params["preservePlatformData"] != "true" {
		t.Fatalf("got %+v", got)
	}

	// Every new folder would keep its versions in the same place
	settings = client.DefaultVersioningSettings()
	settings.Path = filepath.Join(t.TempDir(), "versions")
	if err := client.SetDefaultVersioningSettings(settings); !errors.Is(err, errDefaultVersionsPath) {
		t.Fatalf("got %v", err)
	}
}

// The engine takes no path to mean its working directory for a folder it accepts by itself, whatever the app makes of it
func TestDefaultFolderLocationIsSpelledOut(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	settings := client.DefaultFolderSettings()
	settings.Path = "~/Elsewhere"
	if err := client.SetDefaultFolderSettings(settings); err != nil {
		t.Fatal(err)
	}
	settings = client.DefaultFolderSettings()
	settings.Path = " "
	if err := client.SetDefaultFolderSettings(settings); err != nil {
		t.Fatal(err)
	}
	if got := wrapper.DefaultFolder().Path; got != "~/Sync" {
		t.Fatalf("got %q", got)
	}
}

func TestDefaultDeviceSettings(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	settings := client.DefaultDeviceSettings()
	settings.Compression = "never"
	settings.MaxSendMbits = "8"
	if err := client.SetDefaultDeviceSettings(settings); err != nil {
		t.Fatal(err)
	}
	got := wrapper.DefaultDevice()
	if got.Compression != config.CompressionNever || got.MaxSendKbps != mbitsPerSecondToKiBPerSecond(8) {
		t.Fatalf("got %+v", got)
	}

	// A new device starts from them
	if err := client.AddPeer(settingsTestDeviceID.String()); err != nil {
		t.Fatal(err)
	}
	if device, ok := wrapper.Device(settingsTestDeviceID); !ok || device.Compression != config.CompressionNever {
		t.Fatalf("got %+v", device)
	}
}

func TestDefaultIgnoreLinesKeepWhatWasEntered(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	if err := client.SetDefaultIgnoreLines("*.tmp\n\n// comment \n"); err != nil {
		t.Fatal(err)
	}
	if got := wrapper.DefaultIgnores().Lines; !slices.Equal(got, []string{"*.tmp", "", "// comment "}) {
		t.Fatalf("got %q", got)
	}
	if got := client.DefaultIgnoreLines(); got != "*.tmp\n\n// comment " {
		t.Fatalf("got %q", got)
	}
}

// Every new folder would stop on these
func TestDefaultIgnoreLinesAreChecked(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	if err := client.SetDefaultIgnoreLines("*.tmp\n[abc"); err == nil {
		t.Fatal("saved")
	}
	if got := wrapper.DefaultIgnores().Lines; len(got) != 0 {
		t.Fatalf("got %q", got)
	}
	// A file to include can only be found in a folder
	if err := client.SetDefaultIgnoreLines("#include .stglobalignore\n(?d).DS_Store"); err != nil {
		t.Fatal(err)
	}
}

func TestPlaceDefaultIgnores(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new", "folder")
	folder := config.FolderConfiguration{FilesystemType: config.FilesystemTypeBasic, Path: root}
	if err := placeDefaultIgnores(&folder, []string{"*.tmp"}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(root, ignoreFileName))
	if err != nil || strings.ReplaceAll(string(contents), "\r\n", "\n") != "*.tmp\n" {
		t.Fatalf("got %q, %v", contents, err)
	}

	// Patterns a folder already has stay
	if err := placeDefaultIgnores(&folder, []string{"*.other"}); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(root, ignoreFileName)); strings.ReplaceAll(string(contents), "\r\n", "\n") != "*.tmp\n" {
		t.Fatalf("got %q", contents)
	}
	// Writing no lines removes the file, which isn't what having no default patterns means
	if err := placeDefaultIgnores(&folder, nil); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(root, ignoreFileName)); strings.ReplaceAll(string(contents), "\r\n", "\n") != "*.tmp\n" {
		t.Fatalf("got %q", contents)
	}
}
