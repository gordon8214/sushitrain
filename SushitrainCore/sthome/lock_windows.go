// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package sthome

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
)

// LockHome uses the same byte-range lock as Syncthing's gofrs/flock Windows implementation.
func LockHome(dir string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(dir, "syncthing.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrHomeLocked
		}
		return nil, err
	}
	return file, nil
}
