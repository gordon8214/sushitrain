// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build engineintegration

package sushitrain

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
)

// Load the actual fresh-install defaults rather than duplicating the app's initial values in fixtures.
func freshFolderDefaults(t *testing.T) config.FolderConfiguration {
	t.Helper()
	dir := t.TempDir()
	cfg, _, err := loadEngineConfig(filepath.Join(dir, "config.xml"), testIdentity(t, dir), events.NoopLogger,
		t.TempDir(), &remoteManagedSet{}, false)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.DefaultFolder()
}

func expectStoredFolderDefaults(t *testing.T, clt *Client, id string, want config.FolderConfiguration) {
	t.Helper()
	matches := func(folder config.FolderConfiguration) bool {
		return folder.BlockIndexing == want.BlockIndexing && folder.SendXattrs == want.SendXattrs &&
			folder.SyncXattrs == want.SyncXattrs
	}
	got, ok := clt.cfg().Folder(id)
	if !ok || !matches(got) {
		t.Fatalf("%s has unexpected indexing or metadata settings: %+v", id, got)
	}
	// Automatic acceptance saves asynchronously through the configuration service.
	waitUntil(t, "the folder defaults are saved", func() bool {
		cfg := savedConfig(t, clt.cfg().ConfigPath(), *clt.cert)
		for _, folder := range cfg.Folders {
			if folder.ID == id {
				return matches(folder)
			}
		}
		return false
	})
}

func TestEngineNewFoldersUseEditableMetadataDefaults(t *testing.T) {
	defaults := freshFolderDefaults(t)
	clt, _, _ := startTestEngineWith(t, func(cfg *config.Configuration) {
		cfg.Defaults.Folder = defaults
		// Like the existing engine fixture, use scans rather than native watchers.
		cfg.Defaults.Folder.FSWatcherEnabled = false
	})
	existing, _ := clt.cfg().Folder("test")
	if err := clt.AddFolder("manual", t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	if err := clt.AddSpecialFolder("special", "basic", t.TempDir(), "sendreceive"); err != nil {
		t.Fatal(err)
	}
	if err := clt.AddFolder("encrypted", t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"manual", "special", "encrypted"} {
		expectStoredFolderDefaults(t, clt, id, defaults)
	}
	if encrypted, _ := clt.cfg().Folder("encrypted"); encrypted.Type != config.FolderTypeReceiveEncrypted || !encrypted.IgnorePerms {
		t.Fatalf("receive-encrypted restrictions changed: %+v", encrypted)
	}

	settings := clt.DefaultFolderSettings()
	settings.BlockIndexing, settings.SendXattrs, settings.SyncXattrs = false, false, false
	if err := clt.SetDefaultFolderSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := clt.AddFolder("customized", t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	expectStoredFolderDefaults(t, clt, "customized", clt.cfg().DefaultFolder())
	for _, id := range []string{"manual", "special", "encrypted"} {
		expectStoredFolderDefaults(t, clt, id, defaults)
	}
	expectStoredFolderDefaults(t, clt, "test", existing)
}

func TestEngineAcceptedSharesUseMetadataDefaults(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("automatic=%v/enabled=%v", automatic, enabled), func(t *testing.T) {
				defaults := freshFolderDefaults(t)
				address := "tcp://" + freeLocalAddress(t)
				sender, _, _ := startTestEngineWith(t, func(cfg *config.Configuration) {
					cfg.Options.RawListenAddresses = []string{address}
				})
				receiver, _, _ := startTestEngineWith(t, func(cfg *config.Configuration) {
					cfg.Folders = nil
					cfg.Defaults.Folder = defaults
					cfg.Defaults.Folder.FSWatcherEnabled = false
					cfg.Defaults.Folder.Path = t.TempDir()
				})
				if !enabled {
					settings := receiver.DefaultFolderSettings()
					settings.BlockIndexing, settings.SendXattrs, settings.SyncXattrs = false, false, false
					if err := receiver.SetDefaultFolderSettings(settings); err != nil {
						t.Fatal(err)
					}
				}
				for _, pair := range [][2]*Client{{sender, receiver}, {receiver, sender}} {
					self, other := pair[0], pair[1]
					if err := self.changeConfiguration(func(cfg *config.Configuration) {
						var addresses []string
						if self == receiver {
							addresses = []string{address}
						}
						cfg.SetDevice(config.DeviceConfiguration{
							DeviceID: protocolID(*other.cert), Addresses: addresses, RawNumConnections: 1,
							AutoAcceptFolders: self == receiver && automatic,
						})
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := sender.FolderWithID("test").ShareWithDevice(receiver.DeviceID(), true, ""); err != nil {
					t.Fatal(err)
				}
				if !automatic {
					waitUntil(t, "the shared folder is offered", func() bool {
						pending, err := receiver.internals().PendingFolders(protocolID(*sender.cert))
						_, offered := pending["test"]
						return err == nil && offered
					})
					if err := receiver.AddFolder("test", "", false); err != nil {
						t.Fatal(err)
					}
					if err := receiver.FolderWithID("test").ShareWithDevice(sender.DeviceID(), true, ""); err != nil {
						t.Fatal(err)
					}
				}
				waitUntil(t, "the shared folder is accepted", func() bool {
					_, ok := receiver.cfg().Folder("test")
					return ok
				})
				expectStoredFolderDefaults(t, receiver, "test", receiver.cfg().DefaultFolder())
			})
		}
	}
}
