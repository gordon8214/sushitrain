// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/model"
)

// Receives (debounced) notifications about changes relevant to a file provider. Unlike ClientDelegate, this delegate
// is also called while Client.IgnoreEvents is set. Implementations should return quickly and do their work elsewhere.
// As SetFileProviderDelegate waits for a delivery in progress to finish, they must neither call it nor wait for a thread
// that does.
type FileProviderDelegate interface {
	// The global index of the folder changed. When needsFullPass is set, changes may have happened that cannot be
	// found by looking for higher sequence numbers (e.g. the folder was resumed or its ignore patterns changed).
	OnFolderIndexChanged(folderID string, needsFullPass bool)
	// Folders were added or removed, or their label, type or path changed. A change of only the label is not
	// followed by OnFolderIndexChanged.
	OnFolderConfigurationChanged()
}

const (
	fileProviderEventMask = events.LocalIndexUpdated | events.RemoteIndexUpdated | events.StateChanged |
		events.ConfigSaved | events.FolderPaused | events.FolderResumed
	fileProviderTrailingDelay = 2 * time.Second
	fileProviderMaximumDelay  = 10 * time.Second
)

var fileProviderWatchers sync.Map // *Client => *fileProviderWatcher

type fileProviderWatcher struct {
	delegate      FileProviderDelegate
	ignoreHash    func(folderID string) string
	trailingDelay time.Duration
	maximumDelay  time.Duration
	cancel        context.CancelFunc
	done          chan struct{}
	flushDone     chan struct{}

	// Only used by the goroutine that delivers notifications, as computing hashes reads and parses ignore files
	ignoreHashes map[string]string

	mutex         sync.Mutex
	dirty         map[string]bool // folder ID => needs full pass
	checkIgnores  map[string]bool // folder IDs whose ignore patterns may have changed
	configChanged bool
	firstDirtyAt  time.Time
	folderDigests map[string]folderDigest
}

// The folder configuration fields that matter to a file provider
type folderDigest struct {
	label string
	// Everything else, which may change what the folder contains
	contents string
}

// Starts delivering file provider notifications to the delegate. Pass nil to stop. The notifications continue after the
// engine restarts. The delegate must not wait for the engine to stop, restart or change its configuration, as the engine
// waits for a notification in progress before it stops.
func (clt *Client) SetFileProviderDelegate(delegate FileProviderDelegate) {
	clt.watcherMutex.Lock()
	defer clt.watcherMutex.Unlock()
	clt.fileProviderDelegate = delegate
	run := clt.watchedRun
	if run == nil {
		// Before the engine starts, or after it stopped: the configuration's run, which a later start follows
		run = clt.currentRun()
	}
	clt.attachFileProviderWatcherLocked(run)
}

// followWithFileProviderWatcher points the file provider watcher at a run that starts, or at none when a run stops
func (clt *Client) followWithFileProviderWatcher(r *engineRun) {
	clt.watcherMutex.Lock()
	defer clt.watcherMutex.Unlock()
	clt.watchedRun = r
	clt.attachFileProviderWatcherLocked(r)
}

// attachFileProviderWatcherLocked replaces the watcher with one that watches r for the delegate, if there is a delegate
// and a run. After a restart it reports every folder as changed once. The caller holds watcherMutex.
func (clt *Client) attachFileProviderWatcherLocked(r *engineRun) {
	// The old watcher watches another run (or this one, for an older delegate); stopping it waits for a delivery in
	// progress, so it happens before the run's lock is taken
	if existing, ok := fileProviderWatchers.LoadAndDelete(clt); ok {
		existing.(*fileProviderWatcher).stop()
	}
	delegate := clt.fileProviderDelegate
	if delegate == nil || r == nil {
		return
	}
	// A run being torn down has, or will soon have, a stopped event logger, whose subscriptions never deliver
	r.writes.RLock()
	defer r.writes.RUnlock()
	if r.closed {
		return
	}

	w := newFileProviderWatcher(delegate, func(folderID string) string {
		folder := clt.FolderWithID(folderID)
		if folder == nil {
			return ""
		}
		matcher, err := folder.loadIgnores()
		if err != nil {
			return ""
		}
		return matcher.Hash()
	})
	// The watcher also records the ignore patterns of these folders when it starts
	w.folderDigests = folderConfigurationDigests(r.config.RawCopy())
	if !r.firstRun {
		// Changes around a restart may not have been told: report every folder once
		for folderID := range w.folderDigests {
			w.dirty[folderID] = true
		}
		w.configChanged = true
	}
	w.start(r.ctx, r.evLogger.Subscribe(fileProviderEventMask))
	fileProviderWatchers.Store(clt, w)
}

