// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"golang.org/x/sys/unix"
)

func renameNoReplaceSyscall(fromFd int, from string, toFd int, to string) error {
	return unix.RenameatxNp(fromFd, from, toFd, to, unix.RENAME_EXCL)
}

func renameExchangeSyscall(aFd int, a string, bFd int, b string) error {
	return unix.RenameatxNp(aFd, a, bFd, b, unix.RENAME_SWAP)
}

func statMtime(st *unix.Stat_t) (int64, int64) {
	return st.Mtim.Sec, st.Mtim.Nsec
}

// cloneFile makes dst a copy-on-write clone of src (APFS clonefile)
func cloneFile(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
