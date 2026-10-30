// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/locations"
	"golang.org/x/text/unicode/norm"
)

// How the app places folders. In the app's container (iOS, and the Mac app in the App Sandbox), the folders the app
// creates are in its files directory (or the managed base), and the others belong to other apps. Outside the sandbox on
// macOS, folders are anywhere in the user's home directory, as with Syncthing.
type folderLayout int

const (
	containerFolders folderLayout = iota
	homeFolders
)

// The directory in the home directory that new folders go in when the configuration names no default folder path, as
// Syncthing's own default folder ~/Sync does. Only folders inside it can be removed with their files.
const syncDirectoryName = "Sync"

// Numbers tried after a folder's name when a directory of that name exists
const maxNumberedFolderNames = 99

var (
	ErrNoDefaultFolderPath      = errors.New("no new folder can be created in the default location; choose a location for the folder")
	ErrFolderOverlapsEngineData = errors.New("a folder cannot contain or be inside the folder where the app keeps its configuration and database")
	ErrFolderFilesNotRemovable  = errors.New("the files of this folder are not removed with it; unlink the folder, then remove its files yourself")
)

// expandHome expands a leading ~ to the home directory (the files path) and cleans the path
func (clt *Client) expandHome(p string) string {
	return expandHomeIn(clt.filesPath, p)
}

// expandHomeIn expands a leading ~ to home and cleans the path
func expandHomeIn(home string, p string) string {
	if p == "~" {
		return path.Clean(home)
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return path.Join(home, rest)
	}
	return path.Clean(p)
}

// pathKey is how a path is compared with others: the volumes macOS formats by default ignore case and Unicode
// normalization, so two paths that differ only in those name the same directory (and are taken to, on other volumes)
func pathKey(p string) string {
	return strings.ToLower(norm.NFC.String(path.Clean(p)))
}

// isInsideDirectory reports whether p is strictly inside dir; both are compared as pathKey does
func isInsideDirectory(p string, dir string) bool {
	p, dir = pathKey(p), pathKey(dir)
	if dir == "/" {
		return p != "/" && strings.HasPrefix(p, "/")
	}
	return strings.HasPrefix(p, dir+"/")
}

// pathsOverlap reports whether two paths are the same directory, or one is inside the other
func pathsOverlap(a string, b string) bool {
	return pathKey(a) == pathKey(b) || isInsideDirectory(a, b) || isInsideDirectory(b, a)
}

// withResolved is a path, and where its symbolic links lead if that is elsewhere: those of the part of it that exists
// (all of it, or its nearest existing parent)
func withResolved(p string) []string {
	rest := ""
	for dir := path.Clean(p); ; dir = path.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			resolved = path.Join(resolved, rest)
			if pathKey(resolved) != pathKey(p) {
				return []string{p, resolved}
			}
			return []string{p}
		}
		if dir == "/" || dir == "." {
			return []string{p}
		}
		rest = path.Join(path.Base(dir), rest)
	}
}

// overlapsAnyFolder reports whether a path overlaps one of the folders, also where symbolic links lead
func overlapsAnyFolder(p string, folders []string) bool {
	for _, candidate := range withResolved(p) {
		for _, folder := range folders {
			for _, existing := range withResolved(folder) {
				if pathsOverlap(candidate, existing) {
					return true
				}
			}
		}
	}
	return false
}

// overlapsEngineData reports whether a folder at p would contain, or be inside, the engine's configuration or database
// directory, and so synchronize its keys and database
func overlapsEngineData(p string) bool {
	for _, base := range []locations.BaseDirEnum{locations.ConfigBaseDir, locations.DataBaseDir} {
		dir := locations.GetBaseDir(base)
		if dir == "" {
			continue
		}
		for _, folder := range withResolved(p) {
			for _, data := range withResolved(dir) {
				if pathsOverlap(folder, data) {
					return true
				}
			}
		}
	}
	return false
}

// isProtectedDirectory reports whether p is a directory whose files are never removed with a folder: the root, the home
// directory (the engine's, and the one Syncthing expands ~ to) or one of its parents, or a directory that holds or is
// inside the engine's own data
func (clt *Client) isProtectedDirectory(p string) bool {
	homes := withResolved(clt.expandHome("~"))
	if home, err := os.UserHomeDir(); err == nil {
		homes = append(homes, withResolved(home)...)
	}
	for _, candidate := range withResolved(p) {
		if pathKey(candidate) == "/" || overlapsEngineData(candidate) {
			return true
		}
		for _, home := range homes {
			if pathKey(candidate) == pathKey(home) || isInsideDirectory(home, candidate) {
				return true
			}
		}
	}
	return false
}

