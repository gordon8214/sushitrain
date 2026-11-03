// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"t-shaped.nl/sushitrain/v2/files"
)

func TestBookmarks(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	path := filepath.Join(config, "gtk-3.0", "bookmarks")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("file:///home/someone/Documents Documents\nfile:///mnt/Synctrain/Old%20one Old one\n"),
		0o644)
	logger := slog.Default()
	syncBookmarks("/mnt/Synctrain", []files.Folder{{DirName: "Photos", Label: "Photos"},
		{DirName: "Work stuff", Label: "Work stuff"}}, logger)
	data, _ := os.ReadFile(path)
	want := "file:///home/someone/Documents Documents\nfile:///mnt/Synctrain/Photos Photos\n" +
		"file:///mnt/Synctrain/Work%20stuff Work stuff\n"
	if got := string(data); got != want && got != "file:///home/someone/Documents Documents\n"+
		"file:///mnt/Synctrain/Work%20stuff Work stuff\nfile:///mnt/Synctrain/Photos Photos\n" {
		t.Fatalf("bookmarks:\n%s", got)
	}
	// Removing a folder removes its bookmark, and nothing else
	syncBookmarks("/mnt/Synctrain", []files.Folder{{DirName: "Photos", Label: "Photos"}}, logger)
	data, _ = os.ReadFile(path)
	if got := string(data); got != "file:///home/someone/Documents Documents\nfile:///mnt/Synctrain/Photos Photos\n" {
		t.Fatalf("bookmarks after removing a folder:\n%s", got)
	}
}
