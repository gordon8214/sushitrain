// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"t-shaped.nl/sushitrain/v2/desktopapi"
)

// indexedState compares index versions and disk metadata, never file contents.
func indexedState(data []byte, info os.FileInfo) string {
	type indexed struct {
		Version          string
		Size, ModifiedNs int64
		Deleted, Invalid bool
	}
	var state struct{ Local, Global *indexed }
	if json.Unmarshal(data, &state) != nil || state.Local == nil || state.Global == nil {
		return "unknown"
	}
	l, g := state.Local, state.Global
	if l.Invalid || g.Invalid {
		return "error"
	}
	if l.Deleted || g.Deleted {
		return "syncing"
	}
	if l.Version != g.Version || l.Size != g.Size || l.ModifiedNs != g.ModifiedNs {
		return "syncing"
	}
	if !info.IsDir() && (info.Size() != l.Size || info.ModTime().UnixNano() != l.ModifiedNs) {
		return "scanning"
	}
	return "synced"
}

func (c *Control) itemStatus(ctx context.Context, path string) (desktopapi.ItemStatus, error) {
	item := desktopapi.ItemStatus{Path: path, State: "unknown"}
	var folders []Folder
	result, err := c.Call(ctx, "engine.folders", Request{})
	if err != nil {
		return item, err
	}
	folders, ok := result.([]Folder)
	if !ok {
		return item, errors.New("folders are unavailable")
	}
	// Nested folders belong to their most specific configured root.
	sort.Slice(folders, func(i, j int) bool { return len(folders[i].Path) > len(folders[j].Path) })
	for _, folder := range folders {
		rel, err := desktopapi.Relative(folder.Path, path)
		if err != nil {
			continue
		}
		root, err := filepath.EvalSymlinks(folder.Path)
		if err != nil {
			return item, err
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return item, err
		}
		if resolved != filepath.Join(root, filepath.FromSlash(rel)) {
			return item, errors.New("symbolic links are not managed paths")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return item, err
		}
		item.Folder, item.RelativePath, item.Directory = folder.ID, rel, info.IsDir()
		item.Actions = []string{"open", "status", "rescan"}
		if info.Mode().IsRegular() {
			item.Actions = append(item.Actions, "versions")
		}
		f, err := c.folder(folder.ID)
		if err != nil {
			return item, err
		}
		if rel != "" {
			selected, err := f.IsPathSelected(rel)
			if err != nil {
				return item, err
			}
			if !selected {
				item.State = "ignored"
				return item, nil
			}
		}
		switch {
		case folder.Paused:
			item.State = "paused"
		case folder.Error != "" || folder.Failed > 0 || f.HasUnresolvedItems():
			item.State = "error"
		case folder.State != "idle":
			item.State = folder.State
		case rel == "" || info.IsDir():
			completion, err := f.CompletionForDevice(c.Client.DeviceID())
			if err != nil {
				return item, err
			}
			item.State = "synced"
			if completion.NeedItems > 0 || completion.NeedDeletes > 0 || completion.NeedBytes > 0 {
				item.State = "syncing"
			}
		default:
			state, err := f.FileStateJSON(rel)
			if err != nil {
				return item, err
			}
			item.State = indexedState(state, info)
		}
		return item, nil
	}
	return item, errors.New("this path is not in a synchronized folder")
}
