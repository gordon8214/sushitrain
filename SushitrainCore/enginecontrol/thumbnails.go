// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"t-shaped.nl/sushitrain/v2/gateway"
	"time"
)

func (c *Control) thumbnail(ctx context.Context, p Request) ([]byte, error) {
	folder, err := c.folder(p.Folder)
	if err != nil {
		return nil, err
	}
	entry, err := folder.GetFileInformation(p.Path)
	if err != nil {
		return nil, err
	}
	if entry == nil || entry.IsDirectory() || entry.IsSymlink() {
		return nil, errors.New("no preview is available")
	}
	key := p.Folder + "/" + p.Path + ":" + entry.BlocksHash()
	if local, e := entry.LocalNativePath(); e == nil {
		if info, e := os.Stat(local); e == nil {
			key += fmt.Sprintf(":%d:%d", info.Size(), info.ModTime().UnixNano())
		}
	}
	c.thumbMu.Lock()
	cached := c.thumbnails[key]
	c.thumbMu.Unlock()
	if cached != nil {
		return cached, nil
	}
	select {
	case c.thumbSlots <- struct{}{}:
		defer func() { <-c.thumbSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	c.thumbMu.Lock()
	cached = c.thumbnails[key]
	c.thumbMu.Unlock()
	if cached != nil {
		return cached, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	local, err := entry.LocalNativePath()
	if err != nil || !entry.IsLocallyPresent() {
		if entry.Size() > 16<<20 {
			return nil, errors.New("the file is too large for a remote thumbnail")
		}
		dir, err := os.MkdirTemp("", "synctrain-thumbnail-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		local = filepath.Join(dir, entry.Name())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.OnDemandURL(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, errors.New("thumbnail content is unavailable")
		}
		f, err := os.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		n, err := io.Copy(f, io.LimitReader(resp.Body, (16<<20)+1))
		err = errors.Join(err, f.Close())
		if err != nil {
			return nil, err
		}
		if n > 16<<20 {
			return nil, errors.New("thumbnail content is too large")
		}
	}
	data, err := gateway.NewCommandRenderer().Render(ctx, local, 256)
	if err == nil && len(data) <= 256*1024 {
		c.thumbMu.Lock()
		if len(c.thumbnails) >= 32 {
			for k := range c.thumbnails {
				delete(c.thumbnails, k)
				break
			}
		}
		c.thumbnails[key] = data
		c.thumbMu.Unlock()
	}
	return data, err
}
