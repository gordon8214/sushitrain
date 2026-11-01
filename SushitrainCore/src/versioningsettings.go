// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/fs"
)

const (
	versioningParamKeep         = "keep"
	versioningParamCleanoutDays = "cleanoutDays"
	versioningParamMaxAge       = "maxAge"
	versioningParamCommand      = "command"
	versioningParamPlatformData = "preservePlatformData"

	secondsPerDay    = 24 * 60 * 60
	secondsPerMinute = 60
	// The most days the engine can count: beyond this, age in days doesn't fit its durations
	maxDurationDays = int(math.MaxInt64 / int64(24*time.Hour))
	// What a folder shows until versioning is turned on, as the web GUI does, and what turning it on saves
	defaultVersioningKeep           = 5
	defaultVersioningCleanoutDays   = 0
	defaultVersioningMaxAgeDays     = 365
	defaultVersioningCleanupMinutes = 60
	maxVersioningCleanupMinutes     = config.MaxRescanIntervalS / secondsPerMinute
	maxVersioningMaxAgeDays         = math.MaxInt32 / secondsPerDay
	maxVersioningKeep               = math.MaxInt32
	maxVersioningCleanoutDays       = maxDurationDays
)

var (
	errVersioningType       = errors.New("choose one of the types of versioning")
	errVersioningCommand    = errors.New("enter the command that keeps the old versions")
	errVersionsPathInFolder = errors.New(
		"the versions path can't be in a synchronized folder: versions would go to other devices, and cleaning up removes files")
	errVersionsPathAroundFolder = errors.New(
		"the versions path can't contain a synchronized folder: cleaning up removes files and empty directories in it")
	errVersionsPathShared = errors.New(
		"another folder keeps its versions there: files of the same name would replace each other, and cleaning up counts both")
	errDefaultVersionsPath = errors.New(
		"new folders can't have a versions path: they would all keep their versions in the same place")
)

// VersioningSettings are how a folder keeps old versions of files. Numbers are text, as they were entered, for the save
// to check. Each type uses some of them, and all have a value at all times, so that trying another type shows sensible
// values and turning it on saves what is shown.
type VersioningSettings struct {
	// One of the VersioningType constants
	Type string
	// How many versions of a file Simple keeps
	Keep string
	// Days after which Simple and Trash can remove a version; 0 keeps them
	CleanoutDays string
	// Days after which Staggered removes a version; 0 keeps them
	MaxAgeDays             string
	CleanupIntervalMinutes string
	// What External runs to archive a file
	Command string
	// Where versions go, empty for .stversions in the folder
	Path string
	// Whether Staggered also keeps the platform history of a file. Only when versions are in the default place.
	KeepPlatformHistory bool

	// The settings as they were read, which tell what was edited
	saved *VersioningSettings
}

func newVersioningSettings(versioning config.VersioningConfiguration) *VersioningSettings {
	settings := &VersioningSettings{
		Type:                   versioning.Type,
		Keep:                   strconv.Itoa(defaultVersioningKeep),
		CleanoutDays:           strconv.Itoa(defaultVersioningCleanoutDays),
		MaxAgeDays:             strconv.Itoa(defaultVersioningMaxAgeDays),
		CleanupIntervalMinutes: strconv.Itoa(defaultVersioningCleanupMinutes),
		Command:                versioning.Params[versioningParamCommand],
		Path:                   versioning.FSPath,
		KeepPlatformHistory:    versioning.Params[versioningParamPlatformData] == "true",
	}
	if versioning.Type != VersioningTypeNone {
		if keep, ok := versioning.Params[versioningParamKeep]; ok {
			settings.Keep = keep
		}
		if days, ok := versioning.Params[versioningParamCleanoutDays]; ok {
			settings.CleanoutDays = days
		}
		if maxAge, err := strconv.Atoi(versioning.Params[versioningParamMaxAge]); err == nil {
			settings.MaxAgeDays = strconv.Itoa(wholeUnits(maxAge, secondsPerDay))
		}
		settings.CleanupIntervalMinutes = strconv.Itoa(wholeUnits(versioning.CleanupIntervalS, secondsPerMinute))
	}
	read := *settings
	settings.saved = &read
	return settings
}

// wholeUnits rounds down, but never turns a value into 0, which means something else
func wholeUnits(value int, unit int) int {
	if value <= 0 {
		return 0
	}
	return max(1, value/unit)
}

