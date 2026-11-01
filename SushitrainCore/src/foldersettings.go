// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"encoding"
	"errors"
	"fmt"
	"math"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/config"
)

const (
	// The most that the engine takes: it uses this limit for anything more
	maxConcurrentWrites = 256
	// The engine sets no limit, and starts this many goroutines for every scan
	maxFolderHashers = 256
	// The engine turns these into a time.Duration
	maxDurationSeconds = float64(math.MaxInt64 / int64(time.Second))
	// The shortest delay of the watcher, which the engine would otherwise turn into this
	minWatcherDelayS = 0.01

	xattrEntryPermit = "+"
	xattrEntryDeny   = "-"
)

var (
	folderPullOrders      = []string{"random", "alphabetic", "smallestFirst", "largestFirst", "oldestFirst", "newestFirst"}
	folderBlockPullOrders = []string{"standard", "random", "inOrder"}
	// The other methods exist for other platforms, where a method missing here fails every copy of a block
	folderCopyRangeMethods = []string{"standard", "all"}
	folderXattrModes       = []string{"syncable", "all"}
	// Receive-encrypted can't be changed afterwards, so it isn't something to start every folder with
	defaultFolderTypes = []string{
		config.FolderTypeSendReceive.String(), config.FolderTypeSendOnly.String(), config.FolderTypeReceiveOnly.String(),
	}

	errFolderMetadataSend    = errors.New("this type of folder only applies metadata, it doesn't send it")
	errFolderMetadataSync    = errors.New("this type of folder doesn't apply metadata from other devices")
	errFolderPermissions     = errors.New("receive-encrypted folders always ignore permissions")
	errFolderOwnershipParent = errors.New("ownership can't both be synchronized and copied from the parent directory")
	errXattrEntry            = errors.New("extended attribute filter: enter an attribute name for each rule")
	errDefaultPathRelative   = errors.New("enter the full path for new folders, or leave it empty")
	errMarkerIsFolder        = errors.New("marker name: enter a name inside the folder: the folder itself can't tell that it is there")
)

// FolderSettings are a folder's settings that have no control of their own in the app, which it shows and saves together
// so that the folder restarts once. Numbers are text, as they were entered, for SetSettings to check. The last group is
// for the defaults of new folders, as a folder has its own controls for those. Type and MarkerName are there for a folder
// too: the first to tell what applies to it, the second as it has no other control.
type FolderSettings struct {
	Order                 string
	MinDiskFree           string
	MinDiskFreeUnit       string
	IgnoreDelete          bool
	CaseSensitiveFS       bool
	ModTimeWindowS        string
	DisableSparseFiles    bool
	DisableFsync          bool
	FSWatcherTimeoutS     string
	AutoNormalize         bool
	MarkerName            string
	ScanProgressIntervalS string

	Copiers             string
	Hashers             string
	PullerMaxPendingKiB string
	PullerPauseS        string
	PullerDelayS        string
	MaxConcurrentWrites string
	BlockPullOrder      string
	// Not called CopyRangeMethod: Objective-C takes names that start with copy to return an owned object
	BlockCopyMethod string

	SyncXattrs              bool
	SendXattrs              bool
	SyncOwnership           bool
	SendOwnership           bool
	CopyOwnershipFromParent bool
	IgnorePerms             bool
	XattrMode               string
	// One per line, in order: + to permit or - to deny, then the pattern
	XattrFilterEntries      string
	XattrMaxSingleEntrySize string
	XattrMaxTotalSize       string

	Type             string
	Path             string
	RescanIntervalS  string
	FSWatcherEnabled bool
	FSWatcherDelayS  string
	MaxConflicts     string
	BlockIndexing    bool

	// The settings as they were read, which tell what was edited
	saved *FolderSettings
}

