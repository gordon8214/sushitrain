// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/protocol"
)

// newRelocationClient is a client with a folder in a sandboxed install's container, whose files are there
func newRelocationClient(t *testing.T, paused bool) (*Client, string, string) {
	t.Helper()
	clt, home := newHomeFoldersClient(t, "", config.FolderConfiguration{ID: "photos",
		Path: "$HOME/Library/Containers/test/Data/Documents/photos", MarkerName: config.DefaultMarkerName, Paused: paused})
	if err := os.MkdirAll(locations.GetBaseDir(locations.ConfigBaseDir), 0o700); err != nil {
		t.Fatal(err)
	}
	from := filepath.Join(home, "Library", "Containers", "test", "Data", "Documents", "photos")
	writeFolderWithMarker(t, from)
	return clt, home, from
}

func TestRelocateMovesTheFilesAndTheFolder(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "paused"}[paused], func(t *testing.T) {
			clt, home, from := newRelocationClient(t, paused)
			to := filepath.Join(home, "Sync", "Photos")
			folder := &Folder{client: clt, FolderID: "photos"}
			if err := folder.Relocate(to); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(to, "file.txt")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(from); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the files are still at the old place: %v", err)
			}
			if folder.Path() != to || folder.IsPaused() != paused {
				t.Fatalf("folder at %s, paused %v", folder.Path(), folder.IsPaused())
			}
			if _, err := os.Stat(relocationJournalPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the journal is left: %v", err)
			}
		})
	}
}

