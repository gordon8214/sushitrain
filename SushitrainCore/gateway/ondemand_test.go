// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build gwintegration

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

// These tests write to an on-demand folder on a host that doesn't hold its files, next to a real peer that does. What
// matters is what the peer ends up with, so they check that, including after scans on the host while the peer is away:
// a scan is what turns a mistake here into a file that returns after being deleted, or an edit that never arrives.

const conflictInfix = ".sync-conflict-"

type onDemandFixture struct {
	t          *testing.T
	host, peer *testEngine
	ts         *testService
	token      string
}

// selectPaths does what the app's core does with an on-demand folder's .stignore
func (e *testEngine) selectPaths(folder string, paths []string) error {
	lines, _, err := e.app.Internals.Ignores(folder)
	if err != nil {
		return err
	}
	changed := false
	for _, path := range paths {
		if line := "!/" + path; !slices.Contains(lines, line) {
			lines = append(lines[:len(lines)-1], line, "*")
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return e.app.Internals.SetIgnores(folder, lines)
}

func newOnDemand(t *testing.T, files map[string]string) *onDemandFixture {
	t.Helper()
	host := newTestEngine(t, "shared", nil)
	peer := newTestEngine(t, "shared", nil)
	if err := os.WriteFile(filepath.Join(host.folderPath, ".stignore"), []byte("!*.sync-conflict-*\n*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	host.backend.IsSelective = func(string) bool { return true }
	host.backend.IsOnDemand = func(string) bool { return true }
	host.backend.Select = host.selectPaths
	host.backend.LoadGlobalIgnores = func(string) (func(string) bool, error) {
		return func(name string) bool {
			return name == "globally-ignored" || strings.HasPrefix(name, "globally-ignored/")
		}, nil
	}
	for name, contents := range files {
		peer.write(name, contents)
	}
	peer.scan()
	connect(t, host, peer)
	waitFor(t, peerTimeout, "connection", func() bool { return host.app.Internals.IsConnectedTo(peer.id) })

	ts := newTestService(t, host, "")
	ts.export(true)
	token := ts.pair(map[string]string{"shared": "write"})
	f := &onDemandFixture{t: t, host: host, peer: peer, ts: ts, token: token}
	waitFor(t, peerTimeout, "the peer's index on the host", func() bool {
		ts.sync()
		for name := range files {
			if _, ok, _ := liveItemByPath(ts.store.db, "shared", name); !ok {
				return false
			}
		}
		return true
	})
	for name := range files {
		if _, ok := host.read(name); ok {
			t.Fatalf("the host holds %s", name)
		}
	}
	return f
}

func (f *onDemandFixture) item(p string) Item {
	f.t.Helper()
	return f.ts.itemAt(f.token, p)
}

// finish waits for an operation that may stay pending for a while
func (f *onDemandFixture) finish(req OperationRequest) Operation {
	f.t.Helper()
	resp, data := f.ts.do("POST", "/v1/operations", f.token, req)
	var op Operation
	if resp.StatusCode >= 400 {
		// Refused: the operation comes with the error
		var refused struct {
			Operation *Operation `json:"operation"`
		}
		if err := json.Unmarshal(data, &refused); err != nil || refused.Operation == nil {
			f.t.Fatalf("operation: %d %s", resp.StatusCode, data)
		}
		return *refused.Operation
	}
	if err := json.Unmarshal(data, &op); err != nil {
		f.t.Fatal(err)
	}
	waitFor(f.t, peerTimeout, "the operation", func() bool {
		f.ts.sync()
		f.ts.json("GET", "/v1/operations/"+op.Operation, f.token, nil, http.StatusOK, &op)
		return op.State != stateResultPending
	})
	return op
}

func (f *onDemandFixture) replace(item Item, contents string) Operation {
	f.t.Helper()
	upload, digest := f.ts.upload(f.token, []byte(contents))
	return f.finish(OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "shared", ItemID: item.ID,
		BaseRevision: item.Revision, Upload: upload, UploadDigest: digest})
}

func (f *onDemandFixture) create(parent Item, name, contents string) Operation {
	f.t.Helper()
	upload, digest := f.ts.upload(f.token, []byte(contents))
	return f.finish(OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: "shared", ParentID: parent.ID, Name: name,
		ItemKind: kindFile, Upload: upload, UploadDigest: digest})
}

func (f *onDemandFixture) remove(item Item) Operation {
	f.t.Helper()
	return f.finish(OperationRequest{ID: newUUID(), Kind: opDelete, FolderID: "shared", ItemID: item.ID,
		BaseRevision: item.Revision, SubtreeGeneration: &item.SubtreeGeneration})
}

func (f *onDemandFixture) pausePeer(paused bool) {
	f.t.Helper()
	setDevicePaused(f.t, f.host, f.peer, paused)
	waitFor(f.t, peerTimeout, "the peer's connection", func() bool {
		return f.host.app.Internals.IsConnectedTo(f.peer.id) != paused
	})
}

func (f *onDemandFixture) waitForPeer(name, contents string) {
	f.t.Helper()
	waitFor(f.t, peerTimeout, name+" on the peer", func() bool {
		got, ok := f.peer.read(name)
		return ok && got == contents
	})
}

func (f *onDemandFixture) waitForPeerToLose(name string) {
	f.t.Helper()
	waitFor(f.t, peerTimeout, name+" gone from the peer", func() bool {
		_, ok := f.peer.read(name)
		return !ok
	})
}

// settle gives whatever a scan on the host set off the time to reach the peer
func (f *onDemandFixture) settle() {
	f.t.Helper()
	f.host.scan()
	f.host.scan()
	f.peer.scan()
	time.Sleep(2 * time.Second)
}

func conflictCopies(t *testing.T, e *testEngine) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(e.folderPath, func(p string, _ fs.DirEntry, err error) error {
		if err == nil && strings.Contains(filepath.Base(p), conflictInfix) {
			found = append(found, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func (f *onDemandFixture) wantNoConflicts() {
	f.t.Helper()
	if found := append(conflictCopies(f.t, f.host), conflictCopies(f.t, f.peer)...); len(found) > 0 {
		f.t.Fatalf("conflict copies: %v", found)
	}
}

func TestOnDemandReplaceOfFileNotHeld(t *testing.T) {
	f := newOnDemand(t, map[string]string{"dir/doc.txt": "original"})
	if op := f.replace(f.item("dir/doc.txt"), "edited"); op.State != stateResultCommitted {
		t.Fatalf("replace: %+v", op)
	}
	f.waitForPeer("dir/doc.txt", "edited")
	f.settle()
	f.waitForPeer("dir/doc.txt", "edited")
	f.wantNoConflicts()
	local, ok, _ := f.host.backend.LocalFile("shared", "dir/doc.txt")
	if !isIndexed(local, ok) {
		t.Fatalf("the edit is no longer offered: %+v", local)
	}
	global, _, _ := f.host.backend.GlobalFile("shared", "dir/doc.txt")
	if !global.Version.Equal(local.Version) {
		t.Fatalf("local %v, global %v", local.Version, global.Version)
	}
}

func TestOnDemandReplaceWhilePeerIsAway(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "original"})
	item := f.item("doc.txt")
	f.pausePeer(true)
	if op := f.replace(item, "edited while away"); op.State != stateResultCommitted {
		t.Fatalf("replace: %+v", op)
	}
	f.host.scan()
	f.host.scan()
	f.pausePeer(false)
	f.waitForPeer("doc.txt", "edited while away")
	f.wantNoConflicts()
}

func TestOnDemandCreateInDirectoryNotHeld(t *testing.T) {
	f := newOnDemand(t, map[string]string{"a/b/c/existing.txt": "there"})
	if op := f.create(f.item("a/b/c"), "new.txt", "created"); op.State != stateResultCommitted {
		t.Fatalf("create: %+v", op)
	}
	f.waitForPeer("a/b/c/new.txt", "created")
	f.settle()
	f.waitForPeer("a/b/c/new.txt", "created")
	f.waitForPeer("a/b/c/existing.txt", "there")
	f.wantNoConflicts()
	if _, ok := f.host.read("a/b/c/existing.txt"); ok {
		t.Fatal("creating a file downloaded its neighbor")
	}
}

func TestOnDemandCreateDirectoryInDirectoryNotHeld(t *testing.T) {
	f := newOnDemand(t, map[string]string{"a/b/existing.txt": "there"})
	op := f.finish(OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: "shared", ParentID: f.item("a/b").ID,
		Name: "made", ItemKind: kindDirectory})
	if op.State != stateResultCommitted {
		t.Fatalf("create directory: %+v", op)
	}
	waitFor(t, peerTimeout, "the directory on the peer", func() bool {
		info, err := os.Stat(filepath.Join(f.peer.folderPath, "a/b/made"))
		return err == nil && info.IsDir()
	})
	f.settle()
	if info, err := os.Stat(filepath.Join(f.peer.folderPath, "a/b/made")); err != nil || !info.IsDir() {
		t.Fatalf("the directory is gone from the peer: %v", err)
	}
	f.wantNoConflicts()
}

func TestOnDemandCreateOverDeletion(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "first", "other.txt": "other"})
	root := f.item("")
	if err := os.Remove(filepath.Join(f.peer.folderPath, "doc.txt")); err != nil {
		t.Fatal(err)
	}
	f.peer.scan()
	waitFor(t, peerTimeout, "the deletion on the host", func() bool {
		f.ts.sync()
		_, ok, _ := liveItemByPath(f.ts.store.db, "shared", "doc.txt")
		return !ok
	})
	if op := f.create(root, "doc.txt", "second"); op.State != stateResultCommitted {
		t.Fatalf("create: %+v", op)
	}
	f.waitForPeer("doc.txt", "second")
	f.wantNoConflicts()
}

func TestOnDemandDeleteOfFileNotHeld(t *testing.T) {
	f := newOnDemand(t, map[string]string{"gone.txt": "gone", "stays.txt": "stays"})
	if op := f.remove(f.item("gone.txt")); op.State != stateResultCommitted {
		t.Fatalf("delete: %+v", op)
	}
	f.waitForPeerToLose("gone.txt")
	f.settle()
	f.waitForPeerToLose("gone.txt")
	f.waitForPeer("stays.txt", "stays")
	f.wantNoConflicts()
	local, ok, _ := f.host.backend.LocalFile("shared", "gone.txt")
	if !ok || !local.Deleted || local.IsInvalid() {
		t.Fatalf("the deletion is no longer announced: %+v", local)
	}
}

func TestOnDemandDeleteOfDirectoryNotHeld(t *testing.T) {
	f := newOnDemand(t, map[string]string{"dir/a.txt": "a", "dir/sub/b.txt": "b", "dir/sub/deep/c.txt": "c", "stays.txt": "s"})
	if op := f.remove(f.item("dir")); op.State != stateResultCommitted {
		t.Fatalf("delete: %+v", op)
	}
	f.waitForPeerToLose("dir/sub/deep/c.txt")
	f.waitForPeerToLose("dir/a.txt")
	waitFor(t, peerTimeout, "the directory gone from the peer", func() bool {
		_, err := os.Stat(filepath.Join(f.peer.folderPath, "dir"))
		return errors.Is(err, os.ErrNotExist)
	})
	f.settle()
	if _, err := os.Stat(filepath.Join(f.peer.folderPath, "dir")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the directory came back on the peer: %v", err)
	}
	if _, ok := f.host.read("dir/a.txt"); ok {
		t.Fatal("deleting a directory downloaded it")
	}
	f.waitForPeer("stays.txt", "s")
	f.wantNoConflicts()
}

// The peer learns of the deletion only after scans on the host had their chance to take it back
func TestOnDemandDeletionSurvivesScansWhilePeerIsAway(t *testing.T) {
	f := newOnDemand(t, map[string]string{"gone.txt": "gone", "dir/a.txt": "a", "stays.txt": "s"})
	file, directory := f.item("gone.txt"), f.item("dir")
	f.pausePeer(true)
	if op := f.remove(file); op.State != stateResultCommitted {
		t.Fatalf("delete: %+v", op)
	}
	if op := f.remove(directory); op.State != stateResultCommitted {
		t.Fatalf("delete directory: %+v", op)
	}
	f.host.scan()
	f.host.scan()
	f.pausePeer(false)
	f.waitForPeerToLose("gone.txt")
	f.waitForPeerToLose("dir/a.txt")
	f.settle()
	f.waitForPeerToLose("gone.txt")
	f.waitForPeer("stays.txt", "s")
	f.wantNoConflicts()
}

func TestOnDemandMoveFetchesFirst(t *testing.T) {
	f := newOnDemand(t, map[string]string{"old.txt": "moved contents", "dir/inside.txt": "inside", "target/x.txt": "x"})
	root := f.item("")
	file := f.item("old.txt")
	op := f.finish(OperationRequest{ID: newUUID(), Kind: opMove, FolderID: "shared", ItemID: file.ID, ParentID: root.ID,
		Name: "new.txt", BaseRevision: file.Revision})
	if op.State != stateResultCommitted {
		t.Fatalf("move: %+v", op)
	}
	f.waitForPeer("new.txt", "moved contents")
	f.waitForPeerToLose("old.txt")

	directory, target := f.item("dir"), f.item("target")
	op = f.finish(OperationRequest{ID: newUUID(), Kind: opMove, FolderID: "shared", ItemID: directory.ID, ParentID: target.ID,
		Name: "moved", BaseRevision: directory.Revision})
	if op.State != stateResultCommitted {
		t.Fatalf("move directory: %+v", op)
	}
	f.waitForPeer("target/moved/inside.txt", "inside")
	f.waitForPeerToLose("dir/inside.txt")
	f.settle()
	f.waitForPeer("new.txt", "moved contents")
	f.waitForPeer("target/moved/inside.txt", "inside")
	f.waitForPeerToLose("old.txt")
	f.waitForPeerToLose("dir/inside.txt")
	f.wantNoConflicts()
}

func TestOnDemandMoveOfLargeDirectoryIsRefused(t *testing.T) {
	limit := onDemandMoveLimit
	onDemandMoveLimit = 1
	t.Cleanup(func() { onDemandMoveLimit = limit })
	f := newOnDemand(t, map[string]string{"dir/big.bin": "more than one byte", "other/x.txt": "x"})
	directory, target := f.item("dir"), f.item("other")
	op := f.finish(OperationRequest{ID: newUUID(), Kind: opMove, FolderID: "shared", ItemID: directory.ID, ParentID: target.ID,
		Name: "moved", BaseRevision: directory.Revision})
	if op.State != stateResultFailed || op.Error == nil || op.Error.Code != codeMoveTooLarge {
		t.Fatalf("move: %+v", op)
	}
	if _, ok := f.host.read("dir/big.bin"); ok {
		t.Fatal("the refused move downloaded the directory")
	}
	f.waitForPeer("dir/big.bin", "more than one byte")
}

// When a peer's edit wins a real conflict, the engine renames this device's edit to a conflict copy
func TestOnDemandLostConflictKeepsTheEdit(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "original"})
	item := f.item("doc.txt")
	f.pausePeer(true)
	if op := f.replace(item, "the host's edit"); op.State != stateResultCommitted {
		t.Fatalf("replace: %+v", op)
	}
	// The later edit wins
	time.Sleep(1100 * time.Millisecond)
	f.peer.write("doc.txt", "the peer's edit")
	f.peer.scan()
	f.pausePeer(false)
	waitFor(t, peerTimeout, "both edits on the peer", func() bool {
		f.host.scan()
		f.peer.scan()
		copies := conflictCopies(t, f.peer)
		if len(copies) != 1 {
			return false
		}
		data, _ := os.ReadFile(copies[0])
		return string(data) == "the host's edit"
	})
	f.waitForPeer("doc.txt", "the peer's edit")
}

