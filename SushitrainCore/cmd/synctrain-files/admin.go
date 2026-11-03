// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/files"
)

// Status is what the status command shows
type Status struct {
	Version  string          `json:"version"`
	Mount    string          `json:"mount"`
	Gateways []files.Gateway `json:"gateways"`
	Folders  []files.Folder  `json:"folders"`
}

// RemoteFolder is a folder a gateway lets this client access, and whether it is here
type RemoteFolder struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Write    bool   `json:"write"`
	Added    bool   `json:"added"`
	DirName  string `json:"dirName,omitempty"`
	Selected bool   `json:"selective,omitempty"`
}

// Removal is where files that were only here went when their folder was removed
type Removal struct {
	Leftovers string `json:"leftovers,omitempty"`
	Moved     int    `json:"moved"`
}

// ItemState is the state of an item in the mount
type ItemState struct {
	Path  string `json:"path"`
	State string `json:"state"`
	Keep  bool   `json:"keep"`
}

// mountPath turns a path the CLI was given (absolute, in the mount) into one relative to the mount
func mountPath(mount, path string) (string, error) {
	rel, err := filepath.Rel(mount, filepath.Clean(path))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", errors.New(path + " is not in " + mount)
	}
	return filepath.ToSlash(rel), nil
}

func decode[T any](params json.RawMessage) (T, error) {
	var v T
	if len(params) == 0 {
		return v, nil
	}
	return v, json.Unmarshal(params, &v)
}

func registerAdmin(a *adminsock.Server, s *files.Service, mount string) {
	a.Register("status", func(ctx context.Context, _ json.RawMessage) (any, error) {
		gateways, err := s.Gateways()
		if err != nil {
			return nil, err
		}
		folders, err := s.Folders()
		if err != nil {
			return nil, err
		}
		return Status{Version: versionString(), Mount: mount, Gateways: gateways, Folders: folders}, nil
	})
	a.Register("pair", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ Link string }](params)
		if err != nil {
			return nil, err
		}
		// Long enough for someone to approve it on the gateway; the pairing code itself lasts ten minutes
		ctx, cancel := context.WithTimeout(ctx, 11*time.Minute)
		defer cancel()
		return s.Pair(ctx, p.Link, nil)
	})
	a.Register("gateways.forget", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ ServiceID string }](params)
		if err != nil {
			return nil, err
		}
		return nil, s.Forget(p.ServiceID)
	})
	a.Register("folders.remote", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ ServiceID string }](params)
		if err != nil {
			return nil, err
		}
		remote, err := s.RemoteFolders(ctx, p.ServiceID)
		if err != nil {
			return nil, err
		}
		here, err := s.Folders()
		if err != nil {
			return nil, err
		}
		var result []RemoteFolder
		for _, f := range remote {
			rf := RemoteFolder{ID: f.ID, Label: f.Label, Write: f.Permissions.Write, Selected: f.Selective}
			for _, h := range here {
				if h.ServiceID == p.ServiceID && h.RemoteID == f.ID {
					rf.Added, rf.DirName = true, h.DirName
				}
			}
			result = append(result, rf)
		}
		return result, nil
	})
	a.Register("folders.add", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ ServiceID, FolderID string }](params)
		if err != nil {
			return nil, err
		}
		return s.AddFolder(ctx, p.ServiceID, p.FolderID)
	})
	a.Register("folders.remove", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ ID int64 }](params)
		if err != nil {
			return nil, err
		}
		dest, moved, err := s.RemoveFolder(p.ID)
		return Removal{Leftovers: dest, Moved: moved}, err
	})
	a.Register("keep", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct {
			Path string
			Keep bool
		}](params)
		if err != nil {
			return nil, err
		}
		rel, err := mountPath(mount, p.Path)
		if err != nil {
			return nil, err
		}
		e, err := s.Resolve(rel)
		if err != nil {
			return nil, err
		}
		return nil, s.SetKeep(e.Item, p.Keep)
	})
	a.Register("pending.list", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return s.Edits()
	})
	a.Register("pending.export", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct {
			ID   int64
			Dest string
		}](params)
		if err != nil {
			return nil, err
		}
		return nil, s.ExportEdit(p.ID, p.Dest)
	})
	a.Register("pending.discard", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ ID int64 }](params)
		if err != nil {
			return nil, err
		}
		return nil, s.DiscardEdit(p.ID)
	})
	a.Register("wait", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct {
			Path    string
			Timeout time.Duration
		}](params)
		if err != nil {
			return nil, err
		}
		rel, err := mountPath(mount, p.Path)
		if err != nil {
			return nil, err
		}
		e, err := s.Resolve(rel)
		if err != nil {
			return nil, err
		}
		if p.Timeout <= 0 || p.Timeout > 10*time.Minute {
			p.Timeout = 10 * time.Minute
		}
		ctx, cancel := context.WithTimeout(ctx, p.Timeout)
		defer cancel()
		return nil, s.Wait(ctx, e.Item)
	})
	a.Register("state", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ Path string }](params)
		if err != nil {
			return nil, err
		}
		rel, err := mountPath(mount, p.Path)
		if err != nil {
			return nil, err
		}
		e, err := s.Resolve(rel)
		if err != nil {
			return nil, err
		}
		state, err := s.State(e)
		return ItemState{Path: p.Path, State: state, Keep: e.Keep}, err
	})
}
