//go:build !linux

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package safecopy

import (
	"errors"
	"os"
	"path/filepath"
)

func directoryPath(_ *os.File, path string) (string, error) { return filepath.EvalSymlinks(path) }

func publish(root *os.Root, from, to string, file *os.File) error {
	actual, err := root.Lstat(from)
	if err != nil {
		return err
	}
	held, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(actual, held) {
		return errors.New("the export temporary file changed")
	}
	if err := root.Link(from, to); err != nil {
		return err
	}
	return root.Remove(from)
}
