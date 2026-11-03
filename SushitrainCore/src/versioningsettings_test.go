// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/versioner"
)

var allVersioningTypes = []string{
	VersioningTypeNone, VersioningTypeSimple, VersioningTypeTrashcan, VersioningTypeStaggered, VersioningTypeExternal,
}

func setVersioning(t *testing.T, folder *Folder, versioning config.VersioningConfiguration) {
	t.Helper()
	if err := folder.changeFolderConfiguration(func(fc *config.FolderConfiguration) {
		fc.Versioning = versioning
	}); err != nil {
		t.Fatal(err)
	}
}

// Every type can become every other, with what the page shows, and the versioner then starts: a folder that gets a
// versioning the engine can't start with would stop.
func TestVersioningEveryChangeOfTypeStarts(t *testing.T) {
	for _, from := range allVersioningTypes {
		for _, to := range allVersioningTypes {
			t.Run(from+" to "+to, func(t *testing.T) {
				folder := settingsTestFolder(t)
				settings := folder.VersioningSettings()
				settings.Type = from
				settings.Command = "archive %FILE_PATH%"
				if err := folder.SetVersioningSettings(settings); err != nil {
					t.Fatal(err)
				}

				settings = folder.VersioningSettings()
				settings.Type = to
				settings.Command = "archive %FILE_PATH%"
				if err := folder.SetVersioningSettings(settings); err != nil {
					t.Fatal(err)
				}

				after := *folder.folderConfiguration()
				if after.Versioning.Type != to {
					t.Fatalf("the type is %q", after.Versioning.Type)
				}
				if to != VersioningTypeNone {
					if _, err := versioner.New(after); err != nil {
						t.Fatal(err)
					}
				}
				if after.Versioning.FSType != config.FilesystemTypeBasic {
					t.Fatalf("the file system type is %q", after.Versioning.FSType)
				}
			})
		}
	}
}

func TestVersioningNewTypeSavesWhatIsShown(t *testing.T) {
	tests := []struct {
		name         string
		settingsType string
		want         map[string]string
	}{
		{"simple", VersioningTypeSimple, map[string]string{"keep": "5", "cleanoutDays": "0"}},
		{"trash can", VersioningTypeTrashcan, map[string]string{"cleanoutDays": "0"}},
		{"staggered", VersioningTypeStaggered, map[string]string{"maxAge": "31536000"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			folder := settingsTestFolder(t)
			settings := folder.VersioningSettings()
			settings.Type = tt.settingsType
			if err := folder.SetVersioningSettings(settings); err != nil {
				t.Fatal(err)
			}
			got := folder.folderConfiguration().Versioning
			if !maps.Equal(got.Params, tt.want) {
				t.Fatalf("got %v", got.Params)
			}
			if got.CleanupIntervalS != 3600 {
				t.Fatalf("cleaning up every %d s", got.CleanupIntervalS)
			}
		})
	}
}

func TestVersioningKeepsWhatItDoesNotManage(t *testing.T) {
	folder := settingsTestFolder(t)
	setVersioning(t, folder, config.VersioningConfiguration{
		Type: VersioningTypeStaggered, FSType: config.FilesystemTypeBasic, CleanupIntervalS: 90,
		Params: map[string]string{"maxAge": "86401", "preservePlatformData": "true", "future": "yes"},
	})

	settings := folder.VersioningSettings()
	if !settings.KeepPlatformHistory || settings.MaxAgeDays != "1" || settings.CleanupIntervalMinutes != "1" {
		t.Fatalf("read as %+v", settings)
	}
	settings.Keep = "9"
	if err := folder.SetVersioningSettings(settings); err != nil {
		t.Fatal(err)
	}
	got := folder.folderConfiguration().Versioning
	want := map[string]string{"maxAge": "86401", "preservePlatformData": "true", "future": "yes"}
	if !maps.Equal(got.Params, want) || got.CleanupIntervalS != 90 {
		t.Fatalf("got %+v", got)
	}
}

