// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
	"t-shaped.nl/sushitrain/v2/remoteapi"
	"t-shaped.nl/sushitrain/v2/remoteclient"
)

// testSettleDelay is how long a folder must be quiet in tests before its changes are sent
const testSettleDelay = 100 * time.Millisecond

func writableGateway(t *testing.T, files map[string]string) *gatewaytest.Gateway {
	t.Helper()
	e := gatewaytest.NewEngine(t, "docs", nil)
	for name, contents := range files {
		e.Write(name, contents)
	}
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	return g
}

// dir returns the item of a directory below the mount's root
func (ts *testService) dir(path string) int64 {
	ts.t.Helper()
	return ts.mustLookup(path).Item
}

// create writes a new file
func (ts *testService) create(dirPath, name, contents string) Entry {
	ts.t.Helper()
	e, err := ts.Create(ts.dir(dirPath), name, false)
	if err != nil {
		ts.t.Fatalf("creating %s/%s: %v", dirPath, name, err)
	}
	ts.write(e, contents, true)
	return e
}

// write writes contents to a file, from its start
func (ts *testService) write(e Entry, contents string, truncate bool) {
	ts.t.Helper()
	w, err := ts.OpenWriter(e, truncate)
	if err != nil {
		ts.t.Fatal(err)
	}
	if contents != "" {
		if _, err := w.WriteAt(context.Background(), []byte(contents), 0); err != nil {
			ts.t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		ts.t.Fatal(err)
	}
}

// settled waits until everything in the folder is committed by the gateway
func (ts *testService) settled() {
	ts.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := ts.Wait(ctx, ts.dir("docs")); err != nil {
		ts.t.Fatalf("waiting for changes: %v", err)
	}
}

// gatewayHas checks a file on the gateway's disk
func (ts *testService) gatewayHas(name, want string) {
	ts.t.Helper()
	got, ok := ts.g.Engine.Read(name)
	if !ok || got != want {
		ts.t.Fatalf("the gateway has %q at %s (exists: %v), want %q", got, name, ok, want)
	}
}

func (ts *testService) gatewayLacks(name string) {
	ts.t.Helper()
	if _, err := os.Lstat(ts.g.Engine.Path(name)); err == nil {
		ts.t.Fatalf("the gateway has %s", name)
	}
}

func (ts *testService) gatewayID(path string) string {
	ts.t.Helper()
	var id string
	if err := ts.store.db.QueryRow(`SELECT gateway_id FROM remote_items WHERE item = ?`, ts.mustLookup(path).Item).
		Scan(&id); err != nil {
		ts.t.Fatalf("no gateway item for %s: %v", path, err)
	}
	return id
}

// sent counts the operations of a kind that were sent
func (ts *testService) sent(kind string) int {
	ts.t.Helper()
	var n int
	if err := ts.store.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE kind = ? AND state IN ('committed', 'conflict')`,
		kind).Scan(&n); err != nil {
		ts.t.Fatal(err)
	}
	return n
}

func (ts *testService) readPath(path string) string {
	ts.t.Helper()
	return string(ts.read(ts.mustLookup(path)))
}

func TestCreateEditMoveRemove(t *testing.T) {
	g := writableGateway(t, map[string]string{"keep.txt": "untouched"})
	ts := newTestService(t, g, 0)

	ts.create("docs", "new.txt", "one")
	ts.settled()
	ts.gatewayHas("new.txt", "one")
	if state, _ := ts.State(ts.mustLookup("docs/new.txt")); state != "local" {
		t.Fatalf("a sent file is %s", state)
	}

	// An edit in place, without truncating: the working copy starts from the content
	e := ts.mustLookup("docs/new.txt")
	w, err := ts.OpenWriter(e, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt(context.Background(), []byte("two"), 3); err != nil {
		t.Fatal(err)
	}
	if got := ts.readPath("docs/new.txt"); got != "onetwo" {
		t.Fatalf("while writing it reads %q", got)
	}
	w.Close()
	ts.settled()
	ts.gatewayHas("new.txt", "onetwo")
	if ts.mustLookup("docs/new.txt").Item != e.Item {
		t.Fatal("an edit changed the item")
	}

	// A directory with a file, made here, then renamed before it settles: created where it ends up
	d, err := ts.Create(ts.dir("docs"), "dir", true)
	if err != nil {
		t.Fatal(err)
	}
	ts.create("docs/dir", "a.txt", "a")
	if _, err := ts.Rename(ts.dir("docs"), "dir", ts.dir("docs"), "renamed", false); err != nil {
		t.Fatal(err)
	}
	ts.settled()
	ts.gatewayHas("renamed/a.txt", "a")
	ts.gatewayLacks("dir")
	if ts.sent(remoteapi.OpMove) != 0 {
		t.Fatal("a directory renamed before it was sent was moved on the gateway")
	}

	// Moved after it is on the gateway: moved there, keeping its item
	id := ts.gatewayID("docs/renamed")
	if _, err := ts.Rename(ts.dir("docs"), "renamed", ts.dir("docs"), "moved", false); err != nil {
		t.Fatal(err)
	}
	ts.settled()
	ts.gatewayHas("moved/a.txt", "a")
	if ts.gatewayID("docs/moved") != id || ts.mustLookup("docs/moved").Item != d.Item {
		t.Fatal("a move changed the item")
	}

	// rm -r: the files, then the directory; one removal on the gateway
	if _, err := ts.Remove(ts.dir("docs/moved"), "a.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Remove(ts.dir("docs"), "moved", true); err != nil {
		t.Fatal(err)
	}
	ts.settled()
	ts.gatewayLacks("moved")
	if n := ts.sent(remoteapi.OpDelete); n != 1 {
		t.Fatalf("rm -r sent %d removals", n)
	}
	ts.gatewayHas("keep.txt", "untouched")
	ts.refresh()
	if _, ok := ts.lookup("docs/moved"); ok {
		t.Fatal("the removed directory came back")
	}

	// A removal of a directory that isn't empty is refused
	if _, err := ts.Remove(ts.dir("docs"), "keep.txt", true); !errors.Is(err, ErrNotDir) {
		t.Fatalf("rmdir of a file: %v", err)
	}
}

func TestSavesKeepTheItem(t *testing.T) {
	g := writableGateway(t, map[string]string{"report.txt": "v1"})
	ts := newTestService(t, g, 0)
	id := ts.gatewayID("docs/report.txt")

	// GIO: a temporary file renamed over the target
	tmp := ts.create("docs", ".goutputstream-ABC123", "v2")
	if _, err := ts.Rename(ts.dir("docs"), tmp.Name, ts.dir("docs"), "report.txt", false); err != nil {
		t.Fatal(err)
	}
	ts.settled()
	ts.gatewayHas("report.txt", "v2")
	ts.gatewayLacks(".goutputstream-ABC123")
	if ts.gatewayID("docs/report.txt") != id {
		t.Fatal("a save through a temporary file made a new item on the gateway")
	}

	// vim without backupcopy: the file renamed to a backup, a new one written, the backup removed
	if _, err := ts.Rename(ts.dir("docs"), "report.txt", ts.dir("docs"), "report.txt~", false); err != nil {
		t.Fatal(err)
	}
	ts.create("docs", "report.txt", "v3")
	if _, err := ts.Remove(ts.dir("docs"), "report.txt~", false); err != nil {
		t.Fatal(err)
	}
	ts.settled()
	ts.gatewayHas("report.txt", "v3")
	ts.gatewayLacks("report.txt~")
	if ts.gatewayID("docs/report.txt") != id {
		t.Fatal("vim's save made a new item on the gateway")
	}
	if n := ts.sent(remoteapi.OpCreate) + ts.sent(remoteapi.OpDelete) + ts.sent(remoteapi.OpMove); n != 0 {
		t.Fatalf("saves sent %d creations, removals or moves", n)
	}

	// An app that saves a file it only opened sends the same content again, which archives nothing
	ts.write(ts.mustLookup("docs/report.txt"), "v3", true)
	ts.settled()
	ts.gatewayHas("report.txt", "v3")

	// A lock file stays here
	ts.create("docs", ".~lock.report.txt#", "lock")
	ts.create("docs", ".report.txt.swp", "swap")
	ts.settled()
	ts.gatewayLacks(".~lock.report.txt#")
	ts.gatewayLacks(".report.txt.swp")
	if state, _ := ts.State(ts.mustLookup("docs/.~lock.report.txt#")); state != "local-only" {
		t.Fatalf("a lock file is %s", state)
	}
}

func TestConflicts(t *testing.T) {
	g := writableGateway(t, map[string]string{"shared.txt": "base"})
	ts := newTestService(t, g, 0)
	e := ts.mustLookup("docs/shared.txt")

	// An edit here based on "base", while the gateway's file changes: kept as a conflict copy
	w, err := ts.OpenWriter(e, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt(context.Background(), []byte("mine"), 0); err != nil {
		t.Fatal(err)
	}
	g.Engine.Write("shared.txt", "theirs")
	ts.refresh()
	w.Close()
	ts.settled()
	ts.gatewayHas("shared.txt", "theirs")
	entries, err := os.ReadDir(g.Engine.FolderPath)
	if err != nil {
		t.Fatal(err)
	}
	var copies []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "shared.sync-conflict-") {
			copies = append(copies, entry.Name())
		}
	}
	if len(copies) != 1 {
		t.Fatalf("conflict copies %v", copies)
	}
	ts.gatewayHas(copies[0], "mine")
	ts.refresh()
	if got := ts.readPath("docs/shared.txt"); got != "theirs" {
		t.Fatalf("the item reads %q", got)
	}
	if got := ts.readPath("docs/" + copies[0]); got != "mine" {
		t.Fatalf("the copy reads %q", got)
	}

	// A name taken on the gateway meanwhile: the new file takes a conflict copy's name
	ts.create("docs", "taken.txt", "here")
	g.Engine.Write("taken.txt", "there")
	g.Sync()
	ts.settled()
	ts.gatewayHas("taken.txt", "there")
	ts.refresh()
	children, err := ts.Children(ts.dir("docs"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range children {
		found = found || strings.HasPrefix(c.Name, "taken.sync-conflict-")
	}
	if !found {
		t.Fatalf("no conflict copy of taken.txt here: %v", children)
	}
}

func TestSecondSaveBeforeTheFirstIsSent(t *testing.T) {
	g := writableGateway(t, map[string]string{"doc.txt": "v1"})
	var mutex sync.Mutex
	hold := make(chan struct{})
	holding := false
	ts := newTestServiceWith(t, g, func(c *Config) {
		c.Hook = func(point string) error {
			mutex.Lock()
			h := holding
			mutex.Unlock()
			if point == "outbox.submit" && h {
				<-hold
			}
			return nil
		}
	})
	mutex.Lock()
	holding = true
	mutex.Unlock()
	ts.write(ts.mustLookup("docs/doc.txt"), "v2", true)
	// The first save is being sent when the second is made
	gatewaytest.WaitFor(t, 30*time.Second, "the first save to be sent", func() bool {
		var n int
		_ = ts.store.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE state = 'submitted'`).Scan(&n)
		return n == 1
	})
	ts.write(ts.mustLookup("docs/doc.txt"), "v3", true)
	time.Sleep(3 * testSettleDelay)
	mutex.Lock()
	holding = false
	mutex.Unlock()
	close(hold)
	ts.settled()
	ts.gatewayHas("doc.txt", "v3")
	entries, _ := os.ReadDir(g.Engine.FolderPath)
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "sync-conflict") {
			t.Fatalf("a second save made a conflict copy %s", entry.Name())
		}
	}
}

