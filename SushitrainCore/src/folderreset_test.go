// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"reflect"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
)

func TestFolderResetPreviewAndSave(t *testing.T) {
	for _, kind := range []config.FolderType{config.FolderTypeSendReceive, config.FolderTypeSendOnly, config.FolderTypeReceiveOnly, config.FolderTypeReceiveEncrypted} {
		t.Run(kind.String(), func(t *testing.T) {
			folder := settingsTestFolder(t)
			if err := folder.changeFolderCopy(func(fc *config.FolderConfiguration) error {
				fc.Type = kind
				fc.Copiers, fc.Hashers, fc.RescanIntervalS, fc.MaxConflicts = 7, 8, 120, 2
				fc.FSWatcherEnabled, fc.BlockIndexing = false, false
				fc.Label, fc.Group, fc.MarkerName = "Keep label", "Keep group", "custom-marker"
				fc.Paused = true
				fc.FilesystemType = filesystemTypeTrash
				fc.Versioning = config.VersioningConfiguration{Type: "simple", Params: map[string]string{"keep": "12"}, FSPath: "/custom-history"}
				fc.XattrFilter.Entries = []config.XattrFilterEntry{{Match: "custom.*", Permit: true}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := folder.folderConfiguration().Copy()
			preview := folder.ResetSettingsPreview()
			if preview == nil {
				t.Fatal("missing preview")
			}
			if got := folder.folderConfiguration(); !reflect.DeepEqual(before, *got) {
				t.Fatal("preview persisted changes")
			}
			want := newFolderSettings(folderDefaultsForType(kind))
			for _, field := range folderSyncResetFields {
				if field.typed != "" && !reflect.DeepEqual(reflect.ValueOf(preview).Elem().FieldByName(field.typed).Interface(), reflect.ValueOf(want).Elem().FieldByName(field.typed).Interface()) {
					t.Errorf("wrong default for %s", field.typed)
				}
			}
			if preview.MarkerName != before.MarkerName || preview.Path != before.Path || preview.Type != kind.String() {
				t.Fatal("protected preview fields changed")
			}
			preview.Copiers = "invalid"
			if err := folder.SetSettings(preview); err == nil {
				t.Fatal("invalid reset saved")
			}
			if got := folder.folderConfiguration(); !reflect.DeepEqual(before, *got) {
				t.Fatal("invalid save partially applied")
			}
			preview.Copiers = "3"
			// Changes outside the reset scope made after preview must survive Save.
			if err := folder.changeFolderConfiguration(func(fc *config.FolderConfiguration) {
				fc.Label = "Concurrent label"
				fc.Versioning.Params["keep"] = "20"
			}); err != nil {
				t.Fatal(err)
			}
			if err := folder.SetSettings(preview); err != nil {
				t.Fatal(err)
			}
			after := folder.folderConfiguration().Copy()
			if after.Copiers != 3 || after.RescanIntervalS != 3600 || after.FSWatcherEnabled != want.FSWatcherEnabled || after.BlockIndexing != want.BlockIndexing {
				t.Fatal("reset scan/transfer options did not save")
			}
			if after.Type != before.Type || after.Path != before.Path || after.Group != before.Group || after.MarkerName != before.MarkerName || after.Paused != before.Paused || after.FilesystemType != before.FilesystemType || after.Label != "Concurrent label" || after.Versioning.Params["keep"] != "20" || after.Versioning.FSPath != before.Versioning.FSPath || !reflect.DeepEqual(after.Devices, before.Devices) {
				t.Fatal("protected fields changed")
			}
			again := folder.ResetSettingsPreview()
			again.Copiers = want.Copiers
			if err := folder.SetSettings(again); err != nil {
				t.Fatal(err)
			}
			reset := folder.ResetSettingsPreview()
			if !reset.Equal(folder.Settings()) {
				t.Fatal("repeated reset is not idempotent")
			}
			stable := folder.folderConfiguration().Copy()
			if err := folder.SetSettings(reset); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stable, *folder.folderConfiguration()) {
				t.Fatal("already-default save changed configuration")
			}
		})
	}
}

func TestDefaultFolderResetDoesNotUseCustomizedDefaults(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	current := client.DefaultFolderSettings()
	current.Copiers, current.Path, current.Type = "17", "~/Custom", "sendonly"
	if err := client.SetDefaultFolderSettings(current); err != nil {
		t.Fatal(err)
	}
	before := wrapper.DefaultFolder().Copy()
	existing, _ := wrapper.Folder(advancedTestFolder)
	preview := client.DefaultFolderResetSettingsPreview()
	if preview.Copiers == "17" || preview.Path != "~/Custom" || preview.Type != "sendonly" || preview.SyncXattrs {
		t.Fatal("wrong defaults source or folder-type adaptation")
	}
	if !reflect.DeepEqual(before, wrapper.DefaultFolder()) {
		t.Fatal("preview changed defaults")
	}
	if err := client.SetDefaultFolderSettings(preview); err != nil {
		t.Fatal(err)
	}
	after, _ := wrapper.Folder(advancedTestFolder)
	if !reflect.DeepEqual(existing, after) {
		t.Fatal("reset defaults changed an existing folder")
	}
}

func TestRawFolderResetPreservesBaseline(t *testing.T) {
	client, wrapper := advancedTestClient(t)
	sectionID := AdvancedSectionFolderPrefix + advancedTestFolder
	if err := saveAdvancedField(t, client, sectionID, "copiers", "7"); err != nil {
		t.Fatal(err)
	}
	before, _ := wrapper.Folder(advancedTestFolder)
	preview, err := client.ResetFolderSyncSection(sectionID)
	if err != nil {
		t.Fatal(err)
	}
	afterPreview, _ := wrapper.Folder(advancedTestFolder)
	if !reflect.DeepEqual(before, afterPreview) {
		t.Fatal("raw preview saved changes")
	}
	for _, field := range preview.fields {
		if !IsFolderSyncResetField(field.Path) && field.Value != field.saved {
			t.Fatalf("reset protected %s", field.Path)
		}
		if field.Path == "copiers" && (field.Value == "7" || field.saved != "7") {
			t.Fatal("lost reset baseline")
		}
		if field.Path == "hashers" {
			field.Value = "3"
		}
	}
	if err := client.SaveAdvancedSection(preview); err != nil {
		t.Fatal(err)
	}
	after, _ := wrapper.Folder(advancedTestFolder)
	if after.Copiers == 7 || after.Hashers != 3 || after.Path != before.Path || after.Type != before.Type || !reflect.DeepEqual(after.Versioning, before.Versioning) {
		t.Fatal("raw reset did not preserve scope or edited preview")
	}
	if _, err := client.ResetFolderSyncSection(AdvancedSectionOptions); err == nil {
		t.Fatal("unrelated reset accepted")
	}
}

func TestRawFolderResetValidatesEditedPreview(t *testing.T) {
	for _, sectionID := range []string{AdvancedSectionFolderPrefix + advancedTestFolder, AdvancedSectionDefaultFolder} {
		for _, tc := range []struct {
			name   string
			kind   config.FolderType
			values map[string]string
		}{
			{"negative copiers", config.FolderTypeSendReceive, map[string]string{"copiers": "-1"}},
			{"zero watcher delay", config.FolderTypeSendReceive, map[string]string{"fsWatcherDelayS": "0"}},
			{"ownership conflict", config.FolderTypeSendReceive, map[string]string{"syncOwnership": "true", "copyOwnershipFromParent": "true"}},
			{"send-only metadata", config.FolderTypeSendOnly, map[string]string{"syncOwnership": "true"}},
			{"receive-only metadata", config.FolderTypeReceiveOnly, map[string]string{"sendOwnership": "true"}},
			{"encrypted permissions", config.FolderTypeReceiveEncrypted, map[string]string{"ignorePerms": "false"}},
		} {
			t.Run(sectionID+"/"+tc.name, func(t *testing.T) {
				client, wrapper := advancedTestClient(t)
				var targetErr error
				if err := client.changeConfiguration(func(c *config.Configuration) {
					target, err := advancedTarget(c, sectionID)
					if err != nil {
						targetErr = err
						return
					}
					folder := target.(*config.FolderConfiguration)
					folder.Type = tc.kind
					folder.SyncOwnership, folder.SendOwnership, folder.CopyOwnershipFromParent = false, false, false
					folder.IgnorePerms = tc.kind == config.FolderTypeReceiveEncrypted
				}); err != nil {
					t.Fatal(err)
				}
				if targetErr != nil {
					t.Fatal(targetErr)
				}
				preview, err := client.ResetFolderSyncSection(sectionID)
				if err != nil {
					t.Fatal(err)
				}
				before := wrapper.RawCopy()
				valid := make(map[string]string, len(preview.fields))
				for _, field := range preview.fields {
					valid[field.Path] = field.Value
					if value, ok := tc.values[field.Path]; ok {
						field.Value = value
					}
				}
				if err := client.SaveAdvancedSection(preview); err == nil {
					t.Fatal("invalid reset preview saved")
				}
				if !reflect.DeepEqual(before, wrapper.RawCopy()) {
					t.Fatal("invalid reset partially changed configuration")
				}
				for _, field := range preview.fields {
					field.Value = valid[field.Path]
					if field.Path == "junctionsAsDirs" {
						field.Value = "true"
					}
				}
				if err := client.SaveAdvancedSection(preview); err != nil {
					t.Fatalf("corrected reset preview: %v", err)
				}
				current := wrapper.RawCopy()
				target, err := advancedTarget(&current, sectionID)
				if err != nil || !target.(*config.FolderConfiguration).JunctionsAsDirs {
					t.Fatal("lost raw-only Windows field")
				}
			})
		}
	}
}
