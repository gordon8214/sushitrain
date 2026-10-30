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
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/protocol"
)

// newHomeFoldersClient is a client of the Mac app outside the sandbox, whose home directory is a new temporary directory
// and whose engine data is in its Library, with these folders and default folder path
func newHomeFoldersClient(t *testing.T, defaultFolderPath string, folders ...config.FolderConfiguration) (*Client, string) {
	t.Helper()
	home := t.TempDir()
	// Syncthing expands ~ with HOME, which is the files path in the app
	t.Setenv("HOME", home)
	setEngineDataDirectory(t, filepath.Join(home, "Library", "Application Support", "Synctrain"))
	cfg := config.New(protocol.EmptyDeviceID)
	cfg.Defaults.Folder.Path = defaultFolderPath
	for _, folder := range folders {
		folder.Path = strings.Replace(folder.Path, "$HOME", home, 1)
		if folder.FilesystemType == "" {
			folder.FilesystemType = config.FilesystemTypeBasic
		}
		cfg.Folders = append(cfg.Folders, folder)
	}
	wrapper := config.Wrap(filepath.Join(t.TempDir(), "config.xml"), cfg, protocol.EmptyDeviceID, events.NoopLogger)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = wrapper.Serve(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return (&Client{filesPath: home, folderLayout: homeFolders}).withTestRun(wrapper, nil, nil), home
}

// setEngineDataDirectory points the configuration and database directories at dir for a test
func setEngineDataDirectory(t *testing.T, dir string) {
	t.Helper()
	for _, base := range []locations.BaseDirEnum{locations.ConfigBaseDir, locations.DataBaseDir} {
		before := locations.GetBaseDir(base)
		if err := locations.SetBaseDir(base, dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = locations.SetBaseDir(base, before) })
	}
}

func TestFolderDirectoryNames(t *testing.T) {
	for name, want := range map[string]string{
		"Photos":          "Photos",
		"  Work stuff  ":  "Work stuff",
		".ssh":            "ssh",
		"..":              "",
		".":               "",
		"../x":            "-x",
		"a/b:c\\d":        "a-b-c-d",
		"tab\there\x00":   "tabhere",
		"   ":             "",
		"Fotos über 2026": "Fotos über 2026",
		". .ssh":          "ssh",
		". .config":       "config",
		". .":             "",
		". ..":            "",
		". . .":           "",
		"\u00a0.vim":      "vim",
		"evil\u202egpj":   "evilgpj",
		"zero\u200bwidth": "zerowidth",
		"trailing. ":      "trailing.",
	} {
		if got := folderDirectoryName(name); got != want {
			t.Errorf("%q: got %q, want %q", name, got, want)
		}
	}
	long := folderDirectoryName(strings.Repeat("é", 200))
	if len(long) > 240 || !utf8.ValidString(long) || long == "" {
		t.Errorf("long name cut to %d bytes, valid %v", len(long), utf8.ValidString(long))
	}
}

func TestNewMacFoldersAreDirectChildrenOfTheirBase(t *testing.T) {
	clt, home := newHomeFoldersClient(t, "~")
	for _, label := range []string{". .ssh", ". ..", "..", "a/../../b", strings.Repeat("x", 300)} {
		got, err := clt.defaultFolderPath("id", label)
		if err != nil {
			t.Errorf("%q: %v", label, err)
			continue
		}
		if filepath.Dir(got) != home || strings.HasPrefix(filepath.Base(got), ".") {
			t.Errorf("%q: got %q", label, got)
		}
	}
}

