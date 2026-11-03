// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !linux

package main

import "errors"

// On macOS, Files in Finder is the File Provider extension of the Synctrain app
func serve([]string) error {
	return errors.New("the daemon mounts with FUSE, on Linux only")
}
