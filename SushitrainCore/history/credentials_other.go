//go:build !linux

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package history

import (
	"path/filepath"
	"t-shaped.nl/sushitrain/v2/files"
)

func credentialStore(state string) (files.CredentialStore, func() error, error) {
	return &files.FileCredentials{Path: filepath.Join(state, "credentials.json")}, func() error { return nil }, nil
}
