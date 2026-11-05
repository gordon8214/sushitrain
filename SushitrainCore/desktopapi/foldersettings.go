// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package desktopapi

import "strings"

// Folder editors expose everyday controls first. New tuning fields stay behind Advanced.
// Raw expert configuration and unrelated editors retain their existing presentation.
func FolderFieldAdvanced(section, field string) bool {
	section = strings.TrimPrefix(section, "reset/")
	if section != "defaults/folder" && !strings.HasPrefix(section, "folder/") && !strings.HasPrefix(section, "basic/folder/") {
		return false
	}
	switch strings.ToLower(field) {
	case "id", "label", "group", "path", "type", "rescanintervals", "fswatcherenabled", "mindiskfree", "mindiskfreeunit", "mindiskfree.value", "mindiskfree.unit",
		"syncxattrs", "sendxattrs", "syncownership", "sendownership", "copyownershipfromparent", "ignoreperms":
		return false
	default:
		return true
	}
}

// FolderResetDescription explains the pending reset shared by desktop previews.
const FolderResetDescription = "Restore built-in synchronization defaults. Folder location, direction, sharing, ignore patterns, and versioning stay unchanged. Review the settings, then choose Save."