func TestRemovingAChangedFileIsUndone(t *testing.T) {
	g := writableGateway(t, map[string]string{"doc.txt": "v1"})
	ts := newTestService(t, g, 0)
	if _, err := ts.Remove(ts.dir("docs"), "doc.txt", false); err != nil {
		t.Fatal(err)
	}
	// Changed on the gateway before the removal is sent
	g.Engine.Write("doc.txt", "v2")
	ts.refresh()
	ts.settled()
	ts.gatewayHas("doc.txt", "v2")
	if got := ts.readPath("docs/doc.txt"); got != "v2" {
		t.Fatalf("the file came back as %q", got)
	}
}

func TestOfflineChangesWait(t *testing.T) {
	g := writableGateway(t, nil)
	ts := newTestService(t, g, 0)
	g.Stop()
	ts.create("docs", "offline.txt", "made offline")
	time.Sleep(time.Second)
	if state, _ := ts.State(ts.mustLookup("docs/offline.txt")); state != "pending" {
		t.Fatalf("an unsent file is %s", state)
	}
	if got := ts.readPath("docs/offline.txt"); got != "made offline" {
		t.Fatalf("it reads %q", got)
	}
	g.Start()
	ts.settled()
	ts.gatewayHas("offline.txt", "made offline")
}

func TestUnknownOutcomesAreNotSentAgain(t *testing.T) {
	g := writableGateway(t, nil)
	stop := true
	var mutex sync.Mutex
	ts := newTestServiceWith(t, g, func(c *Config) {
		c.Hook = func(point string) error {
			mutex.Lock()
			defer mutex.Unlock()
			if point == "outbox.submit" && stop {
				return errors.New("stopped before sending")
			}
			return nil
		}
	})
	ts.create("docs", "sent.txt", "x")
	gatewaytest.WaitFor(t, 30*time.Second, "the creation to be ready to send", func() bool {
		var n int
		_ = ts.store.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE state = 'submitted'`).Scan(&n)
		return n == 1
	})
	// The gateway's database is restored: a cursor beyond its change log starts a new authority generation
	client, err := ts.client(ts.folder.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Changes(context.Background(), "docs", "g1.999999", 0, 0); !errors.Is(err,
		remoteclient.ErrCursorExpired) {
		t.Fatalf("a cursor beyond the head: %v", err)
	}
	g2, err := ts.gatewayRef(ts.folder.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.checkCapabilities(context.Background(), g2, client); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	stop = false
	mutex.Unlock()
	ts.wakeOutbox()
	edits, err := ts.Edits()
	if err != nil || len(edits) != 1 || edits[0].State != stateUnknown || !edits[0].Review() {
		t.Fatalf("edits %+v, %v", edits, err)
	}
	time.Sleep(time.Second)
	ts.gatewayLacks("sent.txt")
	// Discarding it removes the file the gateway never confirmed
	if err := ts.DiscardEdit(edits[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := ts.lookup("docs/sent.txt"); ok {
		t.Fatal("a discarded file is still here")
	}
}

func TestCrashRecovery(t *testing.T) {
	g := writableGateway(t, map[string]string{"a.txt": "a"})
	var mutex sync.Mutex
	crashAt := ""
	ts := newTestServiceWith(t, g, func(c *Config) {
		c.Hook = func(point string) error {
			mutex.Lock()
			defer mutex.Unlock()
			if point == crashAt {
				return errors.New("crash at " + point)
			}
			return nil
		}
	})
	crash := func(point string) {
		mutex.Lock()
		crashAt = point
		mutex.Unlock()
	}

	// After staging, before the operations were recorded
	crash("settle.staged")
	ts.create("docs", "staged.txt", "s")
	time.Sleep(5 * testSettleDelay)
	crash("")
	ts.restart()
	ts.settled()
	ts.gatewayHas("staged.txt", "s")

	// After the operation was sent, before its result was recorded: sent again, committed once
	crash("outbox.sent")
	ts.create("docs", "sent.txt", "x")
	gatewaytest.WaitFor(t, 30*time.Second, "the creation to be sent", func() bool {
		_, ok := g.Engine.Read("sent.txt")
		return ok
	})
	crash("")
	ts.restart()
	ts.settled()
	ts.gatewayHas("sent.txt", "x")
	if n := ts.sent(remoteapi.OpCreate); n != 2 {
		t.Fatalf("%d creations committed, want 2", n)
	}

	// While a file was being written: kept for review, not sent
	w, err := ts.OpenWriter(ts.mustLookup("docs/a.txt"), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt(context.Background(), []byte("half writ"), 0); err != nil {
		t.Fatal(err)
	}
	ts.restart()
	edits, err := ts.Edits()
	if err != nil || len(edits) != 1 || edits[0].State != stateBlocked || !edits[0].Content {
		t.Fatalf("edits after a crash while writing: %+v, %v", edits, err)
	}
	if got := ts.readPath("docs/a.txt"); got != "half writ" {
		t.Fatalf("the file reads %q", got)
	}
	dest := filepath.Join(t.TempDir(), "exported")
	if err := ts.ExportEdit(edits[0].ID, dest); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dest); string(data) != "half writ" {
		t.Fatalf("exported %q", data)
	}
	if err := ts.DiscardEdit(edits[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := ts.readPath("docs/a.txt"); got != "a" {
		t.Fatalf("after discarding, the file reads %q", got)
	}
	ts.gatewayHas("a.txt", "a")
	// Nothing staged is left behind
	ts.settled()
	if entries, _ := os.ReadDir(ts.stagedDir()); len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("staged files left: %v", names)
	}
}

func TestRemovingAFolderWithChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	g := writableGateway(t, nil)
	ts := newTestService(t, g, 0)
	g.Stop()
	ts.create("docs", "unsent.txt", "x")
	ts.create("docs", ".~lock.doc.odt#", "lock")
	time.Sleep(5 * testSettleDelay)
	if _, _, err := ts.RemoveFolder(ts.folder.ID); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("removing a folder with unsent changes: %v", err)
	}
	g.Start()
	ts.settled()
	dest, moved, err := ts.RemoveFolder(ts.folder.ID)
	if err != nil || moved != 1 {
		t.Fatalf("removing: %s, %d, %v", dest, moved, err)
	}
	if data, err := os.ReadFile(filepath.Join(dest, ".~lock.doc.odt#")); err != nil || string(data) != "lock" {
		t.Fatalf("the lock file moved out: %q, %v", data, err)
	}
}

func TestAccessChangesAreNoticed(t *testing.T) {
	g := writableGateway(t, map[string]string{"a.txt": "a"})
	ts := newTestService(t, g, 0)
	clients, err := g.Clients()
	if err != nil || len(clients) != 1 {
		t.Fatalf("%+v %v", clients, err)
	}
	if err := g.SetPermission(clients[0].ClientID, "docs", "read"); err != nil {
		t.Fatal(err)
	}
	ts.mutex.Lock()
	ts.changes = nil
	ts.mutex.Unlock()

	client, err := ts.client(ts.folder.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := ts.gatewayRef(ts.folder.ServiceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.checkCapabilities(context.Background(), ref, client); err != nil {
		t.Fatal(err)
	}
	if e := ts.mustLookup("docs/a.txt"); e.Writable {
		t.Fatal("a file of a folder made read-only is writable")
	}
	if _, err := ts.Create(ts.dir("docs"), "new.txt", false); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("created a file in a folder made read-only: %v", err)
	}
	ts.mutex.Lock()
	attrs := 0
	for _, c := range ts.changes {
		attrs += len(c.Attrs)
	}
	ts.mutex.Unlock()
	if attrs < 2 {
		t.Fatalf("the kernel wasn't told that %d nodes changed", attrs)
	}
}

func TestLeavingFilesMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	g := writableGateway(t, map[string]string{"read.txt": "fetched"})
	ts := newTestService(t, g, 0)
	if got := ts.readPath("docs/read.txt"); got != "fetched" {
		t.Fatalf("read %q", got)
	}
	g.Stop()
	ts.create("docs", "unsent.txt", "x")
	ts.create("docs", ".~lock.doc.odt#", "lock")
	// The gateway can't be reached: nothing is taken out
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := ts.Leave(ctx); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("left with a change that isn't on the gateway: %v", err)
	}
	if folders, err := ts.Folders(); err != nil || len(folders) != 1 {
		t.Fatalf("folders after a refused switch: %+v %v", folders, err)
	}

	// Once it can, the change is sent first
	g.Start()
	ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	removals, err := ts.Leave(ctx)
	if err != nil || len(removals) != 1 || removals[0].Moved != 1 {
		t.Fatalf("%+v %v", removals, err)
	}
	ts.gatewayHas("unsent.txt", "x")
	if folders, err := ts.Folders(); err != nil || len(folders) != 0 {
		t.Fatalf("folders after leaving: %+v %v", folders, err)
	}
	if gateways, err := ts.Gateways(); err != nil || len(gateways) != 1 {
		t.Fatalf("the pairing didn't stay: %+v %v", gateways, err)
	}
	if st, err := ts.Storage(); err != nil || st.Used != 0 {
		t.Fatalf("the cache isn't empty: %+v %v", st, err)
	}
}

// A handle opened as a settle commits writes to a working copy of its own, which is sent too
func TestWriteWhileSettling(t *testing.T) {
	g := writableGateway(t, map[string]string{"log.txt": "zero"})
	var ts *testService
	var once sync.Once
	written := make(chan error, 1)
	ts = newTestServiceWith(t, g, func(c *Config) {
		c.SettleDelay = testSettleDelay
		c.Hook = func(point string) error {
			if point != "settle.committed" {
				return nil
			}
			once.Do(func() {
				// It waits for the settle to retire the working copy it would otherwise have written to
				go func() {
					e := ts.mustLookup("docs/log.txt")
					w, err := ts.OpenWriter(e, false)
					if err == nil {
						_, err = w.WriteAt(context.Background(), []byte("two!"), 0)
						if closeErr := w.Close(); err == nil {
							err = closeErr
						}
					}
					written <- err
				}()
			})
			return nil
		}
	})
	ts.write(ts.mustLookup("docs/log.txt"), "one!", true)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * testSettleDelay)
	ts.settled()
	ts.gatewayHas("log.txt", "two!")
}

// A save merged into an operation while its upload ran is sent, not the content uploaded before it
func TestSaveDuringUpload(t *testing.T) {
	g := writableGateway(t, map[string]string{"doc.txt": "base"})
	var ts *testService
	var once sync.Once
	ts = newTestServiceWith(t, g, func(c *Config) {
		c.SettleDelay = testSettleDelay
		c.Hook = func(point string) error {
			if point != "outbox.uploaded" {
				return nil
			}
			once.Do(func() {
				// The second save settles into the same operation before the first upload is recorded
				ts.write(ts.mustLookup("docs/doc.txt"), "second", true)
				gatewaytest.WaitFor(t, 30*time.Second, "the second save to settle", func() bool {
					var queued int
					_ = ts.store.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE state = 'queued'
						AND upload_id = ''`).Scan(&queued)
					return queued == 1
				})
			})
			return nil
		}
	})
	ts.write(ts.mustLookup("docs/doc.txt"), "first", true)
	time.Sleep(5 * testSettleDelay)
	ts.settled()
	ts.gatewayHas("doc.txt", "second")
	if edits, err := ts.Edits(); err != nil || len(edits) != 0 {
		t.Fatalf("edits left: %+v %v", edits, err)
	}
}

