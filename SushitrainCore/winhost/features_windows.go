// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package winhost

import (
	"context"
	"errors"
	"strconv"
	"strings"
	core "t-shaped.nl/sushitrain/v2/src"
)

type versionInfo struct {
	ID                          string
	Size, VersionTime, Modified int64
}
type searchResult struct {
	Folder, Path, Name, URL string
	Directory               bool
	Size                    int64
}
type searchCollector struct {
	ctx     context.Context
	results []searchResult
}

func (s *searchCollector) Result(entry *core.Entry) {
	if entry == nil {
		return
	}
	s.results = append(s.results, searchResult{entry.Folder.FolderID, entry.Path(), entry.Name(), entry.OnDemandURL(), entry.IsDirectory(), entry.Size()})
}
func (s *searchCollector) IsCancelled() bool { return s.ctx.Err() != nil }

func (h *Host) engineExtras(ctx context.Context, method string, p request) (any, error) {
	c := h.client
	switch method {
	case "engine.pending":
		pending, err := c.PendingDevices()
		if err != nil {
			return nil, err
		}
		out := []*core.ObservedDevice{}
		for i := 0; i < pending.Count(); i++ {
			out = append(out, pending.ItemAt(i))
		}
		return out, nil
	case "engine.stats":
		totals, err := c.Statistics()
		if err != nil {
			return nil, err
		}
		return map[string]any{"totals": totals, "download": c.GetTotalDownloadProgress(), "upload": c.GetTotalUploadProgress()}, nil
	case "engine.logs":
		return raw(c.LogLinesJSON(p.Offset))
	case "engine.search":
		results := &searchCollector{ctx: ctx, results: []searchResult{}}
		prefix := strings.TrimSuffix(p.Path, "/")
		if prefix != "" {
			prefix += "/"
		}
		err := c.Search(p.Name, results, 2000, p.Folder, prefix)
		return results.results, err
	case "engine.share":
		return nil, c.FolderWithID(p.Folder).ShareWithDevice(p.Device, p.Enabled, p.Password)
	case "engine.versions", "engine.restoreVersion", "engine.exportVersion":
		folder := c.FolderWithID(p.Folder)
		if !folder.Exists() {
			return nil, errors.New("folder no longer exists")
		}
		versions, err := folder.FileVersions(p.Path)
		if err != nil {
			return nil, err
		}
		out := []versionInfo{}
		for i := 0; i < versions.Count(); i++ {
			version := versions.ItemAt(i)
			id := strconv.FormatInt(version.VersionTime().UnixMilliseconds(), 10)
			if p.ID == id {
				if method == "engine.restoreVersion" {
					return nil, folder.RestoreFileVersion(p.Path, version)
				}
				if method == "engine.exportVersion" {
					return nil, folder.ExportFileVersion(p.Path, version, p.Name)
				}
			}
			out = append(out, versionInfo{id, version.Size, version.VersionTime().UnixMilliseconds(), version.ModTime().UnixMilliseconds()})
		}
		if method != "engine.versions" {
			return nil, errors.New("the selected version is no longer kept")
		}
		return out, nil
	}
	return nil, errors.New("unknown engine operation")
}
