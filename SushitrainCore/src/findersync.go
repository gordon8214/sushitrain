// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/ignore"
	"github.com/syncthing/syncthing/lib/osutil"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/scanner"
	"github.com/syncthing/syncthing/lib/syncthing"
)

type finderItem struct {
	FolderID string `json:"folderID"`
	Path     string `json:"path"`
}

type finderStatus struct {
	Item        finderItem `json:"item"`
	Badge       string     `json:"badge,omitempty"`
	Indexed     bool       `json:"indexed"`
	Directory   bool       `json:"directory"`
	Versions    bool       `json:"versions"`
	RegularFile bool       `json:"regularFile"`
}

// FinderRootsJSON returns only native roots. Finder never opens the engine database itself.
func (clt *Client) FinderRootsJSON() ([]byte, error) {
	type root struct {
		FolderID string `json:"folderID"`
		Path     string `json:"path"`
	}
	roots := []root{}
	if cfg := clt.cfg(); cfg != nil {
		for _, folder := range cfg.Folders() {
			if folder.FilesystemType == config.FilesystemTypeBasic && folder.Type != config.FolderTypeReceiveEncrypted {
				roots = append(roots, root{folder.ID, folder.Filesystem().URI()})
			}
		}
	}
	slices.SortFunc(roots, func(a, b root) int { return strings.Compare(a.FolderID, b.FolderID) })
	return json.Marshal(roots)
}

func finderPathAllowed(path string) bool {
	if filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') || fs.IsInternal(path) {
		return false
	}
	if path == "" {
		return true
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".syncthing.") {
			return false
		}
	}
	return true
}

// FinderStatusJSON queries only requested entries, bounded independently of the folder's size.
func (clt *Client) FinderStatusJSON(request []byte) ([]byte, error) {
	if len(request) > 4*1024*1024 {
		return nil, errors.New("Finder request is too large")
	}
	var items []finderItem
	if err := json.Unmarshal(request, &items); err != nil {
		return nil, err
	}
	if len(items) > 4096 {
		return nil, errors.New("too many Finder items")
	}
	statuses := make([]finderStatus, 0, len(items))
	summaries := make(map[string]string)
	failed := make(map[string]map[string]bool)
	matchers := make(map[string]*ignore.Matcher)
	ignoreErrors := make(map[string]error)
	diskBatches := make(map[string]map[string]protocol.FileInfo)
	itemsByFolder := make(map[string][]finderItem)
	for _, item := range items {
		itemsByFolder[item.FolderID] = append(itemsByFolder[item.FolderID], item)
	}
	// One deadline bounds waiting for all folder loops, rather than multiplying it by the number of roots.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	for _, item := range items {
		status := finderStatus{Item: item}
		indexPath := osutil.NormalizedFilename(item.Path)
		folder := clt.FolderWithID(item.FolderID)
		if folder == nil || !finderPathAllowed(item.Path) {
			statuses = append(statuses, status)
			continue
		}
		fc := folder.folderConfiguration()
		if fc == nil || fc.FilesystemType != config.FilesystemTypeBasic || fc.Type == config.FolderTypeReceiveEncrypted {
			statuses = append(statuses, status)
			continue
		}
		// Lstat each ancestor: a Finder alias through a symlink must not query files outside the registered root.
		root := fc.Filesystem().URI()
		if !finderSafePath(root, item.Path) {
			statuses = append(statuses, status)
			continue
		}
		info, err := os.Lstat(filepath.Join(root, item.Path))
		if err != nil {
			statuses = append(statuses, status)
			continue
		}
		status.Directory = info.IsDir()
		status.RegularFile = info.Mode().IsRegular()
		if item.Path != "" {
			matcher, loaded := matchers[item.FolderID]
			if !loaded {
				matcher, ignoreErrors[item.FolderID] = finderIgnoreMatcher(folder)
				matchers[item.FolderID] = matcher
			}
			if ignoreErrors[item.FolderID] != nil || matcher == nil || matcher.Match(osutil.NativeFilename(indexPath)).IsIgnored() {
				statuses = append(statuses, status)
				continue
			}
		}
		if _, ok := summaries[item.FolderID]; !ok {
			failed[item.FolderID] = make(map[string]bool)
			summaries[item.FolderID] = ""
			if folder.IsPaused() {
				summaries[item.FolderID] = "paused"
			} else if internals := clt.internals(); internals != nil {
				failures, err := internals.FolderErrors(item.FolderID)
				if err == nil {
					for _, failure := range failures {
						failed[item.FolderID][osutil.NormalizedFilename(filepath.ToSlash(failure.Path))] = true
					}
					summaries[item.FolderID] = finderFolderBadge(folder, len(failures) > 0)
				}
			}
		}
		internals := clt.internals()
		if item.Path == "" {
			status.Badge = summaries[item.FolderID]
			statuses = append(statuses, status)
			continue
		}
		if internals == nil {
			statuses = append(statuses, status)
			continue
		}
		local, hasLocal, localErr := internals.LocalFileInfo(item.FolderID, indexPath)
		global, hasGlobal, globalErr := internals.GlobalFileInfo(item.FolderID, indexPath)
		if localErr != nil || globalErr != nil {
			statuses = append(statuses, status)
			continue
		}
		status.Indexed = hasGlobal && !global.IsDeleted()
		status.Versions = status.Indexed && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 && folder.CanBrowseVersions()
		if status.Directory {
			status.Badge = summaries[item.FolderID]
			if hasLocal && !local.IsDirectory() && status.Badge != "paused" {
				status.Badge = "waiting"
			}
		} else {
			clt.mutex.Lock()
			_, downloading := clt.downloadProgress[item.FolderID][indexPath]
			clt.mutex.Unlock()
			summary, failed := summaries[item.FolderID], failed[item.FolderID][indexPath]
			if summary == "paused" || failed || downloading || !hasLocal || local.IsIgnored() || local.IsDeleted() || local.IsInvalid() || local.MustRescan() || local.IsReceiveOnlyChanged() {
				status.Badge = finderFileBadge(local, global, hasLocal, hasGlobal, protocol.FileInfo{}, *fc, summary, failed, downloading)
			} else {
				batch, loaded := diskBatches[item.FolderID]
				if !loaded {
					// Use the same virtual modification times as scans and pulls. The handle stays inside its callback.
					err := internals.RunInFolderLoop(ctx, item.FolderID, func(ctx context.Context, handle syncthing.FolderHandle) error {
						batch = finderDiskMetadata(ctx, handle.Filesystem(), itemsByFolder[item.FolderID], item.FolderID, *fc)
						return ctx.Err()
					})
					if err != nil {
						batch = nil
					}
					diskBatches[item.FolderID] = batch
				}
				if disk, ok := batch[item.Path]; ok {
					status.Badge = finderFileBadge(local, global, hasLocal, hasGlobal, disk, *fc, summary, failed, downloading)
				}
			}
		}
		statuses = append(statuses, status)
	}
	return json.Marshal(statuses)
}

