// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"sync"
)

var selectionLocks sync.Map // folder ID => *sync.Mutex

// Serializes read-modify-write cycles of a folder's ignore file. Selection changes can now originate from the UI and
// from the File Provider work queue at the same time. Returns the function that releases the lock.
func lockSelection(folderID string) func() {
	lock, _ := selectionLocks.LoadOrStore(folderID, &sync.Mutex{})
	mutex := lock.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

// Scans the given paths (relative to the folder root) and waits for the scan to complete
func (fld *Folder) ScanPaths(paths *ListOfStrings) error {
	if fld.client.internals() == nil {
		return ErrStillLoading
	}
	if paths == nil || len(paths.data) == 0 {
		return nil
	}
	return fld.client.scanFolderSubdirs(fld.FolderID, paths.data)
}
