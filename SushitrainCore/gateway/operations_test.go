// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests are about how operations on a folder this device holds in full end: what a folder's ignore patterns and
// the items that are already there refuse, and what a restart in the middle of a delete leaves.

const operationTimeout = 30 * time.Second

// operate sends an operation and returns how it ended; a refused operation comes with the error. It leaves scanning to
// the test: a scan would index what the operation failed to.
func (ts *testService) operate(token string, req OperationRequest) Operation {
	ts.t.Helper()
	resp, data := ts.do("POST", "/v1/operations", token, req)
	if resp.StatusCode >= 400 {
		var refused struct {
			Operation *Operation `json:"operation"`
		}
		if err := json.Unmarshal(data, &refused); err != nil || refused.Operation == nil {
			ts.t.Fatalf("operation: %d %s", resp.StatusCode, data)
		}
		return *refused.Operation
	}
	var op Operation
	if err := json.Unmarshal(data, &op); err != nil {
		ts.t.Fatal(err)
	}
	waitFor(ts.t, operationTimeout, "the operation", func() bool {
		ts.json("GET", "/v1/operations/"+op.Operation, token, nil, http.StatusOK, &op)
		return op.State != stateResultPending
	})
	return op
}

func moveRequest(item, parent Item, name string) OperationRequest {
	return OperationRequest{ID: newUUID(), Kind: opMove, FolderID: item.FolderID, ItemID: item.ID,
		BaseRevision: item.Revision, ParentID: parent.ID, Name: name}
}

func deleteRequest(item Item) OperationRequest {
	return OperationRequest{ID: newUUID(), Kind: opDelete, FolderID: item.FolderID, ItemID: item.ID,
		BaseRevision: item.Revision, SubtreeGeneration: &item.SubtreeGeneration}
}

func createDirectoryRequest(parent Item, name string) OperationRequest {
	return OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: parent.FolderID, ParentID: parent.ID, Name: name,
		ItemKind: kindDirectory}
}

func wantRefused(t *testing.T, op Operation, code string) {
	t.Helper()
	if op.State != stateResultFailed || op.Error == nil || op.Error.Code != code {
		t.Fatalf("want it refused with %s: %+v (%+v)", code, op, op.Error)
	}
}

func wantCommitted(t *testing.T, op Operation) {
	t.Helper()
	if op.State != stateResultCommitted {
		t.Fatalf("want it committed: %+v (%+v)", op, op.Error)
	}
}

func (e *testEngine) sequence() int64 {
	e.t.Helper()
	sequence, err := e.backend.LocalSequence(e.folderID)
	if err != nil {
		e.t.Fatal(err)
	}
	return sequence
}

// holds reports whether an entry exists at name, whatever it is
func (e *testEngine) holds(name string) bool {
	_, err := os.Lstat(filepath.Join(e.folderPath, filepath.FromSlash(name)))
	return err == nil
}

// wantFiles fails unless the folder holds these files on disk, with these contents
func (e *testEngine) wantFiles(files map[string]string) {
	e.t.Helper()
	for name, contents := range files {
		if got, ok := e.read(name); !ok || got != contents {
			e.t.Fatalf("%s holds %q (%v), want %q", name, got, ok, contents)
		}
	}
}

