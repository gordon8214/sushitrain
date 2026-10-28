// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"golang.org/x/text/unicode/norm"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
)

// The fixtures are the contract the Swift client is tested against too: every field must round-trip
func TestAPIFixtures(t *testing.T) {
	types := map[string]any{
		"capabilities.json":            &Capabilities{},
		"folders.json":                 &FoldersResponse{},
		"item-file.json":               &Item{},
		"item-directory.json":          &Item{},
		"snapshot-page.json":           &SnapshotPage{},
		"changes-page.json":            &ChangesPage{},
		"read-lease.json":              &ReadLease{},
		"manifest-page.json":           &ManifestPage{},
		"upload-created.json":          &Upload{},
		"operation-create.json":        &OperationRequest{},
		"operation-replace.json":       &OperationRequest{},
		"operation-move.json":          &OperationRequest{},
		"operation-delete.json":        &OperationRequest{},
		"operation-pending.json":       &Operation{},
		"operation-committed.json":     &Operation{},
		"operation-conflict.json":      &Operation{},
		"error-cursor-expired.json":    &ErrorResponse{},
		"error-revision-mismatch.json": &ErrorResponse{},
		"pairing-request.json":         &PairingRequest{},
		"pairing-approved.json":        &PairingResult{},
	}
	dir := filepath.Join("..", "..", "Docs", "remote-api", "fixtures")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		target, ok := types[entry.Name()]
		if !ok {
			t.Errorf("fixture %s has no Go type", entry.Name())
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, target); err != nil {
			t.Errorf("%s: %v", entry.Name(), err)
			continue
		}
		encoded, err := json.Marshal(target)
		if err != nil {
			t.Fatal(err)
		}
		var want, got any
		_ = json.Unmarshal(data, &want)
		_ = json.Unmarshal(encoded, &got)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s does not round-trip:\nfixture %s\nencoded %s", entry.Name(), data, encoded)
		}
	}
	// Identifiers in the fixtures have the documented formats
	if _, _, ok := parseContentID(types["item-file.json"].(*Item).ContentID); !ok {
		t.Error("fixture content ID does not parse")
	}
	if !hasIDFormat(types["item-file.json"].(*Item).ID, prefixItem) {
		t.Error("fixture item ID has the wrong format")
	}
}

func TestPairing(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	ts := newTestService(t, e, "")
	ts.export(true)

	session, err := ts.StartPairing("127.0.0.1", 443, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(session.URL, "synctrain+pair://127.0.0.1:443/?") || !strings.Contains(session.URL, "secret=") {
		t.Fatalf("pairing URL %s", session.URL)
	}
	client := newUUID()
	resp, _ := ts.do("POST", "/v1/pairing", "", PairingRequest{Secret: "wrong", ClientID: client, ClientName: "phone"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong secret: %d", resp.StatusCode)
	}
	var pending PairingPending
	ts.json("POST", "/v1/pairing", "", PairingRequest{Secret: session.Secret, ClientID: client, ClientName: "phone"},
		http.StatusAccepted, &pending)
	// Single use
	resp, _ = ts.do("POST", "/v1/pairing", "", PairingRequest{Secret: session.Secret, ClientID: client, ClientName: "x"})
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("reused secret: %d", resp.StatusCode)
	}
	var result PairingResult
	ts.json("GET", "/v1/pairing/"+pending.Request, pending.PollToken, nil, http.StatusAccepted, &result)
	if result.State != pairingPending {
		t.Fatalf("state %q", result.State)
	}
	if err := ts.ApprovePairing(pending.Request, map[string]string{"docs": "write"}); err != nil {
		t.Fatal(err)
	}
	ts.json("GET", "/v1/pairing/"+pending.Request, pending.PollToken, nil, http.StatusOK, &result)
	credential := result.Credential
	// Collected once
	resp, _ = ts.do("GET", "/v1/pairing/"+pending.Request, pending.PollToken, nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("second collection: %d", resp.StatusCode)
	}

	var caps Capabilities
	ts.json("GET", "/v1/capabilities", credential, nil, http.StatusOK, &caps)
	if caps.APIVersion != 1 || !hasIDFormat(caps.ServiceID, prefixService) || caps.Certificate == nil ||
		caps.Certificate.SPKI == "" {
		t.Fatalf("capabilities %+v", caps)
	}
	var folders FoldersResponse
	ts.json("GET", "/v1/folders", credential, nil, http.StatusOK, &folders)
	if len(folders.Folders) != 1 || !folders.Folders[0].Permissions.Write {
		t.Fatalf("folders %+v", folders)
	}
	resp, _ = ts.do("GET", "/v1/capabilities", "", nil)
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("no credential: %d", resp.StatusCode)
	}
	if err := ts.RevokeClient(client); err != nil {
		t.Fatal(err)
	}
	resp, _ = ts.do("GET", "/v1/capabilities", credential, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked credential: %d", resp.StatusCode)
	}
}

