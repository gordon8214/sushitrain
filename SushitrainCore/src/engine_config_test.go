// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
package sushitrain

import (
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/syncthing"

	"t-shaped.nl/sushitrain/v2/gateway"
	"t-shaped.nl/sushitrain/v2/sthome"
)

// TestMain points every Syncthing location at a temporary directory, so that no test can touch the real Syncthing home
// of the user running it
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "sushitrain-test-home")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	for _, base := range []locations.BaseDirEnum{locations.ConfigBaseDir, locations.DataBaseDir,
		locations.UserHomeBaseDir} {
		if err := locations.SetBaseDir(base, home); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// fileState is a file's contents and modification time, to tell whether it was written
type fileState struct {
	data    string
	modTime int64
}

func stateOf(t *testing.T, path string) fileState {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fileState{data: string(data), modTime: info.ModTime().UnixNano()}
}

func testIdentity(t *testing.T, dir string) tls.Certificate {
	t.Helper()
	cert, _, err := sthome.LoadIdentity(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func protocolID(cert tls.Certificate) protocol.DeviceID {
	return protocol.NewDeviceID(cert.Certificate[0])
}

func TestEngineConfigThatCannotBeUsedIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	cert := testIdentity(t, dir)
	path := filepath.Join(dir, "config.xml")
	if err := os.WriteFile(path, []byte("<configuration version=\"52\"><folder id="), 0o600); err != nil {
		t.Fatal(err)
	}
	before := stateOf(t, path)

	if _, err := loadEngineConfig(path, cert, events.NoopLogger, t.TempDir(), &remoteManagedSet{}, false); !errors.Is(err,
		sthome.ErrConfiguration) {
		t.Fatalf("got %v", err)
	}
	if after := stateOf(t, path); after != before {
		t.Fatal("the configuration was written")
	}
}

func TestEngineConfigIsCreatedWithTheGUIOff(t *testing.T) {
	dir := t.TempDir()
	cert := testIdentity(t, dir)
	path := filepath.Join(dir, "config.xml")
	if _, err := loadEngineConfig(path, cert, events.NoopLogger, t.TempDir(), &remoteManagedSet{}, false); err != nil {
		t.Fatal(err)
	}
	saved, _, err := config.Load(path, protocolID(cert), events.NoopLogger)
	if err != nil {
		t.Fatal(err)
	}
	if saved.GUI().Enabled {
		t.Fatal("a new configuration has the GUI on")
	}
	if _, err := os.Stat(path + ".v0"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the new configuration was archived")
	}
}

func TestEngineConfigIsOnlyWrittenWhenItChanges(t *testing.T) {
	for _, ios := range []bool{false, true} {
		dir := t.TempDir()
		cert := testIdentity(t, dir)
		path := filepath.Join(dir, "config.xml")
		if _, err := loadEngineConfig(path, cert, events.NoopLogger, t.TempDir(), &remoteManagedSet{}, ios); err != nil {
			t.Fatal(err)
		}
		before := stateOf(t, path)

		if _, err := loadEngineConfig(path, cert, events.NoopLogger, t.TempDir(), &remoteManagedSet{}, ios); err != nil {
			t.Fatal(err)
		}
		if after := stateOf(t, path); after != before {
			t.Fatalf("iOS %v: an unchanged configuration was written", ios)
		}
	}
}

// newLoadTestClient returns a client whose engine directory is a new temporary directory, as NewClient would set it up
// (without its logging)
func newLoadTestClient(t *testing.T) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	previousData := locations.GetBaseDir(locations.DataBaseDir)
	previousConfig := locations.GetBaseDir(locations.ConfigBaseDir)
	for _, base := range []locations.BaseDirEnum{locations.DataBaseDir, locations.ConfigBaseDir} {
		if err := locations.SetBaseDir(base, dir); err != nil {
			t.Fatal(err)
		}
	}
	clt := &Client{filesPath: t.TempDir()}
	t.Cleanup(func() {
		clt.Stop()
		if clt.appLock != nil {
			_ = clt.appLock.Unlock()
		}
		_ = locations.SetBaseDir(locations.DataBaseDir, previousData)
		_ = locations.SetBaseDir(locations.ConfigBaseDir, previousConfig)
	})
	return clt, dir
}

func TestLoadTouchesNothingWhileAnotherInstanceRuns(t *testing.T) {
	clt, dir := newLoadTestClient(t)
	other := flock.New(filepath.Join(dir, "syncthing.lock"))
	if locked, err := other.TryLock(); err != nil || !locked {
		t.Fatalf("locked %v, %v", locked, err)
	}
	defer other.Unlock()

	if err := clt.Load(false); !errors.Is(err, ErrInstanceRunning) {
		t.Fatalf("got %v", err)
	}
	if problem := clt.StartupProblem(); problem != "" {
		t.Fatalf("reported %q", problem)
	}
	for _, name := range []string{"cert.pem", "key.pem", "config.xml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was created", name)
		}
	}
	for name, clear := range map[string]func() error{
		"database":                 clt.ClearDatabase,
		"legacy database":          clt.ClearLegacyDatabase,
		"migrated legacy database": clt.ClearMigratedLegacyDatabase,
		"identity":                 clt.ClearIdentity,
	} {
		if err := clear(); !errors.Is(err, ErrInstanceRunning) {
			t.Fatalf("the %s was cleared without the lock: %v", name, err)
		}
	}
}

