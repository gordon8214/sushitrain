// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/syncthing"
	"github.com/syncthing/syncthing/lib/versioner"
)

const (
	// A cleanup removing a version during a listing ends it, so one more try is worth it
	versionListingAttempts = 2
	// As the engine's temporary files, which scanning skips: an unfinished copy could otherwise reach other devices
	exportedVersionFilePattern = fs.UnixTempPrefix + "version-*.tmp"
)

var (
	errVersionNotListed = errors.New("this version is no longer kept")
	errExportOverFile   = errors.New(
		"that would replace a synchronized file without keeping it as a version: choose another name, or restore the version")
)

// FileVersion is an older version of a file
type FileVersion struct {
	Size        int64
	versionTime time.Time
	modTime     time.Time
}

// VersionTime is when the version was archived, which identifies it
func (v *FileVersion) VersionTime() *Date {
	return &Date{time: v.versionTime}
}

// ModTime is when the version was last modified
func (v *FileVersion) ModTime() *Date {
	return &Date{time: v.modTime}
}

// FileVersions is a list of versions, the newest first
type FileVersions struct {
	items []*FileVersion
}

func (l *FileVersions) Count() int {
	return len(l.items)
}

func (l *FileVersions) ItemAt(index int) *FileVersion {
	return l.items[index]
}

// CanBrowseVersions reports whether the folder's versions can be listed and restored right now. External versioning keeps
// them where the engine can't see them, a folder that doesn't run has no versioner, and a receive-encrypted folder only
// has encrypted names.
func (fld *Folder) CanBrowseVersions() bool {
	folder := fld.folderConfiguration()
	if folder == nil || folder.Paused || folder.Type == config.FolderTypeReceiveEncrypted ||
		!isNativeFilesystem(folder.FilesystemType) {
		return false
	}
	switch folder.Versioning.Type {
	case VersioningTypeSimple, VersioningTypeTrashcan, VersioningTypeStaggered:
		return true
	}
	return false
}

func (fld *Folder) internalVersions(path string) ([]syncthing.FileVersion, error) {
	internals := fld.client.internals()
	if internals == nil {
		return nil, ErrStillLoading
	}
	return internals.FileVersions(fld.FolderID, path)
}

// FileVersions are the older versions of a file. Files are found by their path, whether or not they still exist.
func (fld *Folder) FileVersions(path string) (*FileVersions, error) {
	versions, err := fld.internalVersions(path)
	if err != nil {
		return nil, err
	}
	items := make([]*FileVersion, 0, len(versions))
	for _, version := range versions {
		items = append(items, &FileVersion{Size: version.Size, versionTime: version.VersionTime, modTime: version.ModTime})
	}
	return &FileVersions{items: items}, nil
}

// ExportFileVersion writes a copy of a version's data to a file, which it replaces. Metadata stays behind, and the version
// stays where it is, so this works for what restoring refuses.
func (fld *Folder) ExportFileVersion(path string, version *FileVersion, destination string) (err error) {
	internals := fld.client.internals()
	wrapper := fld.client.cfg()
	if internals == nil || wrapper == nil {
		return ErrStillLoading
	}
	if _, err := os.Lstat(destination); err == nil && isInSynchronizedFolder(destination, wrapper.Folders()) {
		return errExportOverFile
	}
	source, err := internals.OpenFileVersion(fld.FolderID, path, version.versionTime)
	if err != nil {
		return err
	}
	defer source.Close()

	// What is there stays until the whole version arrived
	target, err := os.CreateTemp(filepath.Dir(destination), exportedVersionFilePattern)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = target.Close()
			_ = os.Remove(target.Name())
		}
	}()
	if _, err = io.Copy(target, source); err != nil {
		return err
	}
	if err = target.Sync(); err != nil {
		return err
	}
	if err = target.Close(); err != nil {
		return err
	}
	if err = os.Chtimes(target.Name(), version.modTime, version.modTime); err != nil {
		return err
	}
	if err = os.Chmod(target.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(target.Name(), destination)
}

// RestoreFileVersion puts a version back, as the web GUI does: a file in the way goes to the versions first
func (fld *Folder) RestoreFileVersion(path string, version *FileVersion) error {
	failed, err := fld.restoreVersions(map[string]time.Time{path: version.versionTime})
	if err != nil {
		return err
	}
	if failed.Count() > 0 {
		return errors.New(failed.ItemAt(0).Error)
	}
	return nil
}

