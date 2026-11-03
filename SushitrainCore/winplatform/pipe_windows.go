// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package winplatform

import (
	"errors"
	"golang.org/x/sys/windows"
)

// VerifyPipeServer prevents another Windows user from impersonating a predictable local pipe endpoint.
// This check runs before the client writes pairing secrets or accepts any response.
func VerifyPipeServer(pipe windows.Handle) error {
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(pipe, &pid); err != nil {
		return err
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	other, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !other.User.Sid.Equals(current.User.Sid) {
		return errors.New("the Synctrain pipe belongs to another Windows user")
	}
	return nil
}
