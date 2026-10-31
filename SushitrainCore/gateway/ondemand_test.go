// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build gwintegration

package gateway

import (
	"context"
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

	stfs "github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/ignore"
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

// ignoreLines reads the folder's .stignore as saved: what the engine has loaded lags behind it
func (e *testEngine) ignoreLines() []string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.folderPath, ".stignore"))
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// selectPaths does what the app's core does with an on-demand folder's .stignore
func (e *testEngine) selectPaths(folder string, paths []string, record func(added []string) error) error {
	lines := e.ignoreLines()
	selected := func(path string) bool {
		for ; path != ""; path = parentPath(path) {
			if slices.Contains(lines, "!/"+path) {
				return true
			}
		}
		return false
	}
	var added []string
	for _, path := range paths {
		if !selected(path) {
			lines = append(lines[:len(lines)-1], "!/"+path, "*")
			added = append(added, path)
		}
	}
	if len(added) == 0 {
		return nil
	}
	if err := record(added); err != nil {
		return err
	}
	for _, path := range added {
		lines = slices.DeleteFunc(lines, func(line string) bool { return strings.HasPrefix(line, "!/"+path+"/") })
	}
	return e.app.Internals.SetIgnores(folder, lines)
}

// deselectPaths does what the app's core does when the service deselects
func (e *testEngine) deselectPaths(folder string, paths []string) (func(string) bool, error) {
	lines := e.ignoreLines()
	kept := slices.DeleteFunc(slices.Clone(lines), func(line string) bool {
		return slices.ContainsFunc(paths, func(path string) bool { return line == "!/"+path })
	})
	if len(kept) != len(lines) {
		if err := e.app.Internals.SetIgnores(folder, kept); err != nil {
			return nil, err
		}
	}
	matcher := ignore.New(stfs.NewFilesystem(stfs.FilesystemTypeBasic, e.folderPath))
	if err := matcher.Parse(strings.NewReader(strings.Join(kept, "\n")), ".stignore"); err != nil {
		return nil, err
	}
	return func(name string) bool { return matcher.Match(name).IsIgnored() }, nil
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
	host.backend.Deselect = host.deselectPaths
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
	if err := f.host.selectPaths("shared", []string{"doc.txt"}, func([]string) error { return nil }); err != nil {
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
	f.host.backend.Select = func(folder string, paths []string, record func(added []string) error) error {
		if failing.Load() {
			return errors.New("simulated failure")
		}
		return selectPaths(folder, paths, record)
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

// What follows is about when a path stops being selected: everything above keeps working with any answer to that, as
// long as nothing is deselected too early

// deselect does what the service does every so often, and what a scan then does with it
func (f *onDemandFixture) deselect() {
	f.t.Helper()
	if err := f.ts.deselectWritten(context.Background(), "shared"); err != nil {
		f.t.Fatal(err)
	}
}

func (f *onDemandFixture) selected(path string) bool {
	f.t.Helper()
	return slices.Contains(f.host.ignoreLines(), "!/"+path)
}

func (f *onDemandFixture) wantSelected(paths ...string) {
	f.t.Helper()
	for _, path := range paths {
		if !f.selected(path) {
			f.t.Fatalf("%s is no longer selected: %q", path, f.host.ignoreLines())
		}
	}
}

func (f *onDemandFixture) waitForDeselection(paths ...string) {
	f.t.Helper()
	waitFor(f.t, peerTimeout, "the paths to be deselected", func() bool {
		f.deselect()
		return !slices.ContainsFunc(paths, f.selected)
	})
}

func (f *onDemandFixture) holds(name string) bool {
	_, err := os.Lstat(filepath.Join(f.host.folderPath, name))
	return err == nil
}

// addPeer shares the folder with another device that has all of it, and that the host knows has it
func (f *onDemandFixture) addPeer(names ...string) *testEngine {
	f.t.Helper()
	other := newTestEngine(f.t, "shared", nil)
	connect(f.t, f.host, other)
	connect(f.t, f.peer, other)
	waitFor(f.t, peerTimeout, "the other peer's index on the host", func() bool {
		for _, name := range names {
			global, _, _ := f.host.backend.GlobalFile("shared", name)
			file, ok, _ := f.host.backend.PeerFile("shared", other.id, name)
			if !ok || !file.Version.Equal(global.Version) {
				return false
			}
		}
		return f.host.app.Internals.IsConnectedTo(other.id)
	})
	return other
}

// pauseOnBoth takes a device away from the two others
func (f *onDemandFixture) pauseOnBoth(other *testEngine, paused bool) {
	f.t.Helper()
	setDevicePaused(f.t, f.host, other, paused)
	setDevicePaused(f.t, f.peer, other, paused)
	waitFor(f.t, peerTimeout, "the device's connections", func() bool {
		return f.host.app.Internals.IsConnectedTo(other.id) != paused &&
			f.peer.app.Internals.IsConnectedTo(other.id) != paused
	})
}

func (f *onDemandFixture) read(item Item) string {
	f.t.Helper()
	var lease ReadLease
	f.ts.json("POST", "/v1/items/"+item.ID+"/read-leases", f.token, ReadLeaseRequest{Revision: item.Revision},
		http.StatusCreated, &lease)
	_, data := f.ts.do("GET", "/v1/content/"+lease.Lease, lease.Token, nil)
	return string(data)
}

func TestOnDemandDeselectsAnEditOnceThePeerHasIt(t *testing.T) {
	f := newOnDemand(t, map[string]string{"dir/doc.txt": "original"})
	item := f.item("dir/doc.txt")
	f.pausePeer(true)
	if op := f.replace(item, "edited"); op.State != stateResultCommitted {
		t.Fatalf("replace: %+v", op)
	}
	f.deselect()
	f.deselect()
	f.wantSelected("dir/doc.txt")
	if got, _ := f.host.read("dir/doc.txt"); got != "edited" {
		t.Fatalf("the only copy of the edit holds %q", got)
	}

	f.pausePeer(false)
	f.waitForPeer("dir/doc.txt", "edited")
	before := f.item("dir/doc.txt")
	f.waitForDeselection("dir/doc.txt")
	if f.holds("dir/doc.txt") || f.holds("dir") {
		t.Fatal("the local copy and its empty directory are still here")
	}
	// At once: an operation that comes next must not find an entry for a file that is gone
	local, ok, _ := f.host.backend.LocalFile("shared", "dir/doc.txt")
	if !ok || !local.IsInvalid() {
		t.Fatalf("still announced: %+v", local)
	}
	f.settle()
	f.waitForPeer("dir/doc.txt", "edited")
	f.wantNoConflicts()
	after := f.item("dir/doc.txt")
	if after.Revision != before.Revision {
		t.Fatalf("deselecting changed the item: %s, %s", before.Revision, after.Revision)
	}
	if got := f.read(after); got != "edited" {
		t.Fatalf("read %q", got)
	}

	// From here on it is a file this device doesn't hold
	if op := f.replace(after, "edited again"); op.State != stateResultCommitted {
		t.Fatalf("replace: %+v", op)
	}
	f.waitForPeer("dir/doc.txt", "edited again")
	f.waitForDeselection("dir/doc.txt")
	f.settle()
	f.waitForPeer("dir/doc.txt", "edited again")
	f.wantNoConflicts()
}

func TestOnDemandDeselectsDeletionsOnceThePeerHasThem(t *testing.T) {
	f := newOnDemand(t, map[string]string{"gone.txt": "gone", "dir/a.txt": "a", "dir/sub/b.txt": "b", "stays.txt": "s"})
	file, directory := f.item("gone.txt"), f.item("dir")
	f.pausePeer(true)
	if op := f.remove(file); op.State != stateResultCommitted {
		t.Fatalf("delete: %+v", op)
	}
	if op := f.remove(directory); op.State != stateResultCommitted {
		t.Fatalf("delete directory: %+v", op)
	}
	f.deselect()
	f.wantSelected("gone.txt", "dir")
	f.pausePeer(false)
	f.waitForPeerToLose("gone.txt")
	f.waitForPeerToLose("dir/sub/b.txt")
	f.waitForDeselection("gone.txt", "dir")
	f.settle()
	f.waitForPeerToLose("gone.txt")
	f.waitForPeerToLose("dir/a.txt")
	f.waitForPeer("stays.txt", "s")
	f.wantNoConflicts()

	// The lines no longer take in what appears there
	f.peer.write("gone.txt", "back")
	f.peer.scan()
	waitFor(t, peerTimeout, "the new file on the host", func() bool {
		f.ts.sync()
		_, ok, _ := liveItemByPath(f.ts.store.db, "shared", "gone.txt")
		return ok
	})
	f.settle()
	if f.holds("gone.txt") {
		t.Fatal("what a peer created at a path that was deleted here came to this device")
	}
}

// The peer that is around has it all, and could offer it, but that one may be gone when the device that is away returns
func TestOnDemandWaitsForADeviceThatIsAway(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "original", "gone.txt": "gone", "stays.txt": "s"})
	away := f.addPeer("doc.txt", "gone.txt", "stays.txt")
	item, deleted := f.item("doc.txt"), f.item("gone.txt")
	f.pauseOnBoth(away, true)
	if op := f.replace(item, "edited"); op.State != stateResultCommitted {
		t.Fatalf("replace: %+v", op)
	}
	if op := f.remove(deleted); op.State != stateResultCommitted {
		t.Fatalf("delete: %+v", op)
	}
	f.waitForPeer("doc.txt", "edited")
	f.waitForPeerToLose("gone.txt")
	waitFor(t, peerTimeout, "the peer's index on the host", func() bool {
		local, _, _ := f.host.backend.LocalFile("shared", "gone.txt")
		file, ok, _ := f.host.backend.PeerFile("shared", f.peer.id, "gone.txt")
		return ok && file.Version.Equal(local.Version)
	})
	for range 3 {
		f.deselect()
		f.host.scan()
	}
	f.wantSelected("doc.txt", "gone.txt")
	if got, _ := f.host.read("doc.txt"); got != "edited" {
		t.Fatalf("the file holds %q", got)
	}

	f.pauseOnBoth(away, false)
	f.waitForDeselection("doc.txt", "gone.txt")
	waitFor(t, peerTimeout, "the changes on the device that was away", func() bool {
		got, ok := away.read("doc.txt")
		_, there := away.read("gone.txt")
		return ok && got == "edited" && !there
	})
	if f.holds("doc.txt") {
		t.Fatal("the local copy is still here")
	}
	f.settle()
	f.waitForPeer("doc.txt", "edited")
	if found := conflictCopies(t, away); len(found) > 0 {
		t.Fatalf("conflict copies: %v", found)
	}
}

// Nobody else ever had an entry for it, so nobody will ever add one: a device that is back needs nothing
func TestOnDemandDeselectsWhatWasGoneBeforeAnyoneSawIt(t *testing.T) {
	f := newOnDemand(t, map[string]string{"stays.txt": "s"})
	root := f.item("")
	f.pausePeer(true)
	if op := f.create(root, "temporary.tmp", "short-lived"); op.State != stateResultCommitted {
		t.Fatalf("create: %+v", op)
	}
	if op := f.remove(f.item("temporary.tmp")); op.State != stateResultCommitted {
		t.Fatalf("delete: %+v", op)
	}
	f.deselect()
	f.wantSelected("temporary.tmp")
	f.pausePeer(false)
	f.waitForDeselection("temporary.tmp")
	f.settle()
	f.waitForPeerToLose("temporary.tmp")
}

// A device that ignores a file says that it has seen this version, not that it has the file
func TestOnDemandKeepsWhatNoOtherDeviceHolds(t *testing.T) {
	f := newOnDemand(t, map[string]string{"stays.txt": "s"})
	if err := os.WriteFile(filepath.Join(f.peer.folderPath, ".stignore"), []byte("only-here.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.peer.scan()
	if op := f.create(f.item(""), "only-here.txt", "only here"); op.State != stateResultCommitted {
		t.Fatalf("create: %+v", op)
	}
	waitFor(t, peerTimeout, "the peer's entry on the host", func() bool {
		local, _, _ := f.host.backend.LocalFile("shared", "only-here.txt")
		file, ok, _ := f.host.backend.PeerFile("shared", f.peer.id, "only-here.txt")
		return ok && file.IsInvalid() && file.Version.Equal(local.Version)
	})
	f.deselect()
	f.wantSelected("only-here.txt")
	if got, _ := f.host.read("only-here.txt"); got != "only here" {
		t.Fatalf("the only copy holds %q", got)
	}
}

// Only what the service selected is the service's to deselect
func TestOnDemandLeavesWhatTheUserSelected(t *testing.T) {
	f := newOnDemand(t, map[string]string{"pinned.txt": "pinned", "written.txt": "written"})
	if err := f.host.selectPaths("shared", []string{"pinned.txt"}, func([]string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, peerTimeout, "the file on the host", func() bool {
		f.host.scan()
		got, _ := f.host.read("pinned.txt")
		return got == "pinned"
	})
	if op := f.replace(f.item("pinned.txt"), "pinned, edited"); op.State != stateResultCommitted {
		t.Fatalf("replace: %+v", op)
	}
	if op := f.replace(f.item("written.txt"), "edited"); op.State != stateResultCommitted {
		t.Fatalf("replace: %+v", op)
	}
	f.waitForPeer("pinned.txt", "pinned, edited")
	f.waitForPeer("written.txt", "edited")
	f.waitForDeselection("written.txt")
	f.wantSelected("pinned.txt")
	if got, _ := f.host.read("pinned.txt"); got != "pinned, edited" {
		t.Fatalf("the selected file holds %q", got)
	}
}

func TestOnDemandDeselectsADirectoryWithAllInIt(t *testing.T) {
	f := newOnDemand(t, map[string]string{"stays.txt": "s"})
	lines := append([]string{"(?d)excluded.txt"}, f.host.ignoreLines()...)
	if err := f.host.app.Internals.SetIgnores("shared", lines); err != nil {
		t.Fatal(err)
	}
	f.host.scan()
	op := f.finish(OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: "shared", ParentID: f.item("").ID,
		Name: "made", ItemKind: kindDirectory})
	if op.State != stateResultCommitted {
		t.Fatalf("create directory: %+v", op)
	}
	if op := f.create(f.item("made"), "a.txt", "a"); op.State != stateResultCommitted {
		t.Fatalf("create: %+v", op)
	}
	f.waitForPeer("made/a.txt", "a")
	// What arrives in a selected directory goes with it. What the folder's patterns exclude stays, and what no scan has
	// seen yet holds it up
	f.peer.write("made/from the peer/b.txt", "b")
	f.peer.scan()
	waitFor(t, peerTimeout, "the peer's file on the host", func() bool {
		got, _ := f.host.read("made/from the peer/b.txt")
		return got == "b"
	})
	f.host.write("made/excluded.txt", "stays here")
	f.host.write("made/not scanned.txt", "only here so far")
	for range 3 {
		f.deselect()
		time.Sleep(200 * time.Millisecond)
	}
	f.wantSelected("made")
	if got, _ := f.host.read("made/not scanned.txt"); got != "only here so far" {
		t.Fatalf("the only copy holds %q", got)
	}
	f.host.scan()
	f.waitForPeer("made/not scanned.txt", "only here so far")
	f.waitForDeselection("made")
	if f.holds("made/a.txt") || f.holds("made/from the peer") || f.holds("made/not scanned.txt") {
		t.Fatal("the local copies are still here")
	}
	if got, _ := f.host.read("made/excluded.txt"); got != "stays here" {
		t.Fatalf("what is excluded holds %q", got)
	}
	f.settle()
	f.waitForPeer("made/a.txt", "a")
	f.waitForPeer("made/from the peer/b.txt", "b")
	f.waitForPeer("made/not scanned.txt", "only here so far")
	f.wantNoConflicts()
}

// What changed on disk isn't what the index says the peer has
func TestOnDemandWaitsForAScanOfAChangedFile(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "original"})
	if op := f.replace(f.item("doc.txt"), "edited"); op.State != stateResultCommitted {
		t.Fatalf("replace: %+v", op)
	}
	f.waitForPeer("doc.txt", "edited")
	f.host.write("doc.txt", "changed on disk, and longer")
	waitFor(t, peerTimeout, "the peer's index on the host", func() bool {
		local, _, _ := f.host.backend.LocalFile("shared", "doc.txt")
		file, ok, _ := f.host.backend.PeerFile("shared", f.peer.id, "doc.txt")
		return ok && file.Version.Equal(local.Version)
	})
	f.deselect()
	f.wantSelected("doc.txt")
	if got, _ := f.host.read("doc.txt"); got != "changed on disk, and longer" {
		t.Fatalf("the file holds %q", got)
	}
	f.host.scan()
	f.waitForPeer("doc.txt", "changed on disk, and longer")
	f.waitForDeselection("doc.txt")
	f.settle()
	f.waitForPeer("doc.txt", "changed on disk, and longer")
	f.wantNoConflicts()
}

// An operation may be waiting for the engine to fetch what it selected
func TestOnDemandDeselectionWaitsForOperations(t *testing.T) {
	f := newOnDemand(t, map[string]string{"old.txt": "moved contents"})
	root, file := f.item(""), f.item("old.txt")
	f.pausePeer(true)
	resp, data := f.ts.do("POST", "/v1/operations", f.token, OperationRequest{ID: newUUID(), Kind: opMove, FolderID: "shared",
		ItemID: file.ID, ParentID: root.ID, Name: "new.txt", BaseRevision: file.Revision})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("move: %d %s", resp.StatusCode, data)
	}
	var pending Operation
	if err := json.Unmarshal(data, &pending); err != nil {
		t.Fatal(err)
	}
	waitFor(t, peerTimeout, "the file to be selected", func() bool { return f.selected("old.txt") })
	f.deselect()
	f.wantSelected("old.txt")
	f.pausePeer(false)
	waitFor(t, peerTimeout, "the move", func() bool {
		f.ts.sync()
		f.ts.json("GET", "/v1/operations/"+pending.Operation, f.token, nil, http.StatusOK, &pending)
		return pending.State != stateResultPending
	})
	if pending.State != stateResultCommitted {
		t.Fatalf("move: %+v", pending)
	}
	f.waitForPeer("new.txt", "moved contents")
	f.waitForPeerToLose("old.txt")
	f.waitForDeselection("old.txt", "new.txt")
	f.settle()
	f.waitForPeer("new.txt", "moved contents")
	f.waitForPeerToLose("old.txt")
	f.wantNoConflicts()
}