func TestOnDemandEditAfterDeselection(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "original"})
	if err := f.host.selectPaths("shared", []string{"doc.txt"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, peerTimeout, "the file on the host", func() bool {
		f.host.scan()
		got, _ := f.host.read("doc.txt")
		return got == "original"
	})
	if err := f.host.app.Internals.SetIgnores("shared", []string{"!*.sync-conflict-*", "*"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.host.folderPath, "doc.txt")); err != nil {
		t.Fatal(err)
	}
	f.host.scan()
	item := f.item("doc.txt")
	if op := f.replace(item, "edited"); op.State != stateResultCommitted {
		t.Fatalf("replace: %+v", op)
	}
	f.waitForPeer("doc.txt", "edited")
	f.wantNoConflicts()
}

// An ignored file on disk may be all there is of something: the edit goes elsewhere, and it stays
func TestOnDemandEditKeepsFileOnDiskThatIsNotIndexed(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "original"})
	item := f.item("doc.txt")
	f.host.write("doc.txt", "not indexed")
	op := f.replace(item, "edited")
	if op.State != stateResultConflict {
		t.Fatalf("replace: %+v", op)
	}
	if got, _ := f.host.read("doc.txt"); got != "not indexed" {
		t.Fatalf("the file on disk holds %q", got)
	}
	waitFor(t, peerTimeout, "the conflict copy on the peer", func() bool { return len(conflictCopies(t, f.peer)) == 1 })
	f.waitForPeer("doc.txt", "original")
}