// Equal reports whether both hold the same settings
func (s *VersioningSettings) Equal(other *VersioningSettings) bool {
	first, second := *s, *other
	first.saved, second.saved = nil, nil
	return first == second
}

func (s *VersioningSettings) isEdited() bool {
	return s.saved != nil && !s.Equal(s.saved)
}

// HasCustomPath reports whether versions go somewhere else than the folder's .stversions, which rules out platform history
func (s *VersioningSettings) HasCustomPath() bool {
	return strings.TrimSpace(s.Path) != ""
}

// LosesPlatformHistory reports whether saving ends a platform history that is being kept. What was kept already stays on
// disk, but nothing protects it from cleaning up anymore, so the user should know.
func (s *VersioningSettings) LosesPlatformHistory() bool {
	return s.saved != nil && s.saved.KeepPlatformHistory &&
		(!s.KeepPlatformHistory || s.Type != VersioningTypeStaggered || s.HasCustomPath())
}

// applyTo checks what was edited and puts it in versioning, which is a copy to throw away if this fails. Unlike other
// settings, a new type takes all its values as shown: they were not read from the configuration, so they would otherwise
// not count as edited, and the versioner would miss them.
func (s *VersioningSettings) applyTo(versioning *config.VersioningConfiguration) error {
	saved := s.saved
	if s.Type == VersioningTypeNone && saved.Type != VersioningTypeNone {
		versioning.Reset()
		versioning.FSType = config.FilesystemTypeBasic
		return nil
	}
	if s.Type == VersioningTypeNone && !s.isEdited() {
		return nil
	}

	newType := s.Type != saved.Type
	// Without a new type, this is about the type the configuration has by now, which something else may have changed
	versioningType := versioning.Type
	if newType {
		versioningType = s.Type
	}
	params := maps.Clone(versioning.Params)
	if params == nil {
		params = make(map[string]string)
	}
	if newType {
		delete(params, versioningParamKeep)
		delete(params, versioningParamCleanoutDays)
		delete(params, versioningParamMaxAge)
		delete(params, versioningParamCommand)
	}
	switch versioningType {
	case VersioningTypeSimple:
		if err := errors.Join(
			setNumberParam("versions to keep", s.Keep, saved.Keep, newType, params, versioningParamKeep, 1, maxVersioningKeep, 1),
			setNumberParam("maximum age", s.CleanoutDays, saved.CleanoutDays, newType, params, versioningParamCleanoutDays,
				0, maxVersioningCleanoutDays, 1),
		); err != nil {
			return err
		}
	case VersioningTypeTrashcan:
		if err := setNumberParam("maximum age", s.CleanoutDays, saved.CleanoutDays, newType, params,
			versioningParamCleanoutDays, 0, maxVersioningCleanoutDays, 1); err != nil {
			return err
		}
	case VersioningTypeStaggered:
		if err := setNumberParam("maximum age", s.MaxAgeDays, saved.MaxAgeDays, newType, params, versioningParamMaxAge,
			0, maxVersioningMaxAgeDays, secondsPerDay); err != nil {
			return err
		}
	case VersioningTypeExternal:
		if newType || s.Command != saved.Command {
			command := strings.TrimSpace(s.Command)
			if command == "" {
				return errVersioningCommand
			}
			params[versioningParamCommand] = command
		}
	default:
		// A type from a later version of the engine stays as it is, with whatever it has
		if newType {
			return errVersioningType
		}
	}
	if versioningType != VersioningTypeExternal && (newType || s.CleanupIntervalMinutes != saved.CleanupIntervalMinutes) {
		minutes, err := parseNumberOption("cleanup interval", s.CleanupIntervalMinutes, 0, maxVersioningCleanupMinutes)
		if err != nil {
			return err
		}
		versioning.CleanupIntervalS = minutes * secondsPerMinute
	}
	if s.Path != saved.Path {
		versioning.FSPath = strings.TrimSpace(s.Path)
	}

	// The versioner refuses to start with platform history for anything but staggered in the default place
	if s.KeepPlatformHistory != saved.KeepPlatformHistory {
		delete(params, versioningParamPlatformData)
		if s.KeepPlatformHistory {
			params[versioningParamPlatformData] = "true"
		}
	}
	if versioningType != VersioningTypeStaggered || versioning.FSPath != "" {
		delete(params, versioningParamPlatformData)
	}

	versioning.Type = versioningType
	versioning.Params = params
	// Without this, a path is used as it is: no ~, and not relative to the folder
	versioning.FSType = config.FilesystemTypeBasic
	return nil
}

