// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// The service database keeps item IDs, the change log, clients, leases, uploads, operation receipts and the mutation
// journal. It lives next to Syncthing's database but is separate from it: Syncthing owns its own storage, migrations
// and resets, and the two never share a transaction.
type store struct {
	db  *sql.DB
	now func() time.Time
}

const schemaVersion = 1

// SQLiteDriver is the database/sql driver name the service (and Syncthing, in the same build) uses
const SQLiteDriver = driverName

var schema = []string{
	`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID`,
	`CREATE TABLE folders (
		folder_id TEXT PRIMARY KEY,
		root_item TEXT NOT NULL,
		index_id INTEGER NOT NULL DEFAULT 0,
		local_sequence INTEGER NOT NULL DEFAULT 0,
		seen_sequence INTEGER NOT NULL DEFAULT 0,
		pruned_below INTEGER NOT NULL DEFAULT 0,
		swept_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE items (
		item_id TEXT PRIMARY KEY,
		folder_id TEXT NOT NULL,
		path TEXT NOT NULL,
		parent_item TEXT,
		kind INTEGER NOT NULL,
		deleted INTEGER NOT NULL DEFAULT 0,
		st_sequence INTEGER NOT NULL DEFAULT 0,
		revision TEXT NOT NULL,
		content_id TEXT NOT NULL DEFAULT '',
		size INTEGER NOT NULL DEFAULT 0,
		mod_ns INTEGER NOT NULL DEFAULT 0,
		mode INTEGER NOT NULL DEFAULT 0,
		subtree_gen INTEGER NOT NULL DEFAULT 0,
		change_seq INTEGER NOT NULL,
		changed_at INTEGER NOT NULL,
		sweep INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE UNIQUE INDEX items_live_path ON items(folder_id, path) WHERE deleted = 0`,
	`CREATE INDEX items_changes ON items(folder_id, change_seq)`,
	`CREATE INDEX items_parent ON items(parent_item) WHERE deleted = 0`,
	`CREATE TABLE snapshots (
		snapshot_id TEXT PRIMARY KEY,
		client_id TEXT NOT NULL,
		folder_id TEXT NOT NULL,
		generation INTEGER NOT NULL,
		cursor_seq INTEGER NOT NULL,
		created_at INTEGER NOT NULL
	)`,
	`CREATE TABLE snapshot_pages (
		page_id TEXT PRIMARY KEY,
		snapshot_id TEXT NOT NULL REFERENCES snapshots ON DELETE CASCADE,
		after_path TEXT NOT NULL
	)`,
	`CREATE TABLE exports (folder_id TEXT PRIMARY KEY, allow_write INTEGER NOT NULL) WITHOUT ROWID`,
	`CREATE TABLE clients (
		client_id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		platform TEXT NOT NULL,
		credential_hash BLOB UNIQUE,
		created_at INTEGER NOT NULL,
		last_seen INTEGER NOT NULL DEFAULT 0,
		revoked_at INTEGER
	)`,
	`CREATE TABLE permissions (
		client_id TEXT NOT NULL REFERENCES clients ON DELETE CASCADE,
		folder_id TEXT NOT NULL,
		can_write INTEGER NOT NULL,
		PRIMARY KEY (client_id, folder_id)
	) WITHOUT ROWID`,
	`CREATE TABLE pairing_sessions (
		session_id TEXT PRIMARY KEY,
		secret_hash BLOB NOT NULL UNIQUE,
		attempts INTEGER NOT NULL DEFAULT 0,
		used INTEGER NOT NULL DEFAULT 0,
		expires_at INTEGER NOT NULL
	)`,
	`CREATE TABLE pairing_requests (
		request_id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		poll_hash BLOB NOT NULL UNIQUE,
		client_id TEXT NOT NULL,
		client_name TEXT NOT NULL,
		platform TEXT NOT NULL,
		state TEXT NOT NULL,
		grants TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	)`,
	`CREATE TABLE leases (
		lease_id TEXT PRIMARY KEY,
		token_hash BLOB NOT NULL UNIQUE,
		client_id TEXT NOT NULL,
		folder_id TEXT NOT NULL,
		item_id TEXT NOT NULL,
		revision TEXT NOT NULL,
		content_id TEXT NOT NULL,
		path TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	)`,
	`CREATE UNIQUE INDEX leases_key ON leases(client_id, item_id, revision)`,
	`CREATE TABLE uploads (
		upload_id TEXT PRIMARY KEY,
		token_hash BLOB NOT NULL UNIQUE,
		client_id TEXT NOT NULL,
		folder_id TEXT NOT NULL,
		length INTEGER NOT NULL,
		received INTEGER NOT NULL DEFAULT 0,
		digest BLOB NOT NULL,
		complete INTEGER NOT NULL DEFAULT 0,
		pinned_by TEXT,
		created_at INTEGER NOT NULL,
		touched_at INTEGER NOT NULL
	)`,
	`CREATE TABLE operations (
		op_id TEXT PRIMARY KEY,
		client_id TEXT NOT NULL,
		client_op TEXT NOT NULL,
		kind TEXT NOT NULL,
		folder_id TEXT NOT NULL,
		payload TEXT NOT NULL,
		payload_digest BLOB NOT NULL,
		state TEXT NOT NULL,
		journal TEXT NOT NULL,
		journal_data TEXT NOT NULL DEFAULT '{}',
		result TEXT NOT NULL DEFAULT '',
		http_status INTEGER NOT NULL DEFAULT 0,
		generation INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE UNIQUE INDEX operations_dedup ON operations(client_id, client_op)`,
	`CREATE INDEX operations_open ON operations(state) WHERE state = 'pending'`,
	`CREATE TABLE audit (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		at INTEGER NOT NULL,
		client_id TEXT NOT NULL,
		action TEXT NOT NULL,
		operation TEXT NOT NULL DEFAULT '',
		item_id TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL
	)`,
}