func TestOnDemandRefusesWhatCannotBeSelected(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "original"})
	root := f.item("")
	for _, name := range []string{"star*.txt", "line\nbreak.txt", "edge.txt ", "globally-ignored"} {
		op := f.create(root, name, "x")
		if op.State != stateResultFailed || op.Error == nil || op.Error.Code != codeIgnoredPath {
			t.Fatalf("%q: %+v", name, op)
		}
	}
	lines, _, err := f.host.app.Internals.Ignores("shared")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lines, []string{"!*.sync-conflict-*", "*"}) {
		t.Fatalf("patterns: %q", lines)
	}
}

func TestPlainSelectiveFolderStaysReadOnly(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "original"})
	f.host.backend.IsOnDemand = func(string) bool { return false }
	if err := f.ts.SetExport("shared", true); err == nil {
		t.Fatal("a plain selective folder was exported for writing")
	}
}

// A stop after publishing, before anything is committed: a scan then indexes the file as ignored
func TestOnDemandRecoveryAfterPublication(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "original"})
	item := f.item("doc.txt")
	crash := errors.New("simulated crash")
	f.ts.journalHook = func(_ string, state string) error {
		if state == journalPublished {
			return crash
		}
		return nil
	}
	upload, digest := f.ts.upload(f.token, []byte("edited"))
	resp, data := f.ts.do("POST", "/v1/operations", f.token, OperationRequest{ID: newUUID(), Kind: opReplace,
		FolderID: "shared", ItemID: item.ID, BaseRevision: item.Revision, Upload: upload, UploadDigest: digest})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("interrupted operation: %d %s", resp.StatusCode, data)
	}
	var pending Operation
	_ = json.Unmarshal(data, &pending)
	f.ts.Close()
	f.host.scan()

	ts2 := newTestService(t, f.host, f.ts.home)
	f.ts = ts2
	waitFor(t, 30*time.Second, "recovery", func() bool {
		op, ok, _ := ts2.loadOperation(ts2.store.db, pending.Operation)
		return ok && op.state != stateResultPending
	})
	f.waitForPeer("doc.txt", "edited")
	f.settle()
	f.waitForPeer("doc.txt", "edited")
	f.wantNoConflicts()
}

