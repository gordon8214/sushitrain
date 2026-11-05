// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"reflect"
	"strings"

	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

// One policy for the typed editor and raw configuration adapter. Protected fields
// (identity, location, direction, marker, sharing, versioning, and trash) are absent.
var folderSyncResetFields = []struct{ raw, typed string }{
	{"order", "Order"}, {"minDiskFree.value", "MinDiskFree"}, {"minDiskFree.unit", "MinDiskFreeUnit"},
	{"ignoreDelete", "IgnoreDelete"}, {"caseSensitiveFS", "CaseSensitiveFS"}, {"modTimeWindowS", "ModTimeWindowS"},
	{"disableSparseFiles", "DisableSparseFiles"}, {"disableFsync", "DisableFsync"},
	{"fsWatcherTimeoutS", "FSWatcherTimeoutS"}, {"autoNormalize", "AutoNormalize"},
	{"scanProgressIntervalS", "ScanProgressIntervalS"}, {"copiers", "Copiers"}, {"hashers", "Hashers"},
	{"pullerMaxPendingKiB", "PullerMaxPendingKiB"}, {"pullerPauseS", "PullerPauseS"}, {"pullerDelayS", "PullerDelayS"},
	{"maxConcurrentWrites", "MaxConcurrentWrites"}, {"blockPullOrder", "BlockPullOrder"}, {"copyRangeMethod", "BlockCopyMethod"},
	{"syncXattrs", "SyncXattrs"}, {"sendXattrs", "SendXattrs"}, {"syncOwnership", "SyncOwnership"},
	{"sendOwnership", "SendOwnership"}, {"copyOwnershipFromParent", "CopyOwnershipFromParent"}, {"ignorePerms", "IgnorePerms"},
	{"xattrFilter.macOSMode", "XattrMode"}, {"xattrFilter.entries", "XattrFilterEntries"},
	{"xattrFilter.maxSingleEntrySize", "XattrMaxSingleEntrySize"}, {"xattrFilter.maxTotalSize", "XattrMaxTotalSize"},
	{"rescanIntervalS", "RescanIntervalS"}, {"fsWatcherEnabled", "FSWatcherEnabled"},
	{"fsWatcherDelayS", "FSWatcherDelayS"}, {"maxConflicts", "MaxConflicts"}, {"blockIndexing", "BlockIndexing"},
	// Windows exposes this filesystem tuning option in its raw editor.
	{"junctionsAsDirs", ""},
}

// IsFolderSyncResetField reports whether a raw or typed field belongs in a reset preview.
func IsFolderSyncResetField(field string) bool {
	for _, f := range folderSyncResetFields {
		if field == f.raw || f.typed != "" && field == f.typed {
			return true
		}
	}
	return false
}

// builtInFolderDefaults is also used when creating a fresh configuration. Reading
// it never consults or overwrites a user's customized New Folder Defaults.
func builtInFolderDefaults(ios bool) config.FolderConfiguration {
	conf := config.New(protocol.EmptyDeviceID)
	applyAppDefaults(&conf, ios)
	folder := conf.Defaults.Folder.Copy()
	folder.BlockIndexing, folder.SendXattrs, folder.SyncXattrs = true, true, true
	return folder
}

func folderDefaultsForType(kind config.FolderType) config.FolderConfiguration {
	folder := builtInFolderDefaults(build.IsIOS)
	switch kind {
	case config.FolderTypeSendOnly:
		folder.SyncXattrs, folder.SyncOwnership = false, false
	case config.FolderTypeReceiveOnly:
		folder.SendXattrs, folder.SendOwnership = false, false
	case config.FolderTypeReceiveEncrypted:
		folder.SyncXattrs, folder.SendXattrs = false, false
		folder.SyncOwnership, folder.SendOwnership = false, false
		folder.IgnorePerms = true
	}
	return folder
}

func newFolderResetPreview(folder config.FolderConfiguration) *FolderSettings {
	preview := newFolderSettings(folder)
	defaults := newFolderSettings(folderDefaultsForType(folder.Type))
	to, from := reflect.ValueOf(preview).Elem(), reflect.ValueOf(defaults).Elem()
	for _, f := range folderSyncResetFields {
		if f.typed != "" {
			to.FieldByName(f.typed).Set(from.FieldByName(f.typed))
		}
	}
	return preview
}

// ResetSettingsPreview prepares built-in synchronization defaults without saving
// anything. Its baseline stays the current folder, so Save merges only edits.
func (fld *Folder) ResetSettingsPreview() *FolderSettings {
	folder := fld.folderConfiguration()
	if folder == nil {
		return nil
	}
	return newFolderResetPreview(*folder)
}

// DefaultFolderResetSettingsPreview resets only synchronization defaults, retaining
// the configured location, direction, versioning, and other protected fields.
func (clt *Client) DefaultFolderResetSettingsPreview() *FolderSettings {
	cfg := clt.cfg()
	if cfg == nil {
		return nil
	}
	return newFolderResetPreview(cfg.DefaultFolder())
}

// ResetFolderSyncSection prepares a raw reset draft for native desktop adapters.
// Original field baselines and indices stay intact for SaveAdvancedSection.
func (clt *Client) ResetFolderSyncSection(id string) (*AdvancedSection, error) {
	if id != AdvancedSectionDefaultFolder && !strings.HasPrefix(id, AdvancedSectionFolderPrefix) {
		return nil, ErrAdvancedSectionNotFound
	}
	cfg := clt.cfg()
	if cfg == nil {
		return nil, ErrStillLoading
	}
	current := cfg.RawCopy()
	section, err := newAdvancedSection(&current, id, build.IsIOS)
	if err != nil {
		return nil, err
	}
	target, err := advancedTarget(&current, id)
	if err != nil {
		return nil, err
	}
	folder := target.(*config.FolderConfiguration)
	section.folderSyncBaseline = newFolderSettings(*folder)
	conf := config.New(protocol.EmptyDeviceID)
	conf.Defaults.Folder = folderDefaultsForType(folder.Type)
	defaults, err := newAdvancedSection(&conf, AdvancedSectionDefaultFolder, build.IsIOS)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, f := range defaults.fields {
		values[f.Path] = f.Value
	}
	resetFields := make([]*AdvancedField, 0, len(folderSyncResetFields))
	for _, f := range section.fields {
		if value, ok := values[f.Path]; ok && !f.IsReadOnly && IsFolderSyncResetField(f.Path) {
			f.Value = value
			resetFields = append(resetFields, f)
		}
	}
	section.fields = resetFields
	return section, nil
}
