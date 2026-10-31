// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
)

func TestIgnoresEverything(t *testing.T) {
	for _, pattern := range []string{"*", "**", "/*", "/**", "**/*", "(?d)*", "(?d)(?i)**"} {
		if !ignoresEverything(pattern) {
			t.Errorf("%q", pattern)
		}
	}
	for _, pattern := range []string{"!*", "(?d)!/**", "*.tmp", "/Photos", "**/.DS_Store"} {
		if ignoresEverything(pattern) {
			t.Errorf("%q", pattern)
		}
	}
}

// setIgnores saves patterns and scans, which is when the engine takes them in
func (e *testEngine) setIgnores(lines ...string) {
	e.t.Helper()
	if err := e.app.Internals.SetIgnores(e.folderID, lines); err != nil {
		e.t.Fatal(err)
	}
	e.scan()
}

func (e *testEngine) setPaused(paused bool) {
	e.t.Helper()
	waiter, err := e.cfg.Modify(func(c *config.Configuration) {
		folder, _, _ := c.Folder(e.folderID)
		folder.Paused = paused
		c.SetFolder(folder)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	waiter.Wait()
}

func TestSelectiveFolders(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	selective := SelectiveFolders(e.app, e.cfg)
	if selective("docs") {
		t.Fatal("a folder without patterns")
	}
	e.setIgnores("*.tmp")
	if selective("docs") {
		t.Fatal("a folder with ordinary patterns")
	}
	e.setIgnores("// Nothing but a comment")
	if selective("docs") {
		t.Fatal("a folder whose file holds no patterns")
	}
	ignoreFile := filepath.Join(e.folderPath, ".stignore")

	// The engine drops its patterns when it can't load the file, and tells nobody but the scan: there is no telling what
	// they are
	if err := os.WriteFile(ignoreFile, []byte("#include missing.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Internals.ScanFolderSubdirs("docs", nil); err == nil {
		t.Fatal("a scan took in patterns that include a missing file")
	}
	if _, patterns, err := e.app.Internals.Ignores("docs"); len(patterns) != 0 || err != nil {
		t.Fatalf("the engine's patterns: %v, %v", patterns, err)
	}
	if !selective("docs") {
		t.Fatal("a running folder whose patterns include a missing file")
	}

	e.setIgnores("!/keep.txt", "*")
	if !selective("docs") {
		t.Fatal("a folder whose patterns ignore everything")
	}

	// Syncthing doesn't run a paused folder, whose patterns are in its file
	e.setPaused(true)
	if !selective("docs") {
		t.Fatal("a paused folder whose patterns ignore everything")
	}
	if err := os.WriteFile(ignoreFile, []byte("*.tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if selective("docs") {
		t.Fatal("a paused folder with ordinary patterns")
	}
	if err := os.Remove(ignoreFile); err != nil {
		t.Fatal(err)
	}
	if selective("docs") {
		t.Fatal("a paused folder without patterns")
	}

	// Nor does a missing file say anything about patterns when the folder itself isn't there, as on a volume that isn't
	// mounted
	away := e.folderPath + ".away"
	if err := os.Rename(e.folderPath, away); err != nil {
		t.Fatal(err)
	}
	if !selective("docs") {
		t.Fatal("a paused folder that isn't there")
	}
	if err := os.Rename(away, e.folderPath); err != nil {
		t.Fatal(err)
	}

	// The file is there, but what it includes isn't: there is no telling what the patterns are
	if err := os.WriteFile(ignoreFile, []byte("#include missing.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !selective("docs") {
		t.Fatal("a paused folder whose patterns include a missing file")
	}
	if err := os.Remove(ignoreFile); err != nil {
		t.Fatal(err)
	}

	// Nor when the file is a link to nothing, which reads as a missing file but isn't one
	if err := os.Symlink("nowhere", ignoreFile); err != nil {
		t.Fatal(err)
	}
	if !selective("docs") {
		t.Fatal("a paused folder whose patterns are behind a dangling link")
	}
	if err := os.Remove(ignoreFile); err != nil {
		t.Fatal(err)
	}

	// What can't be read is never written
	if err := os.Mkdir(ignoreFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if !selective("docs") {
		t.Fatal("a folder whose patterns can't be read")
	}
	if !selective("unknown") {
		t.Fatal("a folder that doesn't exist")
	}
}

// A folder whose patterns ignore everything doesn't hold all of its files, so what a client writes could build on a
// version this device doesn't have. It stays read-only for every client, however it got its grant and whenever the
// patterns came.
func TestFolderThatIgnoresEverythingIsReadOnly(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	e.backend.IsSelective = SelectiveFolders(e.app, e.cfg)
	e.write("keep.txt", "kept")
	ts := newTestService(t, e, "")
	ts.export(true)
	before := ts.pair(map[string]string{"docs": "write"})
	root := ts.itemAt(before, "")
	if op := createFile(ts, before, root.ID, "written.txt", []byte("written")); op.State != stateResultCommitted {
		t.Fatalf("a complete folder: %+v", op)
	}

	e.setIgnores("!/keep.txt", "!/written.txt", "*")
	if err := ts.SetExport("docs", true); err == nil {
		t.Fatal("exported for writing")
	}
	after := ts.pair(map[string]string{"docs": "write"})
	raised := ts.pair(map[string]string{"docs": "read"})
	clients, err := ts.Clients()
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range clients {
		if err := ts.SetPermission(client.ClientID, "docs", "write"); err != nil {
			t.Fatal(err)
		}
	}

	createDirectory := func(token string) (*http.Response, []byte) {
		return ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: "docs",
			ParentID: root.ID, Name: "made-" + newUUID(), ItemKind: kindDirectory})
	}
	for name, token := range map[string]string{"paired before": before, "paired after": after, "raised": raised} {
		var folders FoldersResponse
		ts.json("GET", "/v1/folders", token, nil, http.StatusOK, &folders)
		if len(folders.Folders) != 1 || !folders.Folders[0].Selective || !folders.Folders[0].Permissions.Read ||
			folders.Folders[0].Permissions.Write {
			t.Errorf("%s: folders %+v", name, folders.Folders)
		}
		if resp, data := createDirectory(token); resp.StatusCode != http.StatusForbidden ||
			!strings.Contains(string(data), codeFolderReadOnly) {
			t.Errorf("%s: operation %d %s", name, resp.StatusCode, data)
		}
		resp, data := ts.do("POST", "/v1/uploads", token, []byte{}, "Tus-Resumable", tusVersion, "Upload-Length", "1",
			"Upload-Metadata", "folder ZG9jcw==,digest "+b64std(sha256Of([]byte("x"))))
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(data), codeFolderReadOnly) {
			t.Errorf("%s: upload %d %s", name, resp.StatusCode, data)
		}
	}

	// An operation that was accepted before is checked again when it runs
	err = ts.authorizeOperation(&operationRecord{clientID: clients[0].ClientID, folder: "docs"})
	if err == nil || asAPIError(err).code != codeFolderReadOnly {
		t.Fatalf("a pending operation: %v", err)
	}

	// It is the patterns that say so
	e.setIgnores()
	if resp, data := createDirectory(before); resp.StatusCode != http.StatusCreated {
		t.Fatalf("the folder is complete again: %d %s", resp.StatusCode, data)
	}
}