func TestSnapshotAndChanges(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	for _, name := range []string{"a.txt", "b/c.txt", "b/d.txt", "e.txt"} {
		e.write(name, "contents of "+name)
	}
	ts := newTestService(t, e, "")
	ts.export(false)
	token := ts.pair(map[string]string{"docs": "read"})

	var page SnapshotPage
	ts.json("GET", "/v1/folders/docs/snapshot?limit=2", token, nil, http.StatusOK, &page)
	items := page.Items
	for page.Next != "" {
		next := page
		page = SnapshotPage{}
		ts.json("GET", "/v1/folders/docs/snapshot?limit=2&snapshot="+next.Snapshot+"&page="+next.Next, token, nil,
			http.StatusOK, &page)
		// A repeated page request gives the same page
		var again SnapshotPage
		ts.json("GET", "/v1/folders/docs/snapshot?limit=2&snapshot="+next.Snapshot+"&page="+next.Next, token, nil,
			http.StatusOK, &again)
		if len(again.Items) != len(page.Items) || len(page.Items) > 0 && again.Items[0].ID != page.Items[0].ID {
			t.Fatal("snapshot page is not repeatable")
		}
		items = append(items, page.Items...)
	}
	paths := map[string]Item{}
	for _, item := range items {
		paths[item.Path] = item
	}
	for _, want := range []string{"", "a.txt", "b", "b/c.txt", "b/d.txt", "e.txt"} {
		if _, ok := paths[want]; !ok {
			t.Fatalf("snapshot lacks %q: %v", want, paths)
		}
	}
	if paths["b/c.txt"].ParentID == nil || *paths["b/c.txt"].ParentID != paths["b"].ID || paths["a.txt"].Writable {
		t.Fatal("parent or permissions wrong")
	}
	if _, _, ok := parseContentID(paths["a.txt"].ContentID); !ok || paths["a.txt"].Size != int64(len("contents of a.txt")) {
		t.Fatalf("file metadata %+v", paths["a.txt"])
	}

	var changes ChangesPage
	ts.json("GET", "/v1/folders/docs/changes?cursor="+page.Cursor, token, nil, http.StatusOK, &changes)
	since := changes.Cursor

	e.write("new.txt", "new")
	if err := os.Remove(filepath.Join(e.folderPath, "e.txt")); err != nil {
		t.Fatal(err)
	}
	e.write("a.txt", "changed contents")
	ts.sync()
	ts.json("GET", "/v1/folders/docs/changes?cursor="+since, token, nil, http.StatusOK, &changes)
	byPath := map[string]Item{}
	for _, c := range changes.Changes {
		byPath[c.Path] = c
	}
	if _, ok := byPath["new.txt"]; !ok {
		t.Fatalf("created file missing from changes: %v", byPath)
	}
	if !byPath["e.txt"].Deleted || byPath["e.txt"].ID != paths["e.txt"].ID {
		t.Fatal("deletion is not a tombstone of the same item")
	}
	if a := byPath["a.txt"]; a.ID != paths["a.txt"].ID || a.Revision == paths["a.txt"].Revision ||
		a.ContentID == paths["a.txt"].ContentID {
		t.Fatal("edited file did not keep its ID with a new revision")
	}

	// Long poll wakes up on a change
	done := make(chan ChangesPage)
	go func() {
		var c ChangesPage
		ts.json("GET", "/v1/folders/docs/changes?wait=20&cursor="+changes.Cursor, token, nil, http.StatusOK, &c)
		done <- c
	}()
	time.Sleep(200 * time.Millisecond)
	e.write("later.txt", "later")
	ts.sync()
	select {
	case c := <-done:
		if len(c.Changes) == 0 {
			t.Fatal("long poll returned no changes")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("long poll did not wake up")
	}

	resp, data := ts.do("GET", "/v1/folders/docs/changes?cursor=g999.1", token, nil)
	if resp.StatusCode != http.StatusGone || !strings.Contains(string(data), codeCursorExpired) {
		t.Fatalf("cursor from another generation: %d %s", resp.StatusCode, data)
	}
	gen, _ := ts.store.generation()
	resp, _ = ts.do("GET", "/v1/folders/docs/changes?cursor="+cursor{generation: gen, sequence: 1 << 40}.String(),
		token, nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("cursor beyond the head: %d", resp.StatusCode)
	}
	if after, _ := ts.store.generation(); after != gen+1 {
		t.Fatal("a cursor beyond the head did not start a new generation")
	}
}

func TestReadLeaseAndContent(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	contents := strings.Repeat("0123456789abcdef", 64<<10) // 1 MiB, several blocks
	e.write("big.bin", contents)
	ts := newTestService(t, e, "")
	ts.export(false)
	token := ts.pair(map[string]string{"docs": "read"})
	item := ts.itemAt(token, "big.bin")
	if item.Availability != availabilityLocal {
		t.Fatalf("availability %q", item.Availability)
	}

	var lease ReadLease
	ts.json("POST", "/v1/items/"+item.ID+"/read-leases", token, ReadLeaseRequest{Revision: item.Revision},
		http.StatusCreated, &lease)
	var again ReadLease
	ts.json("POST", "/v1/items/"+item.ID+"/read-leases", token, ReadLeaseRequest{Revision: item.Revision},
		http.StatusOK, &again)
	if again.Lease != lease.Lease || again.Token != lease.Token {
		t.Fatal("repeating a lease request did not return the same lease")
	}
	resp, _ := ts.do("POST", "/v1/items/"+item.ID+"/read-leases", token, ReadLeaseRequest{Revision: "r1.stale"})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale revision: %d", resp.StatusCode)
	}

	var manifest ManifestPage
	ts.json("GET", "/v1/content/"+lease.Lease+"/manifest?limit=3", lease.Token, nil, http.StatusOK, &manifest)
	if len(manifest.Blocks) != 3 || manifest.Next == nil || manifest.ContentID != lease.ContentID {
		t.Fatalf("manifest page %+v", manifest)
	}

	resp, data := ts.do("GET", "/v1/content/"+lease.Lease, lease.Token, nil)
	if resp.StatusCode != http.StatusOK || string(data) != contents || resp.Header.Get("ETag") != `"`+lease.ContentID+`"` {
		t.Fatalf("content: %d, %d bytes", resp.StatusCode, len(data))
	}
	resp, data = ts.do("GET", "/v1/content/"+lease.Lease, lease.Token, nil, "Range", "bytes=200000-400000")
	if resp.StatusCode != http.StatusPartialContent || string(data) != contents[200000:400001] {
		t.Fatalf("range: %d, %d bytes", resp.StatusCode, len(data))
	}
	resp, data = ts.do("GET", "/v1/content/"+lease.Lease, lease.Token, nil, "Range", "bytes=0-9", "If-Range", `"other"`)
	if resp.StatusCode != http.StatusOK || len(data) != len(contents) {
		t.Fatalf("If-Range mismatch should send everything: %d", resp.StatusCode)
	}
	// A lease token works only for its own lease and never for metadata
	resp, _ = ts.do("GET", "/v1/items/"+item.ID, lease.Token, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("lease token on metadata: %d", resp.StatusCode)
	}

	// Damage the local copy without changing its size or modification time: it fails verification, and with no
	// peer the content is unavailable rather than served wrong
	path := filepath.Join(e.folderPath, "big.bin")
	stat, _ := os.Stat(path)
	damaged := []byte(contents)
	damaged[300000] ^= 0xff
	if err := os.WriteFile(path, damaged, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stat.ModTime(), stat.ModTime()); err != nil {
		t.Fatal(err)
	}
	ts.content.mutex.Lock()
	for _, rep := range ts.content.reps {
		ts.content.removeLocked(rep)
	}
	ts.content.mutex.Unlock()
	resp, data = ts.do("GET", "/v1/content/"+lease.Lease, lease.Token, nil, "Range", "bytes=262144-393215")
	if resp.StatusCode == http.StatusPartialContent && string(data) == string(damaged[262144:393216]) {
		t.Fatal("served bytes that do not match the block hash")
	}
}

