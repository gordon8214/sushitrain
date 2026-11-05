// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package desktopapi

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"t-shaped.nl/sushitrain/v2/adminsock"
)

type ItemStatus struct {
	Path, Folder, RelativePath, State, Error string
	Directory                                bool
	Actions                                  []string
}

// Relative rejects URLs, relative paths and siblings with a matching prefix.
func Relative(root, path string) (string, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return "", errors.New("choose an absolute local path")
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path is outside the managed folder")
	}
	if rel == "." {
		rel = ""
	}
	return filepath.ToSlash(rel), nil
}

func RegisterItems(a *adminsock.Server, lookup func(context.Context, string) (ItemStatus, error)) {
	a.Register("desktop.items", func(ctx context.Context, data json.RawMessage) (any, error) {
		var p struct{ Paths []string }
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		if len(p.Paths) > 256 {
			return nil, errors.New("request at most 256 visible items")
		}
		items := make([]ItemStatus, 0, len(p.Paths))
		for _, path := range p.Paths {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			item, err := lookup(ctx, path)
			item.Path = path
			if err != nil {
				item.State = "unknown"
				item.Error = err.Error()
				item.Actions = nil
			}
			items = append(items, item)
		}
		return items, nil
	})
}
