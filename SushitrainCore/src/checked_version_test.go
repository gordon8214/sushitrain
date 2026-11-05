// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build engineintegration

package sushitrain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/syncthing"
	"github.com/syncthing/syncthing/lib/versioner"
)

func TestEngineCheckedRestoreStagesSelectedArchive(t *testing.T) {
	for _, kind := range []string{VersioningTypeSimple, VersioningTypeStaggered} {
		t.Run(kind, func(t *testing.T) {
			for _, tc := range []struct {
				name           string
				selectedOffset time.Duration
				rollover       bool
				collision      bool
				sendOnly       bool
			}{
				{name: "same-second", collision: true},
				{name: "collision-after-rollover", selectedOffset: time.Nanosecond, rollover: true, collision: true},
				{name: "second-rollover", rollover: true},
				{name: "second-rollover-send-only", rollover: true, sendOnly: true},
				{name: "older-archive-rollover", selectedOffset: -30 * time.Second, rollover: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					clt, _, dir := startTestEngineWith(t, func(cfg *config.Configuration) {
						if tc.sendOnly {
							cfg.Folders[0].Type = config.FolderTypeSendOnly
						}
						cfg.Folders[0].Versioning = config.VersioningConfiguration{
							Type: kind, Params: map[string]string{"keep": "5", "maxAge": "31536000"},
							CleanupIntervalS: 3600, FSType: config.FilesystemTypeBasic,
						}
					})
					root := filepath.Join(dir, "folder")
					writeTestFile(t, filepath.Join(root, "file.txt"), "current")
					folder := scanTestFolder(t, clt)
					withCheckedRestoreClock(t, folder, func(t *testing.T, ctx context.Context, h syncthing.FolderHandle, root string) {
						time.Sleep(time.Second - time.Nanosecond)
						selectedTime := time.Now().Add(tc.selectedOffset).Truncate(time.Second)
						archive := filepath.Join(root, ".stversions", versioner.TagFilename("file.txt", selectedTime.Format(versioner.TimeFormat)))
						writeTestFile(t, archive, "selected")
						if err := os.Chmod(archive, 0o640); err != nil {
							t.Fatal(err)
						}
						list, err := folder.FileVersions("file.txt")
						if err != nil {
							t.Fatal(err)
						}
						if list.Count() != 1 {
							t.Fatalf("got %d selected archives, want 1", list.Count())
						}
						selected := list.ItemAt(0)
						token, err := folder.FileVersionRevision("file.txt", selected)
						if err != nil {
							t.Fatal(err)
						}
						// Simulate the tick the old pre-call clock check could miss.
						if tc.rollover {
							time.Sleep(time.Nanosecond)
						}
						archiveTime := time.Now().Truncate(time.Second)
						if err := folder.restoreFileVersionChecked(ctx, h, root, "file.txt", selected, token); err != nil {
							t.Fatal(err)
						}
						if !time.Now().Truncate(time.Second).Equal(archiveTime) {
							t.Fatal("archive clock advanced during restore")
						}
						assertCheckedRestoreContents(t, filepath.Join(root, "file.txt"), "selected")
						info, err := os.Stat(filepath.Join(root, "file.txt"))
						if err != nil {
							t.Fatal(err)
						}
						if info.Mode().Perm() != 0o640 {
							t.Fatalf("restored mode %v, want 0640", info.Mode())
						}
						// Check the live contents at the versioner's actual archive tag,
						// which can differ from the selected tag across a second rollover.
						currentArchive := filepath.Join(root, ".stversions", versioner.TagFilename("file.txt", archiveTime.Format(versioner.TimeFormat)))
						if (currentArchive == archive) != tc.collision {
							t.Fatalf("selected tag %s, live archive tag %s: collision = %v, want %v", selectedTime, archiveTime, currentArchive == archive, tc.collision)
						}
						assertCheckedRestoreContents(t, currentArchive, "current")
						list, err = folder.FileVersions("file.txt")
						if err != nil {
							t.Fatal(err)
						}
						wantCount := 1
						if !tc.collision {
							wantCount = 2
							assertCheckedRestoreContents(t, archive, "selected")
						}
						if list.Count() != wantCount {
							t.Fatalf("got %d archives, want %d", list.Count(), wantCount)
						}
						export := filepath.Join(t.TempDir(), "copy")
						err = folder.DesktopExportVersionChecked(ctx, "file.txt", selected, export, token)
						if !tc.collision {
							if err != nil {
								t.Fatalf("unchanged selected archive rejected: %v", err)
							}
							assertCheckedRestoreContents(t, export, "selected")
						} else {
							// Only a real replacement at the selected tag makes its token stale.
							if !errors.Is(err, errVersionNotListed) {
								t.Fatalf("same-tag replacement export: got %v, want stale revision", err)
							}
							if _, err := os.Stat(export); !os.IsNotExist(err) {
								t.Fatal("stale export published", err)
							}
						}
					})
				})
			}
		})
	}
}

// Admit the real folder handle before entering the bubble, and hold its loop
// until the bubble returns. The test clock controls the restore and versioner's
// time.Now without changing the engine's background timers or wall clock.
func withCheckedRestoreClock(t *testing.T, folder *Folder, fn func(*testing.T, context.Context, syncthing.FolderHandle, string)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), engineActionTimeout)
	defer cancel()
	type admission struct {
		ctx  context.Context
		h    syncthing.FolderHandle
		root string
	}
	admitted := make(chan admission)
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- folder.withHistoryTargetContext(ctx, "file.txt", func(ctx context.Context, h syncthing.FolderHandle, root string) error {
			// Done is lazy; allocate it outside the bubble so the engine
			// can cancel its own context after the bubble returns.
			_ = ctx.Done()
			select {
			case admitted <- admission{ctx, h, root}:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	defer func() {
		close(release)
		select {
		case err := <-result:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("timed out releasing the folder loop", ctx.Err())
		}
	}()
	select {
	case handle := <-admitted:
		synctest.Test(t, func(t *testing.T) { fn(t, handle.ctx, handle.h, handle.root) })
	case <-ctx.Done():
		t.Fatal("timed out acquiring the folder loop", ctx.Err())
	}
}

func assertCheckedRestoreContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("%s: got %q, %v; want %q", path, data, err, want)
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
