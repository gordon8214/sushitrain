// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
)

type testService struct {
	*Service
	t       *testing.T
	g       *gatewaytest.Gateway
	cfg     Config
	folder  Folder
	mutex   sync.Mutex
	changes []Changes
}

// newTestService pairs a new service with a gateway of one exported folder, and adds the folder
func newTestService(t *testing.T, g *gatewaytest.Gateway, budget int64) *testService {
	return newTestServiceWith(t, g, func(c *Config) { c.CacheBudget = budget })
}

func newTestServiceWith(t *testing.T, g *gatewaytest.Gateway, modify func(*Config)) *testService {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{StateDir: filepath.Join(dir, "state"), CacheDir: filepath.Join(dir, "cache"),
		Credentials: &FileCredentials{Path: filepath.Join(dir, "credentials.json")}, ClientName: "test",
		AllowLocalGateway: true, SettleDelay: testSettleDelay,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))}
	if modify != nil {
		modify(&cfg)
	}
	ts := &testService{t: t, g: g, cfg: cfg}
	ts.open()
	t.Cleanup(func() { ts.Close() })
	s := ts.Service
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	done := make(chan error, 1)
	var gw Gateway
	go func() {
		var err error
		gw, err = s.Pair(ctx, g.PairingLink(), nil)
		done <- err
	}()
	g.ApproveNext(map[string]string{g.Engine.FolderID: "write"})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var err error
	if ts.folder, err = s.AddFolder(ctx, gw.ServiceID, g.Engine.FolderID); err != nil {
		t.Fatal(err)
	}
	ts.refresh()
	return ts
}

// open opens the service (again) from the test's directories, and starts it
func (ts *testService) open() {
	ts.t.Helper()
	s, err := Open(ts.cfg)
	if err != nil {
		ts.t.Fatal(err)
	}
	ts.Service = s
	s.SetInvalidator(func(c Changes) {
		ts.mutex.Lock()
		defer ts.mutex.Unlock()
		ts.changes = append(ts.changes, c)
	})
	if err := s.Start(); err != nil {
		ts.t.Fatal(err)
	}
}

// restart stops the service as a crash would leave it (nothing more is recorded) and opens it again
func (ts *testService) restart() {
	ts.t.Helper()
	if err := ts.Service.Close(); err != nil {
		ts.t.Fatal(err)
	}
	ts.open()
}

// refresh brings the folder up to date with the gateway, after the gateway caught up with its disk
func (ts *testService) refresh() {
	ts.t.Helper()
	ts.g.Sync()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := ts.Refresh(ctx, ts.folder.ID); err != nil {
		ts.t.Fatal(err)
	}
}

// lookup returns the entry at a slash-separated path below the mount's root
func (ts *testService) lookup(path string) (Entry, bool) {
	ts.t.Helper()
	var e Entry
	var parent int64
	for _, name := range strings.Split(path, "/") {
		var ok bool
		var err error
		if e, ok, err = ts.Lookup(parent, name); err != nil {
			ts.t.Fatal(err)
		} else if !ok {
			return Entry{}, false
		}
		parent = e.Item
	}
	return e, true
}

func (ts *testService) mustLookup(path string) Entry {
	ts.t.Helper()
	e, ok := ts.lookup(path)
	if !ok {
		ts.t.Fatalf("no %s", path)
	}
	return e
}

func (ts *testService) read(e Entry) []byte {
	ts.t.Helper()
	r, err := ts.OpenFile(e.Item)
	if err != nil {
		ts.t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(io.NewSectionReader(readerAt{r}, 0, r.Size()))
	if err != nil {
		ts.t.Fatal(err)
	}
	return data
}

type readerAt struct{ r *Reader }

func (r readerAt) ReadAt(p []byte, off int64) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return r.r.ReadAt(ctx, p, off)
}

func (ts *testService) waitState(e Entry, want string) {
	ts.t.Helper()
	gatewaytest.WaitFor(ts.t, 30*time.Second, e.Name+" to be "+want, func() bool {
		state, err := ts.State(e)
		return err == nil && state == want
	})
}

func (ts *testService) sawEntry(name string) bool {
	ts.mutex.Lock()
	defer ts.mutex.Unlock()
	for _, c := range ts.changes {
		for _, e := range c.Entries {
			if e.Name == name {
				return true
			}
		}
	}
	return false
}