// filesHolding returns every file in the folder with these contents, temporary files included
func (e *testEngine) filesHolding(contents string) []string {
	e.t.Helper()
	var found []string
	err := filepath.WalkDir(e.folderPath, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if data, err := os.ReadFile(p); err == nil && string(data) == contents {
			rel, err := filepath.Rel(e.folderPath, p)
			if err != nil {
				return err
			}
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return found
}

// wantGone fails if the folder holds anything at these paths
func (e *testEngine) wantGone(names ...string) {
	e.t.Helper()
	for _, name := range names {
		if e.holds(name) {
			e.t.Fatalf("%s is still there", name)
		}
	}
}

// wantScanAgrees fails if a scan finds anything that differs from what the index said at the sequence
func (e *testEngine) wantScanAgrees(sequence int64) {
	e.t.Helper()
	e.scan()
	if after := e.sequence(); after != sequence {
		e.t.Fatalf("a scan found differences (%d -> %d)", sequence, after)
	}
}

func TestMoveRefusedByIgnorePatterns(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	files := map[string]string{"notes.txt": "notes", "dir/plain.txt": "plain", "dir/secret.txt": "secret",
		"target/x.txt": "x"}
	for name, contents := range files {
		e.write(name, contents)
	}
	ts := newTestService(t, e, "")
	ts.export(true)
	if err := e.app.Internals.SetIgnores("docs", []string{"*.tmp", "/excluded", "/moved/secret.txt"}); err != nil {
		t.Fatal(err)
	}
	token := ts.pair(map[string]string{"docs": "write"})
	root, target := ts.itemAt(token, ""), ts.itemAt(token, "target")
	notes, dir := ts.itemAt(token, "notes.txt"), ts.itemAt(token, "dir")
	sequence := e.sequence()

	for _, tc := range []struct {
		name   string
		req    OperationRequest
		target string
	}{
		{"a file onto a pattern for its name", moveRequest(notes, root, "notes.tmp"), "notes.tmp"},
		{"a file into another directory", moveRequest(notes, target, "notes.tmp"), "target/notes.tmp"},
		{"a file onto an excluded path", moveRequest(notes, root, "excluded"), "excluded"},
		{"a directory onto an excluded path", moveRequest(dir, root, "excluded"), "excluded"},
		// Nothing is wrong with the name or at the old path: it's what is in the directory that would be excluded
		{"a directory with contents that would be excluded", moveRequest(dir, root, "moved"), "moved"},
	} {
		t.Log(tc.name)
		wantRefused(t, ts.operate(token, tc.req), codeIgnoredPath)
		if e.holds(tc.target) {
			t.Fatalf("%s exists", tc.target)
		}
		e.wantFiles(files)
		e.wantScanAgrees(sequence)
	}

	// It is the patterns that say so
	if err := e.app.Internals.SetIgnores("docs", []string{"*.tmp", "/excluded"}); err != nil {
		t.Fatal(err)
	}
	wantCommitted(t, ts.operate(token, moveRequest(dir, root, "moved")))
	e.wantFiles(map[string]string{"moved/plain.txt": "plain", "moved/secret.txt": "secret"})
	e.wantGone("dir")
}

// The patterns of the folder a move ends up in are the ones that count
func TestMoveToAnotherFolderRefusedByItsIgnorePatterns(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	e.write("notes.txt", "notes")
	other := e.addFolder("other")
	ts := newTestService(t, e, "")
	ts.export(true)
	if err := ts.SetExport("other", true); err != nil {
		t.Fatal(err)
	}
	ts.syncFolder("other")
	if err := e.app.Internals.SetIgnores("other", []string{"*.tmp"}); err != nil {
		t.Fatal(err)
	}
	token := ts.pair(map[string]string{"docs": "write", "other": "write"})
	notes := ts.itemAt(token, "notes.txt")
	var page SnapshotPage
	ts.json("GET", "/v1/folders/other/snapshot", token, nil, http.StatusOK, &page)
	root := slices.IndexFunc(page.Items, func(item Item) bool { return item.Path == "" })
	if root < 0 {
		t.Fatalf("no root in %+v", page.Items)
	}
	otherRoot := page.Items[root]

	move := func(name string) OperationRequest {
		req := moveRequest(notes, otherRoot, name)
		req.DestinationFolderID = "other"
		return req
	}
	wantRefused(t, ts.operate(token, move("notes.tmp")), codeIgnoredPath)
	e.wantFiles(map[string]string{"notes.txt": "notes"})
	if _, err := os.Lstat(filepath.Join(other, "notes.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file arrived: %v", err)
	}
	// The same name is fine in the folder it comes from, which has no such patterns
	wantCommitted(t, ts.operate(token, moveRequest(notes, ts.itemAt(token, ""), "notes.tmp")))
	e.wantFiles(map[string]string{"notes.tmp": "notes"})
}

func TestCreateDirectoryRefusedByIgnorePatterns(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	e.write("dir/x.txt", "x")
	ts := newTestService(t, e, "")
	ts.export(true)
	if err := e.app.Internals.SetIgnores("docs", []string{"*.tmp", "/excluded"}); err != nil {
		t.Fatal(err)
	}
	token := ts.pair(map[string]string{"docs": "write"})
	root, dir := ts.itemAt(token, ""), ts.itemAt(token, "dir")
	sequence := e.sequence()

	for _, tc := range []struct {
		parent Item
		name   string
		path   string
	}{{root, "scratch.tmp", "scratch.tmp"}, {dir, "scratch.tmp", "dir/scratch.tmp"}, {root, "excluded", "excluded"}} {
		wantRefused(t, ts.operate(token, createDirectoryRequest(tc.parent, tc.name)), codeIgnoredPath)
		e.wantGone(tc.path)
		e.wantScanAgrees(sequence)
	}
	// Only there: the same name is fine one level down, which the anchored pattern doesn't take in
	wantCommitted(t, ts.operate(token, createDirectoryRequest(dir, "excluded")))
	if info, err := os.Stat(filepath.Join(e.folderPath, "dir", "excluded")); err != nil || !info.IsDir() {
		t.Fatalf("the directory: %v", err)
	}
}

func TestMoveOntoExistingItem(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	files := map[string]string{"a.txt": "a", "b.txt": "b", "dir/inside.txt": "inside", "target/a.txt": "taken",
		"target/dir/other.txt": "other"}
	for name, contents := range files {
		e.write(name, contents)
	}
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	root, target := ts.itemAt(token, ""), ts.itemAt(token, "target")
	file, dir := ts.itemAt(token, "a.txt"), ts.itemAt(token, "dir")
	sequence := e.sequence()

	for _, tc := range []struct {
		name string
		req  OperationRequest
		path string
	}{
		{"a file onto a file", moveRequest(file, root, "b.txt"), "b.txt"},
		{"a file onto a file in another directory", moveRequest(file, target, "a.txt"), "target/a.txt"},
		{"a file onto a directory", moveRequest(file, root, "dir"), "dir"},
		{"a directory onto a file", moveRequest(dir, root, "b.txt"), "b.txt"},
		{"a directory onto a directory", moveRequest(dir, target, "dir"), "target/dir"},
	} {
		t.Log(tc.name)
		op := ts.operate(token, tc.req)
		wantRefused(t, op, codeNameConflict)
		// The client learns what is in the way
		if op.Error.Item == nil || op.Error.Item.Path != tc.path {
			t.Fatalf("the conflict names %+v", op.Error.Item)
		}
		e.wantFiles(files)
		e.wantScanAgrees(sequence)
	}

	// A file no scan has seen yet is in the way just the same, and may be the only copy of it
	e.write("unscanned.txt", "only here so far")
	resp, data := ts.do("POST", "/v1/operations", token, moveRequest(file, root, "unscanned.txt"))
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(data), codeNameConflict) {
		t.Fatalf("a file no scan has seen: %d %s", resp.StatusCode, data)
	}
	e.wantFiles(files)
	e.wantFiles(map[string]string{"unscanned.txt": "only here so far"})
}

// What a directory holds that no device knows about would be lost, so it keeps a move and a delete from happening. This
// includes what the folder's patterns ignore, which a directory on a device that has its files can hold (.DS_Store).
func TestDirectoryHoldingWhatIsIgnored(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	e.setIgnores(".DS_Store")
	files := map[string]string{"dir/kept.txt": "kept", "dir/sub/deep.txt": "deep", "dir/sub/.DS_Store": "layout"}
	for name, contents := range files {
		e.write(name, contents)
	}
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	root, dir := ts.itemAt(token, ""), ts.itemAt(token, "dir")
	if local, ok, _ := e.backend.LocalFile("docs", "dir/sub/.DS_Store"); ok {
		t.Fatalf("indexed: %+v", local)
	}
	sequence := e.sequence()
	wantRefused(t, ts.operate(token, moveRequest(dir, root, "moved")), codeNotEmpty)
	wantRefused(t, ts.operate(token, deleteRequest(dir)), codeNotEmpty)
	e.wantFiles(files)
	e.wantGone("moved")
	e.wantScanAgrees(sequence)

	// Without it, both are possible
	if err := os.Remove(filepath.Join(e.folderPath, "dir", "sub", ".DS_Store")); err != nil {
		t.Fatal(err)
	}
	dir = ts.itemAt(token, "dir")
	wantCommitted(t, ts.operate(token, moveRequest(dir, root, "moved")))
	wantCommitted(t, ts.operate(token, deleteRequest(ts.itemAt(token, "moved"))))
	e.wantGone("dir", "moved")
}

// A restart in the middle of a delete finishes it, also when the first run didn't get to tell anyone
func TestOperationRecoveryOfADelete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		stopAt string
		gone   []string
	}{
		{"a file, before removing it", "doc.txt", journalPublishing, []string{"doc.txt"}},
		{"a file, once it is indexed", "doc.txt", journalIndexed, []string{"doc.txt"}},
		{"a directory, before removing it", "dir", journalPublishing, []string{"dir/a.txt", "dir/sub/b.txt", "dir/sub", "dir"}},
		{"a directory, once it is indexed", "dir", journalIndexed, []string{"dir/a.txt", "dir/sub/b.txt", "dir/sub", "dir"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, "docs", nil)
			for _, name := range []string{"doc.txt", "dir/a.txt", "dir/sub/b.txt", "stays.txt"} {
				e.write(name, "contents of "+name)
			}
			ts := newTestService(t, e, "")
			ts.export(true)
			token := ts.pair(map[string]string{"docs": "write"})
			item := ts.itemAt(token, tc.path)

			crash := errors.New("simulated crash")
			ts.journalHook = func(_ string, state string) error {
				if state == tc.stopAt {
					return crash
				}
				return nil
			}
			resp, data := ts.do("POST", "/v1/operations", token, deleteRequest(item))
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("interrupted operation: %d %s", resp.StatusCode, data)
			}
			var pending Operation
			if err := json.Unmarshal(data, &pending); err != nil {
				t.Fatal(err)
			}
			if removed := !e.holds(tc.path); removed != (tc.stopAt == journalIndexed) {
				t.Fatalf("removed before the simulated crash: %v", removed)
			}
			ts.Close()

			// A new service on the same database finishes the operation from its journal
			ts2 := newTestService(t, e, ts.home)
			waitFor(t, operationTimeout, "recovery", func() bool {
				op, ok, _ := ts2.loadOperation(ts2.store.db, pending.Operation)
				return ok && op.state != stateResultPending
			})
			var done Operation
			ts2.json("GET", "/v1/operations/"+pending.Operation, token, nil, http.StatusOK, &done)
			if done.State != stateResultCommitted || done.Item == nil || !done.Item.Deleted || done.Item.ID != item.ID {
				t.Fatalf("recovered operation: %+v (%+v)", done, done.Error)
			}
			e.wantGone(tc.gone...)
			e.wantFiles(map[string]string{"stays.txt": "contents of stays.txt"})
			for _, name := range tc.gone {
				if local, ok, _ := e.backend.LocalFile("docs", name); !ok || !local.Deleted {
					t.Fatalf("%s is not indexed as deleted: %+v", name, local)
				}
			}
			e.wantScanAgrees(e.sequence())
			var again Operation
			req := deleteRequest(item)
			req.ID = done.ID
			ts2.json("POST", "/v1/operations", token, req, http.StatusOK, &again)
			if again.Operation != done.Operation || again.State != stateResultCommitted {
				t.Fatalf("replay: %+v", again)
			}
		})
	}
}

