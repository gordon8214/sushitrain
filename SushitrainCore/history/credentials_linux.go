// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package history

import (
	"path/filepath"
	"t-shaped.nl/sushitrain/v2/files"
)

func credentialStore(state string) (files.CredentialStore, func() error, error) {
	file := &files.FileCredentials{Path: filepath.Join(state, "credentials.json")}
	keyring, err := files.NewSecretService("synctrain-history")
	if err != nil {
		return file, func() error { return nil }, nil
	}
	if _, err = files.MoveCredentials(file, keyring); err != nil {
		keyring.Close()
		return nil, nil, err
	}
	return keyring, keyring.Close, nil
}