func newFileProviderWatcher(delegate FileProviderDelegate, ignoreHash func(string) string) *fileProviderWatcher {
	return &fileProviderWatcher{
		delegate:      delegate,
		ignoreHash:    ignoreHash,
		trailingDelay: fileProviderTrailingDelay,
		maximumDelay:  fileProviderMaximumDelay,
		dirty:         map[string]bool{},
		checkIgnores:  map[string]bool{},
		folderDigests: nil,
		ignoreHashes:  map[string]string{},
	}
}

func (w *fileProviderWatcher) start(parent context.Context, sub events.Subscription) {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	w.done = make(chan struct{})
	w.flushDone = make(chan struct{})
	flushes := make(chan struct{}, 1)

	w.mutex.Lock()
	seedFolderIDs := KeysOf(w.folderDigests)
	pending := len(w.dirty) > 0 || w.configChanged
	if pending {
		w.firstDirtyAt = time.Now()
	}
	w.mutex.Unlock()

	// Events are read in one goroutine and delivered in another, so a slow delegate never makes us miss events
	go func() {
		defer close(w.done)
		defer sub.Unsubscribe()
		timer := time.NewTimer(time.Hour)
		timer.Stop()
		if pending {
			timer.Reset(w.trailingDelay)
		}

		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				close(flushes)
				return

			case evt, ok := <-sub.C():
				if !ok {
					close(flushes)
					return
				}
				if delay, changed := w.handle(evt, time.Now()); changed {
					timer.Reset(delay)
				}

			case <-timer.C:
				select {
				case flushes <- struct{}{}:
				default:
					// A flush is pending already and will pick up these changes
				}
			}
		}
	}()

	go func() {
		defer close(w.flushDone)
		// The first change of the ignore patterns needs a full pass too, so record the hashes they have now
		for _, folderID := range seedFolderIDs {
			if ctx.Err() != nil {
				break
			}
			w.ignoreHashes[folderID] = w.ignoreHash(folderID)
		}
		for range flushes {
			w.flush(ctx)
		}
	}()
}

// Stops the watcher. When this returns, the delegate is not called anymore.
func (w *fileProviderWatcher) stop() {
	if w.cancel != nil {
		w.cancel()
		<-w.done
		<-w.flushDone
	}
}

