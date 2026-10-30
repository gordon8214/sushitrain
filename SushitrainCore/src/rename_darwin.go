// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import "golang.org/x/sys/unix"

// renameExclusive renames from to to, failing (with an error that is os.ErrExist) when to exists
func renameExclusive(from string, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
