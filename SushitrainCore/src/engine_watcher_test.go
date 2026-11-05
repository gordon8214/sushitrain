// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build engineintegration

package sushitrain

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
)

func TestEngineWatchedFoldersSurviveDefaultConfigurationChanges(t *testing.T) {
	clt, _, _ := startTestEngineWith(t, func(cfg *config.Configuration) {
		cfg.Defaults.Folder.FSWatcherEnabled = true
		cfg.Defaults.Folder.FSWatcherDelayS = 0.1
		cfg.Defaults.Folder.RescanIntervalS = 0
	})

	roots := make(map[string]string)
	for i := range 3 {
		id, root := fmt.Sprintf("watched-%d", i), t.TempDir()
		if err := clt.AddFolder(id, root, false); err != nil {
			t.Fatal(err)
		}
		roots[id] = root
		waitUntil(t, "the watched folder runs", func() bool {
			return clt.FolderWithID(id).State() == "idle"
		})
		// A synchronous scan waits for the initial scan to finish. No scan is
		// requested after the file below is written.
		if err := clt.internals().ScanFolderSubdirs(id, nil); err != nil {
			t.Fatal(err)
		}
	}

	// Every defaults save notifies all active aggregators, even though their own
	// settings stay the same. Subscriber logging must not race their updates.
	for i := range 50 {
		settings := clt.DefaultFolderSettings()
		settings.MaxConflicts = strconv.Itoa(i + 1)
		if err := clt.SetDefaultFolderSettings(settings); err != nil {
			t.Fatal(err)
		}
	}

	for id, root := range roots {
		cfg, ok := clt.cfg().Folder(id)
		if !ok || !cfg.FSWatcherEnabled || cfg.RescanIntervalS != 0 || cfg.FSWatcherDelayS != 0.1 {
			t.Fatalf("watcher settings changed for %s: %+v", id, cfg)
		}
		// With periodic scans disabled and the initial scan finished, indexing a
		// new file requires the native watcher to remain operational.
		contents := "a change after the defaults were saved"
		writeTestFile(t, filepath.Join(root, "after.txt"), contents)
		waitUntil(t, "the watcher indexes a new file in "+id, func() bool {
			file, ok, err := clt.internals().LocalFileInfo(id, "after.txt")
			return err == nil && ok && !file.IsDeleted() && file.Size == int64(len(contents))
		})
	}
}
