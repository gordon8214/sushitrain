// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package files

import (
	"errors"
	"golang.org/x/sys/windows"
	"unsafe"
)

var (
	credentialDLL = windows.NewLazySystemDLL("advapi32.dll")
	credRead      = credentialDLL.NewProc("CredReadW")
	credWrite     = credentialDLL.NewProc("CredWriteW")
	credDelete    = credentialDLL.NewProc("CredDeleteW")
	credFree      = credentialDLL.NewProc("CredFree")
)

type credentialRecord struct {
	Flags, Type             uint32
	TargetName, Comment     *uint16
	LastWritten             windows.Filetime
	BlobSize                uint32
	Blob                    *byte
	Persist, AttributeCount uint32
	Attributes              uintptr
	TargetAlias, UserName   *uint16
}

// WindowsCredentials stores secrets in the current user's Windows Credential Manager.
type WindowsCredentials struct{}

func credentialName(id string) (*uint16, error) {
	return windows.UTF16PtrFromString("Synctrain:Gateway:" + id)
}
func (WindowsCredentials) Credential(id string) (string, error) {
	name, err := credentialName(id)
	if err != nil {
		return "", err
	}
	var record *credentialRecord
	ok, _, err := credRead.Call(uintptr(unsafe.Pointer(name)), 1, 0, uintptr(unsafe.Pointer(&record)))
	if ok == 0 {
		if errors.Is(err, windows.ERROR_NOT_FOUND) {
			return "", nil
		}
		return "", err
	}
	defer credFree.Call(uintptr(unsafe.Pointer(record)))
	return string(unsafe.Slice(record.Blob, record.BlobSize)), nil
}
func (WindowsCredentials) SetCredential(id, secret string) error {
	name, err := credentialName(id)
	if err != nil {
		return err
	}
	data := []byte(secret)
	if len(data) == 0 {
		return errors.New("empty gateway credential")
	}
	record := credentialRecord{Type: 1, TargetName: name, BlobSize: uint32(len(data)), Blob: &data[0], Persist: 2}
	ok, _, err := credWrite.Call(uintptr(unsafe.Pointer(&record)), 0)
	if ok == 0 {
		return err
	}
	return nil
}
func (WindowsCredentials) DeleteCredential(id string) error {
	name, err := credentialName(id)
	if err != nil {
		return err
	}
	ok, _, err := credDelete.Call(uintptr(unsafe.Pointer(name)), 1, 0)
	if ok == 0 && !errors.Is(err, windows.ERROR_NOT_FOUND) {
		return err
	}
	return nil
}
