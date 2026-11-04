// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
// Package mcpfiles adapts the existing Files service without touching its stores.
package mcpfiles

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/mcpapi"
	"t-shaped.nl/sushitrain/v2/mcpcontrol"
	"t-shaped.nl/sushitrain/v2/safecopy"
)

type Lease func(context.Context, func(context.Context, *files.Service, string) (any, error)) (any, error)
type request struct {
	Item, Parent, ID, Offset, Limit, Bytes                               int64
	Node                                                                 uint64
	Name, NewName, Path, Dest, ServiceID, FolderID, Link, Version, Token string
	Keep                                                                 bool
}

func Register(a *adminsock.Server, lease Lease) {
	for _, method := range []string{"folders", "gateways", "add", "remove", "pair", "forget", "available", "children", "search", "inspect", "read", "replace", "mkdir", "import", "rename", "move", "delete", "download", "keep", "wait", "versions", "versionRestore", "versionExport", "versionCopy", "pending.list", "pending.export", "pending.discard", "transfers", "storage", "storage.budget"} {
		a.Register("mcp.files."+method, func(ctx context.Context, b json.RawMessage) (any, error) {
			var values map[string]any
			if err := json.Unmarshal(b, &values); err != nil {
				return nil, err
			}
			for _, key := range []string{"ID", "Item", "Parent"} {
				if text, ok := values[key].(string); ok {
					if id, err := strconv.ParseInt(text, 10, 64); err == nil {
						values[key] = id
					}
				}
			}
			b, _ = json.Marshal(values)
			var p request
			if err := json.Unmarshal(b, &p); err != nil {
				return nil, err
			}
			return lease(ctx, func(ctx context.Context, s *files.Service, mount string) (any, error) {
				return call(ctx, s, mount, method, p)
			})
		})
	}
}
func call(ctx context.Context, s *files.Service, mount, method string, p request) (any, error) {
	switch method {
	case "transfers":
		return s.Transfers(ctx)
	case "folders":
		return s.Folders()
	case "gateways":
		return s.Gateways()
	case "add":
		return s.AddFolder(ctx, p.ServiceID, p.FolderID)
	case "remove":
		id, err := strconv.ParseInt(p.FolderID, 10, 64)
		if p.Item != 0 {
			id = p.Item
			err = nil
		}
		if err != nil {
			return nil, errors.New("use the local numeric folder identity from folders_list")
		}
		dest, moved, err := s.RemoveFolder(id)
		return files.Removal{Leftovers: dest, Moved: moved}, err
	case "pair":
		return s.Pair(ctx, p.Link, nil)
	case "forget":
		return nil, s.Forget(p.ServiceID)
	case "available":
		return s.RemoteFolders(ctx, p.ServiceID)
	case "children":
		return s.Children(p.Parent)
	case "search":
		queue := []int64{p.Parent}
		out := []files.Entry{}
		visited := 0
		for len(queue) > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			parent := queue[0]
			queue = queue[1:]
			entries, err := s.Children(parent)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				visited++
				if visited > 100000 {
					return nil, errors.New("search exceeds 100000 items; narrow the subtree")
				}
				if strings.Contains(strings.ToLower(e.Name), strings.ToLower(p.Name)) {
					out = append(out, e)
				}
				if e.IsDir() {
					queue = append(queue, e.Item)
				}
			}
		}
		return out, nil
	case "mkdir":
		e, err := s.Create(p.Parent, p.Name, true)
		if err != nil {
			return nil, err
		}
		return mutationResult(ctx, s, e)
	case "import":
		input, err := safecopy.OpenRegular(p.Path)
		if err != nil {
			return nil, err
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil {
			return nil, err
		}
		name := p.Name
		if name == "" {
			name = filepath.Base(p.Path)
		}
		e, err := s.CreateSnapshot(ctx, p.Parent, name, false, input, info.Size(), info.ModTime())
		if err != nil {
			return nil, err
		}
		return mutationResult(ctx, s, e)
	case "pending.list":
		return s.Edits()
	case "pending.export":
		return nil, s.ExportEdit(p.ID, p.Dest)
	case "pending.discard":
		return nil, s.DiscardEdit(p.ID)
	case "storage":
		return s.Storage()
	case "storage.budget":
		return nil, s.SetCacheBudget(p.Bytes)
	case "versions":
		return s.HistoryVersions(ctx, p.Item)
	case "versionRestore":
		e, err := s.RestoreHistoryVersion(ctx, p.Item, p.Version, p.Token)
		if err != nil {
			return nil, err
		}
		return e, s.Wait(ctx, e.Item)
	case "versionCopy":
		e, err := s.CopyHistoryVersion(ctx, p.Item, p.Version, p.Token)
		if err != nil {
			return nil, err
		}
		return e, s.Wait(ctx, e.Item)
	case "versionExport":
		return nil, s.ExportHistoryVersion(ctx, p.Item, p.Version, p.Token, p.Path, mount)
	}
	e, ok, err := s.Item(p.Item)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("item no longer exists")
	}
	if method == "inspect" {
		state, err := s.State(e)
		return map[string]any{"entry": e, "state": state}, err
	}
	if method == "read" || method == "wait" || method == "download" {
		r, err := s.OpenNode(e)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		if method == "read" {
			if p.Limit == 0 {
				p.Limit = mcpapi.MaxContent
			}
			if p.Limit < 1 || p.Limit > mcpapi.MaxContent || p.Offset < 0 || p.Offset > r.Size() {
				return nil, errors.New("invalid content range; maximum 65536 bytes")
			}
			data := make([]byte, p.Limit)
			n, err := r.ReadAt(ctx, data, p.Offset)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			encoding, content := "utf-8", string(data[:n])
			if !utf8.Valid(data[:n]) {
				encoding = "base64"
				content = base64.StdEncoding.EncodeToString(data[:n])
			}
			return map[string]any{"content": content, "encoding": encoding, "offset": p.Offset, "bytes": n, "nextOffset": p.Offset + int64(n), "size": r.Size(), "eof": p.Offset+int64(n) >= r.Size()}, nil
		}
		var destination *safecopy.Destination
		if method == "download" {
			destination, err = safecopy.Create(p.Path, mount)
			if err != nil {
				return nil, err
			}
			defer destination.Close()
		}
		buffer := make([]byte, 256<<10)
		for offset := int64(0); offset < r.Size(); {
			n, err := r.ReadAt(ctx, buffer, offset)
			if n > 0 && destination != nil {
				if _, err := destination.File.Write(buffer[:n]); err != nil {
					return nil, err
				}
			}
			offset += int64(n)
			mcpcontrol.ReportProgress(ctx, offset, r.Size(), "materializing")
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			if n == 0 {
				return nil, io.ErrUnexpectedEOF
			}
		}
		if destination != nil {
			return nil, destination.Commit()
		}
		return map[string]any{"item": e.Item, "materialized": true}, nil
	}
	if method == "keep" {
		return nil, s.SetKeep(e.Item, p.Keep)
	}
	if p.Node == 0 || e.Node != p.Node {
		return nil, errors.New("item revision changed; browse again and supply its node")
	}
	if method == "replace" {
		source, err := safecopy.OpenRegular(p.Path)
		if err != nil {
			return nil, err
		}
		defer source.Close()
		info, err := source.Stat()
		if err != nil {
			return nil, err
		}
		updated, err := s.ImportSnapshot(ctx, e, source, info.Size(), info.ModTime())
		if err != nil {
			return nil, err
		}
		return mutationResult(ctx, s, updated)
	}
	rel, err := s.ItemPath(e.Item)
	if err != nil {
		return nil, err
	}
	parent, err := s.Resolve(path.Dir(rel))
	if err != nil {
		return nil, err
	}
	switch method {
	case "delete":
		updated, err := s.Remove(parent.Item, e.Name, e.IsDir(), e)
		if err != nil {
			return nil, err
		}
		return mutationResult(ctx, s, updated)
	case "rename":
		updated, err := s.Rename(parent.Item, e.Name, parent.Item, p.NewName, true, e)
		if err != nil {
			return nil, err
		}
		return mutationPointer(ctx, s, updated)
	case "move":
		if filepath.IsAbs(p.Path) || strings.Contains(p.Path, "\\") || strings.Contains(p.Path, ":") {
			return nil, errors.New("use a relative destination inside the same folder")
		}
		for _, part := range strings.Split(p.Path, "/") {
			if part == ".." {
				return nil, errors.New("destination escapes the folder")
			}
		}
		folders, err := s.Folders()
		if err != nil {
			return nil, err
		}
		base := ""
		for _, folder := range folders {
			if folder.ID == e.Folder {
				base = folder.DirName
			}
		}
		if base == "" {
			return nil, errors.New("folder no longer exists")
		}
		target, err := s.Resolve(path.Join(base, path.Dir(p.Path)))
		if err != nil {
			return nil, err
		}
		if target.Folder != e.Folder {
			return nil, errors.New("cross-folder moves are unsupported; export and import instead")
		}
		updated, err := s.Rename(parent.Item, e.Name, target.Item, path.Base(p.Path), true, e)
		if err != nil {
			return nil, err
		}
		return mutationPointer(ctx, s, updated)
	}
	return nil, errors.New("unsupported Files operation")
}

func mutationPointer(ctx context.Context, s *files.Service, e *files.Entry) (any, error) {
	if e == nil {
		return map[string]any{"state": "committed"}, nil
	}
	return mutationResult(ctx, s, *e)
}
func mutationResult(ctx context.Context, s *files.Service, e files.Entry) (any, error) {
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.Wait(wait, e.Item)
	if err == nil {
		return map[string]any{"entry": e, "state": "committed"}, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return map[string]any{"entry": e, "state": "queued", "message": "The change is saved locally and awaits its gateway. Inspect pending_changes_list before repeating it."}, nil
	}
	edits, _ := s.Edits()
	for _, edit := range edits {
		if edit.Review() && edit.Error != "" {
			return nil, fmt.Errorf("the native edit is preserved for review: %s", edit.Error)
		}
	}
	return nil, err
}