// An edit stuck on an item that was removed here before it was ever sent can't keep the folder forever
func TestRemovedItemTakesItsStuckEdits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	g := writableGateway(t, nil)
	ts := newTestService(t, g, 0)
	g.Stop()
	ts.create("docs", "torn.txt", "x")
	gatewaytest.WaitFor(t, 30*time.Second, "the creation to be queued", func() bool {
		var n int
		_ = ts.store.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE kind = 'create'`).Scan(&n)
		return n == 1
	})
	// As recovery after a crash leaves a file that was being written
	if _, err := ts.store.db.Exec(`UPDATE outbox SET state = 'blocked' WHERE kind = 'create'`); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Remove(ts.dir("docs"), "torn.txt", false); err != nil {
		t.Fatal(err)
	}
	if edits, err := ts.Edits(); err != nil || len(edits) != 0 {
		t.Fatalf("edits of a removed item: %+v %v", edits, err)
	}
	g.Start()
	if _, _, err := ts.RemoveFolder(ts.folder.ID); err != nil {
		t.Fatal(err)
	}
}

// mv log.1 log.2; mv log log.1: the second move waits for the name the first frees, and nothing conflicts
func TestRotation(t *testing.T) {
	g := writableGateway(t, map[string]string{"log": "newest", "log.1": "older"})
	ts := newTestService(t, g, 0)
	docs := ts.dir("docs")
	if _, err := ts.Rename(docs, "log.1", docs, "log.2", false); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Rename(docs, "log", docs, "log.1", false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * testSettleDelay)
	ts.settled()
	ts.gatewayHas("log.1", "newest")
	ts.gatewayHas("log.2", "older")
	ts.gatewayLacks("log")
	if edits, err := ts.Edits(); err != nil || len(edits) != 0 {
		t.Fatalf("edits left: %+v %v", edits, err)
	}
}

// mv other/x dir/; rm -r dir: x is removed on the gateway too, though it wasn't in dir there
func TestRemovingADirectoryWithWhatMovedIntoIt(t *testing.T) {
	g := writableGateway(t, map[string]string{"other/x.txt": "x", "dir/y.txt": "y"})
	ts := newTestService(t, g, 0)
	if _, err := ts.Rename(ts.dir("docs/other"), "x.txt", ts.dir("docs/dir"), "x.txt", false); err != nil {
		t.Fatal(err)
	}
	dir := ts.dir("docs/dir")
	for _, name := range []string{"x.txt", "y.txt"} {
		if _, err := ts.Remove(dir, name, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ts.Remove(ts.dir("docs"), "dir", true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * testSettleDelay)
	ts.settled()
	ts.gatewayLacks("dir")
	ts.gatewayLacks("other/x.txt")
	ts.gatewayLacks("dir/x.txt")
}

// A directory removed on the gateway while a new file in it was on its way is made again, before the file, though
// its creation was queued after the file's
func TestDirectoryMadeAgainGoesFirst(t *testing.T) {
	g := writableGateway(t, map[string]string{"sub/old.txt": "old"})
	var ts *testService
	var once sync.Once
	ts = newTestServiceWith(t, g, func(c *Config) {
		c.SettleDelay = testSettleDelay
		c.Hook = func(point string) error {
			if point != "outbox.uploaded" {
				return nil
			}
			once.Do(func() {
				g.Engine.Remove("sub")
				ts.refresh()
				gatewaytest.WaitFor(t, 30*time.Second, "the directory's creation to be queued", func() bool {
					var n int
					_ = ts.store.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE kind = 'create'
						AND item_kind = 'directory' AND state IN ` + activeStates).Scan(&n)
					return n == 1
				})
			})
			return nil
		}
	})
	ts.create("docs/sub", "new.txt", "new")
	time.Sleep(5 * testSettleDelay)
	ts.settled()
	ts.gatewayHas("sub/new.txt", "new")
	ts.gatewayLacks("sub/old.txt")
	if edits, err := ts.Edits(); err != nil || len(edits) != 0 {
		t.Fatalf("edits left: %+v %v", edits, err)
	}
}