func createFile(ts *testService, token, parentID, name string, contents []byte) Operation {
	ts.t.Helper()
	upload, digest := ts.upload(token, contents)
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixNano()
	var op Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: ts.e.folderID,
		ParentID: parentID, Name: name, ItemKind: kindFile, Upload: upload, UploadDigest: digest, ModifiedNs: &mod},
		http.StatusCreated, &op)
	return op
}

func TestOperations(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	e.write("keep.txt", "original")
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	root := ts.itemAt(token, "")

	// Create
	op := createFile(ts, token, root.ID, "notes.txt", []byte("hello"))
	if op.State != stateResultCommitted || op.Item == nil || op.Item.Path != "notes.txt" || op.ContentID == "" {
		t.Fatalf("create: %+v", op)
	}
	if got, _ := e.read("notes.txt"); got != "hello" {
		t.Fatalf("created file holds %q", got)
	}
	local, ok, _ := e.backend.LocalFile("docs", "notes.txt")
	if !ok || local.Size != 5 || contentIDOf(local) != op.ContentID {
		t.Fatal("created file is not indexed as committed")
	}
	if local.ModTime().UTC() != time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) {
		t.Fatalf("modification time %v", local.ModTime())
	}

	// Replay: same answer; different payload: rejected
	upload, digest := ts.upload(token, []byte("x"))
	req := OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: "docs", ParentID: root.ID, Name: "x.txt",
		ItemKind: kindFile, Upload: upload, UploadDigest: digest}
	var first, replay Operation
	ts.json("POST", "/v1/operations", token, req, http.StatusCreated, &first)
	ts.json("POST", "/v1/operations", token, req, http.StatusOK, &replay)
	if replay.Operation != first.Operation || replay.State != first.State {
		t.Fatal("replay returned a different operation")
	}
	req.Name = "y.txt"
	resp, _ := ts.do("POST", "/v1/operations", token, req)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("replay with another payload: %d", resp.StatusCode)
	}

	// Name conflict and reserved names
	upload, digest = ts.upload(token, []byte("dup"))
	resp, data := ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: "docs",
		ParentID: root.ID, Name: "notes.txt", ItemKind: kindFile, Upload: upload, UploadDigest: digest})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(data), codeNameConflict) {
		t.Fatalf("existing name: %d %s", resp.StatusCode, data)
	}
	upload, digest = ts.upload(token, []byte("x"))
	resp, _ = ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: "docs",
		ParentID: root.ID, Name: ".stignore", ItemKind: kindFile, Upload: upload, UploadDigest: digest})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("reserved name: %d", resp.StatusCode)
	}

	// Replace with the current base
	notes := ts.itemAt(token, "notes.txt")
	upload, digest = ts.upload(token, []byte("hello, world"))
	var replaced Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs",
		ItemID: notes.ID, BaseRevision: notes.Revision, Upload: upload, UploadDigest: digest}, http.StatusCreated, &replaced)
	if replaced.State != stateResultCommitted || replaced.Item.ID != notes.ID || replaced.Item.Revision == notes.Revision {
		t.Fatalf("replace: %+v", replaced)
	}
	if got, _ := e.read("notes.txt"); got != "hello, world" {
		t.Fatalf("replaced file holds %q", got)
	}

	// Replace with a stale base: the edit is kept as a conflict copy, or rejected when asked
	upload, digest = ts.upload(token, []byte("stale edit"))
	var conflict Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs",
		ItemID: notes.ID, BaseRevision: notes.Revision, Upload: upload, UploadDigest: digest}, http.StatusCreated, &conflict)
	if conflict.State != stateResultConflict || conflict.ConflictItem == nil ||
		!strings.Contains(conflict.ConflictItem.Name, ".sync-conflict-") {
		t.Fatalf("conflict: %+v", conflict)
	}
	if got, _ := e.read(conflict.ConflictItem.Path); got != "stale edit" {
		t.Fatalf("conflict copy holds %q", got)
	}
	if got, _ := e.read("notes.txt"); got != "hello, world" {
		t.Fatal("a stale edit overwrote the current version")
	}
	upload, digest = ts.upload(token, []byte("rejected"))
	resp, _ = ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs",
		ItemID: notes.ID, BaseRevision: notes.Revision, Upload: upload, UploadDigest: digest, OnConflict: onConflictReject})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("rejected conflict: %d", resp.StatusCode)
	}

	// An edit on disk the index hasn't seen makes the replacement a conflict, not an overwrite
	keep := ts.itemAt(token, "keep.txt")
	time.Sleep(10 * time.Millisecond)
	e.write("keep.txt", "edited on the host")
	upload, digest = ts.upload(token, []byte("edited through the API"))
	var unscanned Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs",
		ItemID: keep.ID, BaseRevision: keep.Revision, Upload: upload, UploadDigest: digest}, http.StatusCreated, &unscanned)
	if unscanned.State != stateResultConflict {
		t.Fatalf("unscanned edit: %+v", unscanned)
	}
	if got, _ := e.read("keep.txt"); got != "edited on the host" {
		t.Fatal("an unscanned edit on the host was overwritten")
	}

	// Directories, moves and deletes
	var mkdir Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: "docs",
		ParentID: root.ID, Name: "dir", ItemKind: kindDirectory}, http.StatusCreated, &mkdir)
	if mkdir.State != stateResultCommitted || mkdir.Item.Kind != kindDirectory {
		t.Fatalf("mkdir: %+v", mkdir)
	}
	notes = ts.itemAt(token, "notes.txt")
	var moved Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opMove, FolderID: "docs",
		ItemID: notes.ID, BaseRevision: notes.Revision, ParentID: mkdir.Item.ID, Name: "renamed.txt"},
		http.StatusCreated, &moved)
	if moved.State != stateResultCommitted || moved.Item.ID != notes.ID || moved.Item.Path != "dir/renamed.txt" {
		t.Fatalf("move: %+v", moved)
	}
	if got, _ := e.read("dir/renamed.txt"); got != "hello, world" {
		t.Fatal("moved file missing")
	}
	if _, ok := e.read("notes.txt"); ok {
		t.Fatal("moved file still at its old path")
	}
	movedLocal, ok, _ := e.backend.LocalFile("docs", "dir/renamed.txt")
	if !ok || contentIDOf(movedLocal) != notes.ContentID {
		t.Fatal("a move did not keep the file's block list")
	}
	if old, ok, _ := e.backend.LocalFile("docs", "notes.txt"); !ok || !old.Deleted {
		t.Fatal("the old path is not indexed as deleted")
	}

	dir := ts.itemAt(token, "dir")
	var deleted Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opDelete, FolderID: "docs",
		ItemID: dir.ID, BaseRevision: dir.Revision, SubtreeGeneration: &dir.SubtreeGeneration}, http.StatusCreated, &deleted)
	if deleted.State != stateResultCommitted || !deleted.Item.Deleted {
		t.Fatalf("delete: %+v", deleted)
	}
	if _, err := os.Stat(filepath.Join(e.folderPath, "dir")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted directory still on disk")
	}
	// The scanner agrees with what was committed
	before, _ := e.backend.LocalSequence("docs")
	e.scan()
	if after, _ := e.backend.LocalSequence("docs"); after != before {
		t.Fatalf("a scan found differences after the operations (%d -> %d)", before, after)
	}
}

