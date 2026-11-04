// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package gateway

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"t-shaped.nl/sushitrain/v2/winplatform"
)

// Each directory stays open without FILE_SHARE_DELETE, so its path cannot be redirected while an operation uses it.
// Child opens and renames are relative to those handles and never traverse reparse points.
type dir struct {
	handle windows.Handle
	path   string
}
type fileIdentity struct{ dev, ino uint64 }
type entryStat struct {
	exists  bool
	kind    uint32
	size    int64
	modTime time.Time
	id      fileIdentity
}

const (
	fsKindDirectory = 1
	fsKindRegular   = 2
)

var (
	errSymlink             = errors.New("path goes through a reparse point")
	errExchangeUnsupported = errors.New("the filesystem cannot swap entries")
	errNotDirectory        = windows.ERROR_DIRECTORY
	errCrossDevice         = windows.ERROR_NOT_SAME_DEVICE
)

func ntError(err error) error {
	if status, ok := err.(windows.NTStatus); ok {
		return status.Errno()
	}
	return err
}

func openAt(parent windows.Handle, name string, access, disposition, options uint32) (windows.Handle, error) {
	if !winplatform.ValidComponent(name) {
		return 0, os.ErrInvalid
	}
	u, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: parent, ObjectName: u, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	var h windows.Handle
	var io windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE|windows.FILE_READ_ATTRIBUTES, &oa, &io, nil, windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, disposition, options|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		return 0, &os.PathError{Op: "open relative", Path: name, Err: ntError(err)}
	}
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		if err != nil {
			return 0, err
		}
		return 0, errSymlink
	}
	return h, nil
}

func openRootReadOnly(path string) (*dir, error) { return openRootAccess(path, false) }
func openRoot(path string) (*dir, error)         { return openRootAccess(path, true) }
func openRootAccess(path string, write bool) (*dir, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ)
	if write {
		access |= windows.GENERIC_WRITE
	}
	h, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		if err != nil {
			return nil, err
		}
		return nil, errSymlink
	}
	physical := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(h, &physical[0], uint32(len(physical)), 1)
	if err != nil || n >= uint32(len(physical)) {
		windows.CloseHandle(h)
		if err != nil {
			return nil, err
		}
		return nil, os.ErrInvalid
	}
	return &dir{h, windows.UTF16ToString(physical[:n])}, nil
}
func (d *dir) openDir(rel string) (*dir, error)         { return d.openDirAccess(rel, true) }
func (d *dir) openDirReadOnly(rel string) (*dir, error) { return d.openDirAccess(rel, false) }
func (d *dir) openDirAccess(rel string, write bool) (*dir, error) {
	access := uint32(windows.FILE_GENERIC_READ)
	if write {
		access |= windows.FILE_GENERIC_WRITE
	}
	var h windows.Handle
	err := windows.DuplicateHandle(windows.CurrentProcess(), d.handle, windows.CurrentProcess(), &h, 0, false, windows.DUPLICATE_SAME_ACCESS)
	if err != nil {
		return nil, err
	}
	current := &dir{h, d.path}
	if rel == "" {
		return current, nil
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "." || part == ".." {
			current.close()
			return nil, os.ErrInvalid
		}
		h, err = openAt(current.handle, part, access, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE)
		path := filepath.Join(current.path, part)
		current.close()
		if err != nil {
			return nil, err
		}
		current = &dir{h, path}
	}
	return current, nil
}
func (d *dir) close() {
	if d != nil && d.handle != 0 {
		windows.CloseHandle(d.handle)
		d.handle = 0
	}
}
func (d *dir) lstat(name string) (entryStat, error) {
	h, err := openAt(d.handle, name, windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return entryStat{}, nil
	}
	if err != nil {
		return entryStat{}, err
	}
	defer windows.CloseHandle(h)
	var st windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &st); err != nil {
		return entryStat{}, err
	}
	kind := uint32(fsKindRegular)
	if st.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		kind = fsKindDirectory
	}
	return entryStat{true, kind, int64(uint64(st.FileSizeHigh)<<32 | uint64(st.FileSizeLow)), time.Unix(0, st.LastWriteTime.Nanoseconds()), fileIdentity{uint64(st.VolumeSerialNumber), uint64(st.FileIndexHigh)<<32 | uint64(st.FileIndexLow)}}, nil
}
func (d *dir) createExclusive(name string, mode uint32) (*os.File, error) {
	h, err := openAt(d.handle, name, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_WRITE_THROUGH)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), filepath.Join(d.path, name)), nil
}
func (d *dir) openFile(name string) (*os.File, error) {
	h, err := openAt(d.handle, name, windows.FILE_GENERIC_READ, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), filepath.Join(d.path, name)), nil
}

