// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"log/slog"
	"sync"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/history"
	"t-shaped.nl/sushitrain/v2/safecopy"
	"time"
)

type browserRequest struct {
	Version                 string
	Limit                   int64
	Token                   string
	Modified                int64
	Parent, NewParent, Item int64
	Offset                  int
	Node                    uint64
	Name, NewName, Path     string
	Directory, Keep         bool
}

func registerBrowser(a *adminsock.Server, s *files.Service, mount string) {
	a.Register("files.versions", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, e := decode[browserRequest](b)
		if e != nil {
			return nil, e
		}
		return s.HistoryVersions(ctx, p.Item)
	})
	a.Register("files.versionPreview", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, e := decode[browserRequest](b)
		if e != nil {
			return nil, e
		}
		return s.PreviewHistoryVersion(ctx, p.Item, p.Version, p.Token, previewDir())
	})
	a.Register("files.versionExport", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, err := decode[browserRequest](b)
		if err != nil {
			return nil, err
		}
		return nil, s.ExportHistoryVersion(ctx, p.Item, p.Version, p.Token, p.Path, mount)
	})
	a.Register("files.versionRestore", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, e := decode[browserRequest](b)
		if e != nil {
			return nil, e
		}
		entry, err := s.RestoreHistoryVersion(ctx, p.Item, p.Version, p.Token)
		if err != nil {
			return nil, err
		}
		return nil, s.Wait(ctx, entry.Item)
	})
	a.Register("files.versionCopy", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, e := decode[browserRequest](b)
		if e != nil {
			return nil, e
		}
		entry, err := s.CopyHistoryVersion(ctx, p.Item, p.Version, p.Token)
		if err != nil {
			return nil, err
		}
		return entry.Name, s.Wait(ctx, entry.Item)
	})
	a.Register("files.wait", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, e := decode[browserRequest](b)
		if e != nil {
			return nil, e
		}
		return nil, s.Wait(ctx, p.Item)
	})
	var captureMu sync.Mutex
	captures := map[string]files.HistoryRestoreCapture{}
	a.Register("files.historyTarget", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, e := decode[browserRequest](b)
		if e != nil {
			return nil, e
		}
		return s.HistoryTarget(ctx, p.Item)
	})
	a.Register("files.captureRestore", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, e := decode[browserRequest](b)
		if e != nil {
			return nil, e
		}
		capture, e := s.CaptureHistoryRestore(ctx, p.Item)
		if e != nil {
			return nil, e
		}
		captureMu.Lock()
		defer captureMu.Unlock()
		for k, c := range captures {
			if time.Since(c.Created) > 10*time.Minute {
				delete(captures, k)
			}
		}
		if len(captures) >= 64 {
			return nil, errors.New("close another history operation before restoring")
		}
		token := uuid.NewString()
		captures[token] = capture
		return token, nil
	})
	a.Register("files.restoreDownloaded", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, e := decode[browserRequest](b)
		if e != nil {
			return nil, e
		}
		captureMu.Lock()
		capture, ok := captures[p.Token]
		delete(captures, p.Token)
		captureMu.Unlock()
		if !ok {
			return nil, errors.New("refresh history before restoring")
		}
		return s.RestoreHistoryCapture(ctx, capture, p.Path, time.Unix(0, p.Modified))
	})
	a.Register("files.preview", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, e := decode[browserRequest](b)
		if e != nil {
			return nil, e
		}
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		return s.PreviewFile(ctx, p.Item, p.Node, p.Limit, filepath.Join(base, "synctrain", "files-previews"))
	})

	if err := history.RegisterDesktop(a, mount); err != nil {
		slog.Error("Could not open history sources", "error", err)
	}
	a.Register("files.childrenPage", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, err := decode[browserRequest](b)
		if err != nil {
			return nil, err
		}
		entries, err := s.Children(p.Parent)
		if err != nil {
			return nil, err
		}
		if p.Offset < 0 || p.Offset > len(entries) {
			return nil, errors.New("refresh this directory")
		}
		end := p.Offset + 256
		if end > len(entries) {
			end = len(entries)
		}
		next := 0
		if end < len(entries) {
			next = end
		}
		return map[string]any{"Entries": entries[p.Offset:end], "Next": next}, nil
	})
	a.Register("files.thumbnail", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, err := decode[browserRequest](b)
		if err != nil {
			return nil, err
		}
		return s.Thumbnail(ctx, p.Item, 256)
	})
	a.Register("files.children", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, err := decode[browserRequest](b)
		if err != nil {
			return nil, err
		}
		list, err := s.Children(p.Parent)
		return list, err
	})
	a.Register("files.resolvePath", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, err := decode[browserRequest](b)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(mount, p.Path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errors.New("choose a file in Synctrain's mounted folders")
		}
		return s.Resolve(filepath.ToSlash(rel))
	})
	a.Register("files.import", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, err := decode[browserRequest](b)
		if err != nil {
			return nil, err
		}
		input, err := safecopy.OpenRegular(p.Path)
		if err != nil {
			return nil, err
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("choose a regular file to import")
		}
		return s.CreateSnapshot(ctx, p.Parent, filepath.Base(p.Path), false, input, info.Size(), info.ModTime())
	})
	a.Register("files.path", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, err := decode[browserRequest](b)
		if err != nil {
			return nil, err
		}
		path, err := s.ItemPath(p.Item)
		return filepath.Join(mount, filepath.FromSlash(path)), err
	})
	a.Register("files.search", func(ctx context.Context, b json.RawMessage) (any, error) {
		p, err := decode[browserRequest](b)
		if err != nil {
			return nil, err
		}
		result := []files.Entry{}
		queue := []int64{p.Parent}
		for len(queue) > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			parent := queue[0]
			queue = queue[1:]
			list, err := s.Children(parent)
			if err != nil {
				return nil, err
			}
			for _, e := range list {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if strings.Contains(strings.ToLower(e.Name), strings.ToLower(p.Name)) {
					result = append(result, e)
					if len(result) >= 2000 {
						return result, nil
					}
				}
				if e.IsDir() {
					queue = append(queue, e.Item)
				}
			}
		}
		return result, nil
	})
	for _, method := range []string{"files.keep", "files.create", "files.rename", "files.move", "files.delete", "files.download"} {
		a.Register(method, func(ctx context.Context, b json.RawMessage) (any, error) {
			p, err := decode[browserRequest](b)
			if err != nil {
				return nil, err
			}
			if method == "files.create" {
				return s.Create(p.Parent, p.Name, p.Directory)
			}
			e, ok, err := s.Item(p.Item)
			if err != nil {
				return nil, err
			}
			if !ok || e.Node != p.Node {
				return nil, errors.New("this file changed; refresh before trying again")
			}
			rel, err := s.ItemPath(e.Item)
			if err != nil {
				return nil, err
			}
			parentPath := filepath.ToSlash(filepath.Dir(rel))
			actualParent := int64(0)
			if parentPath != "." {
				parent, err := s.Resolve(parentPath)
				if err != nil {
					return nil, err
				}
				actualParent = parent.Item
			}
			switch method {
			case "files.keep":
				return nil, s.SetKeep(e.Item, p.Keep)
			case "files.move":
				if filepath.IsAbs(p.Path) || p.Path == "" {
					return nil, errors.New("enter a destination relative to Synctrain's mounted folders")
				}
				destination, err := s.Resolve(filepath.ToSlash(filepath.Dir(p.Path)))
				if err != nil {
					return nil, err
				}
				return s.Rename(actualParent, e.Name, destination.Item, filepath.Base(p.Path), true, e)
			case "files.rename":
				newParent := p.NewParent
				if p.NewParent == p.Parent {
					newParent = actualParent
				}
				return s.Rename(actualParent, e.Name, newParent, p.NewName, true, e)
			case "files.delete":
				return s.Remove(actualParent, e.Name, e.IsDir(), e)
			case "files.download":
				if p.Path == "" || !filepath.IsAbs(p.Path) {
					return nil, errors.New("choose a local destination")
				}
				if rel, err := filepath.Rel(mount, p.Path); err == nil && (rel == "." || (!strings.HasPrefix(rel, "../") && rel != "..")) {
					return nil, errors.New("save the copy outside Synctrain's mounted folders")
				}
				r, err := s.OpenFile(e.Item)
				if err != nil {
					return nil, err
				}
				defer r.Close()
				dest, err := safecopy.Create(p.Path, mount)
				if err != nil {
					return nil, err
				}
				defer dest.Close()

				buf := make([]byte, 256*1024)
				for off := int64(0); off < r.Size(); {
					n, err := r.ReadAt(ctx, buf, off)
					if n > 0 {
						if _, e := dest.File.Write(buf[:n]); e != nil {
							return nil, e
						}
						off += int64(n)
					}
					if err != nil && !errors.Is(err, io.EOF) {
						return nil, err
					}
					if n == 0 {
						return nil, io.ErrUnexpectedEOF
					}
				}
				if err = dest.Commit(); err != nil {
					return nil, err
				}
				return nil, nil
			}
			return nil, errors.New("unknown file action")
		})
	}
}