// A peer's edit that arrives between checking a file and indexing the edit of it, here across a restart: devices have
// to see the two as concurrent, or one silently replaces the other
func TestOnDemandEditKeepsNewerVersionThatArrivedMeanwhile(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "original"})
	item := f.item("doc.txt")
	crash := errors.New("simulated crash")
	f.ts.journalHook = func(_ string, state string) error {
		if state == journalPublished {
			return crash
		}
		return nil
	}
	upload, digest := f.ts.upload(f.token, []byte("edited here"))
	resp, data := f.ts.do("POST", "/v1/operations", f.token, OperationRequest{ID: newUUID(), Kind: opReplace,
		FolderID: "shared", ItemID: item.ID, BaseRevision: item.Revision, Upload: upload, UploadDigest: digest})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("interrupted operation: %d %s", resp.StatusCode, data)
	}
	var pending Operation
	_ = json.Unmarshal(data, &pending)
	f.ts.Close()
	f.peer.write("doc.txt", "edited on the peer")
	f.peer.scan()
	waitFor(t, peerTimeout, "the peer's version on the host", func() bool {
		global, ok, _ := f.host.backend.GlobalFile("shared", "doc.txt")
		return ok && global.Size == int64(len("edited on the peer"))
	})

	ts2 := newTestService(t, f.host, f.ts.home)
	f.ts = ts2
	waitFor(t, 30*time.Second, "recovery", func() bool {
		op, ok, _ := ts2.loadOperation(ts2.store.db, pending.Operation)
		return ok && op.state != stateResultPending
	})
	waitFor(t, peerTimeout, "both edits on the peer", func() bool {
		f.host.scan()
		f.peer.scan()
		var contents []string
		entries, _ := os.ReadDir(f.peer.folderPath)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "doc") {
				data, _ := os.ReadFile(filepath.Join(f.peer.folderPath, entry.Name()))
				contents = append(contents, string(data))
			}
		}
		return slices.Contains(contents, "edited here") && slices.Contains(contents, "edited on the peer")
	})
}