// A stop between removing items and indexing that, which the journal has no state for. The delete finishes what is left
// (Docs/remote-api/invariants.md), and indexes all of it without waiting for a scan.
func TestOperationRecoveryOfAPartlyDoneDelete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		removed []string
	}{
		{"a file", "doc.txt", []string{"doc.txt"}},
		{"part of a directory", "dir", []string{"dir/sub/b.txt"}},
		{"a subdirectory of a directory", "dir", []string{"dir/sub/b.txt", "dir/sub"}},
		{"all of a directory", "dir", []string{"dir/sub/b.txt", "dir/sub", "dir/a.txt", "dir"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, "docs", nil)
			for _, name := range []string{"doc.txt", "dir/a.txt", "dir/sub/b.txt", "stays.txt"} {
				e.write(name, "contents of "+name)
			}
			ts := newTestService(t, e, "")
			ts.export(true)
			token := ts.pair(map[string]string{"docs": "write"})
			item := ts.itemAt(token, tc.path)

			crash := errors.New("simulated crash")
			ts.journalHook = func(_ string, state string) error {
				if state != journalPublishing {
					return nil
				}
				for _, name := range tc.removed {
					if err := os.Remove(filepath.Join(e.folderPath, filepath.FromSlash(name))); err != nil &&
						!errors.Is(err, os.ErrNotExist) {
						t.Error(err)
					}
				}
				return crash
			}
			resp, data := ts.do("POST", "/v1/operations", token, deleteRequest(item))
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("interrupted operation: %d %s", resp.StatusCode, data)
			}
			var pending Operation
			if err := json.Unmarshal(data, &pending); err != nil {
				t.Fatal(err)
			}
			ts.Close()

			ts2 := newTestService(t, e, ts.home)
			waitFor(t, operationTimeout, "recovery", func() bool {
				op, ok, _ := ts2.loadOperation(ts2.store.db, pending.Operation)
				return ok && op.state != stateResultPending
			})
			var done Operation
			ts2.json("GET", "/v1/operations/"+pending.Operation, token, nil, http.StatusOK, &done)
			if done.State != stateResultCommitted {
				t.Fatalf("recovered operation: %+v (%+v)", done, done.Error)
			}

			e.wantGone(tc.path)
			e.wantFiles(map[string]string{"stays.txt": "contents of stays.txt"})
			for _, name := range append(tc.removed, tc.path) {
				if local, ok, _ := e.backend.LocalFile("docs", name); !ok || !local.Deleted {
					t.Fatalf("%s is not indexed as deleted: %+v", name, local)
				}
			}
			e.wantScanAgrees(e.sequence())
		})
	}
}

