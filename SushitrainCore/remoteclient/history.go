// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package remoteclient

import (
	"context"
	"errors"
	"io"
	"net/url"
	"t-shaped.nl/sushitrain/v2/remoteapi"
)

func (c *Client) PathVersions(ctx context.Context, folder, path string) (remoteapi.PathVersionList, error) {
	var result remoteapi.PathVersionList
	_, err := c.call(ctx, "GET", []string{"v1", "folders", folder, "versions"}, url.Values{"path": {path}}, withCredential, nil, &result, KindOther, 0)
	if err == nil && (result.FolderID != folder || result.Path != path) {
		return result, errors.New("the history source returned a different file")
	}
	return result, err
}

// HistoryContent is version-bound: a changed archive cannot silently replace the
// bytes whose size and entity tag the user selected in the history browser.
func (c *Client) HistoryContent(ctx context.Context, folder, path string, v remoteapi.ItemVersion) (io.ReadCloser, error) {
	if v.EntityTag == "" || v.Size < 0 {
		return nil, errors.New("the history source needs an update")
	}
	req, err := c.newRequest(ctx, "GET", []string{"v1", "folders", folder, "versions", v.ID, "content"}, url.Values{"path": {path}}, withCredential, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("If-Match", v.EntityTag)
	resp, err := c.do(req, KindVersionNotFound)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 || resp.Header.Get("ETag") != v.EntityTag || resp.ContentLength != v.Size {
		resp.Body.Close()
		return nil, errors.New("the archived version changed or is incomplete")
	}
	return resp.Body, nil
}
