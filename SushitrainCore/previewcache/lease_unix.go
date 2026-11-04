//go:build !windows

// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package previewcache

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func lock(f *os.File, exclusive, nonblock bool) error {
	flags := unix.LOCK_SH
	if exclusive {
		flags = unix.LOCK_EX
	}
	if nonblock {
		flags |= unix.LOCK_NB
	}
	return unix.Flock(int(f.Fd()), flags)
}

func lockBusy(err error) bool { return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) }
