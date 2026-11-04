// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package safecopy

import (
	"errors"
	"os"
	"strings"
)

func openNonblocking(path string) (*os.File, error) {
	if strings.HasPrefix(strings.ToLower(path), `\\.\`) {
		return nil, errors.New("choose a local regular file")
	}
	return os.Open(path)
}
