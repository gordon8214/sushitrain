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
	filesPath := filepath.Dir(folderPath)
	cfg := config.New(protocol.EmptyDeviceID)
	cfg.Folders = []config.FolderConfiguration{{
		ID:             "test",
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
	return &Folder{client: &Client{config: wrapper, filesPath: filesPath}, FolderID: "test"}
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
	if got := clt.defaultFolderPath("abc"); got != "/documents/abc" {
		t.Fatalf("without managed base: %q", got)
	}
	base := setTestManagedBase(t)
	if got := clt.defaultFolderPath("abc"); got != filepath.Join(base, "abc") {
		t.Fatalf("with managed base: %q", got)
	}
	if !clt.isInternalFolderPath("abc", "/documents/abc") || !clt.isInternalFolderPath("abc", filepath.Join(base, "abc")) {
		t.Fatal("legacy and managed locations must both count as internal")
	}
	if clt.isInternalFolderPath("abc", "/elsewhere/abc") {
		t.Fatal("external location counted as internal")
	}
}

func TestReanchorToManagedBase(t *testing.T) {
	fc := config.FolderConfiguration{ID: "test", Path: "/gone/test", MarkerName: config.DefaultMarkerName}
	if reanchorToManagedBase(&fc) {
		t.Fatal("re-anchored without a managed base")
	}

	base := setTestManagedBase(t)
	if reanchorToManagedBase(&fc) {
		t.Fatal("re-anchored without a marker in the managed location")
	}

	writeFolderWithMarker(t, filepath.Join(base, "test"))
	if !reanchorToManagedBase(&fc) || fc.Path != filepath.Join(base, "test") {
		t.Fatalf("did not re-anchor: %q", fc.Path)
	}
	if reanchorToManagedBase(&fc) {
		t.Fatal("re-anchored a folder already in the managed location")
	}

	existing := filepath.Join(t.TempDir(), "test")
	writeFolderWithMarker(t, existing)
	fc.Path = existing
	if reanchorToManagedBase(&fc) || fc.Path != existing {
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
	}
}

func TestMoveToManagedBaseRecoversInterruptedMove(t *testing.T) {
	base := setTestManagedBase(t)
	legacy := filepath.Join(t.TempDir(), "test")
	writeFolderWithMarker(t, filepath.Join(base, "test"))
	folder := newFolderBaseTestFolder(t, legacy, false)

	if err := folder.MoveToManagedBase(); err != nil {
		t.Fatal(err)
	}
	if !folder.IsInManagedBase() {
		t.Fatal("configuration not repointed after interrupted move")
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
