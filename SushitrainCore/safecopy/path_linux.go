// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package safecopy

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

func directoryPath(f *os.File, _ string) (string, error) {
	return filepath.EvalSymlinks(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
}

func publish(root *os.Root, from, to string, file *os.File) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return unix.Linkat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", file.Fd()), int(dir.Fd()), to, unix.AT_SYMLINK_FOLLOW)
}