func TestOnDemandGroupsTheLinesOfADirectory(t *testing.T) {
	f := newOnDemand(t, map[string]string{"dir/a.txt": "a", "dir/sub/b.txt": "b", "mixed/a.txt": "a", "mixed/b.txt": "b",
		"mixed/other.txt": "other"})
	items := []Item{f.item("dir/a.txt"), f.item("dir/sub/b.txt"), f.item("mixed/a.txt"), f.item("mixed/b.txt")}
	f.pausePeer(true)
	for _, item := range items {
		if op := f.replace(item, "edited"); op.State != stateResultCommitted {
			t.Fatalf("replace: %+v", op)
		}
	}
	f.deselect()
	// In dir there is nothing else, but two paths are needed for it to take over, one level up from each
	f.wantSelected("dir/a.txt", "dir/sub/b.txt", "mixed/a.txt", "mixed/b.txt")
	if op := f.create(f.item("dir/sub"), "c.txt", "c"); op.State != stateResultCommitted {
		t.Fatalf("create: %+v", op)
	}
	f.deselect()
	f.wantSelected("dir/a.txt", "dir/sub", "mixed/a.txt", "mixed/b.txt")
	if f.selected("dir/sub/b.txt") || f.selected("dir/sub/c.txt") {
		t.Fatalf("patterns %q", f.host.ignoreLines())
	}
	f.deselect()
	f.wantSelected("dir", "mixed/a.txt", "mixed/b.txt")
	if f.selected("dir/a.txt") || f.selected("dir/sub") {
		t.Fatalf("patterns %q", f.host.ignoreLines())
	}
	f.settle()
	if f.holds("mixed/other.txt") {
		t.Fatal("selecting the directory downloaded a file that wasn't written")
	}
	f.pausePeer(false)
	f.waitForPeer("dir/sub/c.txt", "c")
	f.waitForPeer("mixed/b.txt", "edited")
	f.waitForDeselection("dir", "mixed/a.txt", "mixed/b.txt")
	f.settle()
	if f.holds("dir/sub/c.txt") || f.holds("mixed/a.txt") {
		t.Fatal("the local copies are still here")
	}
	f.waitForPeer("dir/a.txt", "edited")
	f.waitForPeer("dir/sub/b.txt", "edited")
	f.wantNoConflicts()
}

