// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build engineintegration

package sushitrain

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// What an operating system leaves in a directory keeps nobody from removing that directory, whether or not the folder's
// patterns ignore it. Anything else that no device knows about may be the only copy of it, and stays.
func TestEngineRemovesDirectoriesHoldingSystemFiles(t *testing.T) {
	clt, _, dir := startTestEngine(t)
	folder := clt.FolderWithID("test")
	// Patterns can't change while the folder scans, as it does after starting
	deadline := time.Now().Add(30 * time.Second)
	for err := folder.SetIgnoreLines(List([]string{".DS_Store", "*.draft"})); err != nil; err = folder.SetIgnoreLines(List([]string{".DS_Store", "*.draft"})) {
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	root := filepath.Join(dir, "folder")
	write := func(name string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("contents"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ignored/.DS_Store")
	write("indexed/Thumbs.db")
	write("nested/sub/.DS_Store")
	write("kept/notes.draft")
	if err := clt.scanFolderSubdirs("test", nil); err != nil {
		t.Fatal(err)
	}
	remove := func(path string) error {
		t.Helper()
		entry, err := folder.GetFileInformation(path)
		if err != nil || entry == nil || entry.IsDeleted() {
			t.Fatalf("%s: %v, %v", path, entry, err)
		}
		return entry.Remove()
	}

	for _, path := range []string{"ignored", "indexed", "nested"} {
		if err := remove(path); err != nil {
			t.Errorf("removing %s: %v", path, err)
		}
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", path, err)
		}
		// Removing it also tells the index
		deadline := time.Now().Add(30 * time.Second)
		for {
			entry, err := folder.GetFileInformation(path)
			if err == nil && entry != nil && entry.IsDeleted() {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("%s isn't indexed as deleted: %v", path, err)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	if err := remove("kept"); err == nil {
		t.Error("removed a directory that holds the only copy of a file")
	}
	if _, err := os.Lstat(filepath.Join(root, "kept", "notes.draft")); err != nil {
		t.Errorf("the only copy of a file: %v", err)
	}
}
