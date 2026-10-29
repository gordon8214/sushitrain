// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/versioner"
)

// archiveVersion puts a version of a file in the folder's versions directory, as archived at the given time
func (e *testEngine) archiveVersion(name, contents string, archived, modified time.Time) {
	e.t.Helper()
	tagged := versioner.TagFilename(filepath.FromSlash(name), archived.In(time.Local).Format(versioner.TimeFormat))
	p := filepath.Join(e.folderPath, versioner.DefaultPath, tagged)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chtimes(p, modified, modified); err != nil {
		e.t.Fatal(err)
	}
}

func (ts *testService) versions(token string, item Item) ItemVersionList {
	ts.t.Helper()
	var list ItemVersionList
	ts.json("GET", "/v1/items/"+item.ID+"/versions", token, nil, http.StatusOK, &list)
	return list
}

func simpleVersioning(f *config.FolderConfiguration) {
	f.Versioning = config.VersioningConfiguration{Type: "simple", Params: map[string]string{"keep": "10"}}
}

func TestVersions(t *testing.T) {
	e := newTestEngine(t, "docs", simpleVersioning)
	e.write("doc.txt", "current")
	if err := os.MkdirAll(filepath.Join(e.folderPath, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	second := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	e.archiveVersion("doc.txt", "first", first, first.Add(-time.Hour))
	e.archiveVersion("doc.txt", "second!", second, second.Add(-time.Hour))
	e.archiveVersion("other.txt", "not a version of doc.txt", first, first)
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	doc := ts.itemAt(token, "doc.txt")
	root := ts.itemAt(token, "")

	var caps Capabilities
	ts.json("GET", "/v1/capabilities", token, nil, http.StatusOK, &caps)
	if !slices.Contains(caps.Features, featureVersions) {
		t.Fatalf("features %v lack %s", caps.Features, featureVersions)
	}

	// Listing: newest first, times in UTC
	list := ts.versions(token, doc)
	if list.ItemID != doc.ID || list.Revision != doc.Revision || list.Versioning != "simple" || len(list.Versions) != 2 {
		t.Fatalf("versions: %+v", list)
	}
	newest, oldest := list.Versions[0], list.Versions[1]
	if newest.VersionTime != "2026-01-02T12:00:00Z" || newest.Size != 7 || oldest.VersionTime != "2026-01-01T12:00:00Z" ||
		oldest.Size != 5 || oldest.ModifiedNs != first.Add(-time.Hour).UnixNano() || !strings.HasPrefix(oldest.ID, prefixVersion) {
		t.Fatalf("versions: %+v", list.Versions)
	}
	resp, data := ts.do("GET", "/v1/items/"+ts.itemAt(token, "dir").ID+"/versions", token, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("versions of a directory: %d %s", resp.StatusCode, data)
	}

	// Content, whole and in part
	content := "/v1/items/" + doc.ID + "/versions/" + oldest.ID + "/content"
	resp, data = ts.do("GET", content, token, nil)
	if resp.StatusCode != http.StatusOK || string(data) != "first" || resp.Header.Get("ETag") == "" {
		t.Fatalf("version content: %d %q", resp.StatusCode, data)
	}
	resp, data = ts.do("GET", content, token, nil, "Range", "bytes=1-2")
	if resp.StatusCode != http.StatusPartialContent || string(data) != "ir" || resp.Header.Get("Content-Range") != "bytes 1-2/5" {
		t.Fatalf("version range: %d %q %s", resp.StatusCode, data, resp.Header.Get("Content-Range"))
	}
	resp, _ = ts.do("HEAD", content, token, nil)
	if resp.StatusCode != http.StatusOK || resp.ContentLength != 5 {
		t.Fatalf("version head: %d %d", resp.StatusCode, resp.ContentLength)
	}
	// The largest possible end of a range is the end of the content
	resp, data = ts.do("GET", content, token, nil, "Range", "bytes=1-9223372036854775807")
	if resp.StatusCode != http.StatusPartialContent || string(data) != "irst" {
		t.Fatalf("version range to the largest end: %d %q", resp.StatusCode, data)
	}
	for _, id := range []string{"ver_1", "ver_x", "itm_1"} {
		resp, data = ts.do("GET", "/v1/items/"+doc.ID+"/versions/"+id+"/content", token, nil)
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(data), codeVersionNotFound) {
			t.Fatalf("content of version %s: %d %s", id, resp.StatusCode, data)
		}
	}

	// Malformed requests
	for _, req := range []OperationRequest{
		{ID: newUUID(), Kind: opReplace, FolderID: "docs", ItemID: doc.ID, BaseRevision: doc.Revision,
			SourceVersion: oldest.ID, Upload: "upl_x", UploadDigest: "x"},
		{ID: newUUID(), Kind: opDelete, FolderID: "docs", ItemID: doc.ID, BaseRevision: doc.Revision,
			SourceVersion: oldest.ID},
		{ID: newUUID(), Kind: opCreate, FolderID: "docs", ParentID: root.ID, Name: "copy.txt", ItemKind: kindFile,
			SourceVersion: oldest.ID},
		{ID: newUUID(), Kind: opReplace, FolderID: "docs", ItemID: doc.ID, BaseRevision: doc.Revision,
			SourceVersion: "yesterday"},
	} {
		resp, data = ts.do("POST", "/v1/operations", token, req)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("malformed %s: %d %s", req.Kind, resp.StatusCode, data)
		}
	}
	resp, data = ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace,
		FolderID: "docs", ItemID: doc.ID, BaseRevision: doc.Revision, SourceVersion: "ver_1", OnConflict: onConflictReject})
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(data), codeVersionNotFound) {
		t.Fatalf("restoring an unknown version: %d %s", resp.StatusCode, data)
	}

	// A client that may only read the folder can't restore
	readOnly := ts.pair(map[string]string{"docs": "read"})
	resp, data = ts.do("POST", "/v1/operations", readOnly, OperationRequest{ID: newUUID(), Kind: opReplace,
		FolderID: "docs", ItemID: doc.ID, BaseRevision: doc.Revision, SourceVersion: oldest.ID})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("restoring as a reader: %d %s", resp.StatusCode, data)
	}

	// Restore: the version replaces the file with its modification time, the replaced content becomes a version,
	// and the version stays
	restore := OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs", ItemID: doc.ID,
		BaseRevision: doc.Revision, SourceVersion: oldest.ID, OnConflict: onConflictReject}
	var restored Operation
	ts.json("POST", "/v1/operations", token, restore, http.StatusCreated, &restored)
	if restored.State != stateResultCommitted || restored.Item == nil || restored.Item.ID != doc.ID ||
		restored.Item.Revision == doc.Revision {
		t.Fatalf("restore: %+v", restored)
	}
	if got, _ := e.read("doc.txt"); got != "first" {
		t.Fatalf("restored file holds %q", got)
	}
	local, ok, _ := e.backend.LocalFile("docs", "doc.txt")
	if !ok || local.Size != 5 || !local.ModTime().Equal(first.Add(-time.Hour)) {
		t.Fatalf("restored file is indexed as %+v", local)
	}
	// Another spelling of the same version is the same request
	restore.SourceVersion = strings.Replace(restore.SourceVersion, prefixVersion, prefixVersion+"0", 1)
	var replayed Operation
	ts.json("POST", "/v1/operations", token, restore, http.StatusOK, &replayed)
	if replayed.Operation != restored.Operation {
		t.Fatalf("replay with another spelling of the version: %+v", replayed)
	}
	after := ts.versions(token, ts.itemAt(token, "doc.txt"))
	if len(after.Versions) != 3 || !slices.ContainsFunc(after.Versions, func(v ItemVersion) bool { return v.ID == oldest.ID }) ||
		after.Versions[0].Size != int64(len("current")) {
		t.Fatalf("versions after restoring: %+v", after.Versions)
	}

	// Restoring onto a stale base is rejected
	resp, data = ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace,
		FolderID: "docs", ItemID: doc.ID, BaseRevision: doc.Revision, SourceVersion: newest.ID, OnConflict: onConflictReject})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("restoring onto a stale base: %d %s", resp.StatusCode, data)
	}
	if got, _ := e.read("doc.txt"); got != "first" {
		t.Fatalf("a rejected restore changed the file to %q", got)
	}

	// Save a copy
	var copied Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: "docs",
		ParentID: ts.itemAt(token, "dir").ID, Name: "doc copy.txt", ItemKind: kindFile, SourceItemID: doc.ID,
		SourceVersion: newest.ID}, http.StatusCreated, &copied)
	if copied.State != stateResultCommitted || copied.Item == nil || copied.Item.Path != "dir/doc copy.txt" {
		t.Fatalf("copy: %+v", copied)
	}
	if got, _ := e.read("dir/doc copy.txt"); got != "second!" {
		t.Fatalf("copy holds %q", got)
	}
	resp, data = ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opCreate,
		FolderID: "docs", ParentID: root.ID, Name: "doc.txt", ItemKind: kindFile, SourceItemID: doc.ID,
		SourceVersion: newest.ID})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(data), codeNameConflict) {
		t.Fatalf("copy onto an existing name: %d %s", resp.StatusCode, data)
	}
}

