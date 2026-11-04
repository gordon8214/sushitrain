// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build engineintegration

package sushitrain

import (
	"context"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/versioner"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEngineCheckedRestoreStagesSelectedArchive(t *testing.T) {
	for _, kind := range []string{VersioningTypeSimple, VersioningTypeStaggered} {
		t.Run(kind, func(t *testing.T) {
			clt, _, dir := startTestEngineWith(t, func(cfg *config.Configuration) {
				cfg.Folders[0].Versioning = config.VersioningConfiguration{Type: kind, Params: map[string]string{"keep": "5", "maxAge": "31536000"}, CleanupIntervalS: 3600, FSType: config.FilesystemTypeBasic}
			})
			root := filepath.Join(dir, "folder")
			writeTestFile(t, filepath.Join(root, "file.txt"), "current")
			folder := scanTestFolder(t, clt)
			// The selected archive has the exact tag the versioner will assign current
			// content. Move the tag into the current second if listing crossed a tick.
			var selected *FileVersion
			var token string
			var archive string
			for {
				stamp := time.Now().Format(versioner.TimeFormat)
				archive = filepath.Join(root, ".stversions", "file~"+stamp+".txt")
				writeTestFile(t, archive, "selected")
				if err := os.Chmod(archive, 0640); err != nil {
					t.Fatal(err)
				}
				list, err := folder.FileVersions("file.txt")
				if err != nil {
					t.Fatal(err)
				}
				selected = list.ItemAt(0)
				token, err = folder.FileVersionRevision("file.txt", selected)
				if err != nil {
					t.Fatal(err)
				}
				if time.Now().Format(versioner.TimeFormat) == stamp {
					break
				}
				if err := os.Remove(archive); err != nil {
					t.Fatal(err)
				}
			}
			if err := folder.RestoreFileVersionChecked(t.Context(), "file.txt", selected, token); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, "file.txt"))
			if err != nil || string(data) != "selected" {
				t.Fatal(string(data), err)
			}
			info, err := os.Stat(filepath.Join(root, "file.txt"))
			if err != nil || info.Mode().Perm() != 0640 {
				t.Fatal(info, err)
			}
			old, err := os.ReadFile(archive)
			if err != nil || string(old) != "current" {
				t.Fatal("current contents were not archived", string(old), err)
			}
			// Replacing that same tag invalidates the earlier selected archive token.
			export := filepath.Join(t.TempDir(), "copy")
			if err := folder.DesktopExportVersionChecked(t.Context(), "file.txt", selected, export, token); err == nil {
				t.Fatal("stale archive exported")
			}
			if _, err := os.Stat(export); !os.IsNotExist(err) {
				t.Fatal("stale export published", err)
			}
		})
	}
}

func TestEngineCanceledCheckedRestorePreservesCurrent(t *testing.T) {
	_, folder, root := versionedTestFolder(t)
	list, err := folder.FileVersions("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	selected := list.ItemAt(0)
	token, err := folder.FileVersionRevision("kept.txt", selected)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := folder.RestoreFileVersionChecked(ctx, "kept.txt", selected, token); err == nil {
		t.Fatal("canceled restore succeeded")
	}
	data, _ := os.ReadFile(filepath.Join(root, "kept.txt"))
	if string(data) != "current" {
		t.Fatal(string(data))
	}
}
