// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package previewcache

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func lock(f *os.File, exclusive, nonblock bool) error {
	var flags uint32
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if nonblock {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, new(windows.Overlapped))
}

func lockBusy(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }
