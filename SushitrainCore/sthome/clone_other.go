// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !darwin

package sthome

import "errors"

// cloneFile is not available here; files are copied instead
func cloneFile(_, _ string) error {
	return errors.ErrUnsupported
}