// A delete that stopped can't tell what it checked from what is new when its record is missing or no longer describes
// the index. It stops: what it removed stays removed, and nothing else goes.
func TestResumedDeleteWithoutAUsableRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record func(*journalData)
	}{
		{"recorded by a version that kept no sequence", func(data *journalData) { data.DeleteSequence = 0 }},
		{"the index lost its latest commits", func(data *journalData) { data.DeleteSequence += 1000 }},
		{"the index is another one", func(data *journalData) { data.DeleteIndexID++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, "docs", nil)
			e.write("dir/a.txt", "a")
			e.write("dir/sub/b.txt", "b")
			ts := newTestService(t, e, "")
			ts.export(true)
			token := ts.pair(map[string]string{"docs": "write"})
			dir := ts.itemAt(token, "dir")

			ts.journalHook = func(opID string, state string) error {
				if state != journalPublishing {
					return nil
				}
				op, _, err := ts.loadOperation(ts.store.db, opID)
				if err != nil {
					t.Error(err)
				}
				tc.record(&op.data)
				encoded, err := json.Marshal(op.data)
				if err != nil {
					t.Error(err)
				}
				if _, err := ts.store.db.Exec(`UPDATE operations SET journal_data = ? WHERE op_id = ?`, string(encoded),
					opID); err != nil {
					t.Error(err)
				}
				if err := os.Remove(filepath.Join(e.folderPath, "dir", "sub", "b.txt")); err != nil &&
					!errors.Is(err, os.ErrNotExist) {
					t.Error(err)
				}
				return errors.New("simulated crash")
			}
			resp, data := ts.do("POST", "/v1/operations", token, deleteRequest(dir))
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("interrupted operation: %d %s", resp.StatusCode, data)
			}
			var pending Operation
			if err := json.Unmarshal(data, &pending); err != nil {
				t.Fatal(err)
			}
			ts.Close()

			ts2 := newTestService(t, e, ts.home)
			waitFor(t, operationTimeout, "recovery", func() bool {
				op, ok, _ := ts2.loadOperation(ts2.store.db, pending.Operation)
				return ok && op.state != stateResultPending
			})
			var done Operation
			ts2.json("GET", "/v1/operations/"+pending.Operation, token, nil, http.StatusOK, &done)
			wantRefused(t, done, codeRevisionMismatch)
			e.wantFiles(map[string]string{"dir/a.txt": "a"})
			if local, ok, _ := e.backend.LocalFile("docs", "dir/sub/b.txt"); !ok || !local.Deleted {
				t.Fatalf("what it removed is not indexed as deleted: %+v", local)
			}
			if local, ok, _ := e.backend.LocalFile("docs", "dir/a.txt"); !ok || local.Deleted {
				t.Fatalf("what it left is not indexed: %+v", local)
			}
		})
	}
}

