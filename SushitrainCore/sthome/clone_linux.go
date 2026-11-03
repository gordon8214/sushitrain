// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sthome

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile makes to a reflink of from (FICLONE, on btrfs and XFS), which shares its blocks until either changes.
// Where the volume can't, to is left absent and the error returned, so that the file is copied instead.
func cloneFile(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = unix.IoctlFileClone(int(target.Fd()), int(source.Fd()))
	if closeErr := target.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(to)
	}
	return err
}