func TestVersionsWithoutVersioning(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	e.write("doc.txt", "current")
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	doc := ts.itemAt(token, "doc.txt")

	list := ts.versions(token, doc)
	if list.Versioning != versioningNone || list.Versions == nil || len(list.Versions) != 0 {
		t.Fatalf("versions without versioning: %+v", list)
	}
	resp, data := ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace,
		FolderID: "docs", ItemID: doc.ID, BaseRevision: doc.Revision, SourceVersion: "ver_1767268800"})
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(data), codeVersionNotFound) {
		t.Fatalf("restoring without versioning: %d %s", resp.StatusCode, data)
	}
	resp, data = ts.do("GET", "/v1/items/"+doc.ID+"/versions/ver_1767268800/content", token, nil)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(data), codeVersionNotFound) {
		t.Fatalf("version content without versioning: %d %s", resp.StatusCode, data)
	}
}

func TestUnchangedContentsAreNotArchived(t *testing.T) {
	e := newTestEngine(t, "docs", simpleVersioning)
	e.write("log.txt", "written once")
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	doc := ts.itemAt(token, "log.txt")

	// A phone saving a file it only opened: the file takes the new modification time, and nothing is archived
	upload, digest := ts.upload(token, []byte("written once"))
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	modNs := mod.UnixNano()
	var same Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs",
		ItemID: doc.ID, BaseRevision: doc.Revision, Upload: upload, UploadDigest: digest, ModifiedNs: &modNs},
		http.StatusCreated, &same)
	if same.State != stateResultCommitted || same.Item == nil || same.Item.ID != doc.ID {
		t.Fatalf("replace with the same contents: %+v", same)
	}
	if got, _ := e.read("log.txt"); got != "written once" {
		t.Fatalf("file holds %q", got)
	}
	local, ok, _ := e.backend.LocalFile("docs", "log.txt")
	if !ok || !local.ModTime().Equal(mod) {
		t.Fatalf("file is indexed as %+v", local)
	}
	doc = ts.itemAt(token, "log.txt")
	if list := ts.versions(token, doc); len(list.Versions) != 0 {
		t.Fatalf("versions after writing the same contents: %+v", list.Versions)
	}

	// Other contents archive the ones they replace
	upload, digest = ts.upload(token, []byte("written twice"))
	var changed Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs",
		ItemID: doc.ID, BaseRevision: doc.Revision, Upload: upload, UploadDigest: digest}, http.StatusCreated, &changed)
	if changed.State != stateResultCommitted {
		t.Fatalf("replace with other contents: %+v", changed)
	}
	doc = ts.itemAt(token, "log.txt")
	list := ts.versions(token, doc)
	if len(list.Versions) != 1 || list.Versions[0].Size != int64(len("written once")) {
		t.Fatalf("versions after writing other contents: %+v", list.Versions)
	}

	// Restoring a version with the contents the file has archives nothing either
	archived := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	e.archiveVersion("log.txt", "written twice", archived, archived.Add(-time.Hour))
	list = ts.versions(token, doc)
	if len(list.Versions) != 2 {
		t.Fatalf("versions before restoring: %+v", list.Versions)
	}
	var restored Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs",
		ItemID: doc.ID, BaseRevision: doc.Revision, SourceVersion: list.Versions[1].ID, OnConflict: onConflictReject},
		http.StatusCreated, &restored)
	if restored.State != stateResultCommitted {
		t.Fatalf("restore of the same contents: %+v", restored)
	}
	if after := ts.versions(token, ts.itemAt(token, "log.txt")); len(after.Versions) != 2 {
		t.Fatalf("versions after restoring the same contents: %+v", after.Versions)
	}
}

