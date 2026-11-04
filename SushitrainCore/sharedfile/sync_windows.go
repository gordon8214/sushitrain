// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package sharedfile

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

// SyncDirectory flushes the directory held by the caller. Windows cannot reopen
// an os.Root traversal handle with ReOpenFile, so resolve its current name, open
// a writable handle, and verify the file identity before flushing. A renamed or
// replaced path never causes us to flush a different directory.
func SyncDirectory(directory *os.File) error {
	var name []uint16
	for size := uint32(512); ; {
		name = make([]uint16, size)
		n, err := windows.GetFinalPathNameByHandle(windows.Handle(directory.Fd()), &name[0], size, 0)
		if err != nil {
			return err
		}
		if n < size {
			name = name[:n+1]
			break
		}
		size = n + 1
	}
	handle, err := windows.CreateFile(&name[0], windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return &os.PathError{Op: "open directory for flush", Path: directory.Name(), Err: err}
	}
	writable := os.NewFile(uintptr(handle), directory.Name())
	defer writable.Close()
	original, err := directory.Stat()
	if err != nil {
		return err
	}
	actual, err := writable.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(original, actual) {
		return errors.New("directory identity changed before flush")
	}
	return writable.Sync()
}
