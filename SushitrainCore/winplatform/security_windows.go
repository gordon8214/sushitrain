// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

// Package winplatform contains the Windows security and storage primitives shared by the desktop host and gateway.
package winplatform

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
)

func UserSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// PrivateDirectory removes inherited access, including access inherited by newly created secret files.
// Only this user and SYSTEM may access the directory. It never changes the owner of someone else's directory.
func PrivateDirectory(path string) error { return privatePath(path, true) }

func PrivateFile(path string) error { return privatePath(path, false) }

func privatePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() != directory || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a regular directory", path)
	}
	sid, err := UserSID()
	if err != nil {
		return err
	}
	old, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := old.Owner()
	if err != nil {
		return err
	}
	if owner.String() != sid && !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		return fmt.Errorf("%s belongs to another user", path)
	}
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;" + inheritance + ";FA;;;SY)(A;" + inheritance + ";FA;;;" + sid + ")")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func Information(f *os.File) (windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info)
	return info, err
}
