// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !darwin || !cgo

package sushitrain

import "errors"

// SetEnginePriority has an effect on macOS only
func SetEnginePriority(priority string) error {
	return errors.New("not supported on this platform")
}
