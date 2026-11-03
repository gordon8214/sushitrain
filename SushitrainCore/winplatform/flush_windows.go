// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package winplatform

import "golang.org/x/sys/windows"

// FlushDirectory commits directory-entry changes after replacing a journal.
func FlushDirectory(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	file, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(file)
	return windows.FlushFileBuffers(file)
}