func newFolderSettings(folder config.FolderConfiguration) *FolderSettings {
	xattrMode, _ := folder.XattrFilter.MacOSMode.MarshalText()
	settings := &FolderSettings{
		Order:                   folder.Order.String(),
		MinDiskFree:             strconv.FormatFloat(folder.MinDiskFree.Value, 'f', -1, 64),
		MinDiskFreeUnit:         folder.MinDiskFree.Unit,
		IgnoreDelete:            folder.IgnoreDelete,
		CaseSensitiveFS:         folder.CaseSensitiveFS,
		ModTimeWindowS:          strconv.Itoa(folder.RawModTimeWindowS),
		DisableSparseFiles:      folder.DisableSparseFiles,
		DisableFsync:            folder.DisableFsync,
		FSWatcherTimeoutS:       strconv.FormatFloat(folder.FSWatcherTimeoutS, 'f', -1, 64),
		AutoNormalize:           folder.AutoNormalize,
		MarkerName:              folder.MarkerName,
		ScanProgressIntervalS:   strconv.Itoa(folder.ScanProgressIntervalS),
		Copiers:                 strconv.Itoa(folder.Copiers),
		Hashers:                 strconv.Itoa(folder.Hashers),
		PullerMaxPendingKiB:     strconv.Itoa(folder.PullerMaxPendingKiB),
		PullerPauseS:            strconv.Itoa(folder.PullerPauseS),
		PullerDelayS:            strconv.FormatFloat(folder.PullerDelayS, 'f', -1, 64),
		MaxConcurrentWrites:     strconv.Itoa(folder.MaxConcurrentWrites),
		BlockPullOrder:          folder.BlockPullOrder.String(),
		BlockCopyMethod:         folder.CopyRangeMethod.String(),
		SyncXattrs:              folder.SyncXattrs,
		SendXattrs:              folder.SendXattrs,
		SyncOwnership:           folder.SyncOwnership,
		SendOwnership:           folder.SendOwnership,
		CopyOwnershipFromParent: folder.CopyOwnershipFromParent,
		IgnorePerms:             folder.IgnorePerms,
		XattrMode:               string(xattrMode),
		XattrFilterEntries:      xattrEntryLines(folder.XattrFilter.Entries),
		XattrMaxSingleEntrySize: strconv.Itoa(folder.XattrFilter.MaxSingleEntrySize),
		XattrMaxTotalSize:       strconv.Itoa(folder.XattrFilter.MaxTotalSize),
		Type:                    folder.Type.String(),
		Path:                    folder.Path,
		RescanIntervalS:         strconv.Itoa(folder.RescanIntervalS),
		FSWatcherEnabled:        folder.FSWatcherEnabled,
		FSWatcherDelayS:         strconv.FormatFloat(folder.FSWatcherDelayS, 'f', -1, 64),
		MaxConflicts:            strconv.Itoa(folder.MaxConflicts),
		BlockIndexing:           folder.BlockIndexing,
	}
	read := *settings
	settings.saved = &read
	return settings
}

// Equal reports whether both hold the same settings
func (s *FolderSettings) Equal(other *FolderSettings) bool {
	first, second := *s, *other
	first.saved, second.saved = nil, nil
	return first == second
}

// BlockCopyMethods are the methods to choose from, with the one the configuration has if that is another
func (s *FolderSettings) BlockCopyMethods() *ListOfStrings {
	if s.saved != nil {
		return List(choicesWith(folderCopyRangeMethods, s.saved.BlockCopyMethod))
	}
	return List(folderCopyRangeMethods)
}

// MinDiskFreeUnits are the units to choose from, with the one the configuration has if that is another
func (s *FolderSettings) MinDiskFreeUnits() *ListOfStrings {
	if s.saved != nil {
		return List(choicesWith(minHomeDiskFreeUnits, s.saved.MinDiskFreeUnit))
	}
	return List(minHomeDiskFreeUnits)
}

func choicesWith(choices []string, current string) []string {
	if !slices.Contains(choices, current) {
		return append(slices.Clone(choices), current)
	}
	return choices
}