// Records an event and returns the delay after which pending changes should be delivered
func (w *fileProviderWatcher) handle(evt events.Event, now time.Time) (time.Duration, bool) {
	switch evt.Type {
	case events.LocalIndexUpdated, events.RemoteIndexUpdated:
		data, ok := evt.Data.(map[string]any)
		if !ok {
			return 0, false
		}
		folderID, _ := data["folder"].(string)
		return w.markDirty(folderID, false, false, now)

	case events.StateChanged:
		data, ok := evt.Data.(map[string]any)
		if !ok {
			return 0, false
		}
		folderID, _ := data["folder"].(string)
		from, _ := data["from"].(string)
		if from != model.FolderScanning.String() || folderID == "" {
			return 0, false
		}
		// Changed ignore patterns change which entries are selected or hidden. Changes to the index itself are
		// reported by index events. Whether the patterns changed is checked when changes are delivered.
		w.mutex.Lock()
		defer w.mutex.Unlock()
		w.checkIgnores[folderID] = true
		return w.scheduleLocked(now), true

	case events.FolderPaused, events.FolderResumed:
		data, ok := evt.Data.(map[string]string)
		if !ok {
			return 0, false
		}
		return w.markDirty(data["id"], true, true, now)

	case events.ConfigSaved:
		cfg, ok := evt.Data.(config.Configuration)
		if !ok {
			return 0, false
		}
		digests := folderConfigurationDigests(cfg)
		w.mutex.Lock()
		previous := w.folderDigests
		w.folderDigests = digests
		w.mutex.Unlock()
		if previous == nil {
			return 0, false
		}

		var delay time.Duration
		var changed bool
		for folderID, digest := range digests {
			before, existed := previous[folderID]
			if !existed || before.contents != digest.contents {
				delay, changed = w.markDirty(folderID, true, true, now)
			} else if before.label != digest.label {
				// Renaming a folder only renames its root
				delay, changed = w.markDirty("", false, true, now)
			}
		}
		for folderID := range previous {
			if _, ok := digests[folderID]; !ok {
				delay, changed = w.markDirty("", false, true, now)
			}
		}
		return delay, changed
	}
	return 0, false
}

func (w *fileProviderWatcher) markDirty(folderID string, fullPass bool, configChanged bool, now time.Time) (time.Duration, bool) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	if folderID == "" && !configChanged {
		return 0, false
	}
	delay := w.scheduleLocked(now)
	if folderID != "" {
		w.dirty[folderID] = w.dirty[folderID] || fullPass
	}
	w.configChanged = w.configChanged || configChanged
	return delay, true
}

// Returns the delay after which pending changes should be delivered, when a change is about to be recorded
func (w *fileProviderWatcher) scheduleLocked(now time.Time) time.Duration {
	if len(w.dirty) == 0 && len(w.checkIgnores) == 0 && !w.configChanged {
		w.firstDirtyAt = now
	}

	// Deliver after a quiet period, but never later than the maximum delay after the first change
	delay := min(w.trailingDelay, w.firstDirtyAt.Add(w.maximumDelay).Sub(now))
	return max(delay, 0)
}

func (w *fileProviderWatcher) flush(ctx context.Context) {
	w.mutex.Lock()
	dirty := w.dirty
	checkIgnores := w.checkIgnores
	configChanged := w.configChanged
	w.dirty = map[string]bool{}
	w.checkIgnores = map[string]bool{}
	w.configChanged = false
	w.mutex.Unlock()

	for folderID := range checkIgnores {
		if ctx.Err() != nil {
			return
		}
		hash := w.ignoreHash(folderID)
		previous, known := w.ignoreHashes[folderID]
		w.ignoreHashes[folderID] = hash
		if known && previous != hash {
			dirty[folderID] = true
		}
	}

	if configChanged && ctx.Err() == nil {
		w.delegate.OnFolderConfigurationChanged()
	}
	folderIDs := KeysOf(dirty)
	slices.Sort(folderIDs)
	for _, folderID := range folderIDs {
		if ctx.Err() != nil {
			return
		}
		slog.Debug("file provider index changed", "folderID", folderID, "fullPass", dirty[folderID])
		w.delegate.OnFolderIndexChanged(folderID, dirty[folderID])
	}
}

// Digests of the folder configuration fields that matter to a file provider
func folderConfigurationDigests(cfg config.Configuration) map[string]folderDigest {
	digests := make(map[string]folderDigest, len(cfg.Folders))
	for _, fc := range cfg.Folders {
		devices := make([]string, 0, len(fc.Devices))
		for _, dev := range fc.Devices {
			devices = append(devices, dev.DeviceID.String())
		}
		slices.Sort(devices)
		digests[fc.ID] = folderDigest{
			label: fc.Label,
			contents: fmt.Sprintf("%s\x00%s\x00%s\x00%v\x00%s", fc.Path, fc.Type.String(), fc.FilesystemType.String(),
				fc.Paused, strings.Join(devices, ",")),
		}
	}
	return digests
}
