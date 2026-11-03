// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

//go:build !windows

// Package sharedfile opens content files that may be renamed while readers retain their original version.
package sharedfile

import "os"

func Open(name string) (*os.File, error) { return os.Open(name) }
func OpenFile(name string, flag int, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, mode)
}