// Keys of the meta table
const (
	metaServiceID  = "service_id"
	metaGeneration = "authority_generation"
	metaNextSeq    = "next_change_seq"
)

var errUnsupportedSchema = errors.New("the service database was created by a newer version")

func openStore(path string) (*store, error) {
	u := url.URL{Scheme: "file", Path: path, RawQuery: driverOptions}
	db, err := sql.Open(driverName, u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	s := &store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *store) close() error {
	return s.db.Close()
}

// Tables and indexes added after schema 1 shipped, created where missing
var addedSchema = []string{
	`CREATE INDEX IF NOT EXISTS snapshot_pages_snapshot ON snapshot_pages(snapshot_id)`,
	// Items that moved to another folder, as tombstones in the change log of the folder they left. The item itself
	// (in items) is in its new folder, under the same ID.
	`CREATE TABLE IF NOT EXISTS moved_out (
		folder_id TEXT NOT NULL,
		item_id TEXT NOT NULL,
		path TEXT NOT NULL,
		parent_item TEXT,
		kind INTEGER NOT NULL,
		to_folder TEXT NOT NULL,
		revision TEXT NOT NULL,
		change_seq INTEGER NOT NULL,
		changed_at INTEGER NOT NULL,
		PRIMARY KEY (folder_id, item_id)
	)`,
	`CREATE INDEX IF NOT EXISTS moved_out_changes ON moved_out(folder_id, change_seq)`,
}

func (s *store) migrate() error {
	return s.tx(context.Background(), func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			return err
		}
		switch {
		case version == schemaVersion:
			for _, statement := range addedSchema {
				if _, err := tx.Exec(statement); err != nil {
					return err
				}
			}
			return nil
		case version > schemaVersion:
			return fmt.Errorf("%w (schema %d)", errUnsupportedSchema, version)
		case version != 0:
			return fmt.Errorf("no migration from schema %d", version)
		}
		for _, statement := range append(schema, addedSchema...) {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("creating the service database: %w", err)
			}
		}
		for key, value := range map[string]string{
			metaServiceID:  newID(prefixService),
			metaGeneration: "1",
			metaNextSeq:    "1",
			metaTokenKey:   b64.EncodeToString(randomBytes(32)),
		} {
			if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`, key, value); err != nil {
				return err
			}
		}
		_, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
		return err
	})
}

// tx runs fn in a write transaction (BEGIN IMMEDIATE). Transactions must stay short: never wait for the network or a
// folder in one.
func (s *store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Also releases the write lock if fn panics; a no-op after Commit
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func metaValue(q queryer, key string) (string, error) {
	var value string
	err := q.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	return value, err
}

func metaInt(q queryer, key string) (int64, error) {
	value, err := metaValue(q, key)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(value, 10, 64)
}

func setMeta(tx *sql.Tx, key string, value string) error {
	_, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// nextChangeSeq allocates count change log sequence numbers and returns the first
func nextChangeSeq(tx *sql.Tx, count int64) (int64, error) {
	next, err := metaInt(tx, metaNextSeq)
	if err != nil {
		return 0, err
	}
	if err := setMeta(tx, metaNextSeq, strconv.FormatInt(next+count, 10)); err != nil {
		return 0, err
	}
	return next, nil
}

func (s *store) serviceID() (string, error) {
	return metaValue(s.db, metaServiceID)
}

func (s *store) generation() (int64, error) {
	return metaInt(s.db, metaGeneration)
}

// headSequence is the last allocated change sequence
func headSequence(q queryer) (int64, error) {
	next, err := metaInt(q, metaNextSeq)
	return next - 1, err
}

// bumpGeneration starts a new authority generation: all cursors become invalid
func bumpGeneration(tx *sql.Tx) (int64, error) {
	gen, err := metaInt(tx, metaGeneration)
	if err != nil {
		return 0, err
	}
	gen++
	return gen, setMeta(tx, metaGeneration, strconv.FormatInt(gen, 10))
}

// What both *sql.DB and *sql.Tx offer for reading
type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}
