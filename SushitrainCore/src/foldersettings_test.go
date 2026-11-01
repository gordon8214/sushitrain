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
	"testing"

	"github.com/syncthing/syncthing/lib/config"
)

func settingsTestFolder(t *testing.T) *Folder {
	t.Helper()
	folderPath := filepath.Join(t.TempDir(), "folder")
	writeFolderWithMarker(t, folderPath)
	folder := newFolderBaseTestFolder(t, folderPath, false)
	// As it would be when read from disk, so that the tests see what a save changes and not what preparing it fills in
	if err := folder.changeFolderConfiguration(func(*config.FolderConfiguration) {}); err != nil {
		t.Fatal(err)
	}
	return folder
}

func setFolderType(t *testing.T, folder *Folder, folderType config.FolderType) {
	t.Helper()
	if err := folder.changeFolderConfiguration(func(fc *config.FolderConfiguration) { fc.Type = folderType }); err != nil {
		t.Fatal(err)
	}
}

func TestFolderSettingsChangeNothingUnlessEdited(t *testing.T) {
	folder := settingsTestFolder(t)
	before := folder.folderConfiguration().Copy()

	if err := folder.SetSettings(folder.Settings()); err != nil {
		t.Fatal(err)
	}
	if after := folder.folderConfiguration(); !reflect.DeepEqual(before, *after) {
		t.Fatalf("changed:\n%+v\n%+v", before, *after)
	}
}

func TestFolderSettingsLeaveChangesFromElsewhereAlone(t *testing.T) {
	folder := settingsTestFolder(t)
	settings := folder.Settings()
	if err := folder.changeFolderConfiguration(func(fc *config.FolderConfiguration) {
		fc.Copiers = 7
		fc.IgnoreDelete = true
	}); err != nil {
		t.Fatal(err)
	}

	settings.Hashers = "3"
	if err := folder.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	after := folder.folderConfiguration()
	if after.Copiers != 7 || !after.IgnoreDelete || after.Hashers != 3 {
		t.Fatalf("%d, %v, %d", after.Copiers, after.IgnoreDelete, after.Hashers)
	}
}

func TestFolderSettingsAreSaved(t *testing.T) {
	folder := settingsTestFolder(t)
	settings := folder.Settings()
	settings.Order = "newestFirst"
	settings.MinDiskFree = "5"
	settings.MinDiskFreeUnit = "GB"
	settings.IgnoreDelete = true
	settings.CaseSensitiveFS = true
	settings.ModTimeWindowS = "2"
	settings.DisableSparseFiles = true
	settings.DisableFsync = true
	settings.FSWatcherTimeoutS = "90.5"
	settings.AutoNormalize = false
	settings.ScanProgressIntervalS = "-1"
	settings.Copiers = "2"
	settings.Hashers = "4"
	settings.PullerMaxPendingKiB = "2048"
	settings.PullerPauseS = "30"
	settings.PullerDelayS = "0.5"
	settings.MaxConcurrentWrites = "8"
	settings.BlockPullOrder = "inOrder"
	settings.BlockCopyMethod = "all"
	settings.SyncXattrs = true
	settings.SyncOwnership = true
	settings.IgnorePerms = true
	settings.XattrMode = "all"
	settings.XattrFilterEntries = "+com.apple.*\n-*"
	settings.XattrMaxSingleEntrySize = "2048"
	settings.XattrMaxTotalSize = "8192"
	if err := folder.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	after := folder.folderConfiguration()
	want := config.FolderConfiguration{
		Order: config.PullOrderNewestFirst, MinDiskFree: config.Size{Value: 5, Unit: "GB"}, IgnoreDelete: true,
		CaseSensitiveFS: true, RawModTimeWindowS: 2, DisableSparseFiles: true, DisableFsync: true, FSWatcherTimeoutS: 90.5,
		ScanProgressIntervalS: -1, Copiers: 2, Hashers: 4, PullerMaxPendingKiB: 2048, PullerPauseS: 30, PullerDelayS: 0.5,
		MaxConcurrentWrites: 8, BlockPullOrder: config.BlockPullOrderInOrder,
		CopyRangeMethod: config.CopyRangeMethodAllWithFallback, SyncXattrs: true, SyncOwnership: true, IgnorePerms: true,
		XattrFilter: config.XattrFilter{
			MacOSMode: config.MacOSXattrAll, MaxSingleEntrySize: 2048, MaxTotalSize: 8192,
			Entries: []config.XattrFilterEntry{{Match: "com.apple.*", Permit: true}, {Match: "*"}},
		},
	}
	if after.Order != want.Order || after.MinDiskFree != want.MinDiskFree || after.IgnoreDelete != want.IgnoreDelete ||
		after.CaseSensitiveFS != want.CaseSensitiveFS || after.RawModTimeWindowS != want.RawModTimeWindowS ||
		after.DisableSparseFiles != want.DisableSparseFiles || after.DisableFsync != want.DisableFsync ||
		after.FSWatcherTimeoutS != want.FSWatcherTimeoutS || after.AutoNormalize ||
		after.ScanProgressIntervalS != want.ScanProgressIntervalS || after.Copiers != want.Copiers ||
		after.Hashers != want.Hashers || after.PullerMaxPendingKiB != want.PullerMaxPendingKiB ||
		after.PullerPauseS != want.PullerPauseS || after.PullerDelayS != want.PullerDelayS ||
		after.MaxConcurrentWrites != want.MaxConcurrentWrites || after.BlockPullOrder != want.BlockPullOrder ||
		after.CopyRangeMethod != want.CopyRangeMethod || !after.SyncXattrs || !after.SyncOwnership || !after.IgnorePerms {
		t.Fatalf("got %+v", after)
	}
	if !reflect.DeepEqual(after.XattrFilter, want.XattrFilter) {
		t.Fatalf("got %+v", after.XattrFilter)
	}

	// What comes back is what was entered, so that the page has nothing left to save
	reread := folder.Settings()
	if !reread.Equal(settings) {
		t.Fatalf("read back as\n%+v\nnot\n%+v", reread, settings)
	}
}

