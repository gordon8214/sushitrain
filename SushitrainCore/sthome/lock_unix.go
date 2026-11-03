// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !windows

package sthome

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// LockHome takes the lock Syncthing takes on its home directory (a flock on syncthing.lock), so that no Syncthing starts
// on it while the returned file stays open; closing it releases the lock. ErrHomeLocked when a Syncthing runs on it.
func LockHome(dir string) (*os.File, error) {
	lockPath := filepath.Join(dir, "syncthing.lock")
	file, err := os.OpenFile(lockPath, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrHomeLocked
		}
		return nil, err
	}
	// A Syncthing quitting removes its lock file after unlocking it, and one starting then locks a new file: the lock
	// held must be the file's
	held, heldErr := file.Stat()
	current, currentErr := os.Stat(lockPath)
	if heldErr != nil || currentErr != nil || !os.SameFile(held, current) {
		file.Close()
		return nil, ErrHomeLocked
	}
	return file, nil
}
