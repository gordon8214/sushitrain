// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sushitrain

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/locations"
)

// A remote-managed folder is accessed through a gateway's remote file API (Docs/remote-file-provider-plan.md):
// this device's engine must not scan, pull or announce changes for it, or both it and the gateway would publish the
// same edits. The folder stays in the configuration, paused, and the flag is kept separately from the user's pause
// preference: no change of the configuration may resume the folder while it is remote-managed, whichever code path
// (Shortcuts, folder settings, a moved folder's resume marker) tries.

const remoteManagedFileName = "remote-managed.json"

var errRemoteManaged = errors.New("the folder is accessed through a remote file service; switch it back to Syncthing first")

type remoteManagedSet struct {
	mutex   sync.Mutex
	path    string
	folders []string
}

type remoteManagedFile struct {
	Folders []string `json:"folders"`
}

// loadRemoteManaged reads the flags. The fence fails closed: a file that can't be read or parsed is an error (the
// client doesn't start), rather than an empty set that would let the folders be resumed. A file that can't be parsed
// is kept next to it with the suffix .corrupt.
func loadRemoteManaged(path string) (*remoteManagedSet, error) {
	set := &remoteManagedSet{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return set, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the folders accessed through a remote file service: %w", err)
	}
	var f remoteManagedFile
	if err := json.Unmarshal(data, &f); err != nil {
		// The folders stay paused in the configuration meanwhile, and the app sets the list up again from the folders
		// its index binds to a gateway when it starts
		kept := path + ".corrupt"
		if renameErr := os.Rename(path, kept); renameErr != nil {
			return nil, fmt.Errorf("the list of folders accessed through a remote file service is damaged (%w), and "+
				"could not be set aside: %w", err, renameErr)
		}
		slog.Error("The list of folders accessed through a remote file service was damaged and was set aside; "+
			"the app restores it from its gateway bindings", "path", kept, "cause", err)
		return set, nil
	}
	set.folders = f.Folders
	return set, nil
}

func (s *remoteManagedSet) contains(folderID string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return slices.Contains(s.folders, folderID)
}

func (s *remoteManagedSet) list() []string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return slices.Clone(s.folders)
}

func (s *remoteManagedSet) set(folderID string, managed bool) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	folders := slices.DeleteFunc(slices.Clone(s.folders), func(id string) bool { return id == folderID })
	if managed {
		folders = append(folders, folderID)
	}
	slices.Sort(folders)
	if slices.Equal(folders, s.folders) {
		return nil
	}
	data, err := json.Marshal(remoteManagedFile{Folders: folders})
	if err != nil {
		return err
	}
	if err := writeFileSynced(s.path, data); err != nil {
		return err
	}
	s.folders = folders
	return nil
}

// writeFileSynced replaces a file with data through a temporary file that is flushed before it takes the file's place
func writeFileSynced(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// enforce pauses every remote-managed folder in a configuration being changed
func (s *remoteManagedSet) enforce(conf *config.Configuration) {
	for _, folder := range conf.Folders {
		if s.contains(folder.ID) && !folder.Paused {
			folder.Paused = true
			conf.SetFolder(folder)
		}
	}
}

// VerifyConfiguration rejects a configuration change that resumes a remote-managed folder, or adds one unpaused.
// Other changes pass, also while a flagged folder is not paused yet.
func (s *remoteManagedSet) VerifyConfiguration(from, to config.Configuration) error {
	before := from.FolderMap()
	for _, folder := range to.Folders {
		if folder.Paused || !s.contains(folder.ID) {
			continue
		}
		if previous, ok := before[folder.ID]; ok && !previous.Paused {
			continue
		}
		return fmt.Errorf("%s: %w", folder.ID, errRemoteManaged)
	}
	return nil
}

// CommitConfiguration forgets the flag of a folder that is removed from the configuration
func (s *remoteManagedSet) CommitConfiguration(from, to config.Configuration) bool {
	after := to.FolderMap()
	for _, folder := range from.Folders {
		if _, kept := after[folder.ID]; kept || !s.contains(folder.ID) {
			continue
		}
		if err := s.set(folder.ID, false); err != nil {
			slog.Warn("Could not forget a removed folder accessed through a remote file service", "folder", folder.ID,
				"cause", err)
		}
	}
	return true
}

func (s *remoteManagedSet) String() string {
	return "remote-managed folders"
}

func remoteManagedPath() string {
	return filepath.Join(locations.GetBaseDir(locations.ConfigBaseDir), remoteManagedFileName)
}

// SetFolderRemoteManaged marks a folder as accessed through a remote file service, and pauses it, or removes the
// mark. Removing it does not resume the folder: switching back to Syncthing is a separate, explicit step.
func (clt *Client) SetFolderRemoteManaged(folderID string, managed bool) error {
	if clt.remoteManaged == nil {
		return ErrStillLoading
	}
	if managed {
		wasManaged := clt.remoteManaged.contains(folderID)
		if err := clt.remoteManaged.set(folderID, true); err != nil {
			return err
		}
		err := clt.changeConfiguration(func(conf *config.Configuration) {
			clt.remoteManaged.enforce(conf)
		})
		if err != nil && !wasManaged {
			// The folder may not be paused: don't keep a flag the caller is told was not set
			if rollbackErr := clt.remoteManaged.set(folderID, false); rollbackErr != nil {
				slog.Warn("Could not undo marking a folder as accessed through a remote file service",
					"folder", folderID, "cause", rollbackErr)
			}
		}
		return err
	}
	return clt.remoteManaged.set(folderID, false)
}

func (clt *Client) IsFolderRemoteManaged(folderID string) bool {
	return clt.remoteManaged != nil && clt.remoteManaged.contains(folderID)
}

func (clt *Client) RemoteManagedFolderIDs() *ListOfStrings {
	if clt.remoteManaged == nil {
		return List([]string{})
	}
	return List(clt.remoteManaged.list())
}
