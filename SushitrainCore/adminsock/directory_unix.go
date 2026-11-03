// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !windows

package adminsock

import (
	"fmt"
	"os"
	"syscall"
)

// checkPrivateDirectory refuses a directory that someone else could change entries of: one owned by another user
// (other than root), writable by everyone without the sticky bit, or writable by its group unless groupWritable
func checkPrivateDirectory(dir string, groupWritable bool) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	mode := info.Mode()
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() && st.Uid != 0 {
		return fmt.Errorf("%s belongs to another user (%d)", dir, st.Uid)
	}
	switch {
	case mode&0o002 != 0 && mode&os.ModeSticky == 0:
		return fmt.Errorf("%s is writable by everyone; restrict it (chmod o-w) or set its sticky bit", dir)
	case mode&0o020 != 0 && !groupWritable && mode&os.ModeSticky == 0:
		return fmt.Errorf("%s is writable by its group; restrict it (chmod g-w), or allow the group to use the "+
			"admin socket", dir)
	}
	return nil
}