// What the folder's own patterns exclude can't be announced, whatever is selected: it can't be written either, and a
// directory that holds it can still be deleted and moved
func TestOnDemandDirectoryWithExcludedContents(t *testing.T) {
	f := newOnDemand(t, map[string]string{"dir/kept.txt": "kept", "other/x.txt": "x", "globally-ignored/inside.txt": "ignored"})
	if op := f.create(f.item("globally-ignored"), "new.txt", "x"); op.State != stateResultFailed || op.Error == nil ||
		op.Error.Code != codeIgnoredPath {
		t.Fatalf("create below an excluded directory: %+v", op)
	}
	file := f.item("dir/kept.txt")
	target := f.item("other")
	root := f.item("")
	refused := f.finish(OperationRequest{ID: newUUID(), Kind: opMove, FolderID: "shared", ItemID: file.ID, ParentID: root.ID,
		Name: "globally-ignored", BaseRevision: file.Revision})
	if refused.State != stateResultFailed || refused.Error == nil || refused.Error.Code != codeIgnoredPath {
		t.Fatalf("move onto an excluded name: %+v", refused)
	}
	if _, ok := f.host.read("dir/kept.txt"); ok {
		t.Fatal("a move that could never work downloaded the file first")
	}
	refused = f.finish(OperationRequest{ID: newUUID(), Kind: opMove, FolderID: "shared", ItemID: file.ID, ParentID: target.ID,
		Name: "x.txt", BaseRevision: file.Revision})
	if refused.State != stateResultFailed || refused.Error == nil || refused.Error.Code != codeNameConflict {
		t.Fatalf("move onto an existing item: %+v", refused)
	}
	if _, ok := f.host.read("dir/kept.txt"); ok {
		t.Fatal("a move that could never work downloaded the file first")
	}
}

