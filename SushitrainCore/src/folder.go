// Copyright (C) 2024-2026 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/ignore"
	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/protocol"

	"t-shaped.nl/sushitrain/v2/gateway"
)

const (
	ignoreFileName  string = ".stignore"
	versionsDirName string = ".stversions"
)

const (
	VersioningTypeNone      = ""
	VersioningTypeSimple    = "simple"
	VersioningTypeTrashcan  = "trashcan"
	VersioningTypeStaggered = "staggered"
	VersioningTypeExternal  = "external"
)

// CachedIgnore is what a folder's .stignore says, as of the modification time of the file. Reading the file means
// compiling every line.
type CachedIgnore struct {
	mutex   sync.Mutex
	matcher *ignore.Matcher
	modTime time.Time
}

// forget makes the next load read the file
func (cache *CachedIgnore) forget() {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	cache.matcher = nil
}

type Folder struct {
	client   *Client
	FolderID string
}

func (fld *Folder) folderConfiguration() *config.FolderConfiguration {
	// Only this folder's configuration is copied: copying all of them made listing folders cost their count squared
	folderInfo, ok := fld.client.cfg().Folder(fld.FolderID)
	if !ok {
		return nil
	}
	return &folderInfo
}

func (fld *Folder) RescanSubdirectory(path string) error {
	go func() {
		slog.Info("rescan folder", "folderID", fld.FolderID, "subdirectory", path)
		fld.client.scanFolderSubdirs(fld.FolderID, []string{path})
	}()
	return nil
}

func (fld *Folder) Rescan() error {
	go func() {
		slog.Info("rescan", "folder", fld.FolderID)
		fld.client.scanFolderSubdirs(fld.FolderID, nil)
	}()
	return nil
}

func (fld *Folder) RescanIntervalSeconds() int {
	fc := fld.folderConfiguration()
	if fc == nil {
		return 0
	}

	return fc.RescanIntervalS
}

func (fld *Folder) SetRescanInterval(seconds int) error {
	return fld.changeFolderConfiguration(func(config *config.FolderConfiguration) {
		config.RescanIntervalS = seconds
	})
}

func (fld *Folder) WatcherDelaySeconds() int {
	fc := fld.folderConfiguration()
	if fc == nil {
		return 0
	}

	return int(fc.FSWatcherDelayS)
}

func (fld *Folder) SetWatcherDelaySeconds(seconds int) error {
	return fld.changeFolderConfiguration(func(config *config.FolderConfiguration) {
		config.FSWatcherDelayS = float64(seconds)
	})
}

func (fld *Folder) Unlink() error {
	fc := fld.folderConfiguration()
	if fc == nil {
		return errors.New("folder does not exist")
	}
	err := fld.client.changeConfiguration(func(cfg *config.Configuration) {
		folders := make([]config.FolderConfiguration, 0)
		for _, fc := range cfg.Folders {
			if fc.ID != fld.FolderID {
				folders = append(folders, fc)
			}
		}
		cfg.Folders = folders
	})

	if err != nil {
		return err
	}

	return nil
}

func (fld *Folder) filesystem() (fs.Filesystem, error) {
	fc := fld.folderConfiguration()
	if fc == nil {
		return nil, errors.New("folder does not exist")
	}
	return fc.Filesystem(), nil
}

// Remove removes the folder and its files (into the Trash when the folder uses it), only when CanRemoveFiles
func (fld *Folder) Remove() error {
	// The directory that is checked is the one that is removed: both come from this configuration
	cfg := fld.client.cfg()
	if cfg == nil {
		return ErrStillLoading
	}
	fc, ok := cfg.Folder(fld.FolderID)
	if !ok {
		return errors.New("folder does not exist")
	}
	ffs := fc.Filesystem()
	if !fld.client.canRemoveFiles(fc, cfg.FolderList(), ffs.URI()) {
		return ErrFolderFilesNotRemovable
	}

	if fld.IsTrashEnabled() {
		// Stop the folder before moving its root. Keep its configuration until
		// trashing succeeds, so failures remain visible and the user can retry.
		if err := fld.SetPaused(true); err != nil {
			return err
		}
		if err := ffs.RemoveAll(""); err != nil {
			return fmt.Errorf("folder remains paused; could not move it to Trash: %w", err)
		}
		return fld.Unlink()
	}

	err := fld.Unlink()
	if err != nil {
		return err
	}

	// Remove local copy
	return ffs.RemoveAll("")
}