func TestVersioningLeavesChangesFromElsewhereAlone(t *testing.T) {
	folder := settingsTestFolder(t)
	setVersioning(t, folder, config.VersioningConfiguration{
		Type: VersioningTypeSimple, FSType: config.FilesystemTypeBasic, CleanupIntervalS: 3600,
		Params: map[string]string{"keep": "5", "cleanoutDays": "0"},
	})
	settings := folder.VersioningSettings()
	setVersioning(t, folder, config.VersioningConfiguration{
		Type: VersioningTypeSimple, FSType: config.FilesystemTypeBasic, CleanupIntervalS: 3600,
		Params: map[string]string{"keep": "20", "cleanoutDays": "0"},
	})

	settings.CleanoutDays = "7"
	if err := folder.SetVersioningSettings(settings); err != nil {
		t.Fatal(err)
	}
	got := folder.folderConfiguration().Versioning.Params
	if got["keep"] != "20" || got["cleanoutDays"] != "7" {
		t.Fatalf("got %v", got)
	}
}

// A page can stay open with edits while the configuration changes, and its save is only about what was edited
func TestVersioningDoesNotBringBackATypeFromBefore(t *testing.T) {
	folder := settingsTestFolder(t)
	setVersioning(t, folder, config.VersioningConfiguration{
		Type: VersioningTypeSimple, FSType: config.FilesystemTypeBasic, CleanupIntervalS: 3600,
		Params: map[string]string{"keep": "5", "cleanoutDays": "0"},
	})
	settings := folder.VersioningSettings()
	staggered := config.VersioningConfiguration{
		Type: VersioningTypeStaggered, FSType: config.FilesystemTypeBasic, CleanupIntervalS: 3600,
		Params: map[string]string{"maxAge": "86400", "preservePlatformData": "true"},
	}
	setVersioning(t, folder, staggered)

	settings.CleanupIntervalMinutes = "30"
	if err := folder.SetVersioningSettings(settings); err != nil {
		t.Fatal(err)
	}
	got := folder.folderConfiguration().Versioning
	if got.Type != VersioningTypeStaggered || !maps.Equal(got.Params, staggered.Params) || got.CleanupIntervalS != 1800 {
		t.Fatalf("got %+v", got)
	}
}

func TestVersioningStaysOffWhenTurnedOffElsewhere(t *testing.T) {
	folder := settingsTestFolder(t)
	setVersioning(t, folder, config.VersioningConfiguration{
		Type: VersioningTypeSimple, FSType: config.FilesystemTypeBasic,
		Params: map[string]string{"keep": "5", "cleanoutDays": "0"},
	})
	settings := folder.VersioningSettings()
	setVersioning(t, folder, config.VersioningConfiguration{FSType: config.FilesystemTypeBasic})

	settings.Keep = "9"
	if err := folder.SetVersioningSettings(settings); err != nil {
		t.Fatal(err)
	}
	if got := folder.folderConfiguration().Versioning.Type; got != VersioningTypeNone {
		t.Fatalf("versioning came back as %q", got)
	}
}

func TestVersioningLeavesACommandFromElsewhereAlone(t *testing.T) {
	folder := settingsTestFolder(t)
	external := func(command string) config.VersioningConfiguration {
		return config.VersioningConfiguration{
			Type: VersioningTypeExternal, FSType: config.FilesystemTypeBasic, Params: map[string]string{"command": command},
		}
	}
	setVersioning(t, folder, external("old"))
	settings := folder.VersioningSettings()
	setVersioning(t, folder, external("new"))

	settings.Path = filepath.Join(t.TempDir(), "versions")
	if err := folder.SetVersioningSettings(settings); err != nil {
		t.Fatal(err)
	}
	if got := folder.folderConfiguration().Versioning.Params["command"]; got != "new" {
		t.Fatalf("the command is %q", got)
	}
}