func TestLoadKeepsADamagedConfigurationAndCanSetItAside(t *testing.T) {
	clt, dir := newLoadTestClient(t)
	testIdentity(t, dir)
	path := filepath.Join(dir, "config.xml")
	if err := os.WriteFile(path, []byte("not a configuration"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := stateOf(t, path)

	if err := clt.Load(false); err == nil {
		t.Fatal("loaded a damaged configuration")
	}
	if problem := clt.StartupProblem(); problem != StartupProblemDamagedConfiguration {
		t.Fatalf("reported %q", problem)
	}
	if after := stateOf(t, path); after != before {
		t.Fatal("the damaged configuration was written")
	}

	aside, err := clt.SetConfigurationAside()
	if err != nil {
		t.Fatal(err)
	}
	if stateOf(t, aside).data != before.data {
		t.Fatal("the configuration set aside differs")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the configuration is still in place")
	}
}

func TestOnlyADamagedConfigurationCanBeSetAside(t *testing.T) {
	clt, dir := newLoadTestClient(t)
	testIdentity(t, dir)
	path := filepath.Join(dir, "config.xml")
	if _, err := loadEngineConfig(path, testIdentity(t, dir), events.NoopLogger, t.TempDir(),
		&remoteManagedSet{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := clt.SetConfigurationAside(); err == nil {
		t.Fatal("a working configuration was set aside")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestLoadReportsANewerConfiguration(t *testing.T) {
	clt, dir := newLoadTestClient(t)
	cert := testIdentity(t, dir)
	cfg := config.New(protocolID(cert))
	cfg.Version = config.CurrentVersion + 1
	fd, err := os.Create(filepath.Join(dir, "config.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.WriteXML(fd); err != nil {
		t.Fatal(err)
	}
	fd.Close()

	if err := clt.Load(false); !errors.Is(err, sthome.ErrNewerConfiguration) {
		t.Fatalf("got %v", err)
	}
	if problem := clt.StartupProblem(); problem != StartupProblemNewerConfiguration {
		t.Fatalf("reported %q", problem)
	}
	if _, err := clt.SetConfigurationAside(); err == nil {
		t.Fatal("a newer configuration could be set aside")
	}
}

func TestLoadRefusesAHalfMissingIdentity(t *testing.T) {
	clt, dir := newLoadTestClient(t)
	testIdentity(t, dir)
	if err := os.Remove(filepath.Join(dir, "key.pem")); err != nil {
		t.Fatal(err)
	}
	if err := clt.Load(false); err == nil {
		t.Fatal("loaded without a key")
	}
	if problem := clt.StartupProblem(); problem != StartupProblemDamagedIdentity {
		t.Fatalf("reported %q", problem)
	}
	if _, err := os.Stat(filepath.Join(dir, "key.pem")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a new key was created for the old certificate")
	}
}

// A failure after the configuration loaded must not save it: it is served only once loading succeeded
func TestLoadFailingAfterTheConfigurationLeavesItAlone(t *testing.T) {
	clt, dir := newLoadTestClient(t)
	cert := testIdentity(t, dir)
	path := filepath.Join(dir, "config.xml")
	if _, err := loadEngineConfig(path, cert, events.NoopLogger, t.TempDir(), &remoteManagedSet{}, false); err != nil {
		t.Fatal(err)
	}
	// A database from a newer Syncthing
	dbDir := filepath.Join(dir, "index-v2")
	sdb, err := syncthing.OpenDatabase(dbDir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sdb.Close()
	db, err := sql.Open(gateway.SQLiteDriver, "file:"+filepath.Join(dbDir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schemamigrations (schema_version, applied_at, syncthing_version) VALUES (?, 0, 'v9')`,
		sthome.SupportedSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before := stateOf(t, path)

	if err := clt.Load(false); err == nil {
		t.Fatal("loaded a newer database")
	}
	clt.Stop()
	if after := stateOf(t, path); after != before {
		t.Fatal("the configuration was written")
	}
}

func TestLoadServesTheConfiguration(t *testing.T) {
	clt, _ := newLoadTestClient(t)
	if err := clt.Load(false); err != nil {
		t.Fatal(err)
	}
	if err := clt.changeConfiguration(func(cfg *config.Configuration) { cfg.Options.MaxSendKbps = 7 }); err != nil {
		t.Fatal(err)
	}
	if clt.cfg().Options().MaxSendKbps != 7 {
		t.Fatal("the change was not applied")
	}
}