func (fld *Folder) Exists() bool {
	return fld.folderConfiguration() != nil
}

func (fld *Folder) IsPaused() bool {
	fc := fld.folderConfiguration()
	if fc == nil {
		return false
	}

	return fld.folderConfiguration().Paused
}

func (fld *Folder) SetPaused(paused bool) error {
	return fld.changeFolderConfiguration(func(config *config.FolderConfiguration) {
		config.Paused = paused
	})
}

func (fld *Folder) IsWatcherEnabled() bool {
	fc := fld.folderConfiguration()
	if fc == nil {
		return false
	}

	return fld.folderConfiguration().FSWatcherEnabled
}

func (fld *Folder) SetWatcherEnabled(enabled bool) error {
	return fld.changeFolderConfiguration(func(config *config.FolderConfiguration) {
		config.FSWatcherEnabled = enabled
	})
}

// See documentation; -1 means 'automatically determined number', 0 means disabled.
func (fld *Folder) MaxConflicts() int {
	fc := fld.folderConfiguration()
	if fc == nil {
		return -1
	}

	return fld.folderConfiguration().MaxConflicts
}

func (fld *Folder) SetMaxConflicts(mx int) error {
	return fld.changeFolderConfiguration(func(config *config.FolderConfiguration) {
		config.MaxConflicts = mx
	})
}

func (fld *Folder) VersioningType() string {
	fc := fld.folderConfiguration()
	if fc == nil {
		return ""
	}

	return fc.Versioning.Type
}

func (fld *Folder) VersioningPath() string {
	fc := fld.folderConfiguration()
	if fc == nil || fc.Versioning.Type == "" {
		return ""
	}

	ffs := fc.Filesystem()
	if fc.Versioning.FSPath == "" {
		return filepath.Join(ffs.URI(), versionsDirName)
	}

	if fc.Versioning.FSType == config.FilesystemTypeBasic {
		path, err := fs.ExpandTilde(fc.Versioning.FSPath)
		if err != nil {
			path = fc.Versioning.FSPath
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(ffs.URI(), path)
		}
		return path
	}

	return ""
}

// State is the folder's state as the engine names it, which is "error" when it stopped on one (see StateError), and empty
// while the folder isn't running. These are two calls as gomobile returns no value along with an error.
func (fld *Folder) State() string {
	state, _ := fld.stateAndError()
	return state
}

// StateError is the engine's message when the folder stopped on an error, and empty otherwise
func (fld *Folder) StateError() string {
	if _, err := fld.stateAndError(); err != nil {
		return err.Error()
	}
	return ""
}

func (fld *Folder) stateAndError() (string, error) {
	internals := fld.client.internals()
	if internals == nil {
		return "", nil
	}
	state, _, err := internals.FolderState(fld.FolderID)
	return state, err
}

