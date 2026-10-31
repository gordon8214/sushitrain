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

	"github.com/syncthing/syncthing/lib/config"
)

// These tests connect the gateway's engine to an ordinary Syncthing peer over loopback. Run them with
// -tags noassets,gwintegration; they take a few seconds to tens of seconds.

const peerTimeout = 60 * time.Second

func setDevicePaused(t *testing.T, e *testEngine, peer *testEngine, paused bool) {
	t.Helper()
	waiter, err := e.cfg.Modify(func(c *config.Configuration) {
		d, _, _ := c.Device(peer.id)
		d.Paused = paused
		c.SetDevice(d)
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
}

func TestPeersPropagateThroughGateway(t *testing.T) {
	gw := newTestEngine(t, "shared", nil)
	peer := newTestEngine(t, "shared", nil)
	connect(t, gw, peer)
	waitFor(t, peerTimeout, "connection", func() bool { return gw.app.Internals.IsConnectedTo(peer.id) })

	ts := newTestService(t, gw, "")
	ts.export(true)
	token := ts.pair(map[string]string{"shared": "write"})

	// A file written on the peer reaches the gateway's change log and can be read through it
	peer.write("from-peer.txt", "written on the peer")
	peer.scan()
	waitFor(t, peerTimeout, "the peer's file on the gateway", func() bool {
		got, ok := gw.read("from-peer.txt")
		return ok && got == "written on the peer"
	})
	item := ts.itemAt(token, "from-peer.txt")
	var lease ReadLease
	ts.json("POST", "/v1/items/"+item.ID+"/read-leases", token, ReadLeaseRequest{Revision: item.Revision},
		http.StatusCreated, &lease)
	if _, data := ts.do("GET", "/v1/content/"+lease.Lease, lease.Token, nil); string(data) != "written on the peer" {
		t.Fatalf("read %q", data)
	}

	// Files created and replaced through the gateway reach the peer
	root := ts.itemAt(token, "")
	op := createFile(ts, token, root.ID, "from-client.txt", []byte("written through the gateway"))
	if op.State != stateResultCommitted {
		t.Fatalf("create: %+v", op)
	}
	waitFor(t, peerTimeout, "the client's file on the peer", func() bool {
		got, ok := peer.read("from-client.txt")
		return ok && got == "written through the gateway"
	})
	item = ts.itemAt(token, "from-peer.txt")
	upload, digest := ts.upload(token, []byte("replaced through the gateway"))
	var replaced Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "shared",
		ItemID: item.ID, BaseRevision: item.Revision, Upload: upload, UploadDigest: digest}, http.StatusCreated, &replaced)
	if replaced.State != stateResultCommitted {
		t.Fatalf("replace: %+v", replaced)
	}
	waitFor(t, peerTimeout, "the replacement on the peer", func() bool {
		got, _ := peer.read("from-peer.txt")
		return got == "replaced through the gateway"
	})
	// The peer saw an ordinary new version, not a conflict
	entries, _ := os.ReadDir(peer.folderPath)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".sync-conflict-") {
			t.Fatalf("unexpected conflict copy %s on the peer", entry.Name())
		}
	}
}

