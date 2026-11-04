//go:build !windows

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sharedfile

import "os"

func SyncDirectory(directory *os.File) error { return directory.Sync() }