// applyTo checks the settings that were edited, and puts them in folder, which is a copy to throw away if this fails.
// What wasn't edited stays as it is by now. Whatever the engine would silently replace when it prepares the configuration
// is refused: the page would show another value than was entered.
func (s *FolderSettings) applyTo(folder *config.FolderConfiguration) error {
	saved := s.saved
	if err := errors.Join(
		setChoiceOption("file pull order", s.Order, saved.Order, folderPullOrders, &folder.Order),
		setNumberOption("modification time window", s.ModTimeWindowS, saved.ModTimeWindowS, &folder.RawModTimeWindowS,
			0, math.MaxInt32),
		setSecondsOption("watcher timeout", s.FSWatcherTimeoutS, saved.FSWatcherTimeoutS, &folder.FSWatcherTimeoutS, 0),
		setNumberOption("scan progress interval", s.ScanProgressIntervalS, saved.ScanProgressIntervalS,
			&folder.ScanProgressIntervalS, math.MinInt32, config.MaxRescanIntervalS),
		setNumberOption("copiers", s.Copiers, saved.Copiers, &folder.Copiers, 0, math.MaxInt32),
		setNumberOption("hashers", s.Hashers, saved.Hashers, &folder.Hashers, 0, maxFolderHashers),
		setNumberOption("pending data", s.PullerMaxPendingKiB, saved.PullerMaxPendingKiB, &folder.PullerMaxPendingKiB,
			0, math.MaxInt32),
		setNumberOption("pause after a failure", s.PullerPauseS, saved.PullerPauseS, &folder.PullerPauseS,
			0, config.MaxRescanIntervalS),
		setSecondsOption("delay before pulling", s.PullerDelayS, saved.PullerDelayS, &folder.PullerDelayS, 0),
		setNumberOption("files written at once", s.MaxConcurrentWrites, saved.MaxConcurrentWrites,
			&folder.MaxConcurrentWrites, 1, maxConcurrentWrites),
		setChoiceOption("block pull order", s.BlockPullOrder, saved.BlockPullOrder, folderBlockPullOrders,
			&folder.BlockPullOrder),
		setChoiceOption("copy method", s.BlockCopyMethod, saved.BlockCopyMethod, s.BlockCopyMethods().data,
			&folder.CopyRangeMethod),
		setChoiceOption("extended attributes on macOS", s.XattrMode, saved.XattrMode, folderXattrModes,
			&folder.XattrFilter.MacOSMode),
		setNumberOption("largest attribute", s.XattrMaxSingleEntrySize, saved.XattrMaxSingleEntrySize,
			&folder.XattrFilter.MaxSingleEntrySize, 0, math.MaxInt32),
		setNumberOption("attributes per file", s.XattrMaxTotalSize, saved.XattrMaxTotalSize,
			&folder.XattrFilter.MaxTotalSize, 0, math.MaxInt32),
		s.applyMinDiskFree(folder),
		s.applyXattrEntries(folder),
	); err != nil {
		return err
	}
	setBoolOption(s.IgnoreDelete, saved.IgnoreDelete, &folder.IgnoreDelete)
	setBoolOption(s.CaseSensitiveFS, saved.CaseSensitiveFS, &folder.CaseSensitiveFS)
	setBoolOption(s.DisableSparseFiles, saved.DisableSparseFiles, &folder.DisableSparseFiles)
	setBoolOption(s.DisableFsync, saved.DisableFsync, &folder.DisableFsync)
	setBoolOption(s.AutoNormalize, saved.AutoNormalize, &folder.AutoNormalize)
	setBoolOption(s.SyncXattrs, saved.SyncXattrs, &folder.SyncXattrs)
	setBoolOption(s.SendXattrs, saved.SendXattrs, &folder.SendXattrs)
	setBoolOption(s.SyncOwnership, saved.SyncOwnership, &folder.SyncOwnership)
	setBoolOption(s.SendOwnership, saved.SendOwnership, &folder.SendOwnership)
	setBoolOption(s.CopyOwnershipFromParent, saved.CopyOwnershipFromParent, &folder.CopyOwnershipFromParent)
	setBoolOption(s.IgnorePerms, saved.IgnorePerms, &folder.IgnorePerms)
	return s.checkMetadata(folder)
}

