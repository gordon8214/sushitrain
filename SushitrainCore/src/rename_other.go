// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !darwin

package sushitrain

import (
	"errors"
	"os"
)

// renameExclusive renames from to to, failing (with an error that is os.ErrExist) when to exists. Elsewhere than on
// Apple's systems, a target that appears between the check and the rename is not caught.
func renameExclusive(from string, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(from, to)
}