func TestGatewayStreamsFromPeers(t *testing.T) {
	gw := newTestEngine(t, "shared", nil)
	peer := newTestEngine(t, "shared", nil)
	// The gateway doesn't keep this file, so reading it must stream verified blocks from the peer
	if err := os.WriteFile(filepath.Join(gw.folderPath, ".stignore"), []byte("remote-only.bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	connect(t, gw, peer)
	waitFor(t, peerTimeout, "connection", func() bool { return gw.app.Internals.IsConnectedTo(peer.id) })
	contents := strings.Repeat("streamed from the peer ", 50000)
	peer.write("remote-only.bin", contents)
	peer.scan()

	ts := newTestService(t, gw, "")
	ts.export(false)
	token := ts.pair(map[string]string{"shared": "read"})
	var item Item
	waitFor(t, peerTimeout, "the peer's index on the gateway", func() bool {
		ts.sync()
		row, ok, _ := liveItemByPath(ts.store.db, "shared", "remote-only.bin")
		if ok {
			ts.json("GET", "/v1/items/"+row.id, token, nil, http.StatusOK, &item)
		}
		return ok
	})
	if _, ok := gw.read("remote-only.bin"); ok {
		t.Fatal("the gateway pulled a file it ignores")
	}
	if item.Availability != availabilityPeers {
		t.Fatalf("availability %q", item.Availability)
	}
	var lease ReadLease
	ts.json("POST", "/v1/items/"+item.ID+"/read-leases", token, ReadLeaseRequest{Revision: item.Revision},
		http.StatusCreated, &lease)
	resp, data := ts.do("GET", "/v1/content/"+lease.Lease, lease.Token, nil, "Range", "bytes=500000-700000")
	if resp.StatusCode != http.StatusPartialContent || string(data) != contents[500000:700001] {
		t.Fatalf("streamed range: %d, %d bytes", resp.StatusCode, len(data))
	}
}

func TestOfflinePeerEditKeepsBothVersions(t *testing.T) {
	gw := newTestEngine(t, "shared", nil)
	peer := newTestEngine(t, "shared", nil)
	connect(t, gw, peer)
	waitFor(t, peerTimeout, "connection", func() bool { return gw.app.Internals.IsConnectedTo(peer.id) })
	peer.write("doc.txt", "original")
	peer.scan()
	waitFor(t, peerTimeout, "the original on the gateway", func() bool {
		got, _ := gw.read("doc.txt")
		return got == "original"
	})

	ts := newTestService(t, gw, "")
	ts.export(true)
	token := ts.pair(map[string]string{"shared": "write"})
	item := ts.itemAt(token, "doc.txt")

	// The peer goes offline and edits the file; the client edits it through the gateway meanwhile
	setDevicePaused(t, gw, peer, true)
	waitFor(t, peerTimeout, "disconnection", func() bool { return !gw.app.Internals.IsConnectedTo(peer.id) })
	time.Sleep(1100 * time.Millisecond)
	peer.write("doc.txt", "edited on the offline peer")
	peer.scan()
	upload, digest := ts.upload(token, []byte("edited through the gateway"))
	var op Operation
	ts.json("POST", "/v1/operations", token, OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "shared",
		ItemID: item.ID, BaseRevision: item.Revision, Upload: upload, UploadDigest: digest}, http.StatusCreated, &op)
	if op.State != stateResultCommitted {
		t.Fatalf("gateway edit: %+v", op)
	}

	// Back online, Syncthing's conflict handling keeps both edits
	setDevicePaused(t, gw, peer, false)
	waitFor(t, peerTimeout, "both versions on the gateway", func() bool {
		entries, _ := os.ReadDir(gw.folderPath)
		var contents []string
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "doc") {
				data, _ := os.ReadFile(filepath.Join(gw.folderPath, entry.Name()))
				contents = append(contents, string(data))
			}
		}
		joined := strings.Join(contents, "|")
		return strings.Contains(joined, "edited on the offline peer") && strings.Contains(joined, "edited through the gateway")
	})
}

