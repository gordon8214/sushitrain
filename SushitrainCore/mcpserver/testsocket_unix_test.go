//go:build !windows

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package mcpserver

import (
	"path/filepath"
	"testing"
)

func testSocket(_ *testing.T, dir string) string { return filepath.Join(dir, "host.sock") }