// Preserve displaced files when replacing them would lose named streams or NTFS
// extended attributes. Query the protected handle, not a pathname that can change.
func (d *dir) hasXattrs(name string) bool {
	h, err := openAt(d.handle, name, windows.FILE_READ_EA, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h)
	var iosb windows.IO_STATUS_BLOCK
	var eaSize uint32
	const fileEaInformation = 7
	if err := windows.NtQueryInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&eaSize)), 4, fileEaInformation); err != nil || eaSize != 0 {
		return true
	}
	// Eight-byte alignment is required by FILE_STREAM_INFO. An insufficient or
	// malformed enumeration is treated conservatively as metadata to preserve.
	buffer := make([]uint64, 8192)
	data := unsafe.Slice((*byte)(unsafe.Pointer(&buffer[0])), len(buffer)*8)
	if err := windows.GetFileInformationByHandleEx(h, windows.FileStreamInfo, &data[0], uint32(len(data))); err != nil {
		return true
	}
	for offset := 0; ; {
		if offset > len(data)-24 {
			return true
		}
		next := int(binary.LittleEndian.Uint32(data[offset:]))
		size := int(binary.LittleEndian.Uint32(data[offset+4:]))
		if size == 0 || size%2 != 0 || size > len(data)-offset-24 {
			return true
		}
		name := windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&data[offset+24])), size/2))
		if name != "::$DATA" {
			return true
		}
		if next == 0 {
			return false
		}
		if next < 24+size || next%8 != 0 || next > len(data)-offset {
			return true
		}
		offset += next
	}
}
func (d *dir) mkdir(name string, mode uint32) error {
	h, err := openAt(d.handle, name, windows.FILE_GENERIC_WRITE, windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE)
	if err == nil {
		windows.CloseHandle(h)
	}
	return err
}
func (d *dir) remove(name string) error {
	h, err := openAt(d.handle, name, windows.DELETE, windows.FILE_OPEN, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	value := byte(1)
	return windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, &value, 1)
}
func (d *dir) chmod(name string, mode uint32) error {
	h, err := openAt(d.handle, name, windows.FILE_WRITE_ATTRIBUTES, windows.FILE_OPEN, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var old windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &old); err != nil {
		return err
	}
	type basicInfo struct {
		Created, Accessed, Written, Changed int64
		Attributes                          uint32
	}
	attrs := old.FileAttributes
	if mode&0200 == 0 {
		attrs &^= windows.FILE_ATTRIBUTE_NORMAL
		attrs |= windows.FILE_ATTRIBUTE_READONLY
	} else {
		attrs &^= windows.FILE_ATTRIBUTE_READONLY
		if attrs == 0 {
			attrs = windows.FILE_ATTRIBUTE_NORMAL
		}
	}
	info := basicInfo{Attributes: attrs}
	return windows.SetFileInformationByHandle(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
}
func (d *dir) chtimes(name string, mtime time.Time) error {
	h, err := openAt(d.handle, name, windows.FILE_WRITE_ATTRIBUTES, windows.FILE_OPEN, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	ft := windows.NsecToFiletime(mtime.UnixNano())
	return windows.SetFileTime(h, nil, nil, &ft)
}
func (d *dir) sync() error {
	if err := windows.FlushFileBuffers(d.handle); err != nil {
		return &os.PathError{Op: "flush directory", Path: d.path, Err: err}
	}
	return nil
}

func renameHandle(fromDir *dir, from string, toDir *dir, to string, replace bool) error {
	h, err := openAt(fromDir.handle, from, windows.DELETE, windows.FILE_OPEN, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if !winplatform.ValidComponent(to) {
		return os.ErrInvalid
	}
	name, err := windows.UTF16FromString(to)
	if err != nil {
		return err
	}
	name = name[:len(name)-1]
	type renameInfo struct {
		Replace uint32
		Root    windows.Handle
		Length  uint32
		Name    [1]uint16
	}
	offset := unsafe.Offsetof(renameInfo{}.Name)
	data := make([]byte, int(offset)+2*len(name))
	info := (*renameInfo)(unsafe.Pointer(&data[0]))
	info.Root = toDir.handle
	info.Length = uint32(len(name) * 2)
	if replace {
		info.Replace = 1
	}
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&data[offset])), len(name)), name)
	var io windows.IO_STATUS_BLOCK
	if err := ntError(windows.NtSetInformationFile(h, &io, &data[0], uint32(len(data)), windows.FileRenameInformation)); err != nil {
		return &os.LinkError{Op: "rename relative", Old: from, New: to, Err: err}
	}
	return nil
}
func renameNoReplace(a *dir, from string, b *dir, to string) error {
	return renameHandle(a, from, b, to, false)
}
func renameReplace(d *dir, from, to string) error { return renameHandle(d, from, d, to, true) }

var replaceFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

// ReplaceFile publishes atomically while retaining the previous file at a deterministic recovery path.
func replaceWindows(a *dir, from string, b *dir, to string) error {
	if a.path != b.path {
		return errExchangeUnsupported
	}
	target, err := windows.UTF16PtrFromString(filepath.Join(b.path, to))
	if err != nil {
		return err
	}
	incoming, err := windows.UTF16PtrFromString(filepath.Join(a.path, from))
	if err != nil {
		return err
	}
	backup, err := windows.UTF16PtrFromString(filepath.Join(a.path, from+".displaced"))
	if err != nil {
		return err
	}
	ok, _, err := replaceFile.Call(uintptr(unsafe.Pointer(target)), uintptr(unsafe.Pointer(incoming)), uintptr(unsafe.Pointer(backup)), 0, 0, 0)
	if ok == 0 {
		// ReplaceFile's documented error 1177 can move the predecessor aside without publishing the incoming file.
		// Restore it inside the folder-loop callback, before Syncthing can scan a transient deletion.
		if errors.Is(err, windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT_2) {
			if repair := restoreDisplaced(a, from, to); repair != nil {
				return errors.Join(err, repair)
			}
		}
		return &os.LinkError{Op: "replace with recovery copy", Old: from, New: to, Err: err}
	}
	return nil
}

func restoreDisplaced(d *dir, from, to string) error {
	live, err := d.lstat(to)
	if err != nil {
		return err
	}
	if live.exists {
		return nil
	}
	backup, err := d.lstat(from + ".displaced")
	if err != nil {
		return err
	}
	if !backup.exists {
		return nil
	}
	return renameNoReplace(d, from+".displaced", d, to)
}
func repairExchange(root *dir, temp, target string, device, inode uint64) error {
	if parentPath(temp) != parentPath(target) {
		return os.ErrInvalid
	}
	d, err := root.openDir(parentPath(temp))
	if err != nil {
		return err
	}
	defer d.close()
	st, err := d.lstat(baseName(temp))
	if err != nil {
		return err
	}
	if st.exists && st.id.dev == device && st.id.ino == inode {
		return restoreDisplaced(d, baseName(temp), baseName(target))
	}
	return nil
}
func renameExchange(a *dir, from string, b *dir, to string) error {
	if err := replaceWindows(a, from, b, to); err != nil {
		return err
	}
	return renameNoReplace(a, from+".displaced", a, from)
}

// Recovery runs only after the journal has identified the published file by its original staged identity.
func recoverExchange(root *dir, relative string) error {
	d, err := root.openDir(parentPath(relative))
	if err != nil {
		return err
	}
	defer d.close()
	name := baseName(relative)
	st, err := d.lstat(name + ".displaced")
	if err != nil {
		return err
	}
	if !st.exists {
		return nil
	}
	return renameNoReplace(d, name+".displaced", d, name)
}
func isExists(err error) bool {
	return errors.Is(err, os.ErrExist) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS)
}
func cloneFile(string, string) error { return errors.ErrUnsupported }
func openRegularNoFollow(path string) (*os.File, error) {
	d, err := openRootAccess(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer d.close()
	return d.openFile(filepath.Base(path))
}