func TestOnDemandDeselectionSurvivesARestart(t *testing.T) {
	f := newOnDemand(t, map[string]string{"doc.txt": "original"})
	item := f.item("doc.txt")
	f.pausePeer(true)
	if op := f.replace(item, "edited"); op.State != stateResultCommitted {
		t.Fatalf("replace: %+v", op)
	}
	f.ts.Close()
	f.ts = newTestService(t, f.host, f.ts.home)
	f.deselect()
	f.wantSelected("doc.txt")
	f.pausePeer(false)
	f.waitForPeer("doc.txt", "edited")
	f.waitForDeselection("doc.txt")
	if f.holds("doc.txt") {
		t.Fatal("the local copy is still here")
	}
}

func TestOnDemandDeselectsByItself(t *testing.T) {
	interval := deselectInterval
	deselectInterval = 50 * time.Millisecond
	t.Cleanup(func() { deselectInterval = interval })
	f := newOnDemand(t, map[string]string{"doc.txt": "original"})
	if op := f.replace(f.item("doc.txt"), "edited"); op.State != stateResultCommitted {
		t.Fatalf("replace: %+v", op)
	}
	f.waitForPeer("doc.txt", "edited")
	waitFor(t, peerTimeout, "the path to be deselected", func() bool { return !f.selected("doc.txt") })
	waitFor(t, peerTimeout, "the local copy to go", func() bool { return !f.holds("doc.txt") })
}