func (fld *Folder) restoreVersions(versions map[string]time.Time) (*FailedItems, error) {
	internals := fld.client.internals()
	if internals == nil {
		return nil, ErrStillLoading
	}
	// Whatever fails from here on, nothing happened yet
	if _, err := internals.FolderErrors(fld.FolderID); err != nil {
		return nil, err
	}
	failures, err := internals.RestoreFolderVersions(fld.FolderID, versions)
	if err != nil {
		if errors.Is(err, syncthing.ErrNoVersioner) {
			return nil, err
		}
		// A folder with platform history scans in the same step, so the files may well be back already
		return nil, fmt.Errorf("the files may be restored, but the folder could not finish: %w", err)
	}
	items := make([]*FailedItem, 0, len(failures))
	for path, failure := range failures {
		items = append(items, &FailedItem{Path: path, Error: failure.Error()})
	}
	return newFailedItems(items), nil
}

// VersionedFile is a file that no longer exists, of which versions are kept
type VersionedFile struct {
	Path string
	// How many versions are kept
	Versions int
	// The size of the newest version
	Size   int64
	latest time.Time
}

// Latest is when the newest version was archived, which needn't be when the file was deleted
func (f *VersionedFile) Latest() *Date {
	return &Date{time: f.latest}
}

// VersionedFiles is a list of files, by path
type VersionedFiles struct {
	items []*VersionedFile
}

func (l *VersionedFiles) Count() int {
	return len(l.items)
}

func (l *VersionedFiles) ItemAt(index int) *VersionedFile {
	return l.items[index]
}

// DeletedFiles are the files that no longer exist, of which versions are kept. This walks all of the folder's versions.
func (fld *Folder) DeletedFiles() (*VersionedFiles, error) {
	folder := fld.folderConfiguration()
	if folder == nil {
		return nil, errFolderConfigNotFound
	}
	versions, err := fld.allVersions()
	if err != nil {
		return nil, err
	}
	ffs := folder.Filesystem()
	items := make([]*VersionedFile, 0)
	for path, fileVersions := range versions {
		if len(fileVersions) == 0 || versioner.IsPlatformHistoryPath(path) {
			continue
		}
		if _, err := ffs.Lstat(path); !fs.IsNotExist(err) {
			continue
		}
		newest := newestVersion(fileVersions)
		items = append(items, &VersionedFile{Path: path, Versions: len(fileVersions), Size: newest.Size,
			latest: newest.VersionTime})
	}
	slices.SortFunc(items, func(a, b *VersionedFile) int { return cmp.Compare(a.Path, b.Path) })
	return &VersionedFiles{items: items}, nil
}

func (fld *Folder) allVersions() (map[string][]syncthing.FileVersion, error) {
	internals := fld.client.internals()
	if internals == nil {
		return nil, ErrStillLoading
	}
	var versions map[string][]syncthing.FileVersion
	var err error
	for range versionListingAttempts {
		if versions, err = internals.FolderVersions(fld.FolderID); !errors.Is(err, iofs.ErrNotExist) {
			break
		}
	}
	return versions, err
}

// RestoreLatestVersions puts the newest version of each file back, and says which could not be
func (fld *Folder) RestoreLatestVersions(paths *ListOfStrings) (*FailedItems, error) {
	versions, err := fld.allVersions()
	if err != nil {
		return nil, err
	}
	latest := make(map[string]time.Time, paths.Count())
	var failed []*FailedItem
	for _, path := range paths.data {
		fileVersions := versions[path]
		if len(fileVersions) == 0 {
			failed = append(failed, &FailedItem{Path: path, Error: errVersionNotListed.Error()})
			continue
		}
		latest[path] = newestVersion(fileVersions).VersionTime
	}
	if len(latest) == 0 {
		return newFailedItems(failed), nil
	}
	result, err := fld.restoreVersions(latest)
	if err != nil {
		return nil, err
	}
	return newFailedItems(append(failed, result.items...)), nil
}

func newestVersion(versions []syncthing.FileVersion) syncthing.FileVersion {
	return slices.MaxFunc(versions, func(a, b syncthing.FileVersion) int {
		return a.VersionTime.Compare(b.VersionTime)
	})
}
