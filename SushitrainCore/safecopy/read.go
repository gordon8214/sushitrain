// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package safecopy

import (
	"errors"
	"os"
)

// OpenRegular follows a user-selected symlink only to a regular file. Special
// files are opened nonblocking where supported and rejected before any read.
func OpenRegular(path string) (*os.File, error) {
	f, err := openNonblocking(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("choose a regular file")
	}
	return f, nil
}