func (fld *Folder) GetFileInformation(path string) (*Entry, error) {
	m := fld.client.internals()
	if m == nil {
		return nil, nil
	}

	if len(path) == 0 {
		return nil, errors.New("empty path")
	}

	// Strip initial slash
	if path[0] == '/' {
		path = path[1:]
	}

	info, ok, err := m.GlobalFileInfo(fld.FolderID, path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return &Entry{
		info:   info,
		Folder: fld,
	}, nil
}

func (fld *Folder) listEntries(prefix string, directories bool, recurse bool) ([]*model.TreeEntry, error) {
	if fld.client.internals() == nil {
		return nil, nil
	}

	levels := 0
	if recurse {
		levels = -1
	}

	return fld.client.internals().GlobalTree(fld.FolderID, prefix, levels, directories)
}

func flatten(entries []*model.TreeEntry, recurse bool, prefix string) []string {
	newEntries := make([]string, 0)
	for e := range entries {
		entry := entries[e]
		newEntries = append(newEntries, prefix+entry.Name)
		if recurse {
			newEntries = append(newEntries, flatten(entry.Children, recurse, prefix+entry.Name+"/")...)
		}
	}
	return newEntries
}

/*
Returns a list of paths that appear in the folder withn the given prefix, optionally also returning paths for directories,
and optionally recursing. If recursing, the containing directory entries are guaranteed to occur in the list before their
children entries.
*/
func (fld *Folder) List(prefix string, directories bool, recurse bool) (*ListOfStrings, error) {
	entries, err := fld.listEntries(prefix, directories, recurse)
	if err != nil {
		return nil, err
	}

	// Flatten the tree
	names := flatten(entries, recurse, "")
	return List(names), nil
}

func (fld *Folder) ShareWithDevice(deviceID string, toggle bool, encryptionPassword string) error {
	devID, err := protocol.DeviceIDFromString(deviceID)
	if err != nil {
		return err
	}

	err = fld.client.changeConfiguration(func(cfg *config.Configuration) {
		fc := fld.folderConfiguration()
		if fc == nil {
			return
		}

		devices := make([]config.FolderDeviceConfiguration, 0)
		for _, fc := range fc.Devices {
			if fc.DeviceID != devID {
				devices = append(devices, fc)
			}
		}
		fc.Devices = devices

		if toggle {
			fc.Devices = append(fc.Devices, config.FolderDeviceConfiguration{
				DeviceID:           devID,
				EncryptionPassword: encryptionPassword,
			})
		}

		cfg.SetFolder(*fc)
	})
	return err
}

func (fld *Folder) sharedWith() ([]protocol.DeviceID, error) {
	fc := fld.folderConfiguration()
	if fc == nil {
		return nil, errors.New("folder configuration does not exist")
	}

	return fc.DeviceIDs(), nil
}

func (fld *Folder) IsSharedWithDeviceID(deviceID string) bool {
	devIDs, err := fld.sharedWith()
	if err != nil {
		return false
	}

	for _, devID := range devIDs {
		if devID.String() == deviceID {
			return true
		}
	}
	return false
}

func (fld *Folder) SharedWithDeviceIDs() *ListOfStrings {
	devIDs, err := fld.sharedWith()
	if err != nil {
		return nil
	}

	return List(Map(devIDs, func(di protocol.DeviceID) string {
		return di.String()
	}))
}

func (fld *Folder) SharedEncryptedWithDeviceIDs() *ListOfStrings {
	fc := fld.folderConfiguration()
	if fc == nil {
		return nil
	}
	var dis = make([]string, 0)

	for _, dfc := range fc.Devices {
		if len(dfc.EncryptionPassword) > 0 {
			dis = append(dis, dfc.DeviceID.String())
		}
	}

	return List(dis)
}

func (fld *Folder) EncryptionPasswordFor(peer string) string {
	did, err := protocol.DeviceIDFromString(peer)
	if err != nil {
		return ""
	}

	fc := fld.folderConfiguration()
	if fc == nil {
		return ""
	}

	for _, dfc := range fc.Devices {
		if dfc.DeviceID == did {
			return dfc.EncryptionPassword
		}
	}
	return ""
}

func (fld *Folder) ConnectedPeerCount() int {
	fc := fld.folderConfiguration()
	if fc == nil {
		return 0
	}

	devIDs := fld.folderConfiguration().DeviceIDs()
	connected := 0
	for _, devID := range devIDs {
		if devID == fld.client.deviceID() {
			continue
		}
		if fld.client.internals().IsConnectedTo(devID) {
			connected++
		}
	}
	return connected
}

func (fld *Folder) Group() string {
	fc := fld.folderConfiguration()
	if fc == nil {
		return ""
	}
	return fc.Group
}

func (fld *Folder) SetGroup(group string) error {
	return fld.changeFolderConfiguration(func(config *config.FolderConfiguration) {
		config.Group = group
	})
}

func (fld *Folder) Label() string {
	fc := fld.folderConfiguration()
	if fc == nil {
		return ""
	}
	return fc.Label
}

func (fld *Folder) SetLabel(label string) error {
	return fld.changeFolderConfiguration(func(config *config.FolderConfiguration) {
		config.Label = label
	})
}

var (
	errNoClient             = errors.New("client not started up yet")
	errFolderConfigNotFound = errors.New("folder config not founnd")
)

const (
	FolderTypeSendReceive      = "sendrecieve"
	FolderTypeReceiveOnly      = "receiveonly"
	FolderTypeSendOnly         = "sendonly"
	FolderTypeReceiveEncrypted = "receiveencrypted"
)

func (fld *Folder) FolderType() string {
	fc := fld.folderConfiguration()
	if fc == nil {
		return ""
	}

	switch fc.Type {
	case config.FolderTypeReceiveOnly:
		return FolderTypeReceiveOnly
	case config.FolderTypeSendReceive:
		return FolderTypeSendReceive
	case config.FolderTypeSendOnly:
		return FolderTypeSendOnly
	case config.FolderTypeReceiveEncrypted:
		return FolderTypeReceiveEncrypted
	default:
		return ""
	}
}

// Returns true when this folder is 'external', i.e. some other app's folder. Outside the App Sandbox on macOS every
// folder is the user's, wherever it is, so none is.
func (fld *Folder) IsExternal() (bool, error) {
	fc := fld.folderConfiguration()
	if fc == nil {
		return false, errors.New("cannot obtain folder configuration")
	}
	if fld.client.folderLayout == homeFolders {
		return false, nil
	}

	return !fld.client.isInternalFolderPath(fld.FolderID, fc.Path), nil
}

func (fld *Folder) Path() string {
	fc := fld.folderConfiguration()
	if fc == nil {
		return ""
	}
	return fc.Path
}

func (fld *Folder) SetPath(path string) error {
	return fld.changeFolderConfiguration(func(config *config.FolderConfiguration) {
		config.Path = path
	})
}

func (fld *Folder) FilesystemType() string {
	fc := fld.folderConfiguration()
	if fc == nil {
		return ""
	}
	return fc.FilesystemType.String()
}

func (fld *Folder) SetFolderType(folderType string) error {
	return fld.changeFolderConfiguration(func(fc *config.FolderConfiguration) {
		switch folderType {
		case FolderTypeReceiveOnly:
			fc.Type = config.FolderTypeReceiveOnly
		case FolderTypeSendReceive:
			fc.Type = config.FolderTypeSendReceive
		case FolderTypeSendOnly:
			fc.Type = config.FolderTypeSendOnly
		case FolderTypeReceiveEncrypted:
			fc.Type = config.FolderTypeReceiveEncrypted
		default:
			// Don't change
			return
		}
	})
}

// IsSelective reports whether the folder's ignore patterns keep only a selection of its files on this device, as the
// remote file service sees it on every host, or an error if the engine isn't running. It follows the engine's copy of
// the patterns, so a hand edit of .stignore shows at the next scan.
func (fld *Folder) IsSelective() (bool, error) {
	if fld.client == nil {
		return false, errNoClient
	}
	r := fld.client.currentRun()
	if r == nil || r.internals.Load() == nil {
		return false, errNoClient
	}
	if r.isClosed() {
		return false, fld.client.errNotRunning()
	}
	if _, ok := r.config.Folder(fld.FolderID); !ok {
		return false, errors.New("folder does not exist")
	}
	return gateway.SelectiveFolders(r.app, r.config)(fld.FolderID), nil
}

func (fld *Folder) LocalNativePath() (string, error) {
	fc := fld.folderConfiguration()
	if fc == nil {
		return "", errors.New("folder does not exist")
	}

	// This is a bit of a hack, according to similar code in model.warnAboutOverwritingProtectedFiles :-)
	ffs := fld.folderConfiguration().Filesystem()
	if ffs.Type() != fs.FilesystemTypeBasic {
		return "", errors.New("unsupported FS type")
	}
	return ffs.URI(), nil
}

func (fld *Folder) loadIgnores() (*ignore.Matcher, error) {
	cfg := fld.folderConfiguration()
	if cfg == nil {
		return nil, errors.New("folder does not exist")
	}

	return loadIgnoreMatcher(cfg.Filesystem(), fld.ignoreCache())
}

// ignoreCache returns the folder's cache, which is the client's: FolderWithID returns a new Folder for every call
func (fld *Folder) ignoreCache() *CachedIgnore {
	cache, _ := fld.client.ignoreCaches.LoadOrStore(fld.FolderID, &CachedIgnore{})
	return cache.(*CachedIgnore)
}

func (fld *Folder) forgetIgnores() {
	fld.ignoreCache().forget()
}

func loadIgnoreMatcher(ffs fs.Filesystem, cache *CachedIgnore) (*ignore.Matcher, error) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	stat, statErr := ffs.Lstat(ignoreFileName)
	if statErr != nil && !fs.IsNotExist(statErr) {
		return nil, statErr
	}
	// Metadata can remain readable after permission to open the file is revoked.
	if cache.matcher != nil && statErr == nil && !cache.modTime.IsZero() && stat.ModTime().Equal(cache.modTime) {
		file, err := ffs.OpenFile(ignoreFileName, fs.OptReadOnly|fs.OptFollow, 0o666)
		if err != nil {
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		return cache.matcher, nil
	}

	ignores := ignore.New(ffs)
	if err := ignores.Load(ignoreFileName); err != nil {
		if !fs.IsNotExist(err) {
			return nil, err
		}
		// ENOENT can mean the root is missing, not just the ignore file.
		if _, rootErr := ffs.DirNames("."); rootErr != nil {
			return nil, fmt.Errorf("cannot read folder root: %w", rootErr)
		}
		// Only forgive absence of the top-level ignore file, never a load error
		// for a file that exists (for example a missing include or dangling link).
		if _, checkErr := ffs.Lstat(ignoreFileName); !fs.IsNotExist(checkErr) {
			return nil, err
		}
		cache.matcher, cache.modTime = nil, time.Time{}
		return ignores, nil
	}
	if statErr == nil {
		cache.modTime = stat.ModTime()
		cache.matcher = ignores
	}
	return ignores, nil
}

func deleteEmptyParentDirectories(ffs fs.Filesystem, path string) {
	// Try to delete parent directories that are empty
	pathParts := fs.PathComponents(path)
	if len(pathParts) > 1 {
		for pathIndex := len(pathParts) - 2; pathIndex >= 0; pathIndex-- {
			dirPath := strings.Join(pathParts[0:pathIndex+1], string(fs.PathSeparator))
			ffs.Remove(dirPath) // Will only remove directories when empty
		}
	}
}

// Remove the file at `path`, then the parent directories that leaves empty. A directory that isn't empty stays, with an
// error.
func (fld *Folder) deleteLocalFile(path string) error {
	ffs := fld.folderConfiguration().Filesystem()
	if err := ffs.Remove(path); err != nil && !fs.IsNotExist(err) {
		return err
	}
	deleteEmptyParentDirectories(ffs, path)
	return nil
}

// IsPathSelected checks the cached ignore rules even for paths absent from the index.
func (fld *Folder) IsPathSelected(path string) (bool, error) {
	matcher, err := fld.loadIgnores()
	if err != nil {
		return false, err
	}
	return !matcher.Match(path).IsIgnored(), nil
}

func (fld *Folder) IgnoreLines() (*ListOfStrings, error) {
	// Load ignores from file
	ignores, err := fld.loadIgnores()
	if err != nil {
		return nil, err
	}
	return List(ignores.Lines()), nil
}

// This overwrites the ignore file with the given lines
func (fld *Folder) SetIgnoreLines(lines *ListOfStrings) error {
	if _, err := fld.loadIgnores(); err != nil {
		return err
	}

	slog.Info("set ignore", "lines", len(lines.data))
	fld.forgetIgnores()

	state, err := fld.stateAndError()
	if err != nil {
		return err
	}
	if state != "idle" && state != "syncing" {
		return errors.New("cannot change ignore lines when not idle or syncing")
	}

	// Save new ignores (this triggers a reload of ignores and eventually a scan)
	err = fld.client.internals().SetIgnores(fld.FolderID, lines.data)
	if err != nil {
		return err
	}

	return nil
}

func (fld *Folder) Statistics() (*FolderStats, error) {
	if fld.client.internals() == nil {
		return nil, ErrStillLoading
	}

	internals := fld.client.internals()
	globalSize, err := internals.GlobalSize(fld.FolderID)
	if err != nil {
		return nil, err
	}

	localSize, err := internals.LocalSize(fld.FolderID)
	if err != nil {
		return nil, err
	}

	needSize, err := internals.NeedSize(fld.FolderID, protocol.LocalDeviceID)
	if err != nil {
		return nil, err
	}

	return &FolderStats{
		Global:    newFolderCounts(globalSize),
		Local:     newFolderCounts(localSize),
		LocalNeed: newFolderCounts(needSize),
	}, nil
}

type Completion struct {
	CompletionPct float64
	GlobalBytes   int64
	NeedBytes     int64
	GlobalItems   int
	NeedItems     int
	NeedDeletes   int
	Sequence      int64
}

func (fld *Folder) CompletionForDevice(deviceID string) (*Completion, error) {
	if fld.client.internals() == nil {
		return nil, ErrStillLoading
	}

	devID, err := protocol.DeviceIDFromString(deviceID)
	if err != nil {
		return nil, err
	}

	completion, err := fld.client.internals().Completion(devID, fld.FolderID)
	if err != nil {
		return nil, err
	}

	ourCompletion := Completion{
		CompletionPct: completion.CompletionPct,
		GlobalBytes:   completion.GlobalBytes,
		NeedBytes:     completion.NeedBytes,
		GlobalItems:   completion.GlobalItems,
		NeedItems:     completion.NeedItems,
		NeedDeletes:   completion.NeedDeletes,
		Sequence:      completion.Sequence,
	}

	return &ourCompletion, nil
}

func (fld *Folder) FilesNeeded() (*ListOfStrings, error) {
	files := make([]string, 0)

	page := 1
	perPage := 512
	for {
		// Three different statuses are reported, throw all on a big list for now
		progress, queued, rest, err := fld.client.internals().NeedFolderFiles(fld.FolderID, page, perPage)
		if err != nil {
			return nil, err
		}

		batch := append(append(progress, queued...), rest...)

		if len(batch) == 0 {
			break
		}

		for _, fi := range batch {
			files = append(files, fi.FileName())
		}

		page += 1
	}

	return List(files), nil
}

func (fld *Folder) FilesNeededBy(peer string) (*ListOfStrings, error) {
	var devID protocol.DeviceID
	var err error
	if devID, err = protocol.DeviceIDFromString(peer); err != nil {
		return nil, err
	}

	files := make([]string, 0)

	page := 1
	perPage := 512
	for {
		batch, err := fld.client.internals().RemoteNeedFolderFiles(fld.FolderID, devID, page, perPage)
		if err != nil {
			return nil, err
		}

		if len(batch) == 0 {
			break
		}

		for _, fi := range batch {
			files = append(files, fi.FileName())
		}

		page += 1
	}

	return List(files), nil
}

// LocalSequence is the sequence of this device's latest change to the folder's index. Unlike FilesNeededBy it also
// answers for a paused folder, which Syncthing no longer runs.
func (fld *Folder) LocalSequence() (int64, error) {
	if fld.client.internals() == nil {
		return 0, ErrStillLoading
	}
	return fld.client.internals().LocalSequence(fld.FolderID)
}

func (fld *Folder) IsDiskSpaceSufficient() bool {
	if minFree := fld.folderConfiguration().MinDiskFree; minFree.Value > 0 {
		fs := fld.folderConfiguration().Filesystem()

		if usage, err := fs.Usage("."); err == nil {
			if err = config.CheckFreeSpace(minFree, usage); err != nil {
				return false
			}
		}
	}

	return fld.client.IsDiskSpaceSufficient()
}

func (fld *Folder) IsBlockIndexingEnabled() bool {
	fc := fld.folderConfiguration()
	if fc == nil {
		return false
	}
	return fc.BlockIndexing
}

// changeFolderCopy gives block a copy of the folder's configuration, which replaces it only if block returns no error.
// That error comes back as it is.
func (fld *Folder) changeFolderCopy(block func(*config.FolderConfiguration) error) error {
	var invalid error
	err := fld.client.changeConfiguration(func(cfg *config.Configuration) {
		folder, _, ok := cfg.Folder(fld.FolderID)
		if !ok {
			invalid = errFolderConfigNotFound
			return
		}
		changed := folder.Copy()
		if invalid = block(&changed); invalid == nil {
			cfg.SetFolder(changed)
		}
	})
	if invalid != nil {
		return invalid
	}
	return err
}

func (fld *Folder) changeFolderConfiguration(modifier func(*config.FolderConfiguration)) error {
	var innerErr error = nil
	err := fld.client.changeConfiguration(func(cfg *config.Configuration) {
		config := fld.folderConfiguration()
		if config != nil {
			modifier(config)
			cfg.SetFolder(*config)
		} else {
			innerErr = errFolderConfigNotFound
		}
	})
	if err != nil {
		return err
	}
	if innerErr != nil {
		return innerErr
	}
	return nil
}

func (fld *Folder) SetBlockIndexingEnabled(enabled bool) error {
	return fld.changeFolderConfiguration(func(config *config.FolderConfiguration) {
		slog.Info("setting folder block indexing", "enabled", enabled, "folderID", fld.FolderID)
		config.BlockIndexing = enabled
	})
}
