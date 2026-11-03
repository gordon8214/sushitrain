// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func renameNoReplaceSyscall(fromFd int, from string, toFd int, to string) error {
	return unix.Renameat2(fromFd, from, toFd, to, unix.RENAME_NOREPLACE)
}

func renameExchangeSyscall(aFd int, a string, bFd int, b string) error {
	return unix.Renameat2(aFd, a, bFd, b, unix.RENAME_EXCHANGE)
}

func statMtime(st *unix.Stat_t) (int64, int64) {
	return st.Mtim.Sec, st.Mtim.Nsec
}

// cloneFile makes dst a copy-on-write clone of src where the filesystem supports it (FICLONE)
func cloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// systemXattr reports whether the system labels every file with the attribute, so that a file which has it has
// nothing of its own: security.* (an SELinux label, which policy gives a new file too)
func systemXattr(name string) bool {
	return strings.HasPrefix(name, "security.")
}