func finderSafePath(root, path string) bool {
	parts := strings.Split(path, "/")
	for i := 1; i < len(parts); i++ {
		info, err := os.Lstat(filepath.Join(append([]string{root}, parts[:i]...)...))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

func finderDiskMetadata(ctx context.Context, filesystem fs.Filesystem, items []finderItem, folderID string, fc config.FolderConfiguration) map[string]protocol.FileInfo {
	result := make(map[string]protocol.FileInfo)
	for _, item := range items {
		if ctx.Err() != nil {
			break
		}
		if item.FolderID != folderID || item.Path == "" || !finderPathAllowed(item.Path) || !finderSafePath(filesystem.URI(), item.Path) {
			continue
		}
		if _, exists := result[item.Path]; exists {
			continue
		}
		info, err := filesystem.Lstat(item.Path)
		if err != nil || info.IsDir() {
			continue
		}
		disk, err := scanner.CreateFileInfo(info, osutil.NormalizedFilename(item.Path), filesystem, false, false, fc.XattrFilter)
		if err == nil {
			result[item.Path] = disk
		}
	}
	return result
}

func finderIgnoreMatcher(folder *Folder) (*ignore.Matcher, error) {
	filesystem, err := folder.filesystem()
	if err != nil {
		return nil, err
	}
	if _, err := filesystem.Lstat(ignoreFileName); fs.IsNotExist(err) {
		// Finder already validated the requested path. Do not enumerate a potentially huge root just to establish
		// that it has no ignore file; the editor's more extensive missing-file permission check is unnecessary here.
		return ignore.New(filesystem), nil
	}
	return folder.loadIgnores()
}

func finderFolderBadge(folder *Folder, failed bool) string {
	if folder.IsPaused() {
		return "paused"
	}
	internals := folder.client.internals()
	if internals == nil {
		return ""
	}
	state, _, err := internals.FolderState(folder.FolderID)
	if err != nil || state == "error" {
		return "attention"
	}
	if failed {
		return "attention"
	}
	if state == "" {
		return ""
	}
	if folder.FolderType() == FolderTypeReceiveOnly {
		changed, err := internals.ReceiveOnlySize(folder.FolderID)
		if err != nil {
			return ""
		}
		if changed.TotalItems() > 0 {
			return "attention"
		}
	}
	if state != "idle" {
		return "syncing"
	}
	need, err := internals.NeedSize(folder.FolderID, protocol.LocalDeviceID)
	if err != nil {
		return ""
	}
	if need.TotalItems() > 0 {
		if folder.FolderType() == FolderTypeSendOnly {
			return "attention"
		}
		return "waiting"
	}
	return "current"
}

func finderFileBadge(local, global protocol.FileInfo, hasLocal, hasGlobal bool, disk protocol.FileInfo, fc config.FolderConfiguration, summary string, failed, downloading bool) string {
	// The model can return native macOS names even when the lookup used a normalized protocol path.
	local.Name = osutil.NormalizedFilename(local.Name)
	global.Name = osutil.NormalizedFilename(global.Name)
	disk.Name = osutil.NormalizedFilename(disk.Name)
	if local.IsIgnored() {
		return ""
	}
	if summary == "" {
		return ""
	}
	if summary == "paused" {
		return "paused"
	}
	if failed || (hasLocal && (local.IsInvalid() && !local.MustRescan() || local.IsReceiveOnlyChanged())) {
		return "attention"
	}
	if downloading {
		return "syncing"
	}
	if !hasLocal || local.IsDeleted() || local.MustRescan() {
		return "waiting"
	}
	if !local.IsEquivalentOptional(disk, protocol.FileInfoComparison{
		ModTimeWindow: fc.ModTimeWindow(), IgnorePerms: fc.IgnorePerms, IgnoreBlocks: true, IgnoreFlags: protocol.LocalAllFlags, IgnoreOwnership: true, IgnoreXattrs: true,
	}) {
		return "waiting"
	}
	if !hasGlobal || global.IsDeleted() || global.IsInvalid() || !local.Version.Equal(global.Version) || !local.IsEquivalentOptional(global, protocol.FileInfoComparison{
		ModTimeWindow: fc.ModTimeWindow(), IgnorePerms: fc.IgnorePerms, IgnoreOwnership: !fc.SyncOwnership, IgnoreXattrs: !fc.SyncXattrs, FilterNativeXattrs: fc.XattrFilter.Filter,
	}) {
		if fc.Type == config.FolderTypeSendOnly {
			return "attention"
		}
		return "waiting"
	}
	return "current"
}
