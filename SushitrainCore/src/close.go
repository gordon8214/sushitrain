// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"errors"
	"runtime/debug"
)

// Close permanently closes a discarded client and releases its configuration
// ownership. Stop retains ownership for app shutdown and recovery; a host that
// replaces a client during a mode change must use Close instead.
func (clt *Client) Close() error {
	clt.lifecycle.Lock()
	defer clt.lifecycle.Unlock()
	clt.stopRequested = true
	if r := clt.run.Load(); r != nil && !clt.teardown(r) {
		return errors.New("Syncthing has not stopped; its configuration lock was retained")
	}
	if clt.Server != nil {
		if err := clt.Server.Close(); err != nil {
			return err
		}
		clt.Server = nil
	}
	if clt.crashLog != "" {
		if err := debug.SetCrashOutput(nil, debug.CrashOptions{}); err != nil {
			return err
		}
		clt.crashLog = ""
	}
	clt.lockMutex.Lock()
	defer clt.lockMutex.Unlock()
	if clt.appLock != nil {
		if err := clt.appLock.Unlock(); err != nil {
			return err
		}
		clt.appLock = nil
		clt.lockedPath = ""
	}
	if clt.syncthingHomeLock != nil {
		if err := clt.syncthingHomeLock.Close(); err != nil {
			return err
		}
		clt.syncthingHomeLock = nil
		clt.syncthingHomeLockPath = ""
	}
	clt.setStatus(EngineStateStopped, "", true)
	return nil
}
