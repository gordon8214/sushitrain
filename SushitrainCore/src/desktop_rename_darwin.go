// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sushitrain

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

func desktopRenameExclusive(root *os.Root, from, to string) error {
	a, err := root.Open(filepath.Dir(from))
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := root.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer b.Close()
	return unix.RenameatxNp(int(a.Fd()), filepath.Base(from), int(b.Fd()), filepath.Base(to), unix.RENAME_EXCL)
}