// applyDefaultsTo also sets what only new folders take from the defaults, on top of applyTo. Not the marker name: the
// engine only creates a marker of the default name, so another would stop every new folder.
func (s *FolderSettings) applyDefaultsTo(folder *config.FolderConfiguration) error {
	saved := s.saved
	if err := errors.Join(
		setChoiceOption("folder type", s.Type, saved.Type, defaultFolderTypes, &folder.Type),
		setNumberOption("rescan interval", s.RescanIntervalS, saved.RescanIntervalS, &folder.RescanIntervalS,
			0, config.MaxRescanIntervalS),
		setSecondsOption("watcher delay", s.FSWatcherDelayS, saved.FSWatcherDelayS, &folder.FSWatcherDelayS, minWatcherDelayS),
		setNumberOption("maximum conflicts", s.MaxConflicts, saved.MaxConflicts, &folder.MaxConflicts, -1, math.MaxInt32),
		s.applyDefaultPath(folder),
	); err != nil {
		return err
	}
	setBoolOption(s.FSWatcherEnabled, saved.FSWatcherEnabled, &folder.FSWatcherEnabled)
	setBoolOption(s.BlockIndexing, saved.BlockIndexing, &folder.BlockIndexing)
	return s.applyTo(folder)
}

func (s *FolderSettings) applyMinDiskFree(folder *config.FolderConfiguration) error {
	saved := s.saved
	if s.MinDiskFree == saved.MinDiskFree && s.MinDiskFreeUnit == saved.MinDiskFreeUnit {
		return nil
	}
	size, err := parseMinDiskFree(s.MinDiskFree, s.MinDiskFreeUnit, s.MinDiskFreeUnits().data)
	if err != nil {
		return err
	}
	folder.MinDiskFree = size
	return nil
}

func (s *FolderSettings) applyDefaultPath(folder *config.FolderConfiguration) error {
	if s.Path == s.saved.Path {
		return nil
	}
	defaultPath := strings.TrimSpace(s.Path)
	if defaultPath == "" {
		// Spelled out: the engine takes no path to mean its working directory for a folder it accepts by itself
		folder.Path = "~/" + syncDirectoryName
		return nil
	}
	if !filepath.IsAbs(defaultPath) && defaultPath != "~" && !strings.HasPrefix(defaultPath, "~/") {
		return errDefaultPathRelative
	}
	folder.Path = defaultPath
	return nil
}

// applyXattrEntries gives the folder a new list. Copying a folder's configuration shares this one, so changing it in
// place would change the configuration in use, and the engine would see nothing to save.
func (s *FolderSettings) applyXattrEntries(folder *config.FolderConfiguration) error {
	if s.XattrFilterEntries == s.saved.XattrFilterEntries {
		return nil
	}
	entries, err := parseXattrEntries(s.XattrFilterEntries)
	if err != nil {
		return err
	}
	folder.XattrFilter.Entries = entries
	return nil
}

// checkMetadata refuses what was just turned on but has no effect in this type of folder. The engine would turn some of
// it off again, and the rest would do nothing. What a folder already has stays, as it does no harm.
func (s *FolderSettings) checkMetadata(folder *config.FolderConfiguration) error {
	saved := s.saved
	turnedOn := func(value, before bool) bool { return value && !before }
	switch folder.Type {
	case config.FolderTypeSendOnly:
		if turnedOn(s.SyncXattrs, saved.SyncXattrs) || turnedOn(s.SyncOwnership, saved.SyncOwnership) {
			return errFolderMetadataSync
		}
	case config.FolderTypeReceiveOnly:
		if turnedOn(s.SendXattrs, saved.SendXattrs) || turnedOn(s.SendOwnership, saved.SendOwnership) {
			return errFolderMetadataSend
		}
	case config.FolderTypeReceiveEncrypted:
		if turnedOn(s.SyncXattrs, saved.SyncXattrs) || turnedOn(s.SyncOwnership, saved.SyncOwnership) {
			return errFolderMetadataSync
		}
		if turnedOn(s.SendXattrs, saved.SendXattrs) || turnedOn(s.SendOwnership, saved.SendOwnership) {
			return errFolderMetadataSend
		}
		if !s.IgnorePerms && saved.IgnorePerms {
			return errFolderPermissions
		}
	}
	if folder.SyncOwnership && folder.CopyOwnershipFromParent {
		return errFolderOwnershipParent
	}
	return nil
}