func TestRelocateRefusesTargetsItMustNotUse(t *testing.T) {
	clt, home, from := newRelocationClient(t, false)
	if err := os.MkdirAll(filepath.Join(home, "Taken"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A link that leads into the engine's data
	if err := os.Symlink(locations.GetBaseDir(locations.ConfigBaseDir), filepath.Join(home, "Linked")); err != nil {
		t.Fatal(err)
	}
	if err := clt.changeConfiguration(func(cfg *config.Configuration) {
		cfg.SetFolder(config.FolderConfiguration{ID: "work", Path: filepath.Join(home, "Work"),
			FilesystemType: config.FilesystemTypeBasic})
	}); err != nil {
		t.Fatal(err)
	}
	folder := &Folder{client: clt, FolderID: "photos"}
	for target, want := range map[string]error{
		filepath.Join(home, "Taken"):          ErrMoveTargetOccupied,
		"Sync/Photos":                         ErrRelocationTargetInvalid,
		"":                                    ErrRelocationTargetInvalid,
		filepath.Join(from, "inside"):         ErrRelocationTargetInvalid,
		filepath.Join(home, "Work", "Photos"): ErrRelocationTargetInvalid,
		filepath.Join(home, "Library", "Application Support"): ErrFolderOverlapsEngineData,
		filepath.Join(home, "Linked", "Photos"):               ErrFolderOverlapsEngineData,
	} {
		if err := folder.Relocate(target); !errors.Is(err, want) {
			t.Errorf("%q: got %v, want %v", target, err, want)
		}
	}
	if folder.Path() != from || folder.IsPaused() {
		t.Fatalf("folder at %s, paused %v", folder.Path(), folder.IsPaused())
	}
	if _, err := os.Stat(filepath.Join(from, "file.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(relocationJournalPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a journal is left: %v", err)
	}
}

func TestRelocateNeedsTheFolderInPlace(t *testing.T) {
	clt, home, from := newRelocationClient(t, false)
	if err := os.RemoveAll(filepath.Join(from, config.DefaultMarkerName)); err != nil {
		t.Fatal(err)
	}
	folder := &Folder{client: clt, FolderID: "photos"}
	if err := folder.Relocate(filepath.Join(home, "Photos")); !errors.Is(err, ErrRelocationSourceInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestRelocateWaitsForAPendingMove(t *testing.T) {
	clt, home, from := newRelocationClient(t, false)
	if err := writeRelocations(relocationJournalPath(), []folderRelocation{
		{FolderID: "photos", From: from, To: filepath.Join(home, "Elsewhere")},
	}); err != nil {
		t.Fatal(err)
	}
	folder := &Folder{client: clt, FolderID: "photos"}
	if err := folder.Relocate(filepath.Join(home, "Photos")); !errors.Is(err, ErrRelocationPending) {
		t.Fatalf("got %v", err)
	}
	if relocations, err := readRelocations(relocationJournalPath()); err != nil || len(relocations) != 1 ||
		relocations[0].To != filepath.Join(home, "Elsewhere") {
		t.Fatalf("journal %v, %v", relocations, err)
	}
}

func TestADamagedJournalHoldsNoMoveUp(t *testing.T) {
	for name, contents := range map[string]string{"empty": "", "damaged": "[{\"folderID\": "} {
		t.Run(name, func(t *testing.T) {
			clt, home, _ := newRelocationClient(t, false)
			if err := os.WriteFile(relocationJournalPath(), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			folder := &Folder{client: clt, FolderID: "photos"}
			if err := folder.Relocate(filepath.Join(home, "Photos")); err != nil {
				t.Fatal(err)
			}
			aside, _ := filepath.Glob(relocationJournalPath() + ".broken-*")
			if (name == "damaged") != (len(aside) == 1) {
				t.Fatalf("set aside: %v", aside)
			}
		})
	}
}

func TestRelocateIntoNamesTheDirectoryAfterTheFolder(t *testing.T) {
	clt, home, from := newRelocationClient(t, false)
	if err := clt.changeConfiguration(func(cfg *config.Configuration) {
		fc, _, _ := cfg.Folder("photos")
		fc.Label = "Holiday photos"
		cfg.SetFolder(fc)
	}); err != nil {
		t.Fatal(err)
	}
	folder := &Folder{client: clt, FolderID: "photos"}
	// Already there
	if to, err := folder.RelocateInto(filepath.Dir(from)); err != nil || to != from || folder.Path() != from {
		t.Fatalf("moved to %q, %v", to, err)
	}
	base := filepath.Join(home, "Sync")
	if err := os.MkdirAll(filepath.Join(base, "Holiday photos"), 0o700); err != nil {
		t.Fatal(err)
	}
	to, err := folder.RelocateInto(base)
	if err != nil || to != filepath.Join(base, "Holiday photos 2") || folder.Path() != to {
		t.Fatalf("moved to %q (folder at %q), %v", to, folder.Path(), err)
	}
	if _, err := folder.RelocateInto("relative"); !errors.Is(err, ErrRelocationTargetInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestInterruptedRelocationsAreFinished(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, relocationJournalName)
	moved := filepath.Join(dir, "moved")
	writeFolderWithMarker(t, filepath.Join(dir, "new"))
	writeFolderWithMarker(t, filepath.Join(dir, "unmoved"))
	writeFolderWithMarker(t, filepath.Join(dir, "saved"))
	writeFolderWithMarker(t, filepath.Join(dir, "elsewhere"))
	relocations := []folderRelocation{
		// Moved, but the folder still points at the old place
		{FolderID: "moved", From: moved, To: filepath.Join(dir, "new")},
		// Not moved yet
		{FolderID: "unmoved", From: filepath.Join(dir, "unmoved"), To: filepath.Join(dir, "target")},
		// Moved and saved, and resumed by the user since: as it is
		{FolderID: "saved", From: filepath.Join(dir, "old"), To: filepath.Join(dir, "saved"), Paused: true},
		// Pointed elsewhere since: as it is
		{FolderID: "changed", From: filepath.Join(dir, "before"), To: filepath.Join(dir, "after")},
		{FolderID: "removed", From: filepath.Join(dir, "a"), To: filepath.Join(dir, "b")},
	}
	if err := writeRelocations(journal, relocations); err != nil {
		t.Fatal(err)
	}
	conf := config.New(protocol.EmptyDeviceID)
	for _, folder := range []config.FolderConfiguration{
		{ID: "moved", Path: moved, Paused: true},
		{ID: "unmoved", Path: filepath.Join(dir, "unmoved"), Paused: true},
		{ID: "saved", Path: filepath.Join(dir, "saved")},
		{ID: "changed", Path: filepath.Join(dir, "elsewhere"), Paused: true},
	} {
		folder.MarkerName = config.DefaultMarkerName
		conf.SetFolder(folder)
	}

	changed, handled, err := finishRelocations(&conf, journal, dir)
	if err != nil || !changed || len(handled) != len(relocations) {
		t.Fatalf("changed %v, handled %d, %v", changed, len(handled), err)
	}
	for id, want := range map[string]struct {
		path   string
		paused bool
	}{
		"moved": {filepath.Join(dir, "new"), false}, "unmoved": {filepath.Join(dir, "unmoved"), false},
		"saved": {filepath.Join(dir, "saved"), false}, "changed": {filepath.Join(dir, "elsewhere"), true},
	} {
		fc, _, _ := conf.Folder(id)
		if fc.Path != want.path || fc.Paused != want.paused {
			t.Errorf("%s: at %s, paused %v", id, fc.Path, fc.Paused)
		}
	}
	// Nothing left to do
	if changed, _, err := finishRelocations(&conf, filepath.Join(dir, "none.json"), dir); err != nil || changed {
		t.Fatalf("changed %v, %v", changed, err)
	}
}

func TestLoadingFinishesAnInterruptedFolderMove(t *testing.T) {
	for _, remoteManaged := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote-managed"}[remoteManaged], func(t *testing.T) {
			dir := t.TempDir()
			setEngineDataDirectory(t, dir)
			cert := testIdentity(t, dir)
			cfgPath := filepath.Join(dir, "config.xml")
			// Paused for the move, and moved, when the app stopped
			newPath := filepath.Join(dir, "new")
			writeFolderWithMarker(t, newPath)
			conf := config.New(protocolID(cert))
			conf.SetFolder(config.FolderConfiguration{ID: "photos", Path: filepath.Join(dir, "old"), Paused: true,
				MarkerName: config.DefaultMarkerName, FilesystemType: config.FilesystemTypeBasic})
			if err := config.Wrap(cfgPath, conf, protocolID(cert), events.NoopLogger).Save(); err != nil {
				t.Fatal(err)
			}
			if err := writeRelocations(relocationJournalPath(), []folderRelocation{
				{FolderID: "photos", From: filepath.Join(dir, "old"), To: newPath},
			}); err != nil {
				t.Fatal(err)
			}
			managed := &remoteManagedSet{}
			if remoteManaged {
				managed.folders = []string{"photos"}
			}

			if _, _, err := loadEngineConfig(cfgPath, cert, events.NoopLogger, t.TempDir(), managed, false); err != nil {
				t.Fatal(err)
			}
			saved, _, err := config.Load(cfgPath, protocolID(cert), events.NoopLogger)
			if err != nil {
				t.Fatal(err)
			}
			// A remote-managed folder stays paused
			if fc, ok := saved.Folder("photos"); !ok || fc.Path != newPath || fc.Paused != remoteManaged {
				t.Fatalf("folder at %s, paused %v", fc.Path, fc.Paused)
			}
			if _, err := os.Stat(relocationJournalPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the journal is left: %v", err)
			}
		})
	}
}
