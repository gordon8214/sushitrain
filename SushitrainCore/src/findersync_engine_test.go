// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
//go:build engineintegration

package sushitrain

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/osutil"
)

func TestEngineFinderVisibleItems(t *testing.T) {
	clt, _, dir := startTestEngine(t)
	writeTestFile(t, filepath.Join(dir, "folder", "dir", "file.txt"), "current")
	writeTestFile(t, filepath.Join(dir, "folder", "cafe\u0301.txt"), "Unicode fixture")
	writeTestFile(t, filepath.Join(dir, "folder", ".stignore"), "*.tmp\ncafé.ignored\n")
	writeTestFile(t, filepath.Join(dir, "folder", "ignored.tmp"), "ignored")
	writeTestFile(t, filepath.Join(dir, "folder", osutil.NativeFilename("café.ignored")), "ignored Unicode")
	writeTestFile(t, filepath.Join(dir, "folder", `literal\name.txt`), "literal backslash")
	writeTestFile(t, filepath.Join(dir, "folder", "dir", ".stversions", "ordinary.txt"), "ordinary nested name")
	folder := scanTestFolder(t, clt)
	items := []finderItem{{"test", ""}, {"test", "dir"}, {"test", "dir/file.txt"}, {"test", "ignored.tmp"}, {"test", "../escape"}, {"test", "cafe\u0301.txt"}}
	items = append(items, finderItem{"test", `literal\name.txt`}, finderItem{"test", "dir/.stversions/ordinary.txt"}, finderItem{"test", "café.ignored"})
	query := func() []finderStatus {
		t.Helper()
		request, _ := json.Marshal(items)
		data, err := clt.FinderStatusJSON(request)
		if err != nil {
			t.Fatal(err)
		}
		var statuses []finderStatus
		if err := json.Unmarshal(data, &statuses); err != nil {
			t.Fatal(err)
		}
		return statuses
	}
	var statuses []finderStatus
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("last statuses: %+v", statuses)
		}
	})
	waitUntil(t, "Finder reports the completed scan", func() bool {
		statuses = query()

		return statuses[0].Badge == "current" && statuses[1].Badge == "current" && statuses[2].Badge == "current" && statuses[5].Badge == "current" && statuses[6].Badge == "current" && statuses[7].Badge == "current"
	})
	if statuses[0].Badge != "current" || statuses[1].Badge != "current" || statuses[2].Badge != "current" || statuses[3].Badge != "" || statuses[4].Badge != "" || statuses[8].Badge != "" {
		t.Fatalf("%+v", statuses)
	}
	writeTestFile(t, filepath.Join(dir, "folder", "dir", "file.txt"), "unscanned")
	if query()[2].Badge != "waiting" {
		t.Fatal("unscanned edit is current")
	}
	if err := folder.SetPaused(true); err != nil {
		t.Fatal(err)
	}
	if query()[0].Badge != "paused" {
		t.Fatal("root did not pause")
	}
}