// setNumberParam puts a number in a parameter, multiplied by what it counts in the configuration, if it is edited or
// has to be written
func setNumberParam(label string, text string, saved string, write bool, params map[string]string, name string,
	minimum int, maximum int, multiplier int,
) error {
	if text == saved && !write {
		return nil
	}
	number, err := parseNumberOption(label, text, minimum, maximum)
	if err != nil {
		return err
	}
	params[name] = strconv.Itoa(number * multiplier)
	return nil
}

// checkPath refuses a versions path that would put versions in a synchronized folder, a folder in the versions, or the
// versions of two folders together. The folder's own .stversions is fine, as the engine doesn't synchronize that. Not while
// changing the configuration, as this looks at the disk.
func (s *VersioningSettings) checkPath(folderID string, folderRoot string, folders map[string]config.FolderConfiguration) error {
	if s.Path == s.saved.Path || !s.HasCustomPath() {
		return nil
	}
	versionsPath, err := absoluteVersionsPath(strings.TrimSpace(s.Path), folderRoot)
	if err != nil {
		return fmt.Errorf("versions path: %w", err)
	}
	if relative, err := filepath.Rel(pathKey(folderRoot), pathKey(versionsPath)); err == nil && fs.IsInternal(relative) {
		return nil
	}
	for _, folder := range folders {
		if !isNativeFilesystem(folder.FilesystemType) {
			continue
		}
		root, err := fs.ExpandTilde(folder.Path)
		if err != nil {
			continue
		}
		if isAtOrInside(versionsPath, root) {
			return errVersionsPathInFolder
		}
		if isAtOrInside(root, versionsPath) {
			return errVersionsPathAroundFolder
		}
		if other, ok := customVersionsPath(folder); ok && folder.ID != folderID &&
			overlapsAnyFolder(versionsPath, []string{other}) {
			return errVersionsPathShared
		}
	}
	return nil
}

// absoluteVersionsPath is where a versions path leads: the engine takes a relative path from the root of the folder
func absoluteVersionsPath(versionsPath string, folderRoot string) (string, error) {
	expanded, err := fs.ExpandTilde(versionsPath)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		return filepath.Join(folderRoot, expanded), nil
	}
	return expanded, nil
}

// customVersionsPath is where a folder keeps its versions when that is somewhere of its own
func customVersionsPath(folder config.FolderConfiguration) (string, bool) {
	if folder.Versioning.Type == VersioningTypeNone || folder.Versioning.FSPath == "" ||
		!isNativeFilesystem(folder.FilesystemType) {
		return "", false
	}
	root, err := fs.ExpandTilde(folder.Path)
	if err != nil {
		return "", false
	}
	versionsPath, err := absoluteVersionsPath(folder.Versioning.FSPath, root)
	return versionsPath, err == nil
}

// isInVersionsPath reports whether a new folder would go in or around a place where a folder keeps its versions. Its files
// would be taken for versions, so that cleaning up removes them, and the deletions would go to its devices.
func isInVersionsPath(folderPath string, folders map[string]config.FolderConfiguration) bool {
	paths := make([]string, 0, len(folders))
	for _, folder := range folders {
		if versionsPath, ok := customVersionsPath(folder); ok {
			paths = append(paths, versionsPath)
		}
	}
	return overlapsAnyFolder(folderPath, paths)
}

// VersioningSettings are how the folder keeps old versions; nil when the folder is gone
func (fld *Folder) VersioningSettings() *VersioningSettings {
	folder := fld.folderConfiguration()
	if folder == nil {
		return nil
	}
	return newVersioningSettings(folder.Versioning)
}

// SetVersioningSettings saves what was edited, which restarts the folder if anything was. When something can't be used,
// nothing changes and the error says why. The engine's own check of the new versioning can refuse it too.
func (fld *Folder) SetVersioningSettings(settings *VersioningSettings) error {
	current := fld.folderConfiguration()
	wrapper := fld.client.cfg()
	if current == nil || wrapper == nil {
		return errFolderConfigNotFound
	}
	if settings.saved == nil {
		settings.saved = newVersioningSettings(current.Versioning)
	}
	if err := settings.checkPath(fld.FolderID, current.Filesystem().URI(), wrapper.Folders()); err != nil {
		return err
	}
	return fld.changeFolderCopy(func(folder *config.FolderConfiguration) error {
		return settings.applyTo(&folder.Versioning)
	})
}
