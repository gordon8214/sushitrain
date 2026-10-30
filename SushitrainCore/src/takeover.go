// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"encoding/json"
	"errors"
	"path"
	"path/filepath"

	"t-shaped.nl/sushitrain/v2/sthome"
)

// What the Mac app needs to take over Syncthing.app's setup (its backend's home directory), before its own engine
// starts. These change nothing there but its lock file, except KeepGUISetting, which marks a home used directly.

// SummarizeSyncthingHome describes a Syncthing home directory (as JSON, sthome.Summary): its device ID, folders,
// devices, web GUI, database schema, and whether a Syncthing runs on it
func SummarizeSyncthingHome(dir string) ([]byte, error) {
	summary, err := sthome.Summarize(dir)
	if err != nil {
		return nil, err
	}
	return json.Marshal(summary)
}

// IsSyncthingHomeLocked reports whether a Syncthing runs on a home directory (holds its lock)
func IsSyncthingHomeLocked(dir string) bool {
	return sthome.LockHeld(dir)
}

// IsAddressInUse reports whether something listens on a TCP address, such as another Syncthing's port
func IsAddressInUse(address string) bool {
	return sthome.AddressInUse(address)
}

// Why ImportSyncthingHome could not copy a Syncthing home directory (its first result), when the user can do something
// about it
const (
	ImportProblemLocked   = "locked"
	ImportProblemInUse    = "in-use"
	ImportProblemTooNew   = "too-new"
	ImportProblemIdentity = "identity"
)

// ImportSyncthingHome copies a Syncthing home directory's configuration, identity and database into dst (this app's
// configuration directory, or a backup's), which must not exist or be empty. Fails while a Syncthing runs on it, and
// for a home without a usable device identity or from a newer Syncthing; the first result then says which.
func ImportSyncthingHome(src string, dst string) (string, error) {
	err := sthome.Import(src, dst)
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, sthome.ErrHomeLocked):
		return ImportProblemLocked, err
	case errors.Is(err, sthome.ErrHomeInUse), errors.Is(err, sthome.ErrHomeNested):
		return ImportProblemInUse, err
	case errors.Is(err, sthome.ErrHomeTooNew):
		return ImportProblemTooNew, err
	case errors.Is(err, sthome.ErrIdentity):
		return ImportProblemIdentity, err
	}
	return "", err
}

// HoldSyncthingHome keeps the lock on Syncthing's home directory for as long as the app runs, so that Syncthing.app
// can't start on it (with the device identity of the copy the app imported). Calling it again does nothing.
func (clt *Client) HoldSyncthingHome(dir string) error {
	clt.lockMutex.Lock()
	defer clt.lockMutex.Unlock()
	if clt.syncthingHomeLock != nil {
		return nil
	}
	lock, err := sthome.LockHome(dir)
	if err != nil {
		return err
	}
	clt.syncthingHomeLock = lock
	clt.syncthingHomeLockPath = path.Clean(dir)
	return nil
}

// HoldsLockOn reports whether this app holds the lock on a directory: its own configuration directory's, or
// Syncthing's home it holds (HoldSyncthingHome); a lock it holds is no other Syncthing running there
func (clt *Client) HoldsLockOn(dir string) bool {
	clt.lockMutex.Lock()
	defer clt.lockMutex.Unlock()
	dir = path.Clean(dir)
	return (clt.appLock != nil && path.Dir(clt.lockedPath) == dir) ||
		(clt.syncthingHomeLock != nil && clt.syncthingHomeLockPath == dir)
}

// RequestSyncthingShutdown asks the Syncthing running on a home directory to quit, through its REST API on this
// computer; wait for IsSyncthingHomeLocked to turn false
func RequestSyncthingShutdown(dir string) error {
	return sthome.RequestShutdown(dir)
}

// KeepGUISetting records that a configuration's web GUI setting is its own (as for one taken over from Syncthing), so
// that the engine doesn't turn the GUI on for it once, as it does for configurations earlier versions of the app made
func KeepGUISetting(configDir string) error {
	return writeFileSynced(filepath.Join(configDir, guiEnabledMarkerName), nil)
}
