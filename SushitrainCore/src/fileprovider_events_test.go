// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/protocol"
)

type fileProviderNotification struct {
	folderID      string
	fullPass      bool
	configChanged bool
}

type recordingFileProviderDelegate struct {
	notifications chan fileProviderNotification
}

func (d *recordingFileProviderDelegate) OnFolderIndexChanged(folderID string, needsFullPass bool) {
	d.notifications <- fileProviderNotification{folderID: folderID, fullPass: needsFullPass}
}

func (d *recordingFileProviderDelegate) OnFolderConfigurationChanged() {
	d.notifications <- fileProviderNotification{configChanged: true}
}

func (d *recordingFileProviderDelegate) next(t *testing.T) fileProviderNotification {
	t.Helper()
	select {
	case n := <-d.notifications:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notification")
		return fileProviderNotification{}
	}
}

func (d *recordingFileProviderDelegate) expectNone(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case n := <-d.notifications:
		t.Fatalf("unexpected notification %+v", n)
	case <-time.After(within):
	}
}

type watcherTestHarness struct {
	logger   events.Logger
	delegate *recordingFileProviderDelegate
	watcher  *fileProviderWatcher
	hashes   map[string]string
	mutex    sync.Mutex
}

func newWatcherTestHarness(t *testing.T, trailing time.Duration, maximum time.Duration) *watcherTestHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	logger := events.NewLogger()
	go logger.Serve(ctx)

	h := &watcherTestHarness{
		logger:   logger,
		delegate: &recordingFileProviderDelegate{notifications: make(chan fileProviderNotification, 100)},
		hashes:   map[string]string{"a": "one"},
	}
	h.watcher = newFileProviderWatcher(h.delegate, func(folderID string) string {
		h.mutex.Lock()
		defer h.mutex.Unlock()
		return h.hashes[folderID]
	})
	h.watcher.trailingDelay = trailing
	h.watcher.maximumDelay = maximum
	h.watcher.folderDigests = folderConfigurationDigests(testConfiguration("a", "Label A"))
	h.watcher.start(ctx, logger.Subscribe(fileProviderEventMask))
	t.Cleanup(func() {
		h.watcher.stop()
		cancel()
	})
	return h
}

func testConfiguration(folderID string, label string) config.Configuration {
	cfg := config.New(protocol.EmptyDeviceID)
	cfg.Folders = []config.FolderConfiguration{{ID: folderID, Label: label, Path: "/tmp/" + folderID}}
	return cfg
}

func (h *watcherTestHarness) setIgnoreHash(folderID string, hash string) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.hashes[folderID] = hash
}

func scanFinished(folderID string) map[string]any {
	return map[string]any{"folder": folderID, "from": model.FolderScanning.String(), "to": model.FolderIdle.String()}
}

func TestFileProviderWatcherCoalescesIndexEvents(t *testing.T) {
	h := newWatcherTestHarness(t, 30*time.Millisecond, time.Second)
	for range 5 {
		h.logger.Log(events.LocalIndexUpdated, map[string]any{"folder": "a", "filenames": []string{"x"}})
	}
	h.logger.Log(events.RemoteIndexUpdated, map[string]any{"folder": "b", "device": "X"})

	first, second := h.delegate.next(t), h.delegate.next(t)
	if first != (fileProviderNotification{folderID: "a"}) || second != (fileProviderNotification{folderID: "b"}) {
		t.Fatalf("unexpected notifications %+v, %+v", first, second)
	}
	h.delegate.expectNone(t, 100*time.Millisecond)
}

// Logs an index change of another folder and waits for it, to know that earlier events have been handled
func (h *watcherTestHarness) sync(t *testing.T) {
	t.Helper()
	h.logger.Log(events.RemoteIndexUpdated, map[string]any{"folder": "sync", "device": "X"})
	if n := h.delegate.next(t); n != (fileProviderNotification{folderID: "sync"}) {
		t.Fatalf("unexpected notification %+v", n)
	}
}

func TestFileProviderWatcherIgnoreChangesNeedFullPass(t *testing.T) {
	h := newWatcherTestHarness(t, 10*time.Millisecond, time.Second)

	// A scan that leaves the ignore patterns as they were when the watcher started is not interesting by itself:
	// changes to the index have their own events
	h.logger.Log(events.StateChanged, scanFinished("a"))
	h.sync(t)

	// The first change of the ignore patterns already needs a full pass
	h.setIgnoreHash("a", "two")
	h.logger.Log(events.StateChanged, scanFinished("a"))
	if n := h.delegate.next(t); !n.fullPass || n.folderID != "a" {
		t.Fatalf("changed ignores should need a full pass: %+v", n)
	}
	h.logger.Log(events.StateChanged, scanFinished("a"))
	h.sync(t)

	// The patterns of folders added later are recorded on their first scan
	h.setIgnoreHash("new", "one")
	h.logger.Log(events.StateChanged, scanFinished("new"))
	h.sync(t)
	h.setIgnoreHash("new", "two")
	h.logger.Log(events.StateChanged, scanFinished("new"))
	if n := h.delegate.next(t); !n.fullPass || n.folderID != "new" {
		t.Fatalf("changed ignores should need a full pass: %+v", n)
	}

	// Other state transitions are not interesting
	h.logger.Log(events.StateChanged, map[string]any{"folder": "a", "from": "idle", "to": "syncing"})
	h.delegate.expectNone(t, 50*time.Millisecond)
}

