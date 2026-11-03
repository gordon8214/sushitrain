// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

//go:build !windows

package files

import "os"

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
