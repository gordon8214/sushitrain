// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"sync"
	"syscall"

	"github.com/syncthing/syncthing/lib/config"
)

// The managed folder base is a directory (typically inside a shared app group container) where internal folders are
// stored instead of the files path, so that other processes (such as a File Provider extension) can access them.
var (
	managedFolderBaseMutex sync.Mutex
	managedFolderBase      string
)

var (
	ErrNoManagedFolderBase  = errors.New("no managed folder base has been set")
	ErrFolderIsExternal     = errors.New("folder is not stored at its default location")
	ErrMoveAcrossVolumes    = errors.New("folder cannot be moved, as the destination is on another volume")
	ErrMoveTargetOccupied   = errors.New("folder cannot be moved, as the destination already exists")
	ErrUnsupportedFilesytem = errors.New("folder does not use a native filesystem")
)

// Sets the directory below which new internal folders are created. Must be called before Client.Load. An empty path
// restores the default behavior of creating folders in the files path.
func SetManagedFolderBase(basePath string) {
	managedFolderBaseMutex.Lock()
	defer managedFolderBaseMutex.Unlock()
	managedFolderBase = basePath
}

func ManagedFolderBase() string {
	managedFolderBaseMutex.Lock()
	defer managedFolderBaseMutex.Unlock()
	return managedFolderBase
}

func managedFolderPath(folderID string) string {
	base := ManagedFolderBase()
	if base == "" {
		return ""
	}
	return path.Join(base, folderID)
}

// The path at which a new folder with the given ID is created when no path is specified
func (clt *Client) defaultFolderPath(folderID string) string {
	if managed := managedFolderPath(folderID); managed != "" {
		return managed
	}
	return path.Join(clt.filesPath, folderID)
}

// Whether a folder with the given ID at the given path is stored at one of the app-controlled default locations
func (clt *Client) isInternalFolderPath(folderID string, folderPath string) bool {
	if folderPath == path.Join(clt.filesPath, folderID) {
		return true
	}
	managed := managedFolderPath(folderID)
	return managed != "" && folderPath == managed
}

func markerExists(folderPath string, markerName string) bool {
	_, err := os.Stat(path.Join(folderPath, markerName))
	return err == nil
}

// When the managed base moved (e.g. the app group container path changed after a restore), point the folder at the
// managed location if the folder marker is no longer at the configured path but is present in the managed location.
// Returns true when the configuration was changed.
func reanchorToManagedBase(fc *config.FolderConfiguration) bool {
	managed := managedFolderPath(fc.ID)
	if managed == "" || fc.Path == managed {
		return false
	}

	if _, err := os.Stat(path.Join(fc.Path, fc.MarkerName)); !errors.Is(err, os.ErrNotExist) {
		return false
	}

	if !markerExists(managed, fc.MarkerName) {
		return false
	}

	slog.Warn("folder marker found in managed location, re-anchoring", "folderID", fc.ID, "configured", fc.Path, "managed", managed)
	fc.Path = managed
	return true
}

// Whether the folder is stored in the managed folder base
func (fld *Folder) IsInManagedBase() bool {
	fc := fld.folderConfiguration()
	if fc == nil {
		return false
	}
	managed := managedFolderPath(fld.FolderID)
	return managed != "" && fc.Path == managed
}

// Moves an internal folder from the files path to the managed folder base, then points its configuration at the new
// location. The folder is paused while it is being moved. This is a cheap rename on the same volume; the folder data
// is never copied. Calling this for a folder that has already been moved is a no-op.
func (fld *Folder) MoveToManagedBase() error {
	managed := managedFolderPath(fld.FolderID)
	if managed == "" {
		return ErrNoManagedFolderBase
	}

	fc := fld.folderConfiguration()
	if fc == nil {
		return errFolderConfigNotFound
	}
	if !isNativeFilesystem(fc.FilesystemType) {
		return ErrUnsupportedFilesytem
	}
	if fc.Path == managed {
		return nil
	}

	legacy := path.Join(fld.client.filesPath, fld.FolderID)
	if fc.Path != legacy {
		return ErrFolderIsExternal
	}

	// An earlier move may have been interrupted after the rename but before the configuration was saved
	_, legacyErr := os.Lstat(legacy)
	if errors.Is(legacyErr, os.ErrNotExist) && markerExists(managed, fc.MarkerName) {
		slog.Info("folder was already moved to managed base, updating configuration", "folderID", fld.FolderID)
		return fld.SetPath(managed)
	}

	if _, err := os.Lstat(managed); err == nil {
		return ErrMoveTargetOccupied
	}

	if err := os.MkdirAll(path.Dir(managed), 0o700); err != nil {
		return err
	}

	pausedBefore := fc.Paused
	if !pausedBefore {
		if err := fld.SetPaused(true); err != nil {
			return err
		}
	}

	slog.Info("moving folder to managed base", "folderID", fld.FolderID, "from", legacy, "to", managed)
	if err := os.Rename(legacy, managed); err != nil {
		if !pausedBefore {
			if resumeErr := fld.SetPaused(false); resumeErr != nil {
				slog.Warn("could not resume folder after failed move", "folderID", fld.FolderID, "cause", resumeErr)
			}
		}
		if errors.Is(err, syscall.EXDEV) {
			return ErrMoveAcrossVolumes
		}
		return fmt.Errorf("could not move folder: %w", err)
	}

	return fld.changeFolderConfiguration(func(fc *config.FolderConfiguration) {
		fc.Path = managed
		fc.Paused = pausedBefore
	})
}
