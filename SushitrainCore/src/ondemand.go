// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"fmt"
	"strings"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/ignore"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
)

// An on-demand folder is a selective folder that Finder shows in full through the remote file service, on macOS. The
// engine keeps the global index and only the files that were written here, that no other device has, and conflict
// copies, which every on-demand folder selects. The service writes to it (see gateway/ondemand.go), which plain
// selective folders don't allow. Its .stignore says that it is on demand (onDemandPattern), so that nothing else needs
// to remember it.

// errOnDemandSelection refuses what would deselect paths of an on-demand folder. A deselected path is marked ignored at
// the next scan, which also stops announcing it: a deletion made here would then never reach other devices, and an
// edit they don't have yet would be lost to them.
var errOnDemandSelection = errors.New("the files kept for an on-demand folder can't be deselected")

// How many files that no connected device has can stay selected when a folder switches to on demand. Each one is a
// line in .stignore, which every scan matches all paths against.
const maxOnDemandKeptFiles = 10_000

// IsOnDemand reports whether the folder is on demand, or an error if its mode is unknown.
func (fld *Folder) IsOnDemand() (bool, error) {
	selective, err := fld.IsSelective()
	if err != nil || !selective {
		return false, err
	}
	ignores, err := fld.loadIgnores()
	if err != nil {
		return false, err
	}
	return newSelection(ignores.Lines()).isOnDemand(), nil
}

// keepSelected selects what the remote file service wrote. It runs in the folder's loop, so it must not wait for the
// folder: saving the patterns only schedules a scan, and no holder of the selection lock waits for the loop of an
// on-demand folder (deselecting, which can pause it, is refused). Paths that are selected already change nothing, and
// scan nothing.
func (fld *Folder) keepSelected(paths []string) error {
	unlock := lockSelection(fld.FolderID)
	defer unlock()
	fld.cachedIgnore.matcher = nil
	ignores, err := fld.loadIgnores()
	if err != nil {
		return err
	}
	selection := newSelection(ignores.Lines())
	changed, err := selection.keep(paths)
	if err != nil || !changed {
		return err
	}
	return fld.client.internals().SetIgnores(fld.FolderID, selection.patterns())
}

// globalIgnores returns what excludes a path from a selective folder apart from its selection. It reads the patterns
// the way the engine does, which also covers what is below a directory they match, as they come before the selection
// and outrank it.
func (fld *Folder) globalIgnores() (func(path string) bool, error) {
	cfg := fld.folderConfiguration()
	if cfg == nil {
		return nil, errors.New("folder does not exist")
	}
	ignores, err := fld.loadIgnores()
	if err != nil {
		return nil, err
	}
	patterns := Filter(newSelection(ignores.Lines()).lines, isGlobalIgnorePattern)
	matcher := ignore.New(cfg.Filesystem())
	if err := matcher.Parse(strings.NewReader(strings.Join(patterns, "\n")), ignoreFileName); err != nil {
		return nil, err
	}
	return func(path string) bool { return matcher.Match(path).IsIgnored() }, nil
}

// heldByConnectedPeer returns the version of a file on disk, if a connected device has it in full, going by what that
// device itself announced. A device's valid entry at the global version says it has all of the file, so one block's
// availability answers for all of them. Anything else counts as no: then the file stays.
func (fld *Folder) heldByConnectedPeer(name string, onDisk fs.FileInfo, cfg *config.FolderConfiguration) (protocol.Vector, bool, error) {
	internals := fld.client.internals()
	global, ok, err := internals.GlobalFileInfo(fld.FolderID, name)
	if err != nil {
		return protocol.Vector{}, false, err
	}
	if !ok || global.Deleted || global.IsInvalid() || len(global.Blocks) == 0 {
		return protocol.Vector{}, false, nil
	}
	if unchanged, err := fld.isVersion(name, onDisk, global.Version, cfg); err != nil || !unchanged {
		return protocol.Vector{}, false, err
	}
	available, err := internals.BlockAvailability(fld.FolderID, global, global.Blocks[0])
	if err != nil {
		return protocol.Vector{}, false, err
	}
	for _, device := range available {
		if !device.FromTemporary {
			return global.Version, true, nil
		}
	}
	return protocol.Vector{}, false, nil
}

// isVersion reports whether the file on disk is the given version, as the index knows it. The entry can be an ignored
// one, in a folder that became on demand but stopped before removing what it no longer selects.
func (fld *Folder) isVersion(name string, onDisk fs.FileInfo, version protocol.Vector, cfg *config.FolderConfiguration) (bool, error) {
	local, ok, err := fld.client.internals().LocalFileInfo(fld.FolderID, name)
	if err != nil || !ok || local.Deleted || !local.Version.Equal(version) {
		return false, err
	}
	return onDisk.Size() == local.Size && onDisk.ModTime().Sub(local.ModTime()).Abs() <= cfg.ModTimeWindow(), nil
}

// hasUnsentDeletions reports whether a device that shares the folder hasn't got a deletion yet. This device stops
// announcing what it ignores, and a selective device can't tell deletions apart from what it doesn't want either.
func (fld *Folder) hasUnsentDeletions() (bool, error) {
	devices, err := fld.sharedWith()
	if err != nil {
		return false, err
	}
	for _, device := range devices {
		if device == fld.client.deviceID() {
			continue
		}
		completion, err := fld.client.internals().Completion(device, fld.FolderID)
		if err != nil {
			return false, err
		}
		if completion.NeedDeletes > 0 {
			return true, nil
		}
	}
	return false, nil
}

