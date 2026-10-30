// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
)

// writeCustomConfig writes a configuration whose settings differ from what the app would choose, with one folder
// outside the app's files directory, and that folder's marker at its place in the files directory (from which iOS
// moves the folder back into the container)
func writeCustomConfig(t *testing.T, dir, filesPath string) string {
	t.Helper()
	cert := testIdentity(t, dir)
	cfg := config.New(protocolID(cert))
	cfg.GUI.Enabled = true
	// Not one that something on this computer uses, which would move it
	cfg.GUI.RawAddress = freeGUIAddress(t)
	cfg.Options.ProgressUpdateIntervalS = 5
	cfg.Options.URAccepted = -1
	cfg.Options.CREnabled = true
	cfg.Options.RelayReconnectIntervalM = 10
	cfg.Options.ReleasesURL = "https://upgrades.example.org/meta.json"
	cfg.Defaults.Folder.IgnorePerms = false
	cfg.Defaults.Folder.RescanIntervalS = 60
	cfg.Defaults.Folder.FSWatcherEnabled = false
	cfg.Defaults.Folder.BlockIndexing = true
	folder := cfg.Defaults.Folder.Copy()
	folder.ID = "docs"
	folder.Path = filepath.Join(t.TempDir(), "docs")
	folder.ScanProgressIntervalS = 0
	cfg.Folders = []config.FolderConfiguration{folder}
	if err := os.MkdirAll(filepath.Join(filesPath, "docs", ".stfolder"), 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "config.xml")
	fd, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if err := cfg.WriteXML(fd); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMacOSUsesAnExistingConfigurationAsItIs(t *testing.T) {
	dir, filesPath := t.TempDir(), t.TempDir()
	path := writeCustomConfig(t, dir, filesPath)
	writeReportingMarker(dir)
	before := stateOf(t, path)

	cfg, _, err := loadEngineConfig(path, testIdentity(t, dir), events.NoopLogger, filesPath, &remoteManagedSet{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if after := stateOf(t, path); after != before {
		t.Fatal("the configuration was written")
	}
	opts, defaults := cfg.Options(), cfg.DefaultFolder()
	if opts.ProgressUpdateIntervalS != 5 || opts.URAccepted != -1 || !opts.CREnabled ||
		opts.RelayReconnectIntervalM != 10 || opts.ReleasesURL == "" {
		t.Fatalf("options changed: %+v", opts)
	}
	if defaults.IgnorePerms || defaults.RescanIntervalS != 60 || defaults.FSWatcherEnabled || !defaults.BlockIndexing {
		t.Fatalf("folder defaults changed: %+v", defaults)
	}
	folder, _ := cfg.Folder("docs")
	if folder.ScanProgressIntervalS != 0 || folder.Path == filepath.Join(filesPath, "docs") {
		t.Fatalf("the folder changed: %+v", folder)
	}
	if !cfg.GUI().Enabled {
		t.Fatal("the GUI was turned off")
	}
}

func TestIOSAppliesTheAppSettingsAtEveryStart(t *testing.T) {
	dir, filesPath := t.TempDir(), t.TempDir()
	path := writeCustomConfig(t, dir, filesPath)
	cert := testIdentity(t, dir)

	if _, _, err := loadEngineConfig(path, cert, events.NoopLogger, filesPath, &remoteManagedSet{}, true); err != nil {
		t.Fatal(err)
	}
	cfg := savedConfig(t, path, cert)
	opts, defaults := cfg.Options, cfg.Defaults.Folder
	if opts.ProgressUpdateIntervalS != 1 || opts.URAccepted != -1 || opts.CREnabled ||
		opts.RelayReconnectIntervalM != 1 || opts.ReleasesURL != "" || opts.URURL != "" || opts.CRURL != "" {
		t.Fatalf("options not applied: %+v", opts)
	}
	if !defaults.IgnorePerms || defaults.RescanIntervalS != 3600 || defaults.FSWatcherEnabled || defaults.BlockIndexing {
		t.Fatalf("folder defaults not applied: %+v", defaults)
	}
	folder := cfg.Folders[0]
	if folder.ScanProgressIntervalS != -1 || folder.Path != filepath.Join(filesPath, "docs") {
		t.Fatalf("the folder was not anchored: %+v", folder)
	}
}

func TestNewConfigurationsGetThePlatformDefaults(t *testing.T) {
	for _, ios := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.xml")
		cert := testIdentity(t, dir)
		if _, _, err := loadEngineConfig(path, cert, events.NoopLogger, t.TempDir(), &remoteManagedSet{}, ios); err != nil {
			t.Fatal(err)
		}
		// On macOS the defaults are applied once, so they must be in the file
		cfg := savedConfig(t, path, cert)
		opts, defaults := cfg.Options, cfg.Defaults.Folder
		if opts.CREnabled || opts.URAccepted != -1 || opts.ProgressUpdateIntervalS != 1 || cfg.GUI.Enabled == ios ||
			opts.RelayReconnectIntervalM != 1 || opts.ReleasesURL != "" || opts.URURL != "" || opts.CRURL != "" {
			t.Fatalf("iOS %v: options %+v", ios, opts)
		}
		if defaults.RescanIntervalS != 3600 || defaults.BlockIndexing {
			t.Fatalf("iOS %v: folder defaults %+v", ios, defaults)
		}
		// Permissions sync on macOS, as in Syncthing; iOS doesn't expose them
		if defaults.IgnorePerms != ios || defaults.FSWatcherEnabled == ios {
			t.Fatalf("iOS %v: folder defaults %+v", ios, defaults)
		}
	}
}

// loadWithReporting loads a configuration that has reporting on, as one from Syncthing.app can, and returns its options
// as saved
func loadWithReporting(t *testing.T, dir, filesPath, path string, ios bool) config.OptionsConfiguration {
	t.Helper()
	cert := testIdentity(t, dir)
	cfg := savedConfig(t, path, cert)
	cfg.Options.URAccepted = 3
	cfg.Options.CREnabled = true
	writeConfiguration(t, path, cfg)
	if _, _, err := loadEngineConfig(path, cert, events.NoopLogger, filesPath, &remoteManagedSet{}, ios); err != nil {
		t.Fatal(err)
	}
	return savedConfig(t, path, cert).Options
}

func TestReportingIsOffOnBothPlatformsForAConfigurationFromElsewhere(t *testing.T) {
	for _, ios := range []bool{false, true} {
		dir, filesPath := t.TempDir(), t.TempDir()
		path := writeCustomConfig(t, dir, filesPath)
		if opts := loadWithReporting(t, dir, filesPath, path, ios); opts.URAccepted != -1 || opts.CREnabled {
			t.Fatalf("iOS %v: usage reporting %d, crash reporting %v", ios, opts.URAccepted, opts.CREnabled)
		}
	}
}

func TestMacOSKeepsReportingOnceTheUserChose(t *testing.T) {
	dir, filesPath := t.TempDir(), t.TempDir()
	path := writeCustomConfig(t, dir, filesPath)
	// The first load turns it off; the user turning it on afterwards lasts
	loadWithReporting(t, dir, filesPath, path, false)
	for range 2 {
		if opts := loadWithReporting(t, dir, filesPath, path, false); opts.URAccepted != 3 || !opts.CREnabled {
			t.Fatalf("usage reporting %d, crash reporting %v", opts.URAccepted, opts.CREnabled)
		}
	}
}

func TestIOSTurnsReportingOffAtEveryStart(t *testing.T) {
	dir, filesPath := t.TempDir(), t.TempDir()
	path := writeCustomConfig(t, dir, filesPath)
	writeReportingMarker(dir)
	if opts := loadWithReporting(t, dir, filesPath, path, true); opts.URAccepted != -1 || opts.CREnabled {
		t.Fatalf("usage reporting %d, crash reporting %v", opts.URAccepted, opts.CREnabled)
	}
}

func TestMacOSFillsInWhereToReport(t *testing.T) {
	dir, filesPath := t.TempDir(), t.TempDir()
	path := filepath.Join(dir, "config.xml")
	cert := testIdentity(t, dir)
	if _, _, err := loadEngineConfig(path, cert, events.NoopLogger, filesPath, &remoteManagedSet{}, false); err != nil {
		t.Fatal(err)
	}
	if opts := savedConfig(t, path, cert).Options; opts.URURL != "" || opts.CRURL != "" {
		t.Fatalf("a new configuration has %q and %q", opts.URURL, opts.CRURL)
	}
	opts := loadWithReporting(t, dir, filesPath, path, false)
	if opts.URURL == "" || opts.CRURL == "" || opts.URUniqueID == "" {
		t.Fatalf("reporting has nowhere to go: %q, %q, ID %q", opts.URURL, opts.CRURL, opts.URUniqueID)
	}
}

// savedConfig reads the configuration file as it is on disk
func savedConfig(t *testing.T, path string, cert tls.Certificate) config.Configuration {
	t.Helper()
	cfg, _, err := config.Load(path, protocolID(cert), events.NoopLogger)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.RawCopy()
}

func writeConfiguration(t *testing.T, path string, cfg config.Configuration) {
	t.Helper()
	fd, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if err := cfg.WriteXML(fd); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteManagedFoldersStayPausedOnBothPlatforms(t *testing.T) {
	for _, ios := range []bool{false, true} {
		dir, filesPath := t.TempDir(), t.TempDir()
		path := writeCustomConfig(t, dir, filesPath)
		managed := &remoteManagedSet{folders: []string{"docs"}}
		cfg, _, err := loadEngineConfig(path, testIdentity(t, dir), events.NoopLogger, filesPath, managed, ios)
		if err != nil {
			t.Fatal(err)
		}
		if folder, _ := cfg.Folder("docs"); !folder.Paused {
			t.Fatalf("iOS %v: a remote-managed folder is not paused", ios)
		}
	}
}
