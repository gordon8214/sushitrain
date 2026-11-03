// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// CredentialStore keeps the credentials of paired gateways, by service ID
type CredentialStore interface {
	// Credential returns the credential, or "" when there is none (the gateway must be paired again). An error means
	// it can't be read just now, such as a locked keyring, which is worth trying again later.
	Credential(serviceID string) (string, error)
	SetCredential(serviceID, credential string) error
	DeleteCredential(serviceID string) error
}

// FileCredentials keeps credentials in a JSON file only its owner can read, for systems without a keyring
type FileCredentials struct {
	Path  string
	mutex sync.Mutex
}

func (f *FileCredentials) load() (map[string]string, error) {
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	credentials := map[string]string{}
	return credentials, json.Unmarshal(data, &credentials)
}

func (f *FileCredentials) save(credentials map[string]string) error {
	data, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(f.Path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), f.Path)
}

func (f *FileCredentials) Credential(serviceID string) (string, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	credentials, err := f.load()
	return credentials[serviceID], err
}

func (f *FileCredentials) SetCredential(serviceID, credential string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	credentials, err := f.load()
	if err != nil {
		return err
	}
	credentials[serviceID] = credential
	return f.save(credentials)
}

func (f *FileCredentials) DeleteCredential(serviceID string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	credentials, err := f.load()
	if err != nil {
		return err
	}
	delete(credentials, serviceID)
	return f.save(credentials)
}