func TestOperationRecoveryAfterPublication(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	root := ts.itemAt(token, "")

	crash := errors.New("simulated crash")
	ts.journalHook = func(_ string, state string) error {
		if state == journalPublished {
			return crash
		}
		return nil
	}
	upload, digest := ts.upload(token, []byte("survives"))
	resp, data := ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: "docs",
		ParentID: root.ID, Name: "crash.txt", ItemKind: kindFile, Upload: upload, UploadDigest: digest})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("interrupted operation: %d %s", resp.StatusCode, data)
	}
	var pending Operation
	_ = json.Unmarshal(data, &pending)
	if _, ok := e.read("crash.txt"); !ok {
		t.Fatal("the file was not published before the simulated crash")
	}
	ts.Close()

	// A new service on the same database finishes the operation from its journal
	ts2 := newTestService(t, e, ts.home)
	waitFor(t, 30*time.Second, "recovery", func() bool {
		op, ok, _ := ts2.loadOperation(ts2.store.db, pending.Operation)
		return ok && op.state != stateResultPending
	})
	var done Operation
	ts2.json("GET", "/v1/operations/"+pending.Operation, token, nil, http.StatusOK, &done)
	if done.State != stateResultCommitted || done.Item == nil || done.Item.Path != "crash.txt" {
		t.Fatalf("recovered operation: %+v", done)
	}
	local, ok, _ := e.backend.LocalFile("docs", "crash.txt")
	if !ok || local.Size != int64(len("survives")) {
		t.Fatal("recovered file not indexed")
	}
}

