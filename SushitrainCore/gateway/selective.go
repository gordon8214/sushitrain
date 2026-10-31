// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/ignore"
	"github.com/syncthing/syncthing/lib/syncthing"
)

// SelectiveFolders returns what EngineBackend.IsSelective needs: it reports whether a folder's ignore patterns keep only
// a selection of its files on this device (a pattern that ignores everything, usually after exceptions). It answers yes
// when the patterns can't be read, so that such a folder is never written.
func SelectiveFolders(app *syncthing.App, cfg config.Wrapper) func(folderID string) bool {
	return func(folderID string) bool {
		_, patterns, err := app.Internals.Ignores(folderID)
		if err == nil && len(patterns) > 0 {
			return slices.ContainsFunc(patterns, ignoresEverything)
		}
		// Syncthing has no patterns for folders that don't run (paused ones) and none after failing to load them, which
		// it doesn't report: only the folder says
		folder, ok := cfg.Folder(folderID)
		if !ok {
			return true
		}
		ffs := folder.Filesystem()
		// Only a file that isn't there means no patterns, which a folder that isn't there (on a volume that isn't mounted)
		// doesn't say. This runs for every request, so it looks at the root without listing it.
		if _, err := ffs.Lstat(".stignore"); errors.Is(err, os.ErrNotExist) {
			_, err := ffs.Stat(".")
			return err != nil
		}
		matcher := ignore.New(ffs)
		if err := matcher.Load(".stignore"); err != nil {
			return true
		}
		return slices.ContainsFunc(matcher.Patterns(), ignoresEverything)
	}
}

// ignoresEverything reports whether a parsed ignore pattern ("(?d)(?i)!pattern") ignores every file
func ignoresEverything(pattern string) bool {
	pattern = strings.TrimPrefix(pattern, "(?d)")
	pattern = strings.TrimPrefix(pattern, "(?i)")
	switch pattern {
	case "*", "**", "/*", "/**", "**/*", "**/**":
		return true
	}
	return false
}
