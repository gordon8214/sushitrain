// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"t-shaped.nl/sushitrain/v2/safecopy"
	"unicode/utf8"

	"t-shaped.nl/sushitrain/v2/mcpapi"
)

func (c *Control) fileOperation(ctx context.Context, method string, p Request) (any, error) {
	f, err := c.folder(p.Folder)
	if err != nil {
		return nil, err
	}
	if method == "engine.inspect" || method == "engine.read" {
		e, err := f.GetFileInformation(p.Path)
		if err != nil {
			return nil, err
		}
		if e == nil {
			return nil, errors.New("file no longer exists")
		}
		if method == "engine.inspect" {
			peers, _ := e.PeersWithFullCopy()
			state, err := f.FileStateJSON(p.Path)
			if err != nil {
				return nil, err
			}
			return map[string]any{"index": json.RawMessage(state), "entry": entry(e), "conflict": e.IsConflictCopy(), "deleted": e.IsDeleted(), "symlink": e.IsSymlink(), "availableDevices": Strings(peers)}, nil
		}
		if e.IsDirectory() || e.IsSymlink() {
			return nil, errors.New("choose a regular file")
		}
		if p.Offset < 0 || p.Offset > e.Size() {
			return nil, errors.New("invalid content offset")
		}
		if p.Limit == 0 {
			p.Limit = mcpapi.MaxContent
		}
		if p.Limit < 1 || p.Limit > mcpapi.MaxContent {
			return nil, errors.New("content length must be 1–65536")
		}
		if p.Offset == e.Size() {
			return contentResult(nil, p.Offset, e.Size()), nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.OnDemandURL(), nil)
		if err != nil {
			return nil, err
		}
		// The engine's existing signed loopback streaming service handles peers
		// and missing local blocks. Never publish this URL to the MCP client.
		req.Header.Set("Range", rangeHeader(p.Offset, p.Limit))
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusPartialContent && response.StatusCode != http.StatusOK {
			return nil, errors.New("file content is unavailable")
		}
		if response.StatusCode == http.StatusOK && p.Offset > 0 {
			return nil, errors.New("the content service did not honor the requested range")
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, p.Limit))
		if err != nil {
			return nil, err
		}
		return contentResult(data, p.Offset, e.Size()), nil
	}
	if method == "engine.replace" {
		if p.Token == "" {
			return nil, errors.New("capture the current target with file_capture_restore before replacing it")
		}
		source, err := safecopy.OpenRegular(p.Name)
		if err != nil {
			return nil, err
		}
		info, err := source.Stat()
		source.Close()
		if err != nil {
			return nil, err
		}
		return nil, f.RestoreDownloadedVersionContext(ctx, p.Path, p.Name, p.Token, info.ModTime().UnixNano())
	}
	action := strings.TrimPrefix(method, "engine.")
	name, target, source := p.Path, p.Name, ""
	if action == "mkdir" && (path.Base(p.Name) != p.Name || p.Name == "." || p.Name == "..") {
		return nil, errors.New("enter a valid directory name without a path")
	}
	if action == "mkdir" {
		name = path.Join(p.Path, p.Name)
		target = ""
	}
	if action == "import" {
		source = p.ID
		if source == "" {
			source = p.Name
		}
		base := p.Name
		if p.ID == "" || base == "" {
			base = filepath.Base(source)
		}
		name = path.Join(p.Path, base)
		target = ""
	}
	if action == "rename" {
		if path.Base(p.Name) != p.Name || p.Name == "." || p.Name == ".." {
			return nil, errors.New("enter a file name without a directory")
		}
		target = path.Join(path.Dir(p.Path), p.Name)
	}
	if mutator, ok := any(f).(interface {
		DesktopFileAction(context.Context, string, string, string, string, string) error
	}); ok {
		return nil, mutator.DesktopFileAction(ctx, action, name, target, source, p.Token)
	}
	return nil, errors.New("unsupported native file action on this host")
}
func rangeHeader(offset, length int64) string {
	return "bytes=" + strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(offset+length-1, 10)
}
func contentResult(data []byte, offset, size int64) any {
	encoding := "utf-8"
	content := string(data)
	if !utf8.Valid(data) {
		encoding = "base64"
		content = base64.StdEncoding.EncodeToString(data)
	}
	return map[string]any{"content": content, "encoding": encoding, "offset": offset, "bytes": len(data), "size": size, "nextOffset": offset + int64(len(data)), "eof": offset+int64(len(data)) >= size}
}