func TestUploadResume(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	contents := []byte(strings.Repeat("upload ", 1000))
	// A new upload, sent in two parts with a checksum each, one of them wrong
	sum := sha256Of(contents)
	meta := "folder ZG9jcw==,digest " + b64std(sum)
	resp, data := ts.do("POST", "/v1/uploads", token, []byte{}, "Tus-Resumable", tusVersion, "Upload-Length", itoa(len(contents)),
		"Upload-Metadata", meta)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, data)
	}
	var u Upload
	_ = json.Unmarshal(data, &u)
	half := len(contents) / 2
	resp, _ = ts.do("PATCH", "/v1/uploads/"+u.Upload, u.Token, contents[:half], "Tus-Resumable", tusVersion, "Content-Type",
		"application/offset+octet-stream", "Upload-Offset", "0", "Upload-Checksum", "sha256 "+b64std(sha256Of([]byte("wrong"))))
	if resp.StatusCode != statusChecksum {
		t.Fatalf("bad checksum: %d", resp.StatusCode)
	}
	resp, _ = ts.do("HEAD", "/v1/uploads/"+u.Upload, u.Token, nil, "Tus-Resumable", tusVersion)
	if resp.Header.Get("Upload-Offset") != "0" {
		t.Fatalf("offset after a rejected checksum: %s", resp.Header.Get("Upload-Offset"))
	}
	resp, _ = ts.do("PATCH", "/v1/uploads/"+u.Upload, u.Token, contents[:half], "Tus-Resumable", tusVersion, "Content-Type",
		"application/offset+octet-stream", "Upload-Offset", "0", "Upload-Checksum", "sha256 "+b64std(sha256Of(contents[:half])))
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Upload-Offset") != itoa(half) {
		t.Fatalf("first part: %d", resp.StatusCode)
	}
	resp, _ = ts.do("PATCH", "/v1/uploads/"+u.Upload, u.Token, contents[half:], "Tus-Resumable", tusVersion, "Content-Type",
		"application/offset+octet-stream", "Upload-Offset", "0")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("wrong offset: %d", resp.StatusCode)
	}
	resp, _ = ts.do("PATCH", "/v1/uploads/"+u.Upload, u.Token, contents[half:], "Tus-Resumable", tusVersion, "Content-Type",
		"application/offset+octet-stream", "Upload-Offset", itoa(half))
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Upload-Offset") != itoa(len(contents)) {
		t.Fatalf("second part: %d", resp.StatusCode)
	}
	if up, ok, _ := ts.uploadByID(ts.store.db, u.Upload); !ok || !up.complete {
		t.Fatal("upload not complete")
	}
	// Another client's token cannot touch it
	other := ts.pair(map[string]string{"docs": "write"})
	resp, _ = ts.do("HEAD", "/v1/uploads/"+u.Upload, other, nil, "Tus-Resumable", tusVersion)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another client's upload: %d", resp.StatusCode)
	}
}

