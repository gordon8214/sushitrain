// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Names on the mount are composed (NFC), as the gateway sends them, and compared exactly: Linux filesystems are
// case-sensitive, and so is a gateway on Linux.

// normalizedName is a name as stored and looked up
func normalizedName(name string) string {
	return norm.NFC.String(name)
}

// validName reports whether a name can be in the mount and sent to a gateway: one path component of valid UTF-8,
// at most 255 bytes
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && utf8.ValidString(name) &&
		!strings.ContainsAny(name, "/\x00") && strings.TrimSpace(name) != ""
}

// reservedName reports whether a name belongs to Syncthing (the gateway's IsSyncthingPrivatePath): such items are
// never shown, and can't be created
func reservedName(name string) bool {
	lower := strings.ToLower(name)
	return lower == ".stfolder" || lower == ".stignore" || lower == ".stversions" ||
		strings.HasPrefix(lower, ".syncthing.") || strings.HasPrefix(lower, "~syncthing~")
}

// shownName reports whether an item of the gateway with this name is shown in the mount
func shownName(name string) bool {
	return validName(name) && !reservedName(name)
}