// Copying a folder's configuration shares this list. Changing it in place would change the configuration in use, and the
// engine would see nothing to save.
func TestFolderSettingsReplaceXattrEntries(t *testing.T) {
	folder := settingsTestFolder(t)
	if err := folder.changeFolderConfiguration(func(fc *config.FolderConfiguration) {
		fc.XattrFilter.Entries = []config.XattrFilterEntry{{Match: "one", Permit: true}}
	}); err != nil {
		t.Fatal(err)
	}
	before := folder.folderConfiguration().XattrFilter.Entries

	settings := folder.Settings()
	settings.XattrFilterEntries = "-two"
	if err := folder.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	if before[0].Match != "one" {
		t.Fatal("the list in use was changed")
	}
	if entries := folder.folderConfiguration().XattrFilter.Entries; len(entries) != 1 || entries[0].Match != "two" {
		t.Fatalf("got %+v", entries)
	}
}

func TestFolderSettingsAreChecked(t *testing.T) {
	tests := []struct {
		name string
		edit func(*FolderSettings)
	}{
		{"file pull order", func(s *FolderSettings) { s.Order = "sideways" }},
		{"negative modification time window", func(s *FolderSettings) { s.ModTimeWindowS = "-1" }},
		{"no number", func(s *FolderSettings) { s.Copiers = "a few" }},
		{"negative watcher timeout", func(s *FolderSettings) { s.FSWatcherTimeoutS = "-1" }},
		{"not a number as a timeout", func(s *FolderSettings) { s.FSWatcherTimeoutS = "NaN" }},
		{"scan progress the engine would limit", func(s *FolderSettings) { s.ScanProgressIntervalS = "31536001" }},
		{"more hashers than a scan should start", func(s *FolderSettings) { s.Hashers = "257" }},
		{"no writes at once", func(s *FolderSettings) { s.MaxConcurrentWrites = "0" }},
		{"more writes than the engine takes", func(s *FolderSettings) { s.MaxConcurrentWrites = "257" }},
		{"a copy method that doesn't exist here", func(s *FolderSettings) { s.BlockCopyMethod = "ioctl" }},
		{"block pull order", func(s *FolderSettings) { s.BlockPullOrder = "backwards" }},
		{"macOS mode", func(s *FolderSettings) { s.XattrMode = "some" }},
		{"unmarked filter entry", func(s *FolderSettings) { s.XattrFilterEntries = "com.apple.*" }},
		{"empty filter entry", func(s *FolderSettings) { s.XattrFilterEntries = "+" }},
		{"invalid filter pattern", func(s *FolderSettings) { s.XattrFilterEntries = "+[" }},
		{"no free space unit", func(s *FolderSettings) { s.MinDiskFreeUnit = "bananas" }},
		{"all disk space free", func(s *FolderSettings) { s.MinDiskFree, s.MinDiskFreeUnit = "100", "%" }},
		{"copying ownership while synchronizing it", func(s *FolderSettings) {
			s.SyncOwnership, s.CopyOwnershipFromParent = true, true
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			folder := settingsTestFolder(t)
			before := folder.folderConfiguration().Copy()
			settings := folder.Settings()
			settings.PullerPauseS = "9"
			tt.edit(settings)
			if err := folder.SetSettings(settings); err == nil {
				t.Fatal("saved")
			}
			if after := folder.folderConfiguration(); !reflect.DeepEqual(before, *after) {
				t.Fatal("something was changed")
			}
		})
	}
}

