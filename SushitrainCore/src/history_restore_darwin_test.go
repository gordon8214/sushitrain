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

func TestEngineRestoreDownloadedRejectsMetadataOnlyEdit(t *testing.T) {
	clt, _, dir := startTestEngine(t)
	root := filepath.Join(dir, "folder")
	name := filepath.Join(root, "file.txt")
	writeTestFile(t, name, "current")
	folder := scanTestFolder(t, clt)
	source := filepath.Join(t.TempDir(), "download")
	writeTestFile(t, source, "history")
	token, err := folder.CaptureRestoreTarget("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(name, "com.synctrain.history-test", []byte("edited metadata"), 0); err != nil {
		t.Fatal(err)
	}
	if err := folder.RestoreDownloadedVersion("file.txt", source, token, time.Now().UnixNano()); err == nil {
		t.Fatal("overwrote metadata-only edit")
	}
	if contents, _ := os.ReadFile(name); string(contents) != "current" {
		t.Fatal("lost current contents")
	}
	size, err := unix.Getxattr(name, "com.synctrain.history-test", nil)
	if err != nil || size == 0 {
		t.Fatalf("lost metadata: %v", err)
	}
}