func TestVersionsInCustomPath(t *testing.T) {
	// A custom versions path may be shared with another folder, or point into one: its versions are not served
	versions := t.TempDir()
	e := newTestEngine(t, "docs", func(f *config.FolderConfiguration) {
		simpleVersioning(f)
		f.Versioning.FSPath = versions
		f.Versioning.FSType = config.FilesystemTypeBasic
	})
	e.write("doc.txt", "current")
	archived := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tagged := versioner.TagFilename("doc.txt", archived.In(time.Local).Format(versioner.TimeFormat))
	if err := os.WriteFile(filepath.Join(versions, tagged), []byte("older"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	doc := ts.itemAt(token, "doc.txt")

	list := ts.versions(token, doc)
	if list.Versioning != versioningCustomPath || len(list.Versions) != 0 {
		t.Fatalf("versions in a custom path: %+v", list)
	}
	id := versionID(archived)
	resp, data := ts.do("GET", "/v1/items/"+doc.ID+"/versions/"+id+"/content", token, nil)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(data), codeVersionNotFound) {
		t.Fatalf("version content in a custom path: %d %s", resp.StatusCode, data)
	}
	resp, data = ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace,
		FolderID: "docs", ItemID: doc.ID, BaseRevision: doc.Revision, SourceVersion: id})
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(data), codeVersionNotFound) {
		t.Fatalf("restoring from a custom path: %d %s", resp.StatusCode, data)
	}
}