func TestFolderSettingsKeepToTheFolderType(t *testing.T) {
	tests := []struct {
		name       string
		folderType config.FolderType
		edit       func(*FolderSettings)
		want       error
	}{
		{"send-only doesn't apply xattrs", config.FolderTypeSendOnly, func(s *FolderSettings) { s.SyncXattrs = true },
			errFolderMetadataSync},
		{"send-only doesn't apply ownership", config.FolderTypeSendOnly, func(s *FolderSettings) { s.SyncOwnership = true },
			errFolderMetadataSync},
		{"receive-only doesn't send xattrs", config.FolderTypeReceiveOnly, func(s *FolderSettings) { s.SendXattrs = true },
			errFolderMetadataSend},
		{"receive-encrypted doesn't apply", config.FolderTypeReceiveEncrypted,
			func(s *FolderSettings) { s.SyncOwnership = true }, errFolderMetadataSync},
		{"receive-encrypted doesn't send", config.FolderTypeReceiveEncrypted,
			func(s *FolderSettings) { s.SendOwnership = true }, errFolderMetadataSend},
		{"receive-encrypted ignores permissions", config.FolderTypeReceiveEncrypted,
			func(s *FolderSettings) { s.IgnorePerms = false }, errFolderPermissions},
		{"send-only sends", config.FolderTypeSendOnly, func(s *FolderSettings) { s.SendXattrs = true }, nil},
		{"receive-only applies", config.FolderTypeReceiveOnly, func(s *FolderSettings) { s.SyncXattrs = true }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			folder := settingsTestFolder(t)
			setFolderType(t, folder, tt.folderType)
			settings := folder.Settings()
			tt.edit(settings)
			if err := folder.SetSettings(settings); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

// What was there before stays usable: only what is turned on now is refused
func TestFolderSettingsLeaveMetadataAFolderAlreadyHas(t *testing.T) {
	folder := settingsTestFolder(t)
	if err := folder.changeFolderConfiguration(func(fc *config.FolderConfiguration) {
		fc.Type = config.FolderTypeSendOnly
		fc.SyncXattrs = true
	}); err != nil {
		t.Fatal(err)
	}
	settings := folder.Settings()
	settings.Hashers = "2"
	if err := folder.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
}

func TestFolderSettingsCheckTheMarker(t *testing.T) {
	folder := settingsTestFolder(t)
	settings := folder.Settings()
	settings.MarkerName = "marker"
	if err := folder.SetSettings(settings); !errors.Is(err, config.ErrMarkerMissing) {
		t.Fatalf("got %v", err)
	}
	if got := folder.folderConfiguration().MarkerName; got != config.DefaultMarkerName {
		t.Fatalf("the marker became %q", got)
	}

	path, _ := folder.LocalNativePath()
	if err := os.WriteFile(filepath.Join(path, "marker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := folder.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	if got := folder.folderConfiguration().MarkerName; got != "marker" {
		t.Fatalf("the marker is %q", got)
	}

	// No name is the default one, which has to be there as well
	settings = folder.Settings()
	settings.MarkerName = " "
	if err := folder.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	if got := folder.folderConfiguration().MarkerName; got != config.DefaultMarkerName {
		t.Fatalf("the marker is %q", got)
	}
}

// The folder itself always exists, so an emptied folder would no longer stop, and its deletions would go out
func TestFolderSettingsRefuseTheFolderAsItsMarker(t *testing.T) {
	folder := settingsTestFolder(t)
	for _, name := range []string{".", "./", "sub/.."} {
		settings := folder.Settings()
		settings.MarkerName = name
		if err := folder.SetSettings(settings); !errors.Is(err, errMarkerIsFolder) {
			t.Fatalf("%q: got %v", name, err)
		}
	}
}

func TestFolderSettingsOfferWhatTheConfigurationHas(t *testing.T) {
	folder := settingsTestFolder(t)
	if err := folder.changeFolderConfiguration(func(fc *config.FolderConfiguration) {
		fc.CopyRangeMethod = config.CopyRangeMethodIoctl
		fc.MinDiskFree = config.Size{Value: 1, Unit: "kiB"}
	}); err != nil {
		t.Fatal(err)
	}
	settings := folder.Settings()
	if got := settings.BlockCopyMethods().data; len(got) != len(folderCopyRangeMethods)+1 || got[len(got)-1] != "ioctl" {
		t.Fatalf("got %v", got)
	}
	if got := settings.MinDiskFreeUnits().data; got[len(got)-1] != "kiB" {
		t.Fatalf("got %v", got)
	}
	// Not edited, so nothing has to be said about it
	if err := folder.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
}

func TestFolderSettingsCompareWithoutWhatTheyWereReadAs(t *testing.T) {
	folder := settingsTestFolder(t)
	first, second := folder.Settings(), folder.Settings()
	second.saved = nil
	if !first.Equal(second) {
		t.Fatal("the same settings differ")
	}
	second.Hashers = "3"
	if first.Equal(second) {
		t.Fatal("different settings are the same")
	}
}

func TestFolderSettingsOfAFolderThatIsGone(t *testing.T) {
	folder := settingsTestFolder(t)
	settings := folder.Settings()
	gone := &Folder{client: folder.client, FolderID: "gone"}
	if gone.Settings() != nil {
		t.Fatal("got settings")
	}
	if err := gone.SetSettings(settings); !errors.Is(err, errFolderConfigNotFound) {
		t.Fatalf("got %v", err)
	}
}