// A stop between committing and selecting: a scan in between marks what was committed ignored. Both kinds of change
// have to come back from that, by committing again.
func TestOnDemandSelectingFailsOnce(t *testing.T) {
	f := newOnDemand(t, map[string]string{"edited.txt": "original", "deleted.txt": "deleted"})
	edited, deleted := f.item("edited.txt"), f.item("deleted.txt")
	f.pausePeer(true)
	var failing atomic.Bool
	failing.Store(true)
	selectPaths := f.host.backend.Select
	f.host.backend.Select = func(folder string, paths []string) error {
		if failing.Load() {
			return errors.New("simulated failure")
		}
		return selectPaths(folder, paths)
	}

	upload, digest := f.ts.upload(f.token, []byte("edited"))
	f.ts.do("POST", "/v1/operations", f.token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "shared",
		ItemID: edited.ID, BaseRevision: edited.Revision, Upload: upload, UploadDigest: digest})
	f.ts.do("POST", "/v1/operations", f.token, OperationRequest{ID: newUUID(), Kind: opDelete, FolderID: "shared",
		ItemID: deleted.ID, BaseRevision: deleted.Revision})
	f.host.scan()
	f.host.scan()
	failing.Store(false)
	waitFor(t, peerTimeout, "the operations", func() bool {
		f.ts.retryPendingOperations("shared")
		var pending int
		_ = f.ts.store.db.QueryRow(`SELECT count(*) FROM operations WHERE state = ?`, stateResultPending).Scan(&pending)
		return pending == 0
	})
	f.pausePeer(false)
	f.waitForPeer("edited.txt", "edited")
	f.waitForPeerToLose("deleted.txt")
	f.settle()
	f.waitForPeer("edited.txt", "edited")
	f.waitForPeerToLose("deleted.txt")
	f.wantNoConflicts()
}

// A resumed delete has to notice a version it wasn't about, as it can't tell by a file on disk
func TestOnDemandResumedDeleteKeepsNewerVersion(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "original"})
	item := f.item("doc.txt")
	crash := errors.New("simulated crash")
	f.ts.journalHook = func(_ string, state string) error {
		if state == journalPublishing {
			return crash
		}
		return nil
	}
	resp, data := f.ts.do("POST", "/v1/operations", f.token, OperationRequest{ID: newUUID(), Kind: opDelete,
		FolderID: "shared", ItemID: item.ID, BaseRevision: item.Revision})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("interrupted operation: %d %s", resp.StatusCode, data)
	}
	var pending Operation
	_ = json.Unmarshal(data, &pending)
	f.ts.Close()
	f.peer.write("doc.txt", "changed meanwhile")
	f.peer.scan()
	waitFor(t, peerTimeout, "the new version on the host", func() bool {
		global, ok, _ := f.host.backend.GlobalFile("shared", "doc.txt")
		return ok && global.Size == int64(len("changed meanwhile"))
	})

	ts2 := newTestService(t, f.host, f.ts.home)
	f.ts = ts2
	waitFor(t, 30*time.Second, "recovery", func() bool {
		op, ok, _ := ts2.loadOperation(ts2.store.db, pending.Operation)
		return ok && op.state != stateResultPending
	})
	f.settle()
	f.waitForPeer("doc.txt", "changed meanwhile")
}
