// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sthome

import "golang.org/x/sys/unix"

// cloneFile makes to a clone of from (APFS), which shares its blocks until either changes
func cloneFile(from, to string) error {
	return unix.Clonefile(from, to, unix.CLONE_NOFOLLOW)
}
