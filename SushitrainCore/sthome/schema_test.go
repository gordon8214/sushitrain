// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sthome

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/syncthing"
)

// The schema check must know the pinned engine's schema version: this fails when an engine upgrade changes it
func TestSchemaCheckMatchesEngine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "index-v2")
	if err := CheckSchema(dir); err != nil {
		t.Fatalf("a missing database: %v", err)
	}
	sdb, err := syncthing.OpenDatabase(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sdb.Close()
	version, err := SchemaVersion(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	if version != SupportedSchemaVersion {
		t.Fatalf("the engine writes schema %d; update SupportedSchemaVersion (%d)", version, SupportedSchemaVersion)
	}
	if err := CheckSchema(dir); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open(sqliteDriver, "file:"+filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schemamigrations (schema_version, applied_at, syncthing_version) VALUES (?, 0, 'v9')`,
		SupportedSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := CheckSchema(dir); err == nil {
		t.Fatal("a newer schema was accepted")
	}
}
