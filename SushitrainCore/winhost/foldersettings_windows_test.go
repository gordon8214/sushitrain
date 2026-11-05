// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package winhost

import "testing"

func TestFolderEditorSections(t *testing.T) {
	for _, field := range []string{"label", "type", "rescanIntervalS", "fsWatcherEnabled", "fsWatcherDelayS", "maxConflicts", "blockIndexing"} {
		if !visibleFolderEditorField("basic/folder/test", field) {
			t.Errorf("General lost %s", field)
		}
	}
	for _, field := range []string{"copiers", "xattrFilter.entries", "versioning.type", "futureTuningOption"} {
		if visibleFolderEditorField("basic/folder/test", field) {
			t.Errorf("General includes %s", field)
		}
		if !visibleFolderEditorField("folder/test", field) || !visibleFolderEditorField("defaults/folder", field) {
			t.Errorf("Full editor lost %s", field)
		}
	}
	for _, section := range []string{"versioning/test", "defaults/versioning"} {
		if !visibleFolderEditorField(section, "versioning.type") || !visibleFolderEditorField(section, "versioning.params") || visibleFolderEditorField(section, "type") {
			t.Errorf("wrong versioning fields in %s", section)
		}
	}
}

func TestResetEditorOnlyExposesSynchronizationFields(t *testing.T) {
	for _, section := range []string{"reset/folder/test", "reset/defaults/folder"} {
		for _, field := range []string{"copiers", "rescanIntervalS", "syncXattrs", "minDiskFree.value", "xattrFilter.entries", "junctionsAsDirs"} {
			if !visibleFolderEditorField(section, field) {
				t.Errorf("reset lost %s", field)
			}
		}
		for _, field := range []string{"id", "path", "type", "markerName", "paused", "devices", "versioning.params", "filesystemType", "futureOption"} {
			if visibleFolderEditorField(section, field) {
				t.Errorf("reset exposed protected or unclassified field %s", field)
			}
		}
	}
}
