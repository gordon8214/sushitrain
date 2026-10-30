// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sthome

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// SupportedSchemaVersion is the newest database schema the pinned engine knows (internal/db/sqlite
// currentSchemaVersion). A database written by a newer Syncthing is refused rather than opened: the engine would not
// migrate it back.
const SupportedSchemaVersion = 6

// CheckSchema refuses databases a newer Syncthing wrote. A missing database is fine.
func CheckSchema(dbDir string) error {
	entries, err := os.ReadDir(dbDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		version, err := SchemaVersion(filepath.Join(dbDir, entry.Name()))
		if err != nil {
			return fmt.Errorf("reading the schema of %s: %w", entry.Name(), err)
		}
		if version > SupportedSchemaVersion {
			return fmt.Errorf("%s has database schema %d, newer than this engine supports (%d); use a matching Syncthing version",
				entry.Name(), version, SupportedSchemaVersion)
		}
	}
	return nil
}

// SchemaVersion reads the schema version of one database file, without changing it
func SchemaVersion(path string) (int, error) {
	return schemaVersion(path, false)
}

// schemaVersion reads a database's schema version. When immutable, SQLite also creates no write-ahead log or shared
// memory file next to it, and reads only the database file (missing what is still in a write-ahead log).
func schemaVersion(path string, immutable bool) (int, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	query := "mode=ro"
	if immutable {
		query += "&immutable=1"
	}
	uri := url.URL{Scheme: "file", Path: abs, RawQuery: query}
	db, err := sql.Open(sqliteDriver, uri.String())
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var version sql.NullInt64
	err = db.QueryRow(`SELECT MAX(schema_version) FROM schemamigrations`).Scan(&version)
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return 0, nil
	}
	return int(version.Int64), err
}
