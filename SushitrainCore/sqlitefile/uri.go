// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

// Package sqlitefile constructs SQLite file URIs from absolute native paths.
package sqlitefile

import (
	"net/url"
	"path/filepath"
	"strings"
)

// URI keeps drive letters in the path rather than interpreting them as a URI authority.
// Callers pass an absolute path, as SQLite interprets relative URI paths differently.
func URI(path, query string) string {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query}).String()
}
