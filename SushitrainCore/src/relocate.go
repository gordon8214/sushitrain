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
	"path"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/locations"
)

// A folder move in progress: what the next start needs to finish it, should the app stop before the folder points at
// its new place
type folderRelocation struct {
	FolderID string `json:"folderID"`
	From     string `json:"from"`
	To       string `json:"to"`
	// Whether the folder was paused before the move, as it is again afterwards
	Paused bool `json:"paused"`
}

// The journal of folder moves in progress, in the configuration directory
const relocationJournalName = "folder-relocations.json"

var (
	ErrRelocationTargetInvalid = errors.New("a folder can only be moved to a new directory given by its full path, outside other folders and the app's own data")
	ErrRelocationSourceInvalid = errors.New("only a folder whose directory and folder marker are in place can be moved")
	// The folder's files were moved, but it does not point at them yet; the next start of the app finishes that
	ErrRelocationPending = errors.New("the move of this folder finishes when the app starts the next time")
)

var (
	// Held for a whole move, and while a run loads its configuration (which finishes moves from the journal), so
	// that the two never interleave
	relocationMutex sync.Mutex
	// Held for each change of the journal
	relocationJournalMutex sync.Mutex
)

func relocationJournalPath() string {
	return filepath.Join(locations.GetBaseDir(locations.ConfigBaseDir), relocationJournalName)
}

// readRelocations reads the journal. An empty one has no moves; a damaged one is set aside (its moves can then not be
// finished, but no later move is held up by it).
func readRelocations(journal string) ([]folderRelocation, error) {
	data, err := os.ReadFile(journal)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(data) == 0) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var relocations []folderRelocation
	if err := json.Unmarshal(data, &relocations); err != nil {
		aside := journal + ".broken-" + time.Now().UTC().Format("20060102-150405")
		slog.Warn("the journal of folder moves is damaged; setting it aside", "path", journal, "aside", aside, "cause", err)
		if err := os.Rename(journal, aside); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return relocations, nil
}

