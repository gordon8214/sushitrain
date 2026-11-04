// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build engineintegration && darwin

package sushitrain

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEngineCheckedRestorePreservesNativeAttributes(t *testing.T) {
	_, folder, root := versionedTestFolder(t)
	versions, err := folder.FileVersions("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	version := versions.ItemAt(0)
	file, err := folder.client.internals().OpenFileVersion(folder.FolderID, "kept.txt", version.versionTime)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".stversions", file.Name())
	file.Close()
	for name, value := range map[string]string{"com.apple.ResourceFork": "selected fork", "com.synctrain.test": "selected attribute"} {
		if err := unix.Setxattr(path, name, []byte(value), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Setxattr(filepath.Join(root, "kept.txt"), "com.synctrain.remove", []byte("current only"), 0); err != nil {
		t.Fatal(err)
	}
	token, err := folder.FileVersionRevision("kept.txt", version)
	if err != nil {
		t.Fatal(err)
	}
	if err := folder.RestoreFileVersionChecked(t.Context(), "kept.txt", version, token); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"com.apple.ResourceFork": "selected fork", "com.synctrain.test": "selected attribute"} {
		data := make([]byte, 128)
		n, err := unix.Getxattr(filepath.Join(root, "kept.txt"), name, data)
		if err != nil || string(data[:n]) != want {
			t.Fatal(name, string(data), err)
		}
	}
	if _, err := unix.Getxattr(filepath.Join(root, "kept.txt"), "com.synctrain.remove", nil); err == nil {
		t.Fatal("restored unrelated live attributes")
	}
}

func TestEngineDownloadedRestoreRejectsFIFO(t *testing.T) {
	_, folder, root := versionedTestFolder(t)
	token, err := folder.CaptureRestoreTarget("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if err := folder.RestoreDownloadedVersionContext(t.Context(), "kept.txt", fifo, token, time.Now().UnixNano()); err == nil {
		t.Fatal("FIFO accepted")
	}
	contents, _ := os.ReadFile(filepath.Join(root, "kept.txt"))
	if string(contents) != "current" {
		t.Fatal(string(contents))
	}
}