// walkOwnFiles calls fn for the files on disk that include accepts, leaving Syncthing's own alone
func walkOwnFiles(ffs fs.Filesystem, markerName string, include func(path string) bool, fn func(path string, info fs.FileInfo) error) error {
	return ffs.Walk("", func(path string, info fs.FileInfo, err error) error {
		switch {
		case err != nil:
			return err
		case path == "." || path == "":
			return nil
		case fs.IsInternal(path) || path == ignoreFileName || markerName != "" && strings.HasPrefix(path, markerName):
			if info.IsDir() {
				return fs.SkipDir
			}
			return nil
		case info.IsDir() || !include(path):
			return nil
		}
		return fn(path, info)
	})
}

// SwitchToOnDemand makes the folder on demand, and returns how many files stay on this device because no connected
// device has them. Files a connected device has are removed here, and open through Finder from then on. A folder that
// is on demand already removes what an earlier switch left behind.
//
// Making a folder selective ignores all that it has, and this device then stops offering it. So a file only this device
// has stays selected, as does anything else that can't be shown to be on another device. Availability is read while
// the folder runs, as a paused folder doesn't say what its devices have; what changed by the time it is paused, and so
// isn't what was found on another device, stays too.
func (fld *Folder) SwitchToOnDemand() (int, error) {
	cfg := fld.folderConfiguration()
	if cfg == nil {
		return 0, errors.New("folder does not exist")
	}
	if cfg.Type != config.FolderTypeSendReceive || !isNativeFilesystem(cfg.FilesystemType) {
		return 0, errors.New("only send-receive folders in the file system can be on demand")
	}
	selective, err := fld.IsSelective()
	if err != nil {
		return 0, err
	}
	ignores, err := fld.loadIgnores()
	if err != nil {
		return 0, err
	}
	finishing := selective && newSelection(ignores.Lines()).isOnDemand()
	if selective && !finishing {
		// It keeps what it has selected, which is where the files it holds came from
		_, err := fld.changeSelection(func(selection *selection) error { return selection.setOnDemand() })
		return 0, err
	}
	// What a switch decides about: in a folder on demand already, what the selection leaves out
	include := func(path string) bool { return !ignores.Match(path).IsIgnored() }
	if finishing {
		isExcluded, err := fld.globalIgnores()
		if err != nil {
			return 0, err
		}
		include = func(path string) bool { return ignores.Match(path).IsIgnored() && !isExcluded(path) }
	} else {
		for _, line := range ignores.Lines() {
			// Selective folders can only express these. Others would stop applying, and what they keep from other
			// devices would start reaching them.
			if !isGlobalIgnorePattern(line) && !isCommentPattern(line) {
				return 0, fmt.Errorf("the folder's ignore pattern %q can't be kept in an on-demand folder", line)
			}
		}
		unsent, err := fld.hasUnsentDeletions()
		if err != nil {
			return 0, err
		}
		if unsent {
			return 0, errors.New("wait until other devices have this folder's deletions")
		}
	}
	if state, err := fld.State(); err != nil || state != "idle" {
		return 0, errors.Join(errors.New("wait until the folder has finished synchronizing"), err)
	}
	if fld.ConnectedPeerCount() == 0 {
		return 0, errors.New("connect to a device that shares this folder first")
	}
	// What is on disk has to be in the index before anything decides by it
	if err := fld.client.scanFolderSubdirs(fld.FolderID, []string{""}); err != nil {
		return 0, err
	}

	ffs := cfg.Filesystem()
	held := map[string]protocol.Vector{}
	kept := 0
	err = walkOwnFiles(ffs, cfg.MarkerName, include, func(path string, info fs.FileInfo) error {
		name := osutil.NormalizedFilename(path)
		version, ok, err := fld.heldByConnectedPeer(name, info, cfg)
		if err != nil {
			return err
		}
		if ok {
			held[name] = version
		} else {
			kept++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if kept > 0 && len(held) == 0 {
		// Which can also mean that a device connected a moment ago: nothing would be gained
		return 0, errors.New("no connected device has this folder's files yet")
	}
	if kept > maxOnDemandKeptFiles {
		return 0, fmt.Errorf("%d files are only on this device: wait until other devices have them", kept)
	}

	var keep []string
	err = fld.whilePaused(func() error {
		// Nothing changes the index now. Whatever isn't the version found on another device stays.
		err := walkOwnFiles(ffs, cfg.MarkerName, include, func(path string, info fs.FileInfo) error {
			name := osutil.NormalizedFilename(path)
			version, ok := held[name]
			if ok {
				unchanged, err := fld.isVersion(name, info, version, cfg)
				if err != nil {
					return err
				}
				ok = unchanged
			}
			if !ok {
				delete(held, name)
				keep = append(keep, name)
			}
			return nil
		})
		if err != nil {
			return err
		}
		_, err = fld.changeSelection(func(selection *selection) error {
			if !selection.isSelectiveIgnore() {
				if err := selection.setSelective(true); err != nil {
					return err
				}
			}
			if err := selection.setOnDemand(); err != nil {
				return err
			}
			_, err := selection.keep(keep)
			return err
		})
		if err != nil {
			return err
		}
		ignores, err := fld.loadIgnores()
		if err != nil {
			return err
		}
		return cleanSelection(ffs, ignores, cfg.MarkerName, true, func(path string) (bool, error) {
			_, ok := held[osutil.NormalizedFilename(path)]
			return ok, nil
		})
	})
	return len(keep), err
}