// folderDirectoryName is a directory name for a new folder made from its label or ID: one visible path component of at
// most 240 bytes (so that a number still fits), or "" when nothing usable is left. Labels and IDs offered by other
// devices may be anything.
func folderDirectoryName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == ':':
			return '-'
		// Control and format characters (such as direction overrides) would make the name unreadable or misleading
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError:
			return -1
		}
		return r
	}, name)
	// No hidden directory, nor . or ..: leading dots and spaces go together, as in ". .ssh"
	name = strings.TrimLeftFunc(name, func(r rune) bool { return r == '.' || unicode.IsSpace(r) })
	name = strings.TrimRightFunc(name, unicode.IsSpace)
	for len(name) > 240 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = strings.TrimRightFunc(name[:len(name)-size], unicode.IsSpace)
	}
	if !isSafeFolderDirectoryName(name) || strings.HasPrefix(name, ".") {
		return ""
	}
	return name
}

// folderPaths are the expanded paths of the configured folders on native filesystems
func (clt *Client) folderPaths(folders []config.FolderConfiguration) []string {
	paths := make([]string, 0, len(folders))
	for _, folder := range folders {
		if isNativeFilesystem(folder.FilesystemType) && folder.Path != "" {
			paths = append(paths, clt.expandHome(folder.Path))
		}
	}
	return paths
}

// homeFolderBases are the directories a new folder outside the container may go in, in order of preference: the
// configured default folder path (which Syncthing's web GUI also puts new folders in, named after their label) or else
// ~/Sync, then the home directory
func (clt *Client) homeFolderBases(defaultFolderPath string) []string {
	base := clt.expandHome(defaultFolderPath)
	if defaultFolderPath == "" || !path.IsAbs(base) {
		base = clt.expandHome("~/" + syncDirectoryName)
	}
	home := clt.expandHome("~")
	if pathKey(base) == pathKey(home) {
		return []string{home}
	}
	return []string{base, home}
}

// defaultHomeFolderPath is where a new folder outside the container goes: a directory named after its label, or else its
// ID, in the first of homeFolderBases that is in no other folder. The directory never exists yet and overlaps no other
// folder: a name that is taken gets a number.
func (clt *Client) defaultHomeFolderPath(folderID string, label string) (string, error) {
	name, err := folderName(folderID, label)
	if err != nil {
		return "", err
	}
	cfg := clt.cfg()
	if cfg == nil {
		return "", ErrStillLoading
	}
	existing := clt.folderPaths(cfg.FolderList())
	for _, base := range clt.homeFolderBases(cfg.DefaultFolder().Path) {
		if candidate, err := newFolderPathIn(base, name, existing); err == nil || !errors.Is(err, ErrNoDefaultFolderPath) {
			return candidate, err
		}
	}
	return "", ErrNoDefaultFolderPath
}

// folderName is the name of a folder's new directory: from its label, or else its ID
func folderName(folderID string, label string) (string, error) {
	name := folderDirectoryName(label)
	if name == "" {
		name = folderDirectoryName(folderID)
	}
	if name == "" {
		return "", fmt.Errorf("%w: %q; choose a location for the folder", errUnsafeFolderID, folderID)
	}
	return name, nil
}

// newFolderPathIn is a new directory for a folder in base, named name (or with a number added, when that is taken), that
// does not exist and overlaps none of the existing folders nor the engine's data. ErrNoDefaultFolderPath when base is a
// folder or inside one, or no number is left.
func newFolderPathIn(base string, name string, existing []string) (string, error) {
	if slices.ContainsFunc(existing, func(folder string) bool {
		return pathKey(base) == pathKey(folder) || isInsideDirectory(base, folder)
	}) {
		return "", ErrNoDefaultFolderPath
	}
	for number := 1; number <= maxNumberedFolderNames; number++ {
		candidate := path.Join(base, name)
		if number > 1 {
			candidate += " " + strconv.Itoa(number)
		}
		if path.Dir(candidate) != base {
			return "", fmt.Errorf("%w: %q", errUnsafeFolderID, name)
		}
		if overlapsAnyFolder(candidate, existing) || overlapsEngineData(candidate) {
			continue
		}
		if _, err := os.Lstat(candidate); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		return candidate, nil
	}
	return "", ErrNoDefaultFolderPath
}