// checkMarker refuses a marker the folder doesn't have, which would stop it. Not while changing the configuration: a slow
// disk would hold up every other change.
func (s *FolderSettings) checkMarker(folder config.FolderConfiguration) error {
	if s.MarkerName == s.saved.MarkerName {
		return nil
	}
	folder.MarkerName = markerName(s.MarkerName)
	if filepath.Clean(folder.MarkerName) == "." {
		return errMarkerIsFolder
	}
	if err := folder.CheckPath(); err != nil {
		return fmt.Errorf("marker name: %w", err)
	}
	return nil
}

// markerName is the marker as entered, or the default one for none, which is what the engine would make of that
func markerName(text string) string {
	if name := strings.TrimSpace(text); name != "" {
		return name
	}
	return config.DefaultMarkerName
}

func xattrEntryLines(entries []config.XattrFilterEntry) string {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Permit {
			lines = append(lines, xattrEntryPermit+entry.Match)
		} else {
			lines = append(lines, xattrEntryDeny+entry.Match)
		}
	}
	return strings.Join(lines, "\n")
}

func parseXattrEntries(text string) ([]config.XattrFilterEntry, error) {
	entries := []config.XattrFilterEntry{}
	for _, line := range advancedListItems(text, false) {
		pattern, permit := strings.CutPrefix(line, xattrEntryPermit)
		if !permit {
			var deny bool
			if pattern, deny = strings.CutPrefix(line, xattrEntryDeny); !deny {
				return nil, errXattrEntry
			}
		}
		if pattern == "" {
			return nil, errXattrEntry
		}
		// The engine treats a pattern it can't read as one that matches nothing, which fails no one
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("extended attribute filter: %q is not a valid pattern", pattern)
		}
		entries = append(entries, config.XattrFilterEntry{Match: pattern, Permit: permit})
	}
	return entries, nil
}

// setChoiceOption puts an edited value in its option, if it is one of the choices. An option's own parsing can't tell:
// most take anything, and read what they don't know as their first value.
func setChoiceOption[T any, P interface {
	*T
	encoding.TextUnmarshaler
}](label string, text string, saved string, choices []string, option P) error {
	if text == saved {
		return nil
	}
	if !slices.Contains(choices, text) {
		return fmt.Errorf("%s: choose one of the options", label)
	}
	return option.UnmarshalText([]byte(text))
}

// setSecondsOption puts an edited number of seconds in its option, if it is one from minimum on
func setSecondsOption(label string, text string, saved string, option *float64, minimum float64) error {
	if text == saved {
		return nil
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(number) || number < minimum || number > maxDurationSeconds {
		return fmt.Errorf("%s: enter a number of seconds from %g", label, minimum)
	}
	*option = number
	return nil
}

func setBoolOption(value bool, saved bool, option *bool) {
	if value != saved {
		*option = value
	}
}

// Settings are the folder's settings the app has a page for; nil when the folder is gone
func (fld *Folder) Settings() *FolderSettings {
	folder := fld.folderConfiguration()
	if folder == nil {
		return nil
	}
	return newFolderSettings(*folder)
}

// SetSettings saves the settings that were edited, which restarts the folder if any were. When one can't be used,
// nothing changes and the error says why.
func (fld *Folder) SetSettings(settings *FolderSettings) error {
	current := fld.folderConfiguration()
	if current == nil {
		return errFolderConfigNotFound
	}
	if settings.saved == nil {
		settings.saved = newFolderSettings(*current)
	}
	if err := settings.checkMarker(*current); err != nil {
		return err
	}
	return fld.changeFolderCopy(func(folder *config.FolderConfiguration) error {
		if settings.MarkerName != settings.saved.MarkerName {
			folder.MarkerName = markerName(settings.MarkerName)
		}
		return settings.applyTo(folder)
	})
}