func TestNewMacFoldersWithUnusualDefaultFolderPaths(t *testing.T) {
	for defaultPath, parent := range map[string]string{"~/Folders/": "Folders", "Sync": "Sync", "./x": "Sync"} {
		clt, home := newHomeFoldersClient(t, defaultPath)
		if got, err := clt.defaultFolderPath("id", "Photos"); err != nil || got != filepath.Join(home, parent, "Photos") {
			t.Errorf("%q: got %q, %v", defaultPath, got, err)
		}
	}
	// A file where the folder would go is taken, like a directory
	clt, home := newHomeFoldersClient(t, "~")
	if err := os.WriteFile(filepath.Join(home, "Photos"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := clt.defaultFolderPath("id", "Photos"); err != nil || got != filepath.Join(home, "Photos 2") {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestNewMacFoldersRunOutOfNumbers(t *testing.T) {
	clt, home := newHomeFoldersClient(t, "~")
	for number := 1; number <= maxNumberedFolderNames; number++ {
		name := "Photos"
		if number > 1 {
			name += " " + strconv.Itoa(number)
		}
		if err := os.Mkdir(filepath.Join(home, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := clt.defaultFolderPath("id", "Photos"); !errors.Is(err, ErrNoDefaultFolderPath) {
		t.Fatalf("got %v", err)
	}
}

func TestNewMacFoldersGoInSyncByDefault(t *testing.T) {
	clt, home := newHomeFoldersClient(t, "")
	got, err := clt.defaultFolderPath("abcd-1234", "Photos")
	if err != nil || got != filepath.Join(home, "Sync", "Photos") {
		t.Fatalf("got %q, %v", got, err)
	}
	// Without a label, the ID names the directory
	if got, err := clt.defaultFolderPath("abcd-1234", ""); err != nil || got != filepath.Join(home, "Sync", "abcd-1234") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestNewMacFoldersFollowTheDefaultFolderPath(t *testing.T) {
	for defaultPath, parent := range map[string]string{"~": "", "~/Folders": "Folders", "$HOME/Elsewhere": "Elsewhere"} {
		clt, home := newHomeFoldersClient(t, "")
		if err := clt.changeConfiguration(func(cfg *config.Configuration) {
			cfg.Defaults.Folder.Path = strings.Replace(defaultPath, "$HOME", home, 1)
		}); err != nil {
			t.Fatal(err)
		}
		if got, err := clt.defaultFolderPath("id", "Photos"); err != nil || got != filepath.Join(home, parent, "Photos") {
			t.Errorf("%s: got %q, %v", defaultPath, got, err)
		}
	}
}

func TestNewMacFoldersNeverAdoptAnExistingDirectory(t *testing.T) {
	clt, home := newHomeFoldersClient(t, "~")
	for _, name := range []string{"Documents", "Documents 2"} {
		if err := os.Mkdir(filepath.Join(home, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// As another device may offer a folder with any ID
	if got, err := clt.defaultFolderPath("Documents", ""); err != nil || got != filepath.Join(home, "Documents 3") {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := clt.defaultFolderPath(".ssh", ""); err != nil || got != filepath.Join(home, "ssh") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestNewMacFoldersStayOutOfTheEngineData(t *testing.T) {
	clt, home := newHomeFoldersClient(t, "~")
	if got, err := clt.defaultFolderPath("Library", ""); err != nil || got != filepath.Join(home, "Library 2") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestNewMacFoldersAreNotPutInsideOtherFolders(t *testing.T) {
	// Syncthing's own default folder is ~/Sync
	clt, home := newHomeFoldersClient(t, "", config.FolderConfiguration{ID: "default", Path: "$HOME/sync"})
	if got, err := clt.defaultFolderPath("id", "Photos"); err != nil || got != filepath.Join(home, "Photos") {
		t.Fatalf("got %q, %v", got, err)
	}
	// Nor where another folder is, or around one
	clt, home = newHomeFoldersClient(t, "~", config.FolderConfiguration{ID: "photos", Path: "~/Photos/Mine"})
	if got, err := clt.defaultFolderPath("id", "Photos"); err != nil || got != filepath.Join(home, "Photos 2") {
		t.Fatalf("got %q, %v", got, err)
	}
	// And when the home directory is itself a folder, there is no default location
	clt, _ = newHomeFoldersClient(t, "", config.FolderConfiguration{ID: "home", Path: "~"})
	if _, err := clt.defaultFolderPath("id", "Photos"); !errors.Is(err, ErrNoDefaultFolderPath) {
		t.Fatalf("got %v", err)
	}
}

func TestNewMacFolderNeedsAUsableName(t *testing.T) {
	clt, _ := newHomeFoldersClient(t, "")
	if _, err := clt.defaultFolderPath("..", " . "); !errors.Is(err, errUnsafeFolderID) {
		t.Fatalf("got %v", err)
	}
}

func TestOverlappingFolders(t *testing.T) {
	clt, home := newHomeFoldersClient(t, "", config.FolderConfiguration{ID: "work", Path: "~/Work"},
		config.FolderConfiguration{ID: "photos", Path: "$HOME/Library/Photos", FilesystemType: config.FilesystemType("photos")})
	for p, want := range map[string]string{
		filepath.Join(home, "Work"):          "work",
		filepath.Join(home, "work", "Notes"): "work",
		home:                                 "work",
		"~/Work/":                            "work",
		filepath.Join(home, "Workshop"):      "",
		filepath.Join(home, "Library"):       "",
		// The same name in the other Unicode normalization
		filepath.Join(home, "Work", "Cafe\u0301"): "work",
	} {
		if got := clt.OverlappingFolderID(p); got != want {
			t.Errorf("%s: got %q, want %q", p, got, want)
		}
	}
}

func TestMacFoldersAreNeverExternal(t *testing.T) {
	clt, _ := newHomeFoldersClient(t, "", config.FolderConfiguration{ID: "work", Path: "/Volumes/Other/Work"})
	folder := &Folder{client: clt, FolderID: "work"}
	if external, err := folder.IsExternal(); err != nil || external {
		t.Fatalf("external %v, %v", external, err)
	}
}

func TestMacFolderFilesAreRemovableOnlyInsideSync(t *testing.T) {
	clt, home := newHomeFoldersClient(t, "~",
		config.FolderConfiguration{ID: "photos", Path: "~/Sync/Photos"},
		config.FolderConfiguration{ID: "nested", Path: "~/Sync/A/B"},
		config.FolderConfiguration{ID: "slash", Path: "~/Sync/Slash/"},
		config.FolderConfiguration{ID: "case", Path: "$HOME/sync/Case"},
		config.FolderConfiguration{ID: "documents", Path: "~/Documents"},
		config.FolderConfiguration{ID: "escape", Path: "~/Sync/../Documents/Escape"},
		config.FolderConfiguration{ID: "library", Path: "~/Library"},
		config.FolderConfiguration{ID: "elsewhere", Path: "/Volumes/Other/Work"},
		config.FolderConfiguration{ID: "link", Path: "~/Sync/Link"},
	)
	// A link in ~/Sync to a directory elsewhere
	for _, dir := range []string{"Sync", "Elsewhere"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, "Elsewhere"), filepath.Join(home, "Sync", "Link")); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{
		"photos": true, "nested": true, "slash": true, "case": true, "documents": false, "escape": false,
		"library": false, "elsewhere": false, "link": false,
	} {
		if got := (&Folder{client: clt, FolderID: id}).CanRemoveFiles(); got != want {
			t.Errorf("%s: got %v, want %v", id, got, want)
		}
	}
}

func TestTheHomeDirectoryAndItsParentsAreNeverRemoved(t *testing.T) {
	for _, folderPath := range []string{"~", "/", "$HOME/..", "~/Library/Application Support"} {
		clt, _ := newHomeFoldersClient(t, "", config.FolderConfiguration{ID: "folder", Path: folderPath})
		if (&Folder{client: clt, FolderID: "folder"}).CanRemoveFiles() {
			t.Errorf("%s can be removed", folderPath)
		}
	}
}

func TestFolderFilesInsideOrAroundAnotherFolderAreKept(t *testing.T) {
	clt, _ := newHomeFoldersClient(t, "",
		config.FolderConfiguration{ID: "sync", Path: "~/Sync"},
		config.FolderConfiguration{ID: "photos", Path: "~/Sync/Photos"},
		config.FolderConfiguration{ID: "outer", Path: "~/Sync/Outer"},
		config.FolderConfiguration{ID: "inner", Path: "~/Sync/Outer/Inner"},
	)
	for _, id := range []string{"photos", "outer", "inner"} {
		if (&Folder{client: clt, FolderID: id}).CanRemoveFiles() {
			t.Errorf("%s: its files may belong to another folder", id)
		}
	}
}

func TestMacFolderInsideSyncIsRemoved(t *testing.T) {
	clt, home := newHomeFoldersClient(t, "", config.FolderConfiguration{ID: "photos", Path: "~/Sync/Photos"})
	photos := filepath.Join(home, "Sync", "Photos")
	if err := os.MkdirAll(photos, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(photos, "a.jpg"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	folder := &Folder{client: clt, FolderID: "photos"}
	if err := folder.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(photos); !os.IsNotExist(err) {
		t.Fatalf("folder left: %v", err)
	}
	if folder.Exists() {
		t.Fatal("folder still configured")
	}
}

func TestRemoveChecksTheDirectorySyncthingRemoves(t *testing.T) {
	// With HOME elsewhere (as in a test run that sets only SYNCTRAIN_TEST_ROOT), ~ means two different directories
	clt, _ := newHomeFoldersClient(t, "", config.FolderConfiguration{ID: "photos", Path: "~/Sync/Photos"})
	other := t.TempDir()
	t.Setenv("HOME", other)
	photos := filepath.Join(other, "Sync", "Photos")
	if err := os.MkdirAll(photos, 0o700); err != nil {
		t.Fatal(err)
	}
	folder := &Folder{client: clt, FolderID: "photos"}
	if err := folder.Remove(); !errors.Is(err, ErrFolderFilesNotRemovable) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(photos); err != nil {
		t.Fatal(err)
	}
}

func TestContainerFolderFilesAreRemovableAtDefaultLocations(t *testing.T) {
	filesPath := t.TempDir()
	internal := newFolderBaseTestFolderWithID(t, "abc", filepath.Join(filesPath, "abc"), false)
	if !internal.CanRemoveFiles() {
		t.Error("cannot remove a folder at the default location")
	}
	// Named like the folder, but not in the files directory
	external := newFolderBaseTestFolderWithID(t, "abc", filepath.Join(filesPath, "abc"), false)
	external.client.filesPath = t.TempDir()
	if external.CanRemoveFiles() {
		t.Error("can remove another app's folder")
	}
	if err := external.Remove(); !errors.Is(err, ErrFolderFilesNotRemovable) {
		t.Errorf("removed another app's folder: %v", err)
	}
	// IDs that name the files directory or its parent
	for id, folderPath := range map[string]string{".": filesPath, "..": filepath.Dir(filesPath)} {
		folder := newFolderBaseTestFolderWithID(t, id, folderPath, false)
		folder.client.filesPath = filesPath
		if folder.CanRemoveFiles() {
			t.Errorf("%q: can remove the files directory or its parent", id)
		}
	}
}

func TestRemoveRefusesFoldersOutsideSync(t *testing.T) {
	clt, home := newHomeFoldersClient(t, "~", config.FolderConfiguration{ID: "documents", Path: "~/Documents"})
	documents := filepath.Join(home, "Documents")
	if err := os.MkdirAll(documents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(documents, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	folder := &Folder{client: clt, FolderID: "documents"}
	if err := folder.Remove(); !errors.Is(err, ErrFolderFilesNotRemovable) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(documents, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	if !folder.Exists() {
		t.Fatal("the folder was unlinked")
	}
}
