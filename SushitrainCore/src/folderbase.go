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
	"strings"
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
	ErrNoManagedFolderBase   = errors.New("no managed folder base has been set")
	ErrFolderIsExternal      = errors.New("folder is not stored at its default location")
	ErrMoveAcrossVolumes     = errors.New("folder cannot be moved, as the destination is on another volume")
	ErrMoveTargetOccupied    = errors.New("folder cannot be moved, as the destination already exists")
	ErrUnsupportedFilesystem = errors.New("folder does not use a native filesystem")
	errUnsafeFolderID        = errors.New("folder ID cannot be used as a directory name")
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

// Whether a folder ID can name the folder's directory at a default location: a single path component that refers to no
// other directory. Syncthing accepts any non-empty folder ID, including IDs such as "../x" offered by other devices.
func isSafeFolderDirectoryName(folderID string) bool {
	return folderID != "" && folderID != "." && folderID != ".." && !strings.ContainsAny(folderID, "/\\\x00") &&
		folderID == path.Base(folderID)
}

// The location of the folder in the managed base, or an empty string when there is no managed base or the folder ID
// cannot be used as a directory name
func managedFolderPath(folderID string) string {
	base := ManagedFolderBase()
	if base == "" || !isSafeFolderDirectoryName(folderID) {
		return ""
	}
	return path.Join(base, folderID)
}

