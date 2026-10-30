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
	"github.com/syncthing/syncthing/lib/protocol"
)

func loadRemoteManagedForTest(t *testing.T, path string) *remoteManagedSet {
	t.Helper()
	set, err := loadRemoteManaged(path)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// wrapRemoteManaged returns a running configuration with the given folders, guarded by set
func wrapRemoteManaged(t *testing.T, set *remoteManagedSet, folders ...config.FolderConfiguration) config.Wrapper {
	t.Helper()
	id := protocol.LocalDeviceID
	cfg := config.New(id)
	cfg.Folders = folders
	set.enforce(&cfg)
	wrapper := config.Wrap(filepath.Join(t.TempDir(), "config.xml"), cfg, id, events.NoopLogger)
	go func() { _ = wrapper.Serve(t.Context()) }()
	wrapper.Subscribe(set)
	return wrapper
}

func modifyFolder(wrapper config.Wrapper, folderID string, change func(*config.FolderConfiguration)) error {
	waiter, err := wrapper.Modify(func(c *config.Configuration) {
		f, _, _ := c.Folder(folderID)
		change(&f)
		c.SetFolder(f)
	})
	if err == nil {
		waiter.Wait()
	}
	return err
}

func TestRemoteManagedFoldersStayPaused(t *testing.T) {
	path := filepath.Join(t.TempDir(), remoteManagedFileName)
	set := loadRemoteManagedForTest(t, path)
	if err := set.set("remote", true); err != nil {
		t.Fatal(err)
	}
	if !loadRemoteManagedForTest(t, path).contains("remote") {
		t.Fatal("the flag was not persisted")
	}

	wrapper := wrapRemoteManaged(t, set,
		config.FolderConfiguration{ID: "remote", Path: t.TempDir()}, config.FolderConfiguration{ID: "local", Path: t.TempDir()})
	if remote, _ := wrapper.Folder("remote"); !remote.Paused {
		t.Fatal("enforce did not pause the remote-managed folder")
	}
	if local, _ := wrapper.Folder("local"); local.Paused {
		t.Fatal("enforce paused an ordinary folder")
	}
	resume := func(folderID string) error {
		return modifyFolder(wrapper, folderID, func(f *config.FolderConfiguration) { f.Paused = false })
	}
	if err := resume("remote"); !errors.Is(err, errRemoteManaged) {
		t.Fatalf("resuming a remote-managed folder: %v", err)
	}
	if err := resume("local"); err != nil {
		t.Fatalf("resuming an ordinary folder: %v", err)
	}
	if err := set.set("remote", false); err != nil {
		t.Fatal(err)
	}
	if err := resume("remote"); err != nil {
		t.Fatalf("resuming after switching back: %v", err)
	}
}

// Only resuming the folder (or adding it unpaused) is refused; other changes pass, also while a flagged folder runs
func TestRemoteManagedRejectsOnlyResumes(t *testing.T) {
	set := loadRemoteManagedForTest(t, filepath.Join(t.TempDir(), remoteManagedFileName))
	wrapper := wrapRemoteManaged(t, set, config.FolderConfiguration{ID: "remote", Path: t.TempDir()})
	if err := set.set("remote", true); err != nil {
		t.Fatal(err)
	}
	// Flagged while running (as when pausing it failed): unrelated changes still pass
	if err := modifyFolder(wrapper, "remote", func(f *config.FolderConfiguration) { f.Label = "Renamed" }); err != nil {
		t.Fatalf("changing a running flagged folder: %v", err)
	}
	waiter, err := wrapper.Modify(func(c *config.Configuration) { c.Options.MaxSendKbps = 100 })
	if err != nil {
		t.Fatalf("changing an option: %v", err)
	}
	waiter.Wait()

	// Removing the folder forgets its flag
	waiter, err = wrapper.Modify(func(c *config.Configuration) { c.Folders = nil })
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
	if set.contains("remote") {
		t.Fatal("the flag of a removed folder was kept")
	}

	// Adding a flagged folder unpaused is a resume
	if err := set.set("added", true); err != nil {
		t.Fatal(err)
	}
	_, err = wrapper.Modify(func(c *config.Configuration) {
		c.SetFolder(config.FolderConfiguration{ID: "added", Path: t.TempDir()})
	})
	if !errors.Is(err, errRemoteManaged) {
		t.Fatalf("adding a flagged folder unpaused: %v", err)
	}
	if !set.contains("added") {
		t.Fatal("the flag of a folder that was not in the configuration was dropped")
	}
}

// A damaged file is kept aside rather than overwritten (the app restores the list from its gateway bindings)
func TestRemoteManagedCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), remoteManagedFileName)
	damaged := []byte(`{"folders": ["remote"`)
	if err := os.WriteFile(path, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := loadRemoteManaged(path)
	if err != nil || len(set.list()) != 0 {
		t.Fatalf("a damaged file: %v, %v", set, err)
	}
	kept, err := os.ReadFile(path + ".corrupt")
	if err != nil || string(kept) != string(damaged) {
		t.Fatalf("the damaged file was not kept aside: %v", err)
	}

	// An unreadable file is an error too, and stays where it is
	unreadable := filepath.Join(t.TempDir(), remoteManagedFileName)
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRemoteManaged(unreadable); err == nil {
		t.Fatal("an unreadable file was accepted")
	}
	if _, err := os.Stat(unreadable + ".corrupt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unreadable file was set aside")
	}
}

// rejectAll refuses every configuration change
type rejectAll struct{}

func (rejectAll) VerifyConfiguration(_, _ config.Configuration) error { return errors.New("rejected") }
func (rejectAll) CommitConfiguration(_, _ config.Configuration) bool  { return true }
func (rejectAll) String() string                                      { return "reject all" }

// When pausing the folder fails, the flag is not left behind
func TestSetFolderRemoteManagedRollsBack(t *testing.T) {
	set := loadRemoteManagedForTest(t, filepath.Join(t.TempDir(), remoteManagedFileName))
	wrapper := wrapRemoteManaged(t, set, config.FolderConfiguration{ID: "remote", Path: t.TempDir()})
	wrapper.Subscribe(rejectAll{})
	clt := (&Client{remoteManaged: set}).withTestRun(wrapper, nil, nil)
	if err := clt.SetFolderRemoteManaged("remote", true); err == nil {
		t.Fatal("the configuration change did not fail")
	}
	if clt.IsFolderRemoteManaged("remote") {
		t.Fatal("the flag was kept although the folder was not paused")
	}
}