// An edit that stopped once it was staged, when the item moved to another directory before it ran again. The edit
// finishes beside the path it was staged for, as one file. Staged again beside the item's new path, it would be published
// in a directory the journal doesn't name, which can't be indexed: it would never finish, and leave a copy every time.
func TestOperationRecoveryOfAnEditWhoseItemMoved(t *testing.T) {
	e := newTestEngine(t, "docs", nil)
	e.write("from/doc.txt", "original")
	e.write("to/other.txt", "other")
	ts := newTestService(t, e, "")
	ts.export(true)
	token := ts.pair(map[string]string{"docs": "write"})
	doc := ts.itemAt(token, "from/doc.txt")
	to := ts.itemAt(token, "to")

	// The move happens while the edit is stopped here, so that nothing runs the edit again before it
	var stopped atomic.Bool
	ts.journalHook = func(_ string, state string) error {
		if state != journalStaged || !stopped.CompareAndSwap(false, true) {
			return nil
		}
		if op := ts.operate(token, moveRequest(doc, to, "doc.txt")); op.State != stateResultCommitted {
			t.Errorf("the move: %+v (%+v)", op, op.Error)
		}
		return errors.New("simulated crash")
	}
	upload, digest := ts.upload(token, []byte("edited"))
	resp, data := ts.do("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "docs",
		ItemID: doc.ID, BaseRevision: doc.Revision, Upload: upload, UploadDigest: digest})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("interrupted operation: %d %s", resp.StatusCode, data)
	}
	var pending Operation
	if err := json.Unmarshal(data, &pending); err != nil {
		t.Fatal(err)
	}
	ts.Close()

	ts2 := newTestService(t, e, ts.home)
	waitFor(t, operationTimeout, "recovery", func() bool {
		ts2.sync()
		op, ok, _ := ts2.loadOperation(ts2.store.db, pending.Operation)
		return ok && op.state != stateResultPending
	})
	var done Operation
	ts2.json("GET", "/v1/operations/"+pending.Operation, token, nil, http.StatusOK, &done)
	if done.State != stateResultConflict || done.Item == nil || done.Item.Path != "to/doc.txt" || done.ConflictItem == nil ||
		parentPath(done.ConflictItem.Path) != "from" {
		t.Fatalf("recovered operation: %+v (%+v)", done, done.Error)
	}
	if got := e.filesHolding("edited"); !slices.Equal(got, []string{done.ConflictItem.Path}) {
		t.Fatalf("the edit is in %v, want %s", got, done.ConflictItem.Path)
	}
	e.wantFiles(map[string]string{"to/doc.txt": "original"})
	e.wantScanAgrees(e.sequence())
}