func TestFileProviderWatcherConfigurationChanges(t *testing.T) {
	h := newWatcherTestHarness(t, 10*time.Millisecond, time.Second)

	// Saving an unchanged configuration (e.g. when devices are paused) is not interesting
	h.logger.Log(events.ConfigSaved, testConfiguration("a", "Label A"))
	h.delegate.expectNone(t, 50*time.Millisecond)

	// Renaming a folder only renames its root
	h.logger.Log(events.ConfigSaved, testConfiguration("a", "Renamed"))
	if n := h.delegate.next(t); !n.configChanged {
		t.Fatalf("expected configuration change, got %+v", n)
	}
	h.delegate.expectNone(t, 50*time.Millisecond)

	changed := testConfiguration("a", "Renamed")
	changed.Folders[0].Type = config.FolderTypeReceiveOnly
	h.logger.Log(events.ConfigSaved, changed)
	if n := h.delegate.next(t); !n.configChanged {
		t.Fatalf("expected configuration change, got %+v", n)
	}
	if n := h.delegate.next(t); n.folderID != "a" || !n.fullPass {
		t.Fatalf("expected full pass for changed folder, got %+v", n)
	}

	added := testConfiguration("a", "Renamed")
	added.Folders[0].Type = config.FolderTypeReceiveOnly
	added.Folders = append(added.Folders, config.FolderConfiguration{ID: "b", Label: "B", Path: "/tmp/b"})
	h.logger.Log(events.ConfigSaved, added)
	if n := h.delegate.next(t); !n.configChanged {
		t.Fatalf("expected configuration change, got %+v", n)
	}
	if n := h.delegate.next(t); n.folderID != "b" || !n.fullPass {
		t.Fatalf("expected full pass for added folder, got %+v", n)
	}

	h.logger.Log(events.ConfigSaved, changed)
	if n := h.delegate.next(t); !n.configChanged {
		t.Fatalf("expected configuration change for removed folder, got %+v", n)
	}
	h.delegate.expectNone(t, 50*time.Millisecond)

	h.logger.Log(events.FolderResumed, map[string]string{"id": "a", "label": "Renamed"})
	if n := h.delegate.next(t); !n.configChanged {
		t.Fatalf("expected configuration change, got %+v", n)
	}
	if n := h.delegate.next(t); n.folderID != "a" || !n.fullPass {
		t.Fatalf("expected full pass after resume, got %+v", n)
	}
}

func TestFileProviderWatcherMaximumDelay(t *testing.T) {
	h := newWatcherTestHarness(t, 80*time.Millisecond, 150*time.Millisecond)
	start := time.Now()
	stop := time.After(600 * time.Millisecond)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	received := make(chan time.Time, 1)
	go func() {
		h.delegate.next(t)
		received <- time.Now()
	}()

	for {
		select {
		case <-ticker.C:
			h.logger.Log(events.LocalIndexUpdated, map[string]any{"folder": "a"})
			continue
		case at := <-received:
			if at.Sub(start) > 500*time.Millisecond {
				t.Fatalf("notification delayed by continuous events: %v", at.Sub(start))
			}
			return
		case <-stop:
			t.Fatal("no notification while events kept arriving")
		}
	}
}

type blockingFileProviderDelegate struct {
	entered chan struct{}
	release chan struct{}

	mutex    sync.Mutex
	calls    int
	returned int
}

func (d *blockingFileProviderDelegate) OnFolderIndexChanged(folderID string, needsFullPass bool) {
	d.mutex.Lock()
	d.calls++
	d.mutex.Unlock()
	d.entered <- struct{}{}
	<-d.release
	d.mutex.Lock()
	d.returned++
	d.mutex.Unlock()
}

func (d *blockingFileProviderDelegate) OnFolderConfigurationChanged() {}

func TestFileProviderWatcherStopWaitsForDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := events.NewLogger()
	go logger.Serve(ctx)

	delegate := &blockingFileProviderDelegate{entered: make(chan struct{}, 10), release: make(chan struct{})}
	w := newFileProviderWatcher(delegate, func(string) string { return "" })
	w.trailingDelay = time.Millisecond
	w.start(ctx, logger.Subscribe(fileProviderEventMask))

	logger.Log(events.LocalIndexUpdated, map[string]any{"folder": "a"})
	logger.Log(events.LocalIndexUpdated, map[string]any{"folder": "b"})
	select {
	case <-delegate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}

	stopped := make(chan struct{})
	go func() {
		w.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stopped while the delegate was being called")
	case <-time.After(50 * time.Millisecond):
	}
	close(delegate.release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop does not return")
	}

	// Nothing is delivered after stopping, not even changes that were pending
	time.Sleep(20 * time.Millisecond)
	delegate.mutex.Lock()
	defer delegate.mutex.Unlock()
	if delegate.calls != 1 || delegate.returned != 1 {
		t.Fatalf("delegate called %d times after stopping (%d returned)", delegate.calls, delegate.returned)
	}
}