func TestReadOnlyAndIgnored(t *testing.T) {
	e := newTestEngine(t, "docs", func(f *config.FolderConfiguration) {})
	ts := newTestService(t, e, "")
	ts.export(true)
	if err := e.app.Internals.SetIgnores("docs", []string{"*.tmp"}); err != nil {
		t.Fatal(err)
	}
	reader := ts.pair(map[string]string{"docs": "read"})
	writer := ts.pair(map[string]string{"docs": "write"})
	root := ts.itemAt(writer, "")

	resp, _ := ts.do("POST", "/v1/uploads", reader, []byte{}, "Tus-Resumable", tusVersion, "Upload-Length", "1",
		"Upload-Metadata", "folder ZG9jcw==,digest "+b64std(sha256Of([]byte("x"))))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only upload: %d", resp.StatusCode)
	}
	upload, digest := ts.upload(writer, []byte("x"))
	resp, data := ts.do("POST", "/v1/operations", writer, OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: "docs",
		ParentID: root.ID, Name: "scratch.tmp", ItemKind: kindFile, Upload: upload, UploadDigest: digest})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(data), codeIgnoredPath) {
		t.Fatalf("ignored result: %d %s", resp.StatusCode, data)
	}
}

func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func b64std(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func TestUnicodeNames(t *testing.T) {
	// Item paths are in Syncthing's normalized form (NFC) on every host, also where the filesystem and Syncthing's
	// native names use NFD (macOS), so that an item created through the service is one item
	e := newTestEngine(t, "docs", nil)
	e.write(norm.NFD.String("naïve.txt"), "decomposed on disk")
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	root := ts.itemAt(token, "")

	op := createFile(ts, token, root.ID, "café.txt", []byte("hello"))
	if op.State != stateResultCommitted || op.Item.Path != norm.NFC.String("café.txt") {
		t.Fatalf("create: %+v", op)
	}
	ts.sync()
	var page SnapshotPage
	ts.json("GET", "/v1/folders/docs/snapshot", token, nil, http.StatusOK, &page)
	counts := map[string]int{}
	for _, item := range page.Items {
		counts[item.Path]++
	}
	for _, name := range []string{"café.txt", "naïve.txt"} {
		if counts[norm.NFC.String(name)] != 1 || counts[norm.NFD.String(name)] != 0 && norm.NFD.String(name) != norm.NFC.String(name) {
			t.Errorf("%s: %v", name, counts)
		}
	}
}

func TestOperationRecoveryBeforePublicationRecorded(t *testing.T) {
	// A stop between swapping the edit into place and recording it: recovery recognizes the staged file at the target
	// and finishes the replacement, rather than taking the edit for a conflict with itself
	e := newTestEngine(t, "docs", nil)
	e.write("doc.txt", "original")
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	doc := ts.itemAt(token, "doc.txt")

	crash := errors.New("simulated crash")
	var crashed atomic.Bool
	ts.journalHook = func(opID string, state string) error {
		if state == journalPublished || crashed.Load() {
			// The database only ever got as far as publishing
			crashed.Store(true)
			_, _ = ts.store.db.Exec(`UPDATE operations SET journal = ? WHERE op_id = ?`, journalPublishing, opID)
			return crash
		}
		return nil
	}
	upload, digest := ts.upload(token, []byte("edited"))
	resp, data := ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs",
		ItemID: doc.ID, BaseRevision: doc.Revision, Upload: upload, UploadDigest: digest})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("interrupted operation: %d %s", resp.StatusCode, data)
	}
	var pending Operation
	_ = json.Unmarshal(data, &pending)
	if got, _ := e.read("doc.txt"); got != "edited" {
		t.Fatalf("the edit was not in place before the simulated crash: %q", got)
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
		t.Fatalf("recovered operation: %+v", done)
	}
	entries, err := os.ReadDir(e.folderPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".sync-conflict-") || strings.HasPrefix(entry.Name(), ".syncthing.") {
			t.Errorf("left %s", entry.Name())
		}
	}
	local, ok, _ := e.backend.LocalFile("docs", "doc.txt")
	if !ok || local.Size != int64(len("edited")) {
		t.Fatal("recovered edit not indexed")
	}
}

