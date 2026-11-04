// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package enginecontrol

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"t-shaped.nl/sushitrain/v2/previewcache"
	"t-shaped.nl/sushitrain/v2/safecopy"
	core "t-shaped.nl/sushitrain/v2/src"
)

type versionExporter interface {
	DesktopExportVersionChecked(context.Context, string, *core.FileVersion, string, string) error
}

func (c *Control) exportVersion(ctx context.Context, p Request, v *core.FileVersion, preview bool) (any, error) {
	folder, err := c.folder(p.Folder)
	if err != nil {
		return nil, err
	}
	exporter, ok := any(folder).(versionExporter)
	if !ok {
		return nil, errors.New("native exports are unavailable on this platform")
	}
	if !preview {
		return nil, exporter.DesktopExportVersionChecked(ctx, p.Path, v, p.Name, p.Token)
	}
	c.previewMu.Lock()
	defer c.previewMu.Unlock()
	base, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	base = filepath.Join(base, "synctrain", "local-history")
	if err = os.MkdirAll(base, 0700); err != nil {
		return nil, err
	}
	releaseQuota, err := previewcache.Reserve(ctx, base, v.Size)
	if err != nil {
		return nil, err
	}
	defer releaseQuota()
	dir, err := os.MkdirTemp(base, "version-")
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(dir, filepath.Base(p.Path))
	if err = exporter.DesktopExportVersionChecked(ctx, p.Path, v, dest, p.Token); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return dest, nil
}
func (c *Control) exportCurrent(ctx context.Context, p Request) error {
	folder, err := c.folder(p.Folder)
	if err != nil {
		return err
	}
	entry, err := folder.GetFileInformation(p.Path)
	if err != nil {
		return err
	}
	if entry == nil || entry.IsDirectory() || entry.IsSymlink() {
		return errors.New("choose a regular file")
	}
	if identity, ok := any(entry).(interface{ DesktopIdentity() string }); ok && p.Token != "" && identity.DesktopIdentity() != p.Token {
		return errors.New("this file changed; refresh before saving a copy")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.OnDemandURL(), nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && !(response.StatusCode == http.StatusNoContent && entry.Size() == 0) {
		return errors.New("file content is unavailable")
	}
	dest, err := safecopy.Create(p.Name)
	if err != nil {
		return err
	}
	defer dest.Close()
	if n, copyErr := safecopy.Copy(ctx, dest.File, response.Body); copyErr != nil || n != entry.Size() {
		if copyErr == nil {
			copyErr = errors.New("the file changed or the copy is incomplete")
		}
		err = copyErr
		return err
	}
	return dest.Commit()
}