func TestGlobalVersionFallsBack(t *testing.T) {
	// When the device with the newest version leaves the folder, the global version falls back to this device's
	// older one, without a new index sequence; the change log follows it
	gw := newTestEngine(t, "shared", func(f *config.FolderConfiguration) { f.Type = config.FolderTypeSendOnly })
	peer := newTestEngine(t, "shared", nil)
	gw.write("doc.txt", "old")
	gw.scan()
	connect(t, gw, peer)
	waitFor(t, peerTimeout, "connection", func() bool { return gw.app.Internals.IsConnectedTo(peer.id) })
	waitFor(t, peerTimeout, "the file on the peer", func() bool {
		got, ok := peer.read("doc.txt")
		return ok && got == "old"
	})

	ts := newTestService(t, gw, "")
	ts.export(false)
	token := ts.pair(map[string]string{"shared": "read"})
	old := ts.itemAt(token, "doc.txt")

	// A newer version on the peer is global; the send-only gateway doesn't pull it
	peer.write("doc.txt", "newer on the peer")
	peer.scan()
	waitFor(t, peerTimeout, "the peer's version in the change log", func() bool {
		return ts.itemAt(token, "doc.txt").Revision != old.Revision
	})

	waiter, err := gw.cfg.Modify(func(c *config.Configuration) {
		folder, _, _ := c.Folder("shared")
		folder.Devices = slices.DeleteFunc(folder.Devices, func(d config.FolderDeviceConfiguration) bool {
			return d.DeviceID == peer.id
		})
		c.SetFolder(folder)
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
	waitFor(t, peerTimeout, "the gateway's own version in the change log again", func() bool {
		return ts.itemAt(token, "doc.txt").Revision == old.Revision
	})
}

// peerFixture is a gateway and a real peer that both hold all of a folder. What matters is what the peer ends up with,
// so the tests check that, also after scans on both sides: a scan is what turns a mistake here into a file that comes
// back after being deleted, or a change that never arrives.
type peerFixture struct {
	t        *testing.T
	gw, peer *testEngine
	ts       *testService
	token    string
}

func newPeerFixture(t *testing.T, files map[string]string) *peerFixture {
	t.Helper()
	gw := newTestEngine(t, "shared", nil)
	peer := newTestEngine(t, "shared", nil)
	for name, contents := range files {
		peer.write(name, contents)
	}
	peer.scan()
	connect(t, gw, peer)
	waitFor(t, peerTimeout, "connection", func() bool { return gw.app.Internals.IsConnectedTo(peer.id) })
	ts := newTestService(t, gw, "")
	ts.export(true)
	f := &peerFixture{t: t, gw: gw, peer: peer, ts: ts, token: ts.pair(map[string]string{"shared": "write"})}
	for name, contents := range files {
		f.waitForFile(gw, name, contents)
	}
	f.settle()
	return f
}

func (f *peerFixture) item(p string) Item {
	f.t.Helper()
	return f.ts.itemAt(f.token, p)
}

func (f *peerFixture) operate(req OperationRequest) Operation {
	f.t.Helper()
	return f.ts.operate(f.token, req)
}

func (f *peerFixture) create(parent Item, name, contents string) Operation {
	f.t.Helper()
	upload, digest := f.ts.upload(f.token, []byte(contents))
	return f.operate(OperationRequest{ID: newUUID(), Kind: opCreate, FolderID: "shared", ParentID: parent.ID, Name: name,
		ItemKind: kindFile, Upload: upload, UploadDigest: digest})
}

func (f *peerFixture) pausePeer(paused bool) {
	f.t.Helper()
	setDevicePaused(f.t, f.gw, f.peer, paused)
	waitFor(f.t, peerTimeout, "the peer's connection", func() bool {
		return f.gw.app.Internals.IsConnectedTo(f.peer.id) != paused
	})
}

func (f *peerFixture) waitForFile(e *testEngine, name, contents string) {
	f.t.Helper()
	waitFor(f.t, peerTimeout, name+" on "+e.id.Short().String(), func() bool {
		got, ok := e.read(name)
		return ok && got == contents
	})
}

func (f *peerFixture) waitForPeerToLose(name string) {
	f.t.Helper()
	waitFor(f.t, peerTimeout, name+" gone from the peer", func() bool { return !f.peer.holds(name) })
}

// caughtUp reports whether a device is idle, needs nothing, and has all that the other device has announced
func caughtUp(e, other *testEngine) bool {
	if e.backend.FolderState(e.folderID) != "idle" {
		return false
	}
	own, err := e.app.Internals.Completion(e.id, e.folderID)
	if err != nil || own.NeedItems+own.NeedDeletes > 0 {
		return false
	}
	seen, err := e.app.Internals.Completion(other.id, e.folderID)
	if err != nil {
		return false
	}
	announced, err := other.backend.LocalSequence(other.folderID)
	return err == nil && seen.Sequence == announced
}

func (f *peerFixture) waitForAgreement() {
	f.t.Helper()
	waitFor(f.t, peerTimeout, "the devices to agree", func() bool { return caughtUp(f.gw, f.peer) && caughtUp(f.peer, f.gw) })
}

// settle scans both devices and waits for what that set off, and then checks that nothing was left to find
func (f *peerFixture) settle() {
	f.t.Helper()
	f.gw.scan()
	f.peer.scan()
	f.waitForAgreement()
	f.gw.wantScanAgrees(f.gw.sequence())
	f.peer.wantScanAgrees(f.peer.sequence())
}

func conflictCopies(t *testing.T, e *testEngine) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(e.folderPath, func(p string, _ fs.DirEntry, err error) error {
		if err == nil && strings.Contains(filepath.Base(p), ".sync-conflict-") {
			found = append(found, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// wantAnnounced fails unless both devices have the same version of an entry, which is deleted or not
func (f *peerFixture) wantAnnounced(name string, deleted bool) {
	f.t.Helper()
	ours, ok, err := f.gw.backend.LocalFile("shared", name)
	if err != nil || !ok || ours.Deleted != deleted || ours.IsInvalid() {
		f.t.Fatalf("%s on the gateway: %+v (%v, %v)", name, ours, ok, err)
	}
	theirs, ok, err := f.peer.backend.LocalFile("shared", name)
	if err != nil || !ok || theirs.Deleted != deleted || theirs.IsInvalid() {
		f.t.Fatalf("%s on the peer: %+v (%v, %v)", name, theirs, ok, err)
	}
	if !ours.Version.Equal(theirs.Version) {
		f.t.Fatalf("%s: gateway %v, peer %v", name, ours.Version, theirs.Version)
	}
}

// wantBoth settles the devices and checks what both hold, what both say about it, and that neither found a conflict
func (f *peerFixture) wantBoth(files map[string]string, gone ...string) {
	f.t.Helper()
	f.settle()
	for _, e := range []*testEngine{f.gw, f.peer} {
		e.wantFiles(files)
		e.wantGone(gone...)
		if found := conflictCopies(f.t, e); len(found) > 0 {
			f.t.Fatalf("conflict copies: %v", found)
		}
	}
	for name := range files {
		f.wantAnnounced(name, false)
	}
	for _, name := range gone {
		f.wantAnnounced(name, true)
	}
}

func TestDeleteReachesPeer(t *testing.T) {
	f := newPeerFixture(t, map[string]string{"gone.txt": "gone", "stays.txt": "stays"})
	wantCommitted(t, f.operate(deleteRequest(f.item("gone.txt"))))
	f.waitForPeerToLose("gone.txt")
	f.wantBoth(map[string]string{"stays.txt": "stays"}, "gone.txt")
}

func TestDirectoryDeleteReachesPeer(t *testing.T) {
	f := newPeerFixture(t, map[string]string{"dir/a.txt": "a", "dir/sub/b.txt": "b", "dir/sub/deep/c.txt": "c",
		"stays.txt": "stays"})
	wantCommitted(t, f.operate(deleteRequest(f.item("dir"))))
	f.waitForPeerToLose("dir")
	f.wantBoth(map[string]string{"stays.txt": "stays"}, "dir", "dir/a.txt", "dir/sub", "dir/sub/b.txt", "dir/sub/deep",
		"dir/sub/deep/c.txt")
}

func TestCreateDirectoryReachesPeer(t *testing.T) {
	f := newPeerFixture(t, map[string]string{"a/b/existing.txt": "there"})
	op := f.operate(createDirectoryRequest(f.item("a/b"), "made"))
	wantCommitted(t, op)
	waitFor(t, peerTimeout, "the directory on the peer", func() bool {
		info, err := os.Stat(filepath.Join(f.peer.folderPath, "a", "b", "made"))
		return err == nil && info.IsDir()
	})
	f.wantBoth(map[string]string{"a/b/existing.txt": "there"})
	f.wantAnnounced("a/b/made", false)
	for _, e := range []*testEngine{f.gw, f.peer} {
		if info, err := os.Stat(filepath.Join(e.folderPath, "a", "b", "made")); err != nil || !info.IsDir() {
			t.Fatalf("the directory: %v", err)
		}
	}
	// The peer's copy of the entry is no change to the client
	if item := f.item("a/b/made"); item.ID != op.Item.ID || item.Revision != op.Item.Revision {
		t.Fatalf("was %+v, is %+v", op.Item, item)
	}

	// As a client goes on after making a directory
	wantCommitted(t, f.create(f.item("a/b/made"), "inside.txt", "inside"))
	f.waitForFile(f.peer, "a/b/made/inside.txt", "inside")
	f.wantBoth(map[string]string{"a/b/existing.txt": "there", "a/b/made/inside.txt": "inside"})
}

func TestFileMoveReachesPeer(t *testing.T) {
	f := newPeerFixture(t, map[string]string{"dir/old.txt": "moved contents", "dir/stays.txt": "stays", "target/x.txt": "x"})
	file := f.item("dir/old.txt")
	op := f.operate(moveRequest(file, f.item("target"), "new.txt"))
	wantCommitted(t, op)
	f.waitForFile(f.peer, "target/new.txt", "moved contents")
	f.waitForPeerToLose("dir/old.txt")
	f.wantBoth(map[string]string{"target/new.txt": "moved contents", "dir/stays.txt": "stays", "target/x.txt": "x"},
		"dir/old.txt")
	// It is the same item for a client, also once the peer has it
	if item := f.item("target/new.txt"); item.ID != file.ID || item.Revision != op.Item.Revision {
		t.Fatalf("was %+v, is %+v", op.Item, item)
	}
}

func TestDirectoryMoveReachesPeer(t *testing.T) {
	f := newPeerFixture(t, map[string]string{"dir/inside.txt": "inside", "dir/sub/deep.txt": "deep", "target/x.txt": "x"})
	directory, inside := f.item("dir"), f.item("dir/sub/deep.txt")
	op := f.operate(moveRequest(directory, f.item("target"), "moved"))
	wantCommitted(t, op)
	f.waitForFile(f.peer, "target/moved/sub/deep.txt", "deep")
	f.waitForPeerToLose("dir")
	f.wantBoth(map[string]string{"target/moved/inside.txt": "inside", "target/moved/sub/deep.txt": "deep",
		"target/x.txt": "x"}, "dir", "dir/inside.txt", "dir/sub", "dir/sub/deep.txt")
	if item := f.item("target/moved"); item.ID != directory.ID {
		t.Fatalf("was %+v, is %+v", op.Item, item)
	}
	if item := f.item("target/moved/sub/deep.txt"); item.ID != inside.ID {
		t.Fatalf("was %+v, is %+v", inside, item)
	}
}

// The peer learns of the deletions only after scans here had their chance to take them back
func TestDeleteWhilePeerIsAway(t *testing.T) {
	f := newPeerFixture(t, map[string]string{"gone.txt": "gone", "dir/a.txt": "a", "dir/sub/b.txt": "b", "stays.txt": "stays"})
	file, directory := f.item("gone.txt"), f.item("dir")
	f.pausePeer(true)
	wantCommitted(t, f.operate(deleteRequest(file)))
	wantCommitted(t, f.operate(deleteRequest(directory)))
	f.gw.scan()
	f.gw.scan()
	f.peer.scan()
	f.peer.wantFiles(map[string]string{"gone.txt": "gone", "dir/a.txt": "a", "dir/sub/b.txt": "b"})

	f.pausePeer(false)
	f.waitForPeerToLose("gone.txt")
	f.waitForPeerToLose("dir")
	f.wantBoth(map[string]string{"stays.txt": "stays"}, "gone.txt", "dir", "dir/a.txt", "dir/sub", "dir/sub/b.txt")
}

// The file is new, built on a deletion this device took over: it's no conflict with that deletion
func TestCreateOverPeersDeletion(t *testing.T) {
	for _, tc := range []struct {
		name string
		away bool
	}{{"with the peer around", false}, {"while the peer is away", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPeerFixture(t, map[string]string{"doc.txt": "first", "other.txt": "other"})
			root := f.item("")
			if err := os.Remove(filepath.Join(f.peer.folderPath, "doc.txt")); err != nil {
				t.Fatal(err)
			}
			f.peer.scan()
			// The path is free once this device removed the file, not when it heard of the deletion
			waitFor(t, peerTimeout, "the deletion on the gateway", func() bool { return !f.gw.holds("doc.txt") })
			f.waitForAgreement()
			if tc.away {
				f.pausePeer(true)
			}
			wantCommitted(t, f.create(root, "doc.txt", "second"))
			if tc.away {
				f.pausePeer(false)
			}
			f.waitForFile(f.peer, "doc.txt", "second")
			f.wantBoth(map[string]string{"doc.txt": "second", "other.txt": "other"})
		})
	}
}

// setPullerDelay changes how long the folder waits before pulling what it is told about, which restarts it
func (e *testEngine) setPullerDelay(seconds float64) {
	e.t.Helper()
	waiter, err := e.cfg.Modify(func(c *config.Configuration) {
		folder, _, _ := c.Folder(e.folderID)
		folder.PullerDelayS = seconds
		c.SetFolder(folder)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	waiter.Wait()
	waitFor(e.t, peerTimeout, "the folder to run", func() bool { return e.app.Internals.ScanFolderSubdirs(e.folderID, nil) == nil })
}

// holdsAll reports whether a device holds all these contents in files whose names start with prefix, under any name
func (e *testEngine) holdsAll(prefix string, contents ...string) bool {
	entries, err := os.ReadDir(e.folderPath)
	if err != nil {
		return false
	}
	var found []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			data, _ := os.ReadFile(filepath.Join(e.folderPath, entry.Name()))
			found = append(found, string(data))
		}
	}
	return !slices.ContainsFunc(contents, func(c string) bool { return !slices.Contains(found, c) })
}

// interrupt sends an operation that stops at a journal state, as if the service crashed there, and closes the service
func (f *peerFixture) interrupt(req OperationRequest, stopAt string) string {
	f.t.Helper()
	crash := errors.New("simulated crash")
	f.ts.journalHook = func(_ string, state string) error {
		if state == stopAt {
			return crash
		}
		return nil
	}
	resp, data := f.ts.do("POST", "/v1/operations", f.token, req)
	if resp.StatusCode != http.StatusAccepted {
		f.t.Fatalf("interrupted operation: %d %s", resp.StatusCode, data)
	}
	var pending Operation
	if err := json.Unmarshal(data, &pending); err != nil {
		f.t.Fatal(err)
	}
	f.ts.Close()
	return pending.Operation
}

// recover starts the service again and returns how the interrupted operation ended
func (f *peerFixture) recover(id string) Operation {
	f.t.Helper()
	f.ts = newTestService(f.t, f.gw, f.ts.home)
	waitFor(f.t, peerTimeout, "recovery", func() bool {
		op, ok, _ := f.ts.loadOperation(f.ts.store.db, id)
		return ok && op.state != stateResultPending
	})
	var done Operation
	f.ts.json("GET", "/v1/operations/"+id, f.token, nil, http.StatusOK, &done)
	return done
}

// A peer's edit that arrives once an edit here is in place but not yet indexed, here across a restart. The edit is built
// on what this device has, which it can't have replaced by then (the puller doesn't touch a file that differs from the
// index), so the devices see two concurrent versions. Building it on the global version would silently replace the
// peer's edit.
func TestPeerEditArrivingBeforeRecoveryKeepsBothEdits(t *testing.T) {
	for _, tc := range []struct {
		name string
		// Whether the engine runs into the edit on disk before the service gets to it. It then keeps the edit as a
		// conflict copy, as the peer's edit is the later one, and the service publishes it once more.
		pulled bool
		state  string
	}{{"before the engine acts on it", false, stateResultCommitted}, {"after the engine acted on it", true, stateResultConflict}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPeerFixture(t, map[string]string{"doc.txt": "original"})
			if !tc.pulled {
				f.gw.setPullerDelay(3600)
			}
			item := f.item("doc.txt")
			upload, digest := f.ts.upload(f.token, []byte("edited here"))
			id := f.interrupt(OperationRequest{ID: newUUID(), Kind: opReplace, FolderID: "shared", ItemID: item.ID,
				BaseRevision: item.Revision, Upload: upload, UploadDigest: digest}, journalPublished)
			if got, _ := f.gw.read("doc.txt"); got != "edited here" {
				t.Fatalf("the edit was not in place before the simulated crash: %q", got)
			}

			f.peer.write("doc.txt", "edited on the peer")
			f.peer.scan()
			waitFor(t, peerTimeout, "the peer's version on the gateway", func() bool {
				global, ok, _ := f.gw.backend.GlobalFile("shared", "doc.txt")
				return ok && global.Size == int64(len("edited on the peer"))
			})
			if tc.pulled {
				f.waitForAgreement()
			} else if local, _, _ := f.gw.backend.LocalFile("shared", "doc.txt"); local.Size != int64(len("original")) {
				t.Fatalf("the engine acted on it: %+v", local)
			}

			done := f.recover(id)
			if done.State != tc.state {
				t.Fatalf("recovered operation: %+v (%+v)", done, done.Error)
			}
			if !tc.pulled {
				f.gw.setPullerDelay(0)
			}
			for _, e := range []*testEngine{f.peer, f.gw} {
				waitFor(t, peerTimeout, "both edits", func() bool { return e.holdsAll("doc", "edited here", "edited on the peer") })
			}
			f.settle()
			got, _ := f.gw.read("doc.txt")
			f.peer.wantFiles(map[string]string{"doc.txt": got})
		})
	}
}

// A delete that resumes after a restart removes what it checked, and no version that arrived since
func TestResumedDeleteKeepsNewerVersion(t *testing.T) {
	f := newPeerFixture(t, map[string]string{"doc.txt": "original"})
	id := f.interrupt(deleteRequest(f.item("doc.txt")), journalPublishing)
	f.peer.write("doc.txt", "changed meanwhile")
	f.peer.scan()
	f.waitForFile(f.gw, "doc.txt", "changed meanwhile")

	wantRefused(t, f.recover(id), codeRevisionMismatch)
	f.wantBoth(map[string]string{"doc.txt": "changed meanwhile"})
}

// retriedDelete stops a delete just after it recorded that it is publishing, having removed some of what it was going
// to, and runs it again after change, once the change log has caught up: nothing stale is left to stop it then
func (f *peerFixture) retriedDelete(item Item, removed []string, change func()) Operation {
	f.t.Helper()
	var stopped atomic.Bool
	f.ts.journalHook = func(_ string, state string) error {
		if state != journalPublishing || !stopped.CompareAndSwap(false, true) {
			return nil
		}
		for _, name := range removed {
			if err := os.Remove(filepath.Join(f.gw.folderPath, filepath.FromSlash(name))); err != nil {
				f.t.Error(err)
			}
		}
		return errors.New("a failure after publishing was recorded")
	}
	resp, data := f.ts.do("POST", "/v1/operations", f.token, deleteRequest(item))
	if resp.StatusCode != http.StatusAccepted {
		f.t.Fatalf("interrupted operation: %d %s", resp.StatusCode, data)
	}
	var op Operation
	if err := json.Unmarshal(data, &op); err != nil {
		f.t.Fatal(err)
	}
	change()
	f.ts.sync()
	f.ts.retryPendingOperations("")
	waitFor(f.t, peerTimeout, "the operation", func() bool {
		f.ts.json("GET", "/v1/operations/"+op.Operation, f.token, nil, http.StatusOK, &op)
		return op.State != stateResultPending
	})
	return op
}

func TestRetriedDeleteKeepsNewerVersion(t *testing.T) {
	f := newPeerFixture(t, map[string]string{"doc.txt": "original"})
	op := f.retriedDelete(f.item("doc.txt"), nil, func() {
		f.peer.write("doc.txt", "changed meanwhile")
		f.peer.scan()
		f.waitForFile(f.gw, "doc.txt", "changed meanwhile")
	})
	wantRefused(t, op, codeRevisionMismatch)
	f.wantBoth(map[string]string{"doc.txt": "changed meanwhile"})
}

// A directory that is partly gone when a peer edits what is left: the edit stays, and what the delete removed stays
// removed, on both devices
func TestRetriedDeleteKeepsNewerVersionInADirectory(t *testing.T) {
	f := newPeerFixture(t, map[string]string{"dir/a.txt": "a", "dir/b.txt": "b", "stays.txt": "stays"})
	op := f.retriedDelete(f.item("dir"), []string{"dir/a.txt"}, func() {
		f.peer.write("dir/b.txt", "changed meanwhile")
		f.peer.scan()
		f.waitForFile(f.gw, "dir/b.txt", "changed meanwhile")
	})
	wantRefused(t, op, codeRevisionMismatch)
	f.wantBoth(map[string]string{"dir/b.txt": "changed meanwhile", "stays.txt": "stays"}, "dir/a.txt")
}

func TestRetriedDeleteFinishesWhatIsLeft(t *testing.T) {
	f := newPeerFixture(t, map[string]string{"dir/a.txt": "a", "dir/b.txt": "b", "stays.txt": "stays"})
	op := f.retriedDelete(f.item("dir"), []string{"dir/a.txt"}, func() {})
	if op.State != stateResultCommitted {
		t.Fatalf("%+v (%+v)", op, op.Error)
	}
	f.wantBoth(map[string]string{"stays.txt": "stays"}, "dir/a.txt", "dir/b.txt", "dir")
}