func TestEmptyFile(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	root := ts.itemAt(token, "")
	op := createFile(ts, token, root.ID, "empty.txt", nil)
	if op.State != stateResultCommitted {
		t.Fatalf("create empty file: %+v", op)
	}
	if got, ok := e.read("empty.txt"); !ok || got != "" {
		t.Fatalf("empty file holds %q", got)
	}
}

func TestMoveToAnotherFolder(t *testing.T) {
	// The item keeps its ID in its new folder, and the folder it left records a tombstone naming that folder, so
	// that clients of only the old folder see it go
	e := newTestEngine(t, "docs", nil)
	e.write("dir/inside.txt", "inside")
	otherPath := e.addFolder("other")
	ts := newTestService(t, e, "")
	ts.export(true)
	if err := ts.SetExport("other", true); err != nil {
		t.Fatal(err)
	}
	ts.syncFolder("other")
	token := ts.pair(map[string]string{"docs": "write", "other": "write"})
	dir := ts.itemAt(token, "dir")
	inside := ts.itemAt(token, "dir/inside.txt")

	cursorOf := func(folder string) string {
		var page SnapshotPage
		ts.json("GET", "/v1/folders/"+folder+"/snapshot", token, nil, http.StatusOK, &page)
		return page.Cursor
	}
	docsCursor, otherCursor := cursorOf("docs"), cursorOf("other")
	var otherPage SnapshotPage
	ts.json("GET", "/v1/folders/other/snapshot", token, nil, http.StatusOK, &otherPage)
	var otherRoot Item
	for _, item := range otherPage.Items {
		if item.Path == "" {
			otherRoot = item
		}
	}

	var moved Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opMove, FolderID: "docs",
		ItemID: dir.ID, BaseRevision: dir.Revision, ParentID: otherRoot.ID, Name: "dir", DestinationFolderID: "other"},
		http.StatusCreated, &moved)
	if moved.State != stateResultCommitted || moved.Item.ID != dir.ID || moved.Item.FolderID != "other" {
		t.Fatalf("move: %+v", moved)
	}
	if data, err := os.ReadFile(filepath.Join(otherPath, "dir", "inside.txt")); err != nil || string(data) != "inside" {
		t.Fatalf("moved file: %q %v", data, err)
	}

	changes := func(folder, cursor string) map[string]Item {
		var page ChangesPage
		ts.json("GET", "/v1/folders/"+folder+"/changes?cursor="+cursor, token, nil, http.StatusOK, &page)
		byID := map[string]Item{}
		for _, item := range page.Changes {
			byID[item.ID] = item
		}
		return byID
	}
	left := changes("docs", docsCursor)
	for _, id := range []string{dir.ID, inside.ID} {
		if item, ok := left[id]; !ok || !item.Deleted || item.MovedToFolderID != "other" || item.FolderID != "docs" {
			t.Errorf("tombstone in the folder it left: %+v (%v)", item, ok)
		}
	}
	arrived := changes("other", otherCursor)
	for _, id := range []string{dir.ID, inside.ID} {
		if item, ok := arrived[id]; !ok || item.Deleted || item.FolderID != "other" || item.MovedToFolderID != "" {
			t.Errorf("item in its new folder: %+v (%v)", item, ok)
		}
	}
}
