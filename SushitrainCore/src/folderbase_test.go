// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/protocol"
)

func setTestManagedBase(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "group", "Folders")
	SetManagedFolderBase(base)
	t.Cleanup(func() { SetManagedFolderBase("") })
	return base
}

func newFolderBaseTestFolder(t *testing.T, folderPath string, paused bool) *Folder {
	t.Helper()
	return newFolderBaseTestFolderWithID(t, "test", folderPath, paused)
}

func newFolderBaseTestFolderWithID(t *testing.T, folderID string, folderPath string, paused bool) *Folder {
	t.Helper()
	filesPath := filepath.Dir(folderPath)
	cfg := config.New(protocol.EmptyDeviceID)
	cfg.Folders = []config.FolderConfiguration{{
		ID:             folderID,
		Path:           folderPath,
		FilesystemType: config.FilesystemTypeBasic,
		MarkerName:     config.DefaultMarkerName,
		Paused:         paused,
	}}
	wrapper := config.Wrap(filepath.Join(t.TempDir(), "config.xml"), cfg, protocol.EmptyDeviceID, events.NoopLogger)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = wrapper.Serve(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return &Folder{client: &Client{config: wrapper, filesPath: filesPath}, FolderID: folderID}
}

func writeFolderWithMarker(t *testing.T, folderPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(folderPath, config.DefaultMarkerName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folderPath, "file.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultFolderPath(t *testing.T) {
	clt := &Client{filesPath: "/documents"}
	if got, err := clt.defaultFolderPath("abc"); err != nil || got != "/documents/abc" {
		t.Fatalf("without managed base: %q, %v", got, err)
	}
	base := setTestManagedBase(t)
	if got, err := clt.defaultFolderPath("abc"); err != nil || got != filepath.Join(base, "abc") {
		t.Fatalf("with managed base: %q, %v", got, err)
	}
	if !clt.isInternalFolderPath("abc", "/documents/abc") || !clt.isInternalFolderPath("abc", filepath.Join(base, "abc")) {
		t.Fatal("legacy and managed locations must both count as internal")
	}
	if clt.isInternalFolderPath("abc", "/elsewhere/abc") {
		t.Fatal("external location counted as internal")
	}
}

func TestUnsafeFolderIDs(t *testing.T) {
	clt := &Client{filesPath: "/documents"}
	base := setTestManagedBase(t)
	for _, id := range []string{"..", "../x", "a/b", ".", "", "a\\b", "a\x00b", "/abs", "x/.."} {
		if path, err := clt.defaultFolderPath(id); !errors.Is(err, errUnsafeFolderID) {
			t.Errorf("%q: default path %q, %v", id, path, err)
		}
		if managed := managedFolderPath(id); managed != "" {
			t.Errorf("%q: managed path %q", id, managed)
		}
	}

	for _, id := range []string{"..", "../x", "a/b"} {
		// A folder with such an ID at a location the app controls stays where it is
		filesPath := t.TempDir()
		legacy := filepath.Join(filesPath, id)
		writeFolderWithMarker(t, legacy)
		folder := newFolderBaseTestFolderWithID(t, id, legacy, false)
		folder.client.filesPath = filesPath
		if err := folder.MoveToManagedBase(); !errors.Is(err, errUnsafeFolderID) {
			t.Errorf("%q: moved to the managed base: %v", id, err)
		}
		if folder.Path() != legacy || folder.IsInManagedBase() || folder.IsPaused() {
			t.Errorf("%q: configuration changed to %q", id, folder.Path())
		}

		// Even when its marker is gone, and one exists where the ID points in the managed base
		writeFolderWithMarker(t, filepath.Join(base, id))
		fc := config.FolderConfiguration{ID: id, Path: filepath.Join("/gone", id), MarkerName: config.DefaultMarkerName}
		if reanchorToManagedBase(&fc, "/gone") || fc.Path != filepath.Join("/gone", id) {
			t.Errorf("%q: re-anchored to %q", id, fc.Path)
		}
	}
}

func TestReanchorToManagedBase(t *testing.T) {
	filesPath := filepath.Join(t.TempDir(), "Documents")
	fc := config.FolderConfiguration{ID: "test", Path: "/old-container/group/Folders/test", MarkerName: config.DefaultMarkerName}
	if reanchorToManagedBase(&fc, filesPath) {
		t.Fatal("re-anchored without a managed base")
	}

	base := setTestManagedBase(t)
	if reanchorToManagedBase(&fc, filesPath) {
		t.Fatal("re-anchored without a marker in the managed location")
	}

	// External folders stay where they are, even when they are not available
	writeFolderWithMarker(t, filepath.Join(base, "test"))
	for _, external := range []string{"/gone/test", "/old-container/Folders/test", "/old-container/group/Other/test", filepath.Join(base, "other")} {
		fc.Path = external
		if reanchorToManagedBase(&fc, filesPath) || fc.Path != external {
			t.Fatalf("re-anchored external folder %q", external)
		}
	}

	// A previous managed base, and the files path, are locations the app controls
	for _, earlier := range []string{"/old-container/group/Folders/test", filepath.Join(filesPath, "test")} {
		fc.Path = earlier
		if !reanchorToManagedBase(&fc, filesPath) || fc.Path != filepath.Join(base, "test") {
			t.Fatalf("did not re-anchor %q: %q", earlier, fc.Path)
		}
	}
	if reanchorToManagedBase(&fc, filesPath) {
		t.Fatal("re-anchored a folder already in the managed location")
	}

	existing := filepath.Join(filesPath, "test")
	writeFolderWithMarker(t, existing)
	fc.Path = existing
	if reanchorToManagedBase(&fc, filesPath) || fc.Path != existing {
		t.Fatal("re-anchored a folder whose marker is still present")
	}
}

func TestMoveToManagedBase(t *testing.T) {
	for _, paused := range []bool{false, true} {
		base := setTestManagedBase(t)
		legacy := filepath.Join(t.TempDir(), "test")
		writeFolderWithMarker(t, legacy)
		folder := newFolderBaseTestFolder(t, legacy, paused)

		if external, err := folder.IsExternal(); err != nil || external {
			t.Fatalf("legacy folder considered external: %v, %v", external, err)
		}
		if err := folder.MoveToManagedBase(); err != nil {
			t.Fatal(err)
		}

		managed := filepath.Join(base, "test")
		if folder.Path() != managed || !folder.IsInManagedBase() {
			t.Fatalf("configuration not updated: %q", folder.Path())
		}
		if folder.IsPaused() != paused {
			t.Fatalf("paused state not restored, expected %v", paused)
		}
		if data, err := os.ReadFile(filepath.Join(managed, "file.txt")); err != nil || string(data) != "hello" {
			t.Fatalf("contents not moved: %q, %v", data, err)
		}
		if _, err := os.Lstat(legacy); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("legacy location still exists")
		}
		if external, err := folder.IsExternal(); err != nil || external {
			t.Fatalf("moved folder considered external: %v, %v", external, err)
		}
		if err := folder.MoveToManagedBase(); err != nil {
			t.Fatalf("second move is not a no-op: %v", err)
		}
		if _, err := os.Lstat(resumeAfterMovePath("test")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("marker of the move was left behind")
		}
	}
}

func TestMoveToManagedBaseRecoversInterruptedMove(t *testing.T) {
	for _, pausedBefore := range []bool{false, true} {
		base := setTestManagedBase(t)
		managed := filepath.Join(base, "test")
		legacy := filepath.Join(t.TempDir(), "test")
		filesPath := filepath.Dir(legacy)
		writeResumeMarker := func() {
			if !pausedBefore {
				if err := os.WriteFile(resumeAfterMovePath("test"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}

		// The app stopped right after the rename: the saved configuration has the folder paused, at its old location
		writeFolderWithMarker(t, managed)
		writeResumeMarker()

		// At launch, the folder is re-anchored, and resumed unless it was paused before the move
		fc := config.FolderConfiguration{ID: "test", Path: legacy, MarkerName: config.DefaultMarkerName, Paused: true}
		if !recoverManagedFolder(&fc, filesPath) || fc.Path != managed || fc.Paused != pausedBefore {
			t.Fatalf("not recovered after interrupted move: %q, paused %v", fc.Path, fc.Paused)
		}
		if _, err := os.Lstat(resumeAfterMovePath("test")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("marker of the move was left behind")
		}

		// Moving again also completes the move
		writeResumeMarker()
		folder := newFolderBaseTestFolder(t, legacy, true)
		if err := folder.MoveToManagedBase(); err != nil {
			t.Fatal(err)
		}
		if !folder.IsInManagedBase() || folder.IsPaused() != pausedBefore {
			t.Fatalf("not recovered after interrupted move: %q, paused %v", folder.Path(), folder.IsPaused())
		}
		if _, err := os.Lstat(resumeAfterMovePath("test")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("marker of the move was left behind")
		}
	}

	// The app stopped after pausing the folder, before the rename
	base := setTestManagedBase(t)
	legacy := filepath.Join(t.TempDir(), "test")
	writeFolderWithMarker(t, legacy)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resumeAfterMovePath("test"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fc := config.FolderConfiguration{ID: "test", Path: legacy, MarkerName: config.DefaultMarkerName, Paused: true}
	if !recoverManagedFolder(&fc, filepath.Dir(legacy)) || fc.Path != legacy || fc.Paused {
		t.Fatalf("not resumed after interrupted move: %q, paused %v", fc.Path, fc.Paused)
	}
}

func TestMoveToManagedBaseRefusals(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "test")
	writeFolderWithMarker(t, legacy)
	folder := newFolderBaseTestFolder(t, legacy, false)
	if err := folder.MoveToManagedBase(); !errors.Is(err, ErrNoManagedFolderBase) {
		t.Fatalf("moved without a managed base: %v", err)
	}

	base := setTestManagedBase(t)
	if err := os.MkdirAll(filepath.Join(base, "test"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := folder.MoveToManagedBase(); !errors.Is(err, ErrMoveTargetOccupied) {
		t.Fatalf("moved onto an existing directory: %v", err)
	}
	if folder.Path() != legacy || folder.IsPaused() {
		t.Fatal("refused move changed the configuration")
	}

	if err := folder.SetPath(filepath.Join(t.TempDir(), "external")); err != nil {
		t.Fatal(err)
	}
	if err := folder.MoveToManagedBase(); !errors.Is(err, ErrFolderIsExternal) {
		t.Fatalf("moved an external folder: %v", err)
	}
}

func TestRemoveSuperfluousSelectionEntriesTakesSelectionLock(t *testing.T) {
	folder := newFeedTestFolder(t, "*\n")
	unlock := lockSelection(folder.FolderID)
	done := make(chan error, 1)
	go func() { done <- folder.RemoveSuperfluousSelectionEntries() }()

	select {
	case <-done:
		unlock()
		t.Fatal("selection entries were changed while the selection was locked")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("selection lock was not released")
	}
}
