// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package winplatform

import (
	"golang.org/x/sys/windows"
	"os"
)

var reopenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

// SnapshotReader takes a read-only handle that prevents concurrent writes and deletion for its lifetime.
// Reopening by handle preserves the object selected by the caller's rooted open.
func SnapshotReader(source *os.File) (*os.File, error) {
	h, _, err := reopenFile.Call(source.Fd(), windows.GENERIC_READ, windows.FILE_SHARE_READ, windows.FILE_FLAG_SEQUENTIAL_SCAN)
	if windows.Handle(h) == windows.InvalidHandle {
		return nil, err
	}
	return os.NewFile(h, source.Name()), nil
}