func TestVersionsPlatformHistory(t *testing.T) {
	// The folder keeps foreign platform metadata with its versions, as the NAS does: the API's replacements archive
	// through the platform history, and restoring copies content only
	e := newTestEngine(t, "docs", func(f *config.FolderConfiguration) {
		f.Versioning = config.VersioningConfiguration{Type: "staggered",
			Params: map[string]string{"preservePlatformData": "true"}}
	})
	e.write("doc.txt", "original")
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	doc := ts.itemAt(token, "doc.txt")

	upload, digest := ts.upload(token, []byte("edited"))
	var edited Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs",
		ItemID: doc.ID, BaseRevision: doc.Revision, Upload: upload, UploadDigest: digest}, http.StatusCreated, &edited)
	if edited.State != stateResultCommitted {
		t.Fatalf("edit: %+v", edited)
	}
	list := ts.versions(token, *edited.Item)
	if list.Versioning != "staggered" || len(list.Versions) != 1 || list.Versions[0].Size != int64(len("original")) {
		t.Fatalf("versions: %+v", list)
	}

	var restored Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs",
		ItemID: doc.ID, BaseRevision: edited.Item.Revision, SourceVersion: list.Versions[0].ID,
		OnConflict: onConflictReject}, http.StatusCreated, &restored)
	if restored.State != stateResultCommitted {
		t.Fatalf("restore: %+v", restored)
	}
	if got, _ := e.read("doc.txt"); got != "original" {
		t.Fatalf("restored file holds %q", got)
	}
	// Staggered versioning keeps one version per 30 seconds at first, so the replaced content may have been thinned out;
	// the restored version stays
	after := ts.versions(token, *restored.Item)
	if !slices.ContainsFunc(after.Versions, func(v ItemVersion) bool { return v.ID == list.Versions[0].ID }) {
		t.Fatalf("versions after restoring: %+v", after.Versions)
	}
}

func TestVersionRestoreRecovery(t *testing.T) {
	// A stop after staging a version: the next run publishes the staged copy of the version it recorded
	e := newTestEngine(t, "docs", simpleVersioning)
	e.write("doc.txt", "current")
	archived := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	e.archiveVersion("doc.txt", "older", archived, archived)
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	doc := ts.itemAt(token, "doc.txt")
	version := ts.versions(token, doc).Versions[0]

	crash := errors.New("simulated crash")
	var crashed atomic.Bool
	ts.journalHook = func(opID string, state string) error {
		if state == journalPublishing || crashed.Load() {
			crashed.Store(true)
			_, _ = ts.store.db.Exec(`UPDATE operations SET journal = ? WHERE op_id = ?`, journalStaged, opID)
			return crash
		}
		return nil
	}
	resp, data := ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace,
		FolderID: "docs", ItemID: doc.ID, BaseRevision: doc.Revision, SourceVersion: version.ID})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("interrupted restore: %d %s", resp.StatusCode, data)
	}
	var pending Operation
	_ = json.Unmarshal(data, &pending)
	if got, _ := e.read("doc.txt"); got != "current" {
		t.Fatalf("the file changed before the simulated crash: %q", got)
	}
	ts.Close()

	ts2 := newTestService(t, e, ts.home)
	waitFor(t, 30*time.Second, "recovery", func() bool {
		op, ok, _ := ts2.loadOperation(ts2.store.db, pending.Operation)
		return ok && op.state != stateResultPending
	})
	var done Operation
	ts2.json("GET", "/v1/operations/"+pending.Operation, token, nil, http.StatusOK, &done)
	if done.State != stateResultCommitted || done.Item == nil || done.Item.ID != doc.ID {
		t.Fatalf("recovered restore: %+v", done)
	}
	if got, _ := e.read("doc.txt"); got != "older" {
		t.Fatalf("recovered restore left %q", got)
	}
}