// offeredFolderLabel is the label another device offers a folder under that this device does not have yet, if any
func (clt *Client) offeredFolderLabel(folderID string) string {
	internals, cfg := clt.internals(), clt.cfg()
	if internals == nil || cfg == nil {
		return ""
	}
	devices := cfg.DeviceList()
	slices.SortFunc(devices, func(a, b config.DeviceConfiguration) int {
		return strings.Compare(a.DeviceID.String(), b.DeviceID.String())
	})
	for _, device := range devices {
		pending, err := internals.PendingFolders(device.DeviceID)
		if err != nil {
			continue
		}
		if folder, ok := pending[folderID]; ok {
			for _, offer := range folder.OfferedBy {
				if offer.Label != "" {
					return offer.Label
				}
			}
		}
	}
	return ""
}

// DefaultFolderPath is where AddFolder puts a new folder with this ID when it is given no path. Outside the App Sandbox
// on macOS that is a new directory named after the label another device offers the folder under (or else its ID), in
// the configured default folder path or ~/Sync.
func (clt *Client) DefaultFolderPath(folderID string) (string, error) {
	if clt.internals() == nil {
		return "", ErrStillLoading
	}
	return clt.defaultFolderPath(folderID, clt.offeredFolderLabel(folderID))
}

// OverlappingFolderID is the ID of a configured folder that a folder at the given path would contain or be inside (or
// share its directory with), or "" if there is none. Folders inside one another are not supported.
func (clt *Client) OverlappingFolderID(folderPath string) string {
	cfg := clt.cfg()
	if cfg == nil || folderPath == "" {
		return ""
	}
	expanded := clt.expandHome(folderPath)
	for _, folder := range cfg.FolderList() {
		if isNativeFilesystem(folder.FilesystemType) && folder.Path != "" &&
			overlapsAnyFolder(expanded, []string{clt.expandHome(folder.Path)}) {
			return folder.ID
		}
	}
	return ""
}

// UsesHomeFolders reports whether folders are the user's, anywhere in the home directory (macOS outside the App
// Sandbox), rather than in the app's container
func (clt *Client) UsesHomeFolders() bool {
	return clt.folderLayout == homeFolders
}

// CanRemoveFiles reports whether Remove may delete the folder's files. In the container that is for folders at the
// app's default locations. Outside the App Sandbox on macOS it is for folders inside ~/Sync, where new folders go when
// the configuration names no other place; elsewhere the files are the user's to remove in Finder. Never for a folder
// inside or around another folder, the home directory, a directory that holds it, or one that holds the engine's own
// data.
func (fld *Folder) CanRemoveFiles() bool {
	cfg := fld.client.cfg()
	if cfg == nil {
		return false
	}
	fc, ok := cfg.Folder(fld.FolderID)
	if !ok || !isNativeFilesystem(fc.FilesystemType) {
		return false
	}
	return fld.client.canRemoveFiles(fc, cfg.FolderList(), fc.Filesystem().URI())
}

// canRemoveFiles is CanRemoveFiles for a folder of a configuration whose files are in root, the directory its
// filesystem would remove
func (clt *Client) canRemoveFiles(fc config.FolderConfiguration, folders []config.FolderConfiguration, root string) bool {
	if !isNativeFilesystem(fc.FilesystemType) {
		return false
	}
	// The configured path, as the app expands it, and the filesystem's root, as Syncthing does (with $HOME), and where
	// its symbolic links lead: the files are removed only when every one of them passes
	paths := append([]string{clt.expandHome(fc.Path)}, withResolved(root)...)
	for _, p := range paths {
		if clt.isProtectedDirectory(p) {
			return false
		}
		for _, other := range folders {
			if other.ID != fc.ID && isNativeFilesystem(other.FilesystemType) && other.Path != "" &&
				pathsOverlap(p, clt.expandHome(other.Path)) {
				return false
			}
		}
	}
	if clt.folderLayout == homeFolders {
		syncDirectories := withResolved(clt.expandHome("~/" + syncDirectoryName))
		for _, p := range paths {
			if !slices.ContainsFunc(syncDirectories, func(dir string) bool { return isInsideDirectory(p, dir) }) {
				return false
			}
		}
		return true
	}
	return clt.isInternalFolderPath(fc.ID, fc.Path)
}
