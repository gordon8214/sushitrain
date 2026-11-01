// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"cmp"
	"errors"
	"slices"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

var (
	errRevertFolderType   = errors.New("only receive-only and receive-encrypted folders can revert local changes")
	errOverrideFolderType = errors.New("only send-only folders can override changes")
)

// FailedItem is something a folder could not scan or synchronize, and why
type FailedItem struct {
	Path  string
	Error string
}

// FailedItems is a list of what failed, by path
type FailedItems struct {
	items []*FailedItem
}

func (l *FailedItems) Count() int {
	return len(l.items)
}

func (l *FailedItems) ItemAt(index int) *FailedItem {
	return l.items[index]
}

func newFailedItems(items []*FailedItem) *FailedItems {
	slices.SortFunc(items, func(a, b *FailedItem) int { return cmp.Compare(a.Path, b.Path) })
	return &FailedItems{items: items}
}

// FailedItems are what the folder could not scan or synchronize, each path once, with all it failed on. None while it
// doesn't run, as it hasn't tried then.
func (fld *Folder) FailedItems() *FailedItems {
	internals := fld.client.internals()
	if internals == nil {
		return newFailedItems(nil)
	}
	failed, err := internals.FolderErrors(fld.FolderID)
	if err != nil {
		return newFailedItems(nil)
	}
	byPath := make(map[string]*FailedItem, len(failed))
	items := make([]*FailedItem, 0, len(failed))
	for _, item := range failed {
		// A file can fail both a scan and a pull
		if existing, ok := byPath[item.Path]; ok {
			existing.Error += "; " + item.Err
			continue
		}
		byPath[item.Path] = &FailedItem{Path: item.Path, Error: item.Err}
		items = append(items, byPath[item.Path])
	}
	return newFailedItems(items)
}

// FailedItemCount is how many items FailedItems has. This reads and sorts them all, so it isn't for the main thread.
func (fld *Folder) FailedItemCount() int {
	return fld.FailedItems().Count()
}

// LocalChangedItems counts what was added or changed here in a receive-only or receive-encrypted folder
func (fld *Folder) LocalChangedItems() int {
	internals := fld.client.internals()
	if internals == nil {
		return 0
	}
	counts, err := internals.ReceiveOnlySize(fld.FolderID)
	if err != nil {
		return 0
	}
	// A receive-encrypted folder keeps what it deleted as records, which aren't changes anymore: reverting does nothing to
	// them. In a receive-only folder, a deletion is a change like any other.
	if folder := fld.folderConfiguration(); folder != nil && folder.Type == config.FolderTypeReceiveEncrypted {
		return counts.TotalItems() - counts.Deleted
	}
	return counts.TotalItems()
}

// OutOfSyncItems counts what differs from what other devices have, which is what a send-only folder can override
func (fld *Folder) OutOfSyncItems() int {
	internals := fld.client.internals()
	if internals == nil {
		return 0
	}
	counts, err := internals.NeedSize(fld.FolderID, protocol.LocalDeviceID)
	if err != nil {
		return 0
	}
	return counts.TotalItems()
}

// Revert removes what changed locally in a receive-only folder and fetches the rest again, or removes what doesn't belong
// in a receive-encrypted folder. Returns when it is done, which can take as long as a scan the folder is busy with.
func (fld *Folder) Revert() error {
	folder := fld.folderConfiguration()
	if folder == nil {
		return errFolderConfigNotFound
	}
	if folder.Type != config.FolderTypeReceiveOnly && folder.Type != config.FolderTypeReceiveEncrypted {
		return errRevertFolderType
	}
	internals := fld.client.internals()
	if internals == nil {
		return ErrStillLoading
	}
	return internals.Revert(fld.FolderID)
}

// Override makes what is in a send-only folder what other devices have. Returns when it is done, which can take as long as
// a scan the folder is busy with.
func (fld *Folder) Override() error {
	folder := fld.folderConfiguration()
	if folder == nil {
		return errFolderConfigNotFound
	}
	if folder.Type != config.FolderTypeSendOnly {
		return errOverrideFolderType
	}
	internals := fld.client.internals()
	if internals == nil {
		return ErrStillLoading
	}
	return internals.Override(fld.FolderID)
}
