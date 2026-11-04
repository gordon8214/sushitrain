// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package history

import (
	"os"
	"path/filepath"
	"t-shaped.nl/sushitrain/v2/adminsock"
)

func RegisterDesktop(a *adminsock.Server, forbidden ...string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	state := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(state) {
		state = filepath.Join(home, ".local", "state")
	}
	state = filepath.Join(state, "synctrain", "history")
	if override := os.Getenv("SYNCTRAIN_HISTORY_STATE"); filepath.IsAbs(override) {
		state = override
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	store, err := Open(state, filepath.Join(cache, "synctrain", "history"), forbidden...)
	if err != nil {
		return err
	}
	store.Register(a)
	a.OnClose(store.Close)
	return nil
}
