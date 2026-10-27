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
type FileProviderDelegate interface {
	// The global index of the folder changed. When needsFullPass is set, changes may have happened that cannot be
	// found by looking for higher sequence numbers (e.g. the folder was resumed or its ignore patterns changed).
	OnFolderIndexChanged(folderID string, needsFullPass bool)
	// Folders were added or removed, or their label, type or path changed
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

	mutex         sync.Mutex
	dirty         map[string]bool // folder ID => needs full pass
	configChanged bool
	firstDirtyAt  time.Time
	folderDigests map[string]string
	ignoreHashes  map[string]string
}

// Starts delivering file provider notifications to the delegate. Pass nil to stop.
func (clt *Client) SetFileProviderDelegate(delegate FileProviderDelegate) {
	if existing, ok := fileProviderWatchers.LoadAndDelete(clt); ok {
		existing.(*fileProviderWatcher).stop()
	}
	if delegate == nil {
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
	if clt.config != nil {
		w.folderDigests = folderConfigurationDigests(clt.config.RawCopy())
	}
	fileProviderWatchers.Store(clt, w)
	w.start(clt.ctx, clt.evLogger.Subscribe(fileProviderEventMask))
}

func newFileProviderWatcher(delegate FileProviderDelegate, ignoreHash func(string) string) *fileProviderWatcher {
	return &fileProviderWatcher{
		delegate:      delegate,
		ignoreHash:    ignoreHash,
		trailingDelay: fileProviderTrailingDelay,
		maximumDelay:  fileProviderMaximumDelay,
		dirty:         map[string]bool{},
		folderDigests: nil,
		ignoreHashes:  map[string]string{},
	}
}

func (w *fileProviderWatcher) start(parent context.Context, sub events.Subscription) {
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	w.done = make(chan struct{})
	flushes := make(chan struct{}, 1)

	// Events are read in one goroutine and delivered in another, so a slow delegate never makes us miss events
	go func() {
		defer close(w.done)
		defer sub.Unsubscribe()
		timer := time.NewTimer(time.Hour)
		timer.Stop()

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
		for range flushes {
			w.flush()
		}
	}()
}

func (w *fileProviderWatcher) stop() {
	if w.cancel != nil {
		w.cancel()
		<-w.done
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
		// Changed ignore patterns change which entries are selected or hidden
		hash := w.ignoreHash(folderID)
		w.mutex.Lock()
		previous, known := w.ignoreHashes[folderID]
		w.ignoreHashes[folderID] = hash
		w.mutex.Unlock()
		return w.markDirty(folderID, known && previous != hash, false, now)

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
			if previous[folderID] != digest {
				delay, changed = w.markDirty(folderID, true, true, now)
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
	if len(w.dirty) == 0 && !w.configChanged {
		w.firstDirtyAt = now
	}
	if folderID != "" {
		w.dirty[folderID] = w.dirty[folderID] || fullPass
	}
	w.configChanged = w.configChanged || configChanged

	// Deliver after a quiet period, but never later than the maximum delay after the first change
	delay := min(w.trailingDelay, w.firstDirtyAt.Add(w.maximumDelay).Sub(now))
	return max(delay, 0), true
}

func (w *fileProviderWatcher) flush() {
	w.mutex.Lock()
	dirty := w.dirty
	configChanged := w.configChanged
	w.dirty = map[string]bool{}
	w.configChanged = false
	w.mutex.Unlock()

	if configChanged {
		w.delegate.OnFolderConfigurationChanged()
	}
	folderIDs := KeysOf(dirty)
	slices.Sort(folderIDs)
	for _, folderID := range folderIDs {
		slog.Debug("file provider index changed", "folderID", folderID, "fullPass", dirty[folderID])
		w.delegate.OnFolderIndexChanged(folderID, dirty[folderID])
	}
}

// A digest of the folder configuration fields that matter to a file provider
func folderConfigurationDigests(cfg config.Configuration) map[string]string {
	digests := make(map[string]string, len(cfg.Folders))
	for _, fc := range cfg.Folders {
		devices := make([]string, 0, len(fc.Devices))
		for _, dev := range fc.Devices {
			devices = append(devices, dev.DeviceID.String())
		}
		slices.Sort(devices)
		digests[fc.ID] = fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%v\x00%s", fc.Label, fc.Path, fc.Type.String(),
			fc.FilesystemType.String(), fc.Paused, strings.Join(devices, ","))
	}
	return digests
}