// rm -r of a directory while a save in it is on its way: the save changes what the gateway has below the directory
// after it was removed here, which the removal must get past. With another device's change in it, it is undone.
func TestRemovingADirectoryWhileASaveInItIsSent(t *testing.T) {
	for _, theirs := range []bool{false, true} {
		t.Run(map[bool]string{false: "own", true: "theirs"}[theirs], func(t *testing.T) {
			g := writableGateway(t, map[string]string{"dir/f.txt": "v1", "dir/g.txt": "g"})
			var ts *testService
			var once sync.Once
			ts = newTestServiceWith(t, g, func(c *Config) {
				c.SettleDelay = testSettleDelay
				c.Hook = func(point string) error {
					if point != "outbox.submit" {
						return nil
					}
					once.Do(func() {
						dir := ts.dir("docs/dir")
						for _, name := range []string{"f.txt", "g.txt"} {
							if _, err := ts.Remove(dir, name, false); err != nil {
								t.Error(err)
							}
						}
						if _, err := ts.Remove(ts.dir("docs"), "dir", true); err != nil {
							t.Error(err)
						}
						if theirs {
							g.Engine.Write("dir/new.txt", "theirs")
							g.Sync()
						}
					})
					return nil
				}
			})
			ts.write(ts.mustLookup("docs/dir/f.txt"), "v2", true)
			time.Sleep(5 * testSettleDelay)
			ts.settled()
			ts.refresh()
			if theirs {
				ts.gatewayHas("dir/new.txt", "theirs")
				ts.gatewayHas("dir/f.txt", "v2")
				if got := ts.readPath("docs/dir/new.txt"); got != "theirs" {
					t.Fatalf("the other device's file reads %q", got)
				}
			} else {
				ts.gatewayLacks("dir")
				if _, ok := ts.lookup("docs/dir"); ok {
					t.Fatal("the directory came back")
				}
			}
			if edits, err := ts.Edits(); err != nil || len(edits) != 0 {
				t.Fatalf("edits left: %+v %v", edits, err)
			}
		})
	}
}
