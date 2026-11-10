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
	"sync"
	"time"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/files"
)

// mountPath turns a path the CLI was given (absolute, in the mount) into one relative to the mount
func mountPath(mount, path string) (string, error) {
	rel, err := filepath.Rel(mount, filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
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

// registerAdmin registers the daemon's methods. foldersChanged is called after a folder is added or removed.
func registerAdmin(a *adminsock.Server, s *files.Service, mount string, foldersChanged func()) {
	registerItems(a, s, mount)
	registerBrowser(a, s, mount)
	a.Register("status", func(ctx context.Context, _ json.RawMessage) (any, error) {
		gateways, err := s.Gateways()
		if err != nil {
			return nil, err
		}
		folders, err := s.Folders()
		if err != nil {
			return nil, err
		}
		return files.Status{Version: versionString(), Mount: mount, Gateways: gateways, Folders: folders}, nil
	})
	var pairingMu sync.Mutex
	pairings := map[string]context.CancelFunc{}
	a.Register("pair.cancel", func(_ context.Context, data json.RawMessage) (any, error) {
		p, err := decode[struct{ ID string }](data)
		if err != nil {
			return nil, err
		}
		pairingMu.Lock()
		stop := pairings[p.ID]
		pairingMu.Unlock()
		if stop != nil {
			stop()
		}
		return nil, nil
	})
	a.Register("pair", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ Link, ID string }](params)
		if err != nil {
			return nil, err
		}
		// Long enough for someone to approve it on the gateway; the pairing code itself lasts ten minutes
		ctx, cancel := context.WithTimeout(ctx, 11*time.Minute)
		defer cancel()
		if p.ID != "" {
			pairingMu.Lock()
			if pairings[p.ID] != nil {
				pairingMu.Unlock()
				return nil, errors.New("this pairing request is already running")
			}
			pairings[p.ID] = cancel
			pairingMu.Unlock()
			defer func() { pairingMu.Lock(); delete(pairings, p.ID); pairingMu.Unlock() }()
		}
		return s.Pair(ctx, p.Link, nil)
	})
	a.Register("gateways.forget", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ ServiceID string }](params)
		if err != nil {
			return nil, err
		}
		return nil, s.Forget(p.ServiceID)
	})
	a.Register("gateways.address", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ ServiceID, Address string }](params)
		if err != nil {
			return nil, err
		}
		return s.SetGatewayAddress(ctx, p.ServiceID, p.Address)
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
		var result []files.RemoteFolder
		for _, f := range remote {
			rf := files.RemoteFolder{ID: f.ID, Label: f.Label, Write: f.Permissions.Write, Selective: f.Selective}
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
		f, err := s.AddFolder(ctx, p.ServiceID, p.FolderID)
		if err == nil {
			foldersChanged()
		}
		return f, err
	})
	a.Register("folders.remove", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ ID int64 }](params)
		if err != nil {
			return nil, err
		}
		dest, moved, err := s.RemoveFolder(p.ID)
		if err == nil {
			foldersChanged()
		}
		return files.Removal{Leftovers: dest, Moved: moved}, err
	})
	// Every folder out of the mount, for a switch to Synchronize mode (the window stops the daemon afterwards)
	a.Register("leave", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ Timeout time.Duration }](params)
		if err != nil {
			return nil, err
		}
		if p.Timeout <= 0 || p.Timeout > 10*time.Minute {
			p.Timeout = 2 * time.Minute
		}
		ctx, cancel := context.WithTimeout(ctx, p.Timeout)
		defer cancel()
		removals, err := s.Leave(ctx)
		if len(removals) > 0 {
			foldersChanged()
		}
		return removals, err
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
		// The daemon's working directory means nothing to its clients
		if !filepath.IsAbs(p.Dest) {
			return nil, errors.New("the destination must be an absolute path")
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
	// item resolves the path a request names, in the mount
	item := func(params json.RawMessage) (files.Entry, error) {
		p, err := decode[struct{ Path string }](params)
		if err != nil {
			return files.Entry{}, err
		}
		rel, err := mountPath(mount, p.Path)
		if err != nil {
			return files.Entry{}, err
		}
		return s.Resolve(rel)
	}
	version := func(params json.RawMessage) (files.Entry, string, error) {
		e, err := item(params)
		if err != nil {
			return e, "", err
		}
		p, err := decode[struct{ Version string }](params)
		return e, p.Version, err
	}
	a.Register("versions.list", func(ctx context.Context, params json.RawMessage) (any, error) {
		e, err := item(params)
		if err != nil {
			return nil, err
		}
		return s.Versions(ctx, e.Item)
	})
	a.Register("versions.preview", func(ctx context.Context, params json.RawMessage) (any, error) {
		e, id, err := version(params)
		if err != nil {
			return nil, err
		}
		path, err := s.VersionToFile(ctx, e.Item, id, previewDir())
		return map[string]string{"file": path}, err
	})
	a.Register("versions.restore", func(ctx context.Context, params json.RawMessage) (any, error) {
		e, id, err := version(params)
		if err != nil {
			return nil, err
		}
		return nil, s.RestoreVersion(ctx, e.Item, id)
	})
	a.Register("versions.copy", func(ctx context.Context, params json.RawMessage) (any, error) {
		e, id, err := version(params)
		if err != nil {
			return nil, err
		}
		name, err := s.CopyVersion(ctx, e.Item, id)
		return map[string]string{"name": name}, err
	})
	a.Register("storage", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return s.Storage()
	})
	// For windows that poll it: unlike mcp.files.transfers, it doesn't bring bookmarks and places up to date each time
	a.Register("transfers", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return s.Transfers(ctx)
	})
	a.Register("storage.budget", func(ctx context.Context, params json.RawMessage) (any, error) {
		p, err := decode[struct{ Bytes int64 }](params)
		if err != nil {
			return nil, err
		}
		return nil, s.SetCacheBudget(p.Bytes)
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
		return files.ItemState{Path: p.Path, State: state, Keep: e.Keep}, err
	})
}

// previewDir is where versions are downloaded for previewing (and removed a day later): on disk, in this user's own
// cache, rather than the runtime directory, which is memory, or /tmp, which is anyone's
func previewDir() string {
	return filepath.Join(xdgDir("XDG_CACHE_HOME", ".cache"), "synctrain", "previews")
}
