//go:build !darwin && !linux

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"errors"
	"os"
)

func desktopRenameExclusive(root *os.Root, from, to string) error {
	info, err := root.Lstat(from)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("unsupported: synchronized directory moves on this platform; use Files mode or the native file manager")
	}
	if err = root.Link(from, to); err != nil {
		return err
	}
	return root.Remove(from)
}