// writeRelocations replaces the journal (durably), or removes it when no move is in progress
func writeRelocations(journal string, relocations []folderRelocation) error {
	if len(relocations) == 0 {
		if err := os.Remove(journal); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(relocations, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSynced(journal, data)
}

// changeRelocations applies a change to the journal
func changeRelocations(change func([]folderRelocation) []folderRelocation) error {
	relocationJournalMutex.Lock()
	defer relocationJournalMutex.Unlock()
	journal := relocationJournalPath()
	relocations, err := readRelocations(journal)
	if err != nil {
		return err
	}
	return writeRelocations(journal, change(relocations))
}

// checkRelocationTarget checks that a folder at from (with the given ID) may move to to: not inside or around its own
// directory, another folder or the engine's data, also where symbolic links in the target's parents lead
func (clt *Client) checkRelocationTarget(folderID string, from string, to string) error {
	for _, target := range withResolved(to) {
		for _, source := range withResolved(from) {
			if pathsOverlap(source, target) {
				if pathKey(source) == pathKey(target) && source != target {
					return fmt.Errorf("%w: renaming only the case of its letters is not supported", ErrRelocationTargetInvalid)
				}
				return fmt.Errorf("%w: it would be inside or around the folder's own directory", ErrRelocationTargetInvalid)
			}
		}
		if overlapsEngineData(target) {
			return ErrFolderOverlapsEngineData
		}
	}
	if other := clt.OverlappingFolderID(to); other != "" && other != folderID {
		return fmt.Errorf("%w: it would overlap the folder %q", ErrRelocationTargetInvalid, other)
	}
	return nil
}

// Relocate moves the folder's directory to newPath, a directory that does not exist yet on the same volume, and points
// the folder there. The files are renamed, never copied; the folder is paused while it moves. Should the app stop
// before the folder points at its new place, the next start finishes the move (finishRelocations), and
// ErrRelocationPending says so when that is known already.
func (fld *Folder) Relocate(newPath string) error {
	relocationMutex.Lock()
	defer relocationMutex.Unlock()
	return fld.relocate(newPath)
}

func (fld *Folder) relocate(newPath string) error {
	clt := fld.client
	cfg := clt.cfg()
	if cfg == nil {
		return ErrStillLoading
	}
	fc, ok := cfg.Folder(fld.FolderID)
	if !ok {
		return errFolderConfigNotFound
	}
	if !isNativeFilesystem(fc.FilesystemType) {
		return ErrUnsupportedFilesystem
	}
	relocationJournalMutex.Lock()
	pending, err := readRelocations(relocationJournalPath())
	relocationJournalMutex.Unlock()
	if err != nil {
		return err
	}
	if slices.ContainsFunc(pending, func(r folderRelocation) bool { return r.FolderID == fld.FolderID }) {
		return ErrRelocationPending
	}

	// The marker is how an interrupted move is recognized at the next start, and a link would be moved instead of the
	// directory it leads to
	from := clt.expandHome(fc.Path)
	if info, err := os.Lstat(from); err != nil || !info.IsDir() || !markerExists(from, fc.MarkerName) {
		return ErrRelocationSourceInvalid
	}
	to := path.Clean(newPath)
	if newPath == "" || !path.IsAbs(to) {
		return ErrRelocationTargetInvalid
	}
	if err := clt.checkRelocationTarget(fld.FolderID, from, to); err != nil {
		return err
	}
	if _, err := os.Lstat(to); err == nil {
		return ErrMoveTargetOccupied
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !onSameVolume(from, to) {
		return ErrMoveAcrossVolumes
	}
	if err := os.MkdirAll(path.Dir(to), 0o755); err != nil {
		return err
	}
	// Its parent exists now, so its links resolve
	if err := clt.checkRelocationTarget(fld.FolderID, from, to); err != nil {
		return err
	}

	relocation := folderRelocation{FolderID: fld.FolderID, From: from, To: to, Paused: fc.Paused}
	if err := changeRelocations(func(relocations []folderRelocation) []folderRelocation {
		return append(relocations, relocation)
	}); err != nil {
		return err
	}
	forget := func() {
		if err := changeRelocations(func(relocations []folderRelocation) []folderRelocation {
			return withoutRelocation(relocations, fld.FolderID)
		}); err != nil {
			slog.Warn("could not update the journal of folder moves", "folderID", fld.FolderID, "cause", err)
		}
	}
	// Resumes the folder after its files stayed where they were; the journal keeps the move if that fails, for the
	// next start to resume it
	resume := func() bool {
		if !fc.Paused {
			if err := fld.SetPaused(false); err != nil {
				slog.Warn("could not resume folder after its move failed", "folderID", fld.FolderID, "cause", err)
				return false
			}
		}
		forget()
		return true
	}
	if !fc.Paused {
		if err := fld.SetPaused(true); err != nil {
			forget()
			return err
		}
	}

	slog.Info("moving folder", "folderID", fld.FolderID, "from", from, "to", to)
	if err := renameExclusive(from, to); err != nil {
		resume()
		if errors.Is(err, syscall.EXDEV) {
			return ErrMoveAcrossVolumes
		} else if errors.Is(err, os.ErrExist) {
			return ErrMoveTargetOccupied
		}
		return fmt.Errorf("could not move folder: %w", err)
	}

	// A folder that became remote-managed meanwhile stays paused, as it must
	if err := fld.changeFolderConfiguration(func(changed *config.FolderConfiguration) {
		changed.Path = to
		changed.Paused = relocation.Paused || clt.IsFolderRemoteManaged(fld.FolderID)
	}); err != nil {
		// Put the files back where the folder points; failing that, the next start points it at them
		if renameErr := renameExclusive(to, from); renameErr != nil {
			slog.Warn("could not put a moved folder back", "folderID", fld.FolderID, "cause", renameErr)
			return fmt.Errorf("%w (%v)", ErrRelocationPending, err)
		}
		if !resume() {
			return fmt.Errorf("%w (%v)", ErrRelocationPending, err)
		}
		return err
	}
	forget()
	return nil
}

// RelocateInto moves the folder into a new directory in base, named as a new folder would be (after its label, or
// else its ID, with a number added when that is taken), and returns its new path. A folder already in base stays.
func (fld *Folder) RelocateInto(base string) (string, error) {
	relocationMutex.Lock()
	defer relocationMutex.Unlock()
	clt := fld.client
	cfg := clt.cfg()
	if cfg == nil {
		return "", ErrStillLoading
	}
	fc, ok := cfg.Folder(fld.FolderID)
	if !ok {
		return "", errFolderConfigNotFound
	}
	if base == "" || !path.IsAbs(base) {
		return "", ErrRelocationTargetInvalid
	}
	base = path.Clean(base)
	from := clt.expandHome(fc.Path)
	if pathKey(path.Dir(from)) == pathKey(base) {
		return from, nil
	}
	name, err := folderName(fc.ID, fc.Label)
	if err != nil {
		return "", err
	}
	others := slices.DeleteFunc(cfg.FolderList(), func(other config.FolderConfiguration) bool { return other.ID == fc.ID })
	to, err := newFolderPathIn(base, name, clt.folderPaths(others))
	if errors.Is(err, ErrNoDefaultFolderPath) {
		return "", fmt.Errorf("%w: it is inside another folder, or has no free name left", ErrRelocationTargetInvalid)
	} else if err != nil {
		return "", err
	}
	return to, fld.relocate(to)
}

func withoutRelocation(relocations []folderRelocation, folderID string) []folderRelocation {
	return slices.DeleteFunc(relocations, func(r folderRelocation) bool { return r.FolderID == folderID })
}

// onSameVolume reports whether to (or its nearest existing parent) is on the volume of from, so that a rename can move
// one to the other
func onSameVolume(from string, to string) bool {
	var source syscall.Stat_t
	if err := syscall.Stat(from, &source); err != nil {
		return false
	}
	for dir := path.Dir(to); ; dir = path.Dir(dir) {
		var target syscall.Stat_t
		if err := syscall.Stat(dir, &target); err == nil {
			return source.Dev == target.Dev
		}
		if dir == "/" {
			return false
		}
	}
}

// finishRelocations finishes the folder moves an earlier run left in the journal, for folders that still point where
// they were: one whose directory (with its marker) was moved points at its new place, and every one is paused or not
// as before its move. A folder pointed elsewhere since is left alone. Returns whether the configuration changed, and
// the moves it handled, for the caller to remove from the journal once the configuration is saved.
func finishRelocations(conf *config.Configuration, journal string, filesPath string) (bool, []folderRelocation, error) {
	relocations, err := readRelocations(journal)
	if err != nil || len(relocations) == 0 {
		return false, nil, err
	}
	changed := false
	for _, relocation := range relocations {
		fc, _, ok := conf.Folder(relocation.FolderID)
		if !ok || pathKey(expandHomeIn(filesPath, fc.Path)) != pathKey(relocation.From) {
			continue
		}
		before := fc
		if !markerExists(relocation.From, fc.MarkerName) && markerExists(relocation.To, fc.MarkerName) {
			slog.Warn("finishing an interrupted folder move", "folderID", fc.ID, "from", relocation.From, "to", relocation.To)
			fc.Path = relocation.To
		}
		fc.Paused = relocation.Paused
		if fc.Path != before.Path || fc.Paused != before.Paused {
			conf.SetFolder(fc)
			changed = true
		}
	}
	return changed, relocations, nil
}
