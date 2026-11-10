// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package gateway

import (
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/syncthing"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestHistoryByPath(t *testing.T) {
	e := newTestEngine(t, "docs", simpleVersioning)
	stamp := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	name := "gone/café ?#.txt"
	if runtime.GOOS == "windows" {
		name = "gone/café +#.txt"
	} // Win32 reserves question marks; keep URL-escaping coverage.
	e.archiveVersion(name, "archived", stamp, stamp)
	ts := newTestService(t, e, "")
	ts.export(false)
	token := ts.pair(map[string]string{"docs": "read"})
	query := "?path=" + url.QueryEscape(name)
	var list PathVersionList
	ts.json("GET", "/v1/folders/docs/versions"+query, token, nil, http.StatusOK, &list)
	if list.FolderID != "docs" || list.Path != name || len(list.Versions) != 1 {
		t.Fatalf("history: %+v", list)
	}
	content := "/v1/folders/docs/versions/" + list.Versions[0].ID + "/content" + query
	response, body := ts.do("GET", content, token, nil)
	if response.StatusCode != 200 || string(body) != "archived" {
		t.Fatalf("download: %d %q", response.StatusCode, body)
	}
	response, body = ts.do("HEAD", content, token, nil)
	if response.StatusCode != 200 || len(body) != 0 || response.Header.Get("Content-Length") != "8" {
		t.Fatalf("head: %d %q", response.StatusCode, body)
	}
	response, body = ts.do("GET", content, token, nil, "Range", "bytes=1-3")
	if response.StatusCode != 206 || string(body) != "rch" {
		t.Fatalf("range: %d %q", response.StatusCode, body)
	}
	for _, invalid := range []string{"../outside", "/absolute", "a//b", ".", ".stversions/secret", "x/.stignore"} {
		response, _ := ts.do("GET", "/v1/folders/docs/versions?path="+url.QueryEscape(invalid), token, nil)
		if response.StatusCode < 400 {
			t.Fatalf("accepted %q", invalid)
		}
	}
	response, _ = ts.do("GET", "/v1/folders/other/versions"+query, token, nil)
	if response.StatusCode != 404 {
		t.Fatalf("other folder: %d", response.StatusCode)
	}
	response, _ = ts.do("GET", content, "", nil)
	if response.StatusCode < 400 {
		t.Fatal("unauthenticated history")
	}
	// The archive remains and no current file has been created by reading it.
	if _, err := os.Stat(filepath.Join(e.folderPath, filepath.FromSlash(name))); !os.IsNotExist(err) {
		t.Fatalf("read changed live file: %v", err)
	}
}

func TestHistoryRejectsSymlinks(t *testing.T) {
	e := newTestEngine(t, "docs", simpleVersioning)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.folderPath, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "file"), filepath.Join(e.folderPath, "link.txt")); err != nil {
		t.Fatal(err)
	}
	ts := newTestService(t, e, "")
	ts.export(false)
	token := ts.pair(map[string]string{"docs": "read"})
	for _, name := range []string{"linked/file.txt", "link.txt"} {
		response, _ := ts.do("GET", "/v1/folders/docs/versions?path="+url.QueryEscape(name), token, nil)
		if response.StatusCode != 404 {
			t.Fatalf("symlink %q: %d", name, response.StatusCode)
		}
	}
}

func TestHistoryLiteralBackslash(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	e := newTestEngine(t, "docs", simpleVersioning)
	stamp := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	name := `literal\name.txt`
	e.archiveVersion(name, "archived", stamp, stamp)
	ts := newTestService(t, e, "")
	ts.export(false)
	token := ts.pair(map[string]string{"docs": "read"})
	var list PathVersionList
	query := "?path=" + url.QueryEscape(name)
	ts.json("GET", "/v1/folders/docs/versions"+query, token, nil, http.StatusOK, &list)
	if len(list.Versions) != 1 {
		t.Fatalf("literal backslash: %+v", list)
	}
	path := "/v1/folders/docs/versions/" + list.Versions[0].ID + "/content" + query
	response, body := ts.do("GET", path, token, nil, "If-Match", list.Versions[0].EntityTag)
	if response.StatusCode != 200 || string(body) != "archived" {
		t.Fatalf("download: %d %q", response.StatusCode, body)
	}
	// Replacing an archive slot with equal-sized content still invalidates the selection, even preserving mtime.
	e.archiveVersion(name, "changed!", stamp, stamp)
	response, _ = ts.do("GET", path, token, nil, "If-Match", list.Versions[0].EntityTag)
	if response.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("changed archive: %d", response.StatusCode)
	}
}

type disappearingHistoryBackend struct {
	RemoteFileBackend
	missing time.Time
}

func (b disappearingHistoryBackend) OpenFileVersion(folder, name string, stamp time.Time) (fs.File, error) {
	if stamp.Equal(b.missing) {
		return nil, syncthing.ErrVersionNotFound
	}
	return b.RemoteFileBackend.OpenFileVersion(folder, name, stamp)
}

func TestHistoryListKeepsSurvivingVersionsDuringCleanup(t *testing.T) {
	e := newTestEngine(t, "docs", simpleVersioning)
	stamp := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	e.archiveVersion("file.txt", "older", stamp, stamp)
	e.archiveVersion("file.txt", "newer", stamp.Add(time.Hour), stamp.Add(time.Hour))
	ts := newTestServiceWithBackend(t, e, "", disappearingHistoryBackend{e.backend, stamp})
	ts.export(false)
	token := ts.pair(map[string]string{"docs": "read"})
	var list PathVersionList
	ts.json("GET", "/v1/folders/docs/versions?path=file.txt", token, nil, http.StatusOK, &list)
	if len(list.Versions) != 1 || list.Versions[0].ID != versionID(stamp.Add(time.Hour)) {
		t.Fatalf("lost surviving versions: %+v", list)
	}
}
