// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !linux

package files

import (
	"errors"
	"os"
)

// reflink is for Linux filesystems; elsewhere content is copied
func reflink(dst, src *os.File) error {
	return errors.ErrUnsupported
}
