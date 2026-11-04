// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build engineintegration

package sushitrain

import (
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/versioner"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEngineRestoreDownloadedHistory(t *testing.T) {
	_, folder, root := versionedTestFolder(t)
	source := filepath.Join(t.TempDir(), "download")
	writeTestFile(t, source, "from another host")
	captured, err := folder.CaptureRestoreTarget("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := folder.RestoreDownloadedVersion("kept.txt", source, captured, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(root, "kept.txt")); string(contents) != "from another host" {
		t.Fatalf("restored: %q", contents)
	}
	versions, err := folder.FileVersions("kept.txt")
	if err != nil || versions.Count() < 2 {
		t.Fatalf("replaced contents not archived: %v", err)
	}

	captured, err = folder.CaptureRestoreTarget("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "kept.txt"), "unsent local edit")
	if err := folder.RestoreDownloadedVersion("kept.txt", source, captured, time.Now().UnixNano()); err == nil {
		t.Fatal("overwrote unscanned edit")
	}
	if contents, _ := os.ReadFile(filepath.Join(root, "kept.txt")); string(contents) != "unsent local edit" {
		t.Fatalf("lost edit: %q", contents)
	}
}

func TestEngineRestoreDownloadedWithoutVersioning(t *testing.T) {
	clt, _, dir := startTestEngine(t)
	root := filepath.Join(dir, "folder")
	writeTestFile(t, filepath.Join(root, "file.txt"), "current")
	folder := scanTestFolder(t, clt)
	source := filepath.Join(t.TempDir(), "download")
	writeTestFile(t, source, "remote archive")
	captured, err := folder.CaptureRestoreTarget("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := folder.RestoreDownloadedVersion("file.txt", source, captured, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if contents, _ := os.ReadFile(filepath.Join(root, "file.txt")); string(contents) != "remote archive" {
		t.Fatalf("restored: %q", contents)
	}
}

func TestEngineRestoreDownloadedSendOnlyArchivesDestination(t *testing.T) {
	for _, kind := range []string{"simple", "staggered"} {
		t.Run(kind, func(t *testing.T) {
			clt, _, dir := startTestEngineWith(t, func(cfg *config.Configuration) {
				cfg.Folders[0].Type = config.FolderTypeSendOnly
				cfg.Folders[0].Versioning = config.VersioningConfiguration{Type: kind, Params: map[string]string{"keep": "5", "preservePlatformData": "false"}, FSType: config.FilesystemTypeBasic}
				if kind == "staggered" {
					cfg.Folders[0].Versioning.Params["preservePlatformData"] = "true"
				}
			})
			root := filepath.Join(dir, "folder")
			writeTestFile(t, filepath.Join(root, "file.txt"), "current")
			folder := scanTestFolder(t, clt)
			source := filepath.Join(t.TempDir(), "download")
			writeTestFile(t, source, "history")
			token, err := folder.CaptureRestoreTarget("file.txt")
			if err != nil {
				t.Fatal(err)
			}
			if err := folder.RestoreDownloadedVersion("file.txt", source, token, time.Now().UnixNano()); err != nil {
				t.Fatal(err)
			}
			cfg := folder.folderConfiguration()
			v, err := versioner.New(*cfg)
			if err != nil {
				t.Fatal(err)
			}
			versions, err := v.GetVersions()
			if err != nil || len(versions["file.txt"]) != 1 {
				t.Fatalf("Send Only did not archive current contents: %v %v", versions, err)
			}
		})
	}
}