func TestVersioningPlatformHistoryNeverReachesTheVersionerWrongly(t *testing.T) {
	staggered := config.VersioningConfiguration{
		Type: VersioningTypeStaggered, FSType: config.FilesystemTypeBasic,
		Params: map[string]string{"maxAge": "86400", "preservePlatformData": "true"},
	}
	tests := []struct {
		name  string
		edit  func(*VersioningSettings)
		kept  bool
		loses bool
	}{
		{"unchanged", func(*VersioningSettings) {}, true, false},
		{"turned off", func(s *VersioningSettings) { s.KeepPlatformHistory = false }, false, true},
		{"another type", func(s *VersioningSettings) { s.Type = VersioningTypeTrashcan }, false, true},
		{"no versioning", func(s *VersioningSettings) { s.Type = VersioningTypeNone }, false, true},
		{"a custom path", func(s *VersioningSettings) { s.Path = filepath.Join(t.TempDir(), "versions") }, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			folder := settingsTestFolder(t)
			setVersioning(t, folder, staggered)
			settings := folder.VersioningSettings()
			tt.edit(settings)
			if got := settings.LosesPlatformHistory(); got != tt.loses {
				t.Fatalf("loses: %v", got)
			}
			if err := folder.SetVersioningSettings(settings); err != nil {
				t.Fatal(err)
			}
			after := *folder.folderConfiguration()
			if got := after.Versioning.Params["preservePlatformData"] == "true"; got != tt.kept {
				t.Fatalf("kept: %v", got)
			}
			if after.Versioning.Type != "" {
				if _, err := versioner.New(after); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestVersioningPlatformHistoryCanBeTurnedOn(t *testing.T) {
	folder := settingsTestFolder(t)
	settings := folder.VersioningSettings()
	settings.Type = VersioningTypeStaggered
	settings.KeepPlatformHistory = true
	if err := folder.SetVersioningSettings(settings); err != nil {
		t.Fatal(err)
	}
	after := *folder.folderConfiguration()
	if after.Versioning.Params["preservePlatformData"] != "true" {
		t.Fatalf("got %+v", after.Versioning)
	}
	if _, err := versioner.New(after); err != nil {
		t.Fatal(err)
	}
}

func TestVersioningStartsPlatformHistory(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "versions")
	none := config.VersioningConfiguration{FSType: config.FilesystemTypeBasic}
	simple := config.VersioningConfiguration{
		Type: VersioningTypeSimple, FSType: config.FilesystemTypeBasic,
		Params: map[string]string{"keep": "5", "cleanoutDays": "0"},
	}
	staggered := config.VersioningConfiguration{
		Type: VersioningTypeStaggered, FSType: config.FilesystemTypeBasic, Params: map[string]string{"maxAge": "86400"},
	}
	elsewhere := staggered
	elsewhere.FSPath = outside
	kept := config.VersioningConfiguration{
		Type: VersioningTypeStaggered, FSType: config.FilesystemTypeBasic,
		Params: map[string]string{"maxAge": "86400", "preservePlatformData": "true"},
	}
	// The toggle stays on when it goes out of sight, for another type or a custom path
	keepWith := func(versioningType string) func(*VersioningSettings) {
		return func(s *VersioningSettings) { s.Type, s.KeepPlatformHistory = versioningType, true }
	}
	tests := []struct {
		name   string
		before config.VersioningConfiguration
		edit   func(*VersioningSettings)
		starts bool
	}{
		{"turned on", staggered, keepWith(VersioningTypeStaggered), true},
		{"from no versioning", none, keepWith(VersioningTypeStaggered), true},
		{"from another type", simple, keepWith(VersioningTypeStaggered), true},
		{"unchanged", staggered, func(*VersioningSettings) {}, false},
		{"another edit", staggered, func(s *VersioningSettings) { s.MaxAgeDays = "2" }, false},
		{"already kept", kept, keepWith(VersioningTypeStaggered), false},
		{"already kept, another edit", kept, func(s *VersioningSettings) { s.MaxAgeDays = "2" }, false},
		{"turned off", kept, func(s *VersioningSettings) { s.KeepPlatformHistory = false }, false},
		{"on, then another type", staggered, keepWith(VersioningTypeTrashcan), false},
		{"on, then no versioning", staggered, keepWith(VersioningTypeNone), false},
		{"on, then a custom path", staggered, func(s *VersioningSettings) {
			s.KeepPlatformHistory, s.Path = true, outside
		}, false},
		{"on with a custom path", elsewhere, keepWith(VersioningTypeStaggered), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			folder := settingsTestFolder(t)
			setVersioning(t, folder, tt.before)
			settings := folder.VersioningSettings()
			tt.edit(settings)
			if got := settings.StartsPlatformHistory(); got != tt.starts {
				t.Fatalf("starts: %v", got)
			}
			if err := folder.SetVersioningSettings(settings); err != nil {
				t.Fatal(err)
			}
			// What it says has to be what saving does
			keeps := folder.folderConfiguration().Versioning.Params["preservePlatformData"] == "true"
			if started := keeps && tt.before.Params["preservePlatformData"] != "true"; started != tt.starts {
				t.Fatalf("started: %v", started)
			}
		})
	}

	unread := &VersioningSettings{Type: VersioningTypeStaggered, KeepPlatformHistory: true}
	if unread.StartsPlatformHistory() {
		t.Fatal("settings that were never read start it")
	}
}

func TestVersioningKeepsACustomPath(t *testing.T) {
	folder := settingsTestFolder(t)
	outside := filepath.Join(t.TempDir(), "versions")
	settings := folder.VersioningSettings()
	settings.Type = VersioningTypeTrashcan
	settings.Path = outside
	if err := folder.SetVersioningSettings(settings); err != nil {
		t.Fatal(err)
	}
	if got := folder.VersioningPath(); got != outside {
		t.Fatalf("got %q", got)
	}

	settings = folder.VersioningSettings()
	settings.Type = VersioningTypeSimple
	if err := folder.SetVersioningSettings(settings); err != nil {
		t.Fatal(err)
	}
	if got := folder.VersioningPath(); got != outside {
		t.Fatalf("got %q after a change of type", got)
	}

	settings = folder.VersioningSettings()
	settings.Path = ""
	if err := folder.SetVersioningSettings(settings); err != nil {
		t.Fatal(err)
	}
	if got := folder.folderConfiguration().Versioning.FSPath; got != "" {
		t.Fatalf("got %q after clearing it", got)
	}
}

func TestVersioningIsChecked(t *testing.T) {
	tests := []struct {
		name string
		edit func(*VersioningSettings)
	}{
		{"unknown type", func(s *VersioningSettings) { s.Type = "shredder" }},
		{"nothing to keep", func(s *VersioningSettings) { s.Type, s.Keep = VersioningTypeSimple, "0" }},
		{"no number", func(s *VersioningSettings) { s.Type, s.Keep = VersioningTypeSimple, "many" }},
		{"negative age", func(s *VersioningSettings) { s.Type, s.CleanoutDays = VersioningTypeTrashcan, "-1" }},
		{"an age that overflows into removing everything", func(s *VersioningSettings) {
			s.Type, s.CleanoutDays = VersioningTypeTrashcan, "106752"
		}},
		{"the same for simple", func(s *VersioningSettings) { s.Type, s.CleanoutDays = VersioningTypeSimple, "999999" }},
		{"no maximum age", func(s *VersioningSettings) { s.Type, s.MaxAgeDays = VersioningTypeStaggered, "" }},
		{"a maximum age that doesn't fit", func(s *VersioningSettings) {
			s.Type, s.MaxAgeDays = VersioningTypeStaggered, "25000"
		}},
		{"no command", func(s *VersioningSettings) { s.Type, s.Command = VersioningTypeExternal, " " }},
		{"cleanup interval", func(s *VersioningSettings) { s.Type, s.CleanupIntervalMinutes = VersioningTypeSimple, "-5" }},
		{"cleanup interval the engine would limit", func(s *VersioningSettings) {
			s.Type, s.CleanupIntervalMinutes = VersioningTypeSimple, "99999999"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			folder := settingsTestFolder(t)
			before := folder.folderConfiguration().Copy()
			settings := folder.VersioningSettings()
			tt.edit(settings)
			if err := folder.SetVersioningSettings(settings); err == nil {
				t.Fatal("saved")
			}
			if after := folder.folderConfiguration(); !reflect.DeepEqual(before, *after) {
				t.Fatalf("something was changed:\n%+v\n%+v", before, *after)
			}
		})
	}
}

func TestVersioningPathFollowsLinks(t *testing.T) {
	folder := settingsTestFolder(t)
	root, _ := folder.LocalNativePath()
	base := t.TempDir()
	if err := os.Symlink(root, filepath.Join(base, "inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(root), filepath.Join(base, "around")); err != nil {
		t.Fatal(err)
	}

	settings := folder.VersioningSettings()
	settings.Type = VersioningTypeTrashcan
	settings.Path = filepath.Join(base, "inside", "versions")
	if err := folder.SetVersioningSettings(settings); !errors.Is(err, errVersionsPathInFolder) {
		t.Fatalf("got %v", err)
	}
	settings = folder.VersioningSettings()
	settings.Type = VersioningTypeTrashcan
	settings.Path = filepath.Join(base, "around")
	if err := folder.SetVersioningSettings(settings); !errors.Is(err, errVersionsPathAroundFolder) {
		t.Fatalf("got %v", err)
	}
}

func TestVersioningPathIsNotSharedWithAnotherFolder(t *testing.T) {
	folder := settingsTestFolder(t)
	shared := filepath.Join(t.TempDir(), "versions")
	other := config.FolderConfiguration{
		ID: "other", FilesystemType: config.FilesystemTypeBasic, Path: t.TempDir(),
		Versioning: config.VersioningConfiguration{
			Type: VersioningTypeTrashcan, FSPath: shared, FSType: config.FilesystemTypeBasic,
		},
	}
	if err := folder.client.changeConfiguration(func(cfg *config.Configuration) { cfg.SetFolder(other) }); err != nil {
		t.Fatal(err)
	}

	settings := folder.VersioningSettings()
	settings.Type = VersioningTypeTrashcan
	settings.Path = shared
	if err := folder.SetVersioningSettings(settings); !errors.Is(err, errVersionsPathShared) {
		t.Fatalf("got %v", err)
	}

	// And a new folder doesn't go in there either
	if !isInVersionsPath(filepath.Join(shared, "new"), folder.client.cfg().Folders()) {
		t.Fatal("a folder in another's versions path is fine")
	}
	if isInVersionsPath(filepath.Join(t.TempDir(), "new"), folder.client.cfg().Folders()) {
		t.Fatal("a folder elsewhere isn't")
	}
}

func TestVersioningPathStaysOutOfFolders(t *testing.T) {
	folder := settingsTestFolder(t)
	root, _ := folder.LocalNativePath()
	tests := []struct {
		name string
		path string
		want error
	}{
		{"in the folder", filepath.Join(root, "versions"), errVersionsPathInFolder},
		{"in the folder, in other letters", filepath.Join(strings.ToUpper(root), "versions"), errVersionsPathInFolder},
		{"the folder itself", root, errVersionsPathInFolder},
		{"relative, so in the folder", "versions", errVersionsPathInFolder},
		{"around the folder", filepath.Dir(root), errVersionsPathAroundFolder},
		{"around the folder, in other letters", strings.ToUpper(filepath.Dir(root)), errVersionsPathAroundFolder},
		{"the folder's own versions", filepath.Join(root, ".stversions"), nil},
		{"relative to the folder's own versions", ".stversions/old", nil},
		{"outside the folder", filepath.Join(t.TempDir(), "versions"), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := folder.VersioningSettings()
			settings.Type = VersioningTypeTrashcan
			settings.Path = tt.path
			if err := folder.SetVersioningSettings(settings); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}
