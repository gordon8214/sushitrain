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

func TestBookmarksLeaveTheUsersAndTakeNoLines(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	path := filepath.Join(config, "gtk-3.0", "bookmarks")
	os.MkdirAll(filepath.Dir(path), 0o755)
	// The user's own bookmark of a place inside a folder stays
	os.WriteFile(path, []byte("file:///mnt/Synctrain/Photos/Trips Trips\n"), 0o644)
	syncBookmarks("/mnt/Synctrain", []files.Folder{{DirName: "Photos", Label: "Photos\nsmb://evil/share Documents"}},
		slog.Default())
	data, _ := os.ReadFile(path)
	if got := string(data); got != "file:///mnt/Synctrain/Photos/Trips Trips\n"+
		"file:///mnt/Synctrain/Photos Photos smb://evil/share Documents\n" {
		t.Fatalf("bookmarks:\n%s", got)
	}
}

func TestStringArrays(t *testing.T) {
	for _, text := range []string{`@as []`, `['/home/a']`, `["/home/o'brien/x", '/home/b']`} {
		list, ok := parseStringArray(text)
		if !ok {
			t.Fatalf("%s wasn't read", text)
		}
		again, ok := parseStringArray(formatStringArray(list))
		if !ok || len(again) != len(list) {
			t.Fatalf("%s: %q, then %q", text, list, again)
		}
		for i := range list {
			if again[i] != list[i] {
				t.Fatalf("%s: %q, then %q", text, list, again)
			}
		}
	}
	if list, _ := parseStringArray(`["/home/o'brien/x"]`); len(list) != 1 || list[0] != "/home/o'brien/x" {
		t.Fatalf("%q", list)
	}
}
