// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"path/filepath"
	"testing"
)

// An index made by an earlier version is brought up to date when it is opened
func TestMigratingAnIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "files.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// As schema 2 left it
	for _, statement := range []string{
		`ALTER TABLE outbox DROP COLUMN awaited_generation`,
		`ALTER TABLE outbox DROP COLUMN resends`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	if s, err = OpenStore(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema %d after opening: %v", version, err)
	}
	rows, err := s.db.Query(`SELECT ` + outboxColumns + ` FROM outbox`)
	if err != nil {
		t.Fatalf("the outbox wasn't brought up to date: %v", err)
	}
	rows.Close()
}