func TestBrowseAndFollow(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("readme.txt", "hello")
	e.Write("sub/deeper/a.txt", "a")
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(true)
	ts := newTestService(t, g, 0)

	root := ts.mustLookup("docs")
	if root.Kind != kindRoot || !root.Writable {
		t.Fatalf("folder root %+v", root)
	}
	readme := ts.mustLookup("docs/readme.txt")
	if readme.Kind != kindFile || readme.Size != 5 || readme.ContentID == "" {
		t.Fatalf("readme %+v", readme)
	}
	if got := ts.read(readme); string(got) != "hello" {
		t.Fatalf("read %q", got)
	}
	children, err := ts.Children(root.Item)
	if err != nil || len(children) != 2 || children[0].Name != "readme.txt" || children[1].Name != "sub" {
		t.Fatalf("children %+v, %v", children, err)
	}
	folders, err := ts.Children(0)
	if err != nil || len(folders) != 1 || folders[0].Name != "docs" {
		t.Fatalf("mount root %+v, %v", folders, err)
	}
	// Syncthing's own names aren't shown
	if _, ok := ts.lookup("docs/.stfolder"); ok {
		t.Fatal(".stfolder is shown")
	}

	// A new file and a removed one, from the change feed
	e.Write("sub/new.txt", "new")
	e.Remove("sub/deeper")
	ts.refresh()
	if _, ok := ts.lookup("docs/sub/new.txt"); !ok {
		t.Fatal("the new file isn't shown")
	}
	if _, ok := ts.lookup("docs/sub/deeper"); ok {
		t.Fatal("the removed directory is still shown")
	}
	if !ts.sawEntry("new.txt") || !ts.sawEntry("deeper") {
		t.Fatal("the mount wasn't told about the changes")
	}

	// A changed file keeps its item, and gets a new node
	e.Write("readme.txt", "hello again")
	ts.refresh()
	changed := ts.mustLookup("docs/readme.txt")
	if changed.Item != readme.Item || changed.Node == readme.Node || changed.ContentID == readme.ContentID {
		t.Fatalf("after a change %+v, before %+v", changed, readme)
	}
	if got := ts.read(changed); string(got) != "hello again" {
		t.Fatalf("read %q", got)
	}

	// A cursor the gateway can't serve: a fresh listing, which keeps items and nodes
	if err := ts.store.tx(context.Background(), func(tx *sqlTx) error {
		_, err := tx.Exec(`UPDATE cursors SET cursor = 'g999999.0'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ts.refresh()
	again := ts.mustLookup("docs/readme.txt")
	if again.Item != changed.Item || again.Node != changed.Node {
		t.Fatalf("after a fresh listing %+v, before %+v", again, changed)
	}

	// Removing the folder takes it out of the mount
	if _, _, err := ts.RemoveFolder(ts.folder.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := ts.lookup("docs"); ok {
		t.Fatal("the removed folder is still in the mount")
	}
	gateways, err := ts.Gateways()
	if err != nil || len(gateways) != 1 {
		t.Fatalf("gateways %+v, %v", gateways, err)
	}
	if err := ts.Forget(gateways[0].ServiceID); err != nil {
		t.Fatal(err)
	}
}

func TestReadsOnDemand(t *testing.T) {
	contents := make([]byte, 3<<20)
	_, _ = rand.Read(contents)
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("video.bin", string(contents))
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(false)
	ts := newTestService(t, g, 0)
	entry := ts.mustLookup("docs/video.bin")
	if state, _ := ts.State(entry); state != "remote" {
		t.Fatalf("state before reading: %s", state)
	}

	r, err := ts.OpenFile(entry.Item)
	if err != nil {
		t.Fatal(err)
	}
	// A read near the end first: it waits only for its blocks
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	buf := make([]byte, 1000)
	off := int64(len(contents) - 5000)
	if n, err := r.ReadAt(ctx, buf, off); err != nil || n != len(buf) || !bytes.Equal(buf, contents[off:off+1000]) {
		t.Fatalf("read at %d: %d, %v", off, n, err)
	}
	// Past the end
	if n, err := r.ReadAt(ctx, buf, int64(len(contents))); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read past the end: %d, %v", n, err)
	}
	all := make([]byte, len(contents))
	if _, err := r.ReadAt(ctx, all, 0); err != nil || !bytes.Equal(all, contents) {
		t.Fatalf("the whole file: %v", err)
	}
	r.Close()
	gatewaytest.WaitFor(t, 30*time.Second, "the download to finish", func() bool {
		state, _ := ts.State(entry)
		return state == "local"
	})
	// Opened again, it is local
	r, err = ts.OpenFile(entry.Item)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Local() {
		t.Fatal("cached content was fetched again")
	}
	r.Close()

	// An open reader keeps the content it opened while the file changes on the gateway
	old, err := ts.OpenFile(entry.Item)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	e.Write("video.bin", "short now")
	ts.refresh()
	if _, err := old.ReadAt(ctx, all, 0); err != nil || !bytes.Equal(all, contents) {
		t.Fatalf("the old reader after a change: %v", err)
	}
	if got := ts.read(ts.mustLookup("docs/video.bin")); string(got) != "short now" {
		t.Fatalf("the new version reads %q", got)
	}
}

func TestKeepAndEvict(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", nil)
	for _, name := range []string{"a.bin", "b.bin", "kept/c.bin"} {
		data := make([]byte, 600<<10)
		_, _ = rand.Read(data)
		e.Write(name, string(data))
	}
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(false)
	// Room for one file
	ts := newTestService(t, g, 700<<10)
	a, b := ts.mustLookup("docs/a.bin"), ts.mustLookup("docs/b.bin")
	ts.read(a)
	ts.waitState(a, "local")
	// Used later, so a is the one to go
	time.Sleep(1100 * time.Millisecond)
	ts.read(b)
	ts.waitState(b, "local")
	if err := ts.cache.evict(); err != nil {
		t.Fatal(err)
	}
	if state, _ := ts.State(a); state != "remote" {
		t.Fatalf("the least recently used file is %s", state)
	}
	if state, _ := ts.State(b); state != "local" {
		t.Fatalf("the most recently used file is %s", state)
	}

	// Keeping a directory downloads what is in it, and keeps it beyond the budget
	kept := ts.mustLookup("docs/kept")
	if err := ts.SetKeep(kept.Item, true); err != nil {
		t.Fatal(err)
	}
	c := ts.mustLookup("docs/kept/c.bin")
	if !c.Keep {
		t.Fatal("a file in a kept directory isn't kept")
	}
	gatewaytest.WaitFor(t, 30*time.Second, "the kept file to download", func() bool {
		state, _ := ts.State(c)
		return state == "kept"
	})
	ts.read(a)
	ts.waitState(a, "local")
	if err := ts.cache.evict(); err != nil {
		t.Fatal(err)
	}
	if state, _ := ts.State(c); state != "kept" {
		t.Fatalf("the kept file is %s after trimming", state)
	}
	// Stop keeping it: its content goes
	if err := ts.SetKeep(kept.Item, false); err != nil {
		t.Fatal(err)
	}
	if state, _ := ts.State(ts.mustLookup("docs/kept/c.bin")); state != "remote" {
		t.Fatalf("a file no longer kept is %s", state)
	}
}

func TestLocalGatewayIsRefused(t *testing.T) {
	e := gatewaytest.NewEngine(t, "docs", nil)
	g := gatewaytest.NewGateway(t, e, "")
	dir := t.TempDir()
	s, err := Open(Config{StateDir: dir, CacheDir: dir, Credentials: &FileCredentials{Path: filepath.Join(dir, "c")}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Pair(context.Background(), g.PairingLink(), nil); !errors.Is(err, ErrLocalGateway) {
		t.Fatalf("pairing with this machine: %v", err)
	}
}

// A partial download whose file was removed (the cache was emptied) starts over, rather than trusting the record of
// its verified blocks
func TestEmptiedTransfersStartOver(t *testing.T) {
	contents := make([]byte, 3<<20)
	_, _ = rand.Read(contents)
	e := gatewaytest.NewEngine(t, "docs", nil)
	e.Write("big.bin", string(contents))
	g := gatewaytest.NewGateway(t, e, "")
	g.Export(false)
	ts := newTestService(t, g, 0)
	entry := ts.mustLookup("docs/big.bin")
	r, err := ts.OpenFile(entry.Item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadAt(context.Background(), make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	r.Close()
	ts.restart()
	parts, _ := filepath.Glob(filepath.Join(ts.cfg.CacheDir, "Transfers", "*.part"))
	if len(parts) == 0 {
		t.Fatal("no partial download to remove")
	}
	for _, p := range parts {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if got := ts.read(ts.mustLookup("docs/big.bin")); !bytes.Equal(got, contents) {
		t.Fatal("read other bytes than the gateway's after the partial file was removed")
	}
}
