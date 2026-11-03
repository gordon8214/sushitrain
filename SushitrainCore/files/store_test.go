// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package files

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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
		`DELETE FROM meta WHERE key = '` + metaNextFolder + `'`,
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
	if next, err := metaValue(s.db, metaNextFolder); err != nil || next != "1" {
		t.Fatalf("the next folder ID is %q: %v", next, err)
	}
}

// A folder added after one was removed doesn't get its ID, which something left of the removed one could refer to
func TestFolderIDsAreNotUsedAgain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	g := writableGateway(t, map[string]string{"doc.txt": "doc"})
	ts := newTestService(t, g, 0)
	removed := ts.folder.ID
	if _, _, err := ts.RemoveFolder(removed); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	added, err := ts.AddFolder(ctx, ts.folder.ServiceID, g.Engine.FolderID)
	if err != nil {
		t.Fatal(err)
	}
	if added.ID == removed {
		t.Fatalf("the new folder got the removed one's ID %d", removed)
	}
	ts.folder = added
	ts.refresh()
	if got := ts.readPath("docs/doc.txt"); got != "doc" {
		t.Fatalf("doc.txt reads %q", got)
	}
}
