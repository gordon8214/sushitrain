// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"context"
	"errors"
	"path/filepath"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/desktopapi"
	"t-shaped.nl/sushitrain/v2/files"
)

func registerItems(a *adminsock.Server, s *files.Service, mount string) {
	desktopapi.RegisterItems(a, func(ctx context.Context, path string) (desktopapi.ItemStatus, error) {
		item := desktopapi.ItemStatus{Path: path}
		folders, err := s.Folders()
		if err != nil {
			return item, err
		}
		for _, folder := range folders {
			rel, err := desktopapi.Relative(filepath.Join(mount, folder.DirName), path)
			if err != nil {
				continue
			}
			entry, err := s.Resolve(filepath.ToSlash(filepath.Join(folder.DirName, rel)))
			if err != nil {
				return item, err
			}
			item.Folder, item.RelativePath, item.Directory = folder.RemoteID, rel, entry.IsDir()
			item.State, err = s.State(entry)
			if err != nil {
				return item, err
			}
			if item.State == "" {
				item.State = "remote"
			}
			item.Actions = []string{"open", "status"}
			if entry.Keep {
				item.Actions = append(item.Actions, "stop-keeping")
			} else {
				item.Actions = append(item.Actions, "keep")
			}
			if !entry.IsDir() {
				item.Actions = append(item.Actions, "versions")
			}
			return item, nil
		}
		return item, errors.New("this path is not in Synctrain's mounted folders")
	})
}
