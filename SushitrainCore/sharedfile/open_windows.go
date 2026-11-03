// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package sharedfile

import (
	"golang.org/x/sys/windows"
	"os"
)

func Open(name string) (*os.File, error) { return OpenFile(name, os.O_RDONLY, 0) }
func OpenFile(name string, flag int, mode os.FileMode) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ)
	switch flag & (os.O_WRONLY | os.O_RDWR) {
	case os.O_WRONLY:
		access = windows.GENERIC_WRITE
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	}
	creation := uint32(windows.OPEN_EXISTING)
	if flag&os.O_CREATE != 0 {
		creation = windows.OPEN_ALWAYS
		if flag&os.O_EXCL != 0 {
			creation = windows.CREATE_NEW
		} else if flag&os.O_TRUNC != 0 {
			creation = windows.CREATE_ALWAYS
		}
	} else if flag&os.O_TRUNC != 0 {
		creation = windows.TRUNCATE_EXISTING
	}
	if flag&os.O_APPEND != 0 {
		access &^= windows.FILE_WRITE_DATA
		access |= windows.FILE_APPEND_DATA
	}
	h, err := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, creation, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(h), name), nil
}