// The path at which a new folder with the given ID (and label, outside the container) is created when no path is
// specified
func (clt *Client) defaultFolderPath(folderID string, label string) (string, error) {
	if clt.folderLayout == homeFolders {
		return clt.defaultHomeFolderPath(folderID, label)
	}
	if !isSafeFolderDirectoryName(folderID) {
		return "", fmt.Errorf("%w: %q; choose a location for the folder", errUnsafeFolderID, folderID)
	}
	if managed := managedFolderPath(folderID); managed != "" {
		return managed, nil
	}
	return path.Join(clt.filesPath, folderID), nil
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

// Repairs the configuration of a folder after its container moved or a move into the managed base was interrupted.
// Called for every folder when the configuration is loaded. Returns true when the configuration was changed.
func recoverManagedFolder(fc *config.FolderConfiguration, filesPath string) bool {
	resumed := resumeAfterInterruptedMove(fc)
	return reanchorToManagedBase(fc, filesPath) || resumed
}

// When the managed base moved (e.g. the app group container path changed after a restore) or a move to the managed base
// was interrupted, point the folder at the managed location if the folder marker is no longer at the configured path
// but is present in the managed location. Only folders at a location the app controls (the files path or an earlier
// managed base) are re-anchored, never folders the user placed elsewhere. Returns true when the configuration was
// changed.
func reanchorToManagedBase(fc *config.FolderConfiguration, filesPath string) bool {
	managed := managedFolderPath(fc.ID)
	if managed == "" || fc.Path == managed {
		return false
	}
	if fc.Path != path.Join(filesPath, fc.ID) && !isEarlierManagedPath(fc.Path, managed) {
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

// Whether a path is where a folder was stored in the managed base before the container holding the base moved. Only the
// location of the container changes, so such a path ends in the same three components as the current managed path: the
// two directories below the container that hold the folders, and the folder ID.
func isEarlierManagedPath(folderPath string, managed string) bool {
	return lastPathComponents(folderPath, 3) == lastPathComponents(managed, 3)
}

func lastPathComponents(p string, count int) string {
	p = path.Clean(p)
	start := len(p)
	for range count {
		start = strings.LastIndexByte(p[:start], '/')
		if start < 0 {
			return p
		}
	}
	return p[start:]
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
// is never copied. Calling this for a folder that has already been moved is a no-op. Folders whose ID cannot be used as
// a directory name stay where they are.
func (fld *Folder) MoveToManagedBase() error {
	if !isSafeFolderDirectoryName(fld.FolderID) {
		return fmt.Errorf("%w: %q", errUnsafeFolderID, fld.FolderID)
	}
	managed := managedFolderPath(fld.FolderID)
	if managed == "" {
		return ErrNoManagedFolderBase
	}

	fc := fld.folderConfiguration()
	if fc == nil {
		return errFolderConfigNotFound
	}
	if !isNativeFilesystem(fc.FilesystemType) {
		return ErrUnsupportedFilesystem
	}
	if fc.Path == managed {
		return nil
	}

	legacy := path.Join(fld.client.filesPath, fld.FolderID)
	if fc.Path != legacy {
		return ErrFolderIsExternal
	}

	// An earlier move may have been interrupted after the rename but before the configuration was saved
	resumeMarker := resumeAfterMovePath(fld.FolderID)
	_, legacyErr := os.Lstat(legacy)
	if errors.Is(legacyErr, os.ErrNotExist) && markerExists(managed, fc.MarkerName) {
		slog.Info("folder was already moved to managed base, updating configuration", "folderID", fld.FolderID)
		_, resumeErr := os.Lstat(resumeMarker)
		if err := fld.changeFolderConfiguration(func(fc *config.FolderConfiguration) {
			fc.Path = managed
			fc.Paused = fc.Paused && resumeErr != nil
		}); err != nil {
			return err
		}
		removeResumeMarker(resumeMarker)
		return nil
	}

	if _, err := os.Lstat(managed); err == nil {
		return ErrMoveTargetOccupied
	}

	if err := os.MkdirAll(path.Dir(managed), 0o700); err != nil {
		return err
	}

	// Should the app stop before the move has completed, the marker makes the next launch resume the folder
	pausedBefore := fc.Paused
	if !pausedBefore {
		if err := os.WriteFile(resumeMarker, nil, 0o600); err != nil {
			return err
		}
		if err := fld.SetPaused(true); err != nil {
			removeResumeMarker(resumeMarker)
			return err
		}
	}

	slog.Info("moving folder to managed base", "folderID", fld.FolderID, "from", legacy, "to", managed)
	if err := os.Rename(legacy, managed); err != nil {
		if !pausedBefore {
			if resumeErr := fld.SetPaused(false); resumeErr != nil {
				slog.Warn("could not resume folder after failed move", "folderID", fld.FolderID, "cause", resumeErr)
			} else {
				removeResumeMarker(resumeMarker)
			}
		}
		if errors.Is(err, syscall.EXDEV) {
			return ErrMoveAcrossVolumes
		}
		return fmt.Errorf("could not move folder: %w", err)
	}

	if err := fld.changeFolderConfiguration(func(fc *config.FolderConfiguration) {
		fc.Path = managed
		fc.Paused = pausedBefore
	}); err != nil {
		return err
	}
	if !pausedBefore {
		removeResumeMarker(resumeMarker)
	}
	return nil
}

// The file that is present while a folder that was not paused is paused to move it into the managed base. The pause
// itself is saved (Syncthing saves every configuration change on its own), so the file records that it was temporary.
func resumeAfterMovePath(folderID string) string {
	managed := managedFolderPath(folderID)
	if managed == "" {
		return ""
	}
	return path.Join(path.Dir(managed), ".synctrain-resume-"+folderID)
}

func removeResumeMarker(resumeMarker string) {
	if err := os.Remove(resumeMarker); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("could not remove marker of folder move", "path", resumeMarker, "cause", err)
	}
}

// Resumes a folder that was paused for a move into the managed base that did not complete. Returns true when the
// configuration was changed.
func resumeAfterInterruptedMove(fc *config.FolderConfiguration) bool {
	resumeMarker := resumeAfterMovePath(fc.ID)
	if resumeMarker == "" {
		return false
	}
	if _, err := os.Lstat(resumeMarker); err != nil {
		return false
	}
	slog.Warn("folder was paused for a move that did not complete, resuming", "folderID", fc.ID)
	removeResumeMarker(resumeMarker)
	changed := fc.Paused
	fc.Paused = false
	return changed
}
